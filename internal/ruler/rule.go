// Package ruler evaluates recording rules per shard, merges the results, and
// writes derived metrics back to storage.
package ruler

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// MergeStrategy controls how per-shard results for the same output series are
// combined.
type MergeStrategy string

const (
	// MergeSum adds values across shards (counter/rate-safe).
	MergeSum MergeStrategy = "sum"
	// MergeMax keeps the element-wise maximum.
	MergeMax MergeStrategy = "max"
	// MergeMin keeps the element-wise minimum.
	MergeMin MergeStrategy = "min"
	// MergeLast keeps the sample with the newest timestamp.
	MergeLast MergeStrategy = "last"
	// MergeKeep keeps per-shard series with their shard label.
	MergeKeep MergeStrategy = "keep"
)

// Duration is a time.Duration that unmarshals from Go duration strings or
// numeric seconds.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch value := raw.(type) {
	case string:
		parsed, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("ruler: invalid duration %q: %w", value, err)
		}
		*d = Duration(parsed)
	case float64:
		*d = Duration(time.Duration(value * float64(time.Second)))
	default:
		return fmt.Errorf("ruler: invalid duration %v", raw)
	}
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// File is one rule group file.
type File struct {
	Version int     `json:"version" yaml:"version"`
	Groups  []Group `json:"groups" yaml:"groups"`
}

// Group is a set of rules evaluated on one interval.
type Group struct {
	Name            string   `json:"name" yaml:"name"`
	Tenant          string   `json:"tenant" yaml:"tenant"`
	Interval        Duration `json:"interval,omitempty" yaml:"interval,omitempty"`
	EvaluationDelay Duration `json:"evaluation_delay,omitempty" yaml:"evaluation_delay,omitempty"`
	Rules           []Rule   `json:"rules" yaml:"rules"`
}

// Rule is a single recording rule.
type Rule struct {
	Record  string            `json:"record" yaml:"record"`
	Expr    string            `json:"expr" yaml:"expr"`
	Labels  map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Sources []string          `json:"sources,omitempty" yaml:"sources,omitempty"`
	Merge   MergeStrategy     `json:"merge,omitempty" yaml:"merge,omitempty"`
}

var metricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// ParseFile parses and validates a rule group document.
func ParseFile(data []byte) (*File, error) {
	var file File
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("ruler: parse rules: %w", err)
	}
	if file.Version != 0 && file.Version != 1 {
		return nil, fmt.Errorf("ruler: unsupported rules version %d (expected 1)", file.Version)
	}
	if err := file.Validate(); err != nil {
		return nil, err
	}
	return &file, nil
}

// Validate checks the rule document.
func (f *File) Validate() error {
	if len(f.Groups) == 0 {
		return fmt.Errorf("ruler: rules file has no groups")
	}
	seenGroups := make(map[string]struct{}, len(f.Groups))
	for i := range f.Groups {
		group := &f.Groups[i]
		if strings.TrimSpace(group.Name) == "" {
			return fmt.Errorf("ruler: group[%d] has no name", i)
		}
		if _, exists := seenGroups[group.Name]; exists {
			return fmt.Errorf("ruler: duplicate group %q", group.Name)
		}
		seenGroups[group.Name] = struct{}{}
		if group.Interval > 0 && time.Duration(group.Interval) < 5*time.Second {
			return fmt.Errorf("ruler: group %q interval must be at least 5s", group.Name)
		}
		if len(group.Rules) == 0 {
			return fmt.Errorf("ruler: group %q has no rules", group.Name)
		}
		seenRules := make(map[string]struct{}, len(group.Rules))
		for j := range group.Rules {
			rule := &group.Rules[j]
			if !metricNamePattern.MatchString(rule.Record) {
				return fmt.Errorf("ruler: group %q rule[%d] has invalid record name %q", group.Name, j, rule.Record)
			}
			if strings.TrimSpace(rule.Expr) == "" {
				return fmt.Errorf("ruler: group %q rule %q has no expr", group.Name, rule.Record)
			}
			if _, exists := seenRules[rule.Record]; exists {
				return fmt.Errorf("ruler: group %q has duplicate record %q", group.Name, rule.Record)
			}
			seenRules[rule.Record] = struct{}{}
			if !rule.mergeStrategy().Valid() {
				return fmt.Errorf("ruler: group %q rule %q has invalid merge strategy %q", group.Name, rule.Record, rule.Merge)
			}
		}
	}
	return nil
}

// IntervalOrDefault returns the group interval, defaulting to one minute.
func (g Group) IntervalOrDefault() time.Duration {
	if g.Interval > 0 {
		return time.Duration(g.Interval)
	}
	return time.Minute
}

// EvaluationDelayOrDefault returns the evaluation delay.
func (g Group) EvaluationDelayOrDefault() time.Duration {
	if g.EvaluationDelay > 0 {
		return time.Duration(g.EvaluationDelay)
	}
	return 0
}

func (r Rule) mergeStrategy() MergeStrategy {
	if r.Merge == "" {
		return MergeSum
	}
	return r.Merge
}

// MergeStrategy returns the effective merge strategy.
func (r Rule) MergeStrategy() MergeStrategy { return r.mergeStrategy() }

// Valid reports whether the strategy is recognized.
func (s MergeStrategy) Valid() bool {
	switch s {
	case MergeSum, MergeMax, MergeMin, MergeLast, MergeKeep:
		return true
	default:
		return false
	}
}
