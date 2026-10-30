package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	doctorstore "github.com/radryc/doctor/internal/store"
	"sigs.k8s.io/yaml"
)

// Querier executes instant PromQL queries through doctor-query.
type Querier interface {
	PromInstant(ctx context.Context, query string, at time.Time) (data map[string]any, partial bool, err error)
}

// Duration unmarshals from Go duration strings or numeric seconds.
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
			return fmt.Errorf("alerting: invalid duration %q: %w", value, err)
		}
		*d = Duration(parsed)
	case float64:
		*d = Duration(time.Duration(value * float64(time.Second)))
	default:
		return fmt.Errorf("alerting: invalid duration %v", raw)
	}
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// RuleFile is one alert rule group file.
type RuleFile struct {
	Version int         `json:"version" yaml:"version"`
	Groups  []RuleGroup `json:"groups" yaml:"groups"`
}

// RuleGroup is a set of alert rules evaluated on one interval.
type RuleGroup struct {
	Name      string      `json:"name" yaml:"name"`
	Tenant    string      `json:"tenant" yaml:"tenant"`
	Partition string      `json:"partition" yaml:"partition"`
	Interval  Duration    `json:"interval,omitempty" yaml:"interval,omitempty"`
	Rules     []AlertRule `json:"rules" yaml:"rules"`
}

// AlertRule is one Sentinel PromQL alert rule.
type AlertRule struct {
	Alert         string            `json:"alert" yaml:"alert"`
	Expr          string            `json:"expr" yaml:"expr"`
	For           Duration          `json:"for,omitempty" yaml:"for,omitempty"`
	KeepFiringFor Duration          `json:"keep_firing_for,omitempty" yaml:"keep_firing_for,omitempty"`
	Labels        map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	Notify        []string          `json:"notify,omitempty" yaml:"notify,omitempty"`
}

var alertNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// ParseRuleFile parses and validates a rule group document.
func ParseRuleFile(data []byte) (*RuleFile, error) {
	var file RuleFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("alerting: parse rules: %w", err)
	}
	if file.Version != 0 && file.Version != 1 {
		return nil, fmt.Errorf("alerting: unsupported rules version %d", file.Version)
	}
	if err := file.Validate(); err != nil {
		return nil, err
	}
	return &file, nil
}

// Validate checks the rule group document.
func (f *RuleFile) Validate() error {
	if len(f.Groups) == 0 {
		return fmt.Errorf("alerting: rules file has no groups")
	}
	seenGroups := map[string]struct{}{}
	for i := range f.Groups {
		group := &f.Groups[i]
		if strings.TrimSpace(group.Name) == "" {
			return fmt.Errorf("alerting: group[%d] has no name", i)
		}
		if _, exists := seenGroups[group.Name]; exists {
			return fmt.Errorf("alerting: duplicate group %q", group.Name)
		}
		seenGroups[group.Name] = struct{}{}
		if strings.TrimSpace(group.Partition) == "" {
			return fmt.Errorf("alerting: group %q has no partition", group.Name)
		}
		if group.Interval > 0 && time.Duration(group.Interval) < 5*time.Second {
			return fmt.Errorf("alerting: group %q interval must be at least 5s", group.Name)
		}
		if len(group.Rules) == 0 {
			return fmt.Errorf("alerting: group %q has no rules", group.Name)
		}
		seenRules := map[string]struct{}{}
		for j := range group.Rules {
			rule := &group.Rules[j]
			if strings.TrimSpace(rule.Alert) == "" {
				return fmt.Errorf("alerting: group %q rule[%d] has no alert name", group.Name, j)
			}
			if !alertNamePattern.MatchString(rule.Alert) {
				return fmt.Errorf("alerting: group %q rule %q has invalid alert name", group.Name, rule.Alert)
			}
			if strings.TrimSpace(rule.Expr) == "" {
				return fmt.Errorf("alerting: group %q rule %q has no expr", group.Name, rule.Alert)
			}
			if _, exists := seenRules[rule.Alert]; exists {
				return fmt.Errorf("alerting: group %q has duplicate alert %q", group.Name, rule.Alert)
			}
			seenRules[rule.Alert] = struct{}{}
			if rule.For < 0 || rule.KeepFiringFor < 0 {
				return fmt.Errorf("alerting: group %q rule %q has negative duration", group.Name, rule.Alert)
			}
		}
	}
	return nil
}

// IntervalOrDefault returns the group interval, defaulting to one minute.
func (g RuleGroup) IntervalOrDefault() time.Duration {
	if g.Interval > 0 {
		return time.Duration(g.Interval)
	}
	return time.Minute
}

// RuleGroupRef identifies one stored rule group.
type RuleGroupRef struct {
	Tenant    string
	Partition string
	Group     string
	File      *RuleFile
}

func (s *Store) rulePath(tenant, partition, group string) string {
	return fmt.Sprintf("%s/rules/%s/%s/%s.yaml", s.Root, tenant, partition, group)
}

// GetRuleGroup loads one stored rule group.
func (s *Store) GetRuleGroup(ctx context.Context, tenant, partition, group string) (*RuleFile, error) {
	data, err := s.Files.ReadFile(ctx, s.rulePath(tenant, partition, group))
	if err != nil {
		if isNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	file, err := ParseRuleFile(data)
	if err != nil {
		return nil, err
	}
	for i := range file.Groups {
		if file.Groups[i].Tenant == "" {
			file.Groups[i].Tenant = tenant
		}
		if file.Groups[i].Partition == "" {
			file.Groups[i].Partition = partition
		}
	}
	return file, nil
}

// PutRuleGroup stores one rule group.
func (s *Store) PutRuleGroup(ctx context.Context, tenant, partition, group string, file *RuleFile) error {
	if err := file.Validate(); err != nil {
		return err
	}
	for i := range file.Groups {
		file.Groups[i].Tenant = tenant
		file.Groups[i].Partition = partition
	}
	data, err := yaml.Marshal(file)
	if err != nil {
		return err
	}
	path := s.rulePath(tenant, partition, group)
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "alert rule update"})
	return err
}

// DeleteRuleGroup removes one rule group.
func (s *Store) DeleteRuleGroup(ctx context.Context, tenant, partition, group string) error {
	path := s.rulePath(tenant, partition, group)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return ErrNotFound
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
		LogicalPath:       path,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "alert rule delete"})
	return err
}

// ListRuleGroups returns every rule group, optionally scoped to one tenant.
func (s *Store) ListRuleGroups(ctx context.Context, tenant string) ([]RuleGroupRef, error) {
	root := s.Root + "/rules"
	tenants, err := s.Files.ListDir(ctx, root)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []RuleGroupRef
	for _, tenantEntry := range tenants {
		if !tenantEntry.IsDir || (tenant != "" && tenantEntry.Name != tenant) {
			continue
		}
		partitions, err := s.Files.ListDir(ctx, root+"/"+tenantEntry.Name)
		if err != nil {
			continue
		}
		for _, partitionEntry := range partitions {
			if !partitionEntry.IsDir {
				continue
			}
			files, err := s.Files.ListDir(ctx, root+"/"+tenantEntry.Name+"/"+partitionEntry.Name)
			if err != nil {
				continue
			}
			for _, entry := range files {
				if entry.IsDir || !strings.HasSuffix(entry.Name, ".yaml") {
					continue
				}
				group := strings.TrimSuffix(entry.Name, ".yaml")
				file, err := s.GetRuleGroup(ctx, tenantEntry.Name, partitionEntry.Name, group)
				if err != nil {
					continue
				}
				out = append(out, RuleGroupRef{Tenant: tenantEntry.Name, Partition: partitionEntry.Name, Group: group, File: file})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		if out[i].Partition != out[j].Partition {
			return out[i].Partition < out[j].Partition
		}
		return out[i].Group < out[j].Group
	})
	return out, nil
}

// RuleEvalResult summarizes one rule evaluation.
type RuleEvalResult struct {
	Tenant    string `json:"tenant"`
	Partition string `json:"partition"`
	Group     string `json:"group"`
	Rule      string `json:"rule"`
	State     string `json:"state,omitempty"`
	Samples   int    `json:"samples"`
	Partial   bool   `json:"partial,omitempty"`
	Error     string `json:"error,omitempty"`
}

// EvaluateRuleGroup evaluates all rules in one group.
func (s *Service) EvaluateRuleGroup(ctx context.Context, group RuleGroup) []RuleEvalResult {
	results := make([]RuleEvalResult, 0, len(group.Rules))
	for _, rule := range group.Rules {
		results = append(results, s.evaluateRule(ctx, group, rule))
	}
	return results
}

func (s *Service) evaluateRule(ctx context.Context, group RuleGroup, rule AlertRule) RuleEvalResult {
	result := RuleEvalResult{Tenant: group.Tenant, Partition: group.Partition, Group: group.Name, Rule: rule.Alert}
	if s.Querier == nil {
		result.Error = "query backend is not configured"
		return result
	}
	now := s.now()
	data, partial, err := s.Querier.PromInstant(ctx, rule.Expr, now)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Partial = partial
	if partial {
		// Partial data must not resolve or newly fire alerts.
		return result
	}
	items := prometheusResultSlice(data)
	result.Samples = len(items)
	if len(items) == 0 {
		s.resolveRuleAlerts(ctx, group, rule, now)
		return result
	}
	for _, item := range items {
		mapItem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		metric, _ := mapItem["metric"].(map[string]any)
		value, _, err := promSampleValue(mapItem)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		labels := make(map[string]string, len(rule.Labels)+len(metric)+5)
		for key, val := range metric {
			if key == "__name__" {
				continue
			}
			if str, ok := val.(string); ok {
				labels[key] = str
			}
		}
		for key, val := range rule.Labels {
			labels[key] = val
		}
		labels["alertname"] = rule.Alert
		labels["rule"] = rule.Alert
		labels["rule_group"] = group.Name
		labels["partition"] = group.Partition
		labels["source"] = "sentinel"
		existing, _ := s.Store.GetAlert(ctx, group.Tenant, group.Partition, Fingerprint("sentinel", group.Tenant, group.Partition, labels))
		status := StatusFiring
		startsAt := now
		if rule.For > 0 {
			if existing != nil && (existing.Status == StatusPending || existing.Status == StatusFiring) {
				startsAt = existing.StartsAt
				if now.Sub(startsAt) < time.Duration(rule.For) {
					status = StatusPending
				}
			} else {
				status = StatusPending
			}
		}
		severity := labels["severity"]
		if severity == "" {
			severity = "warning"
		}
		annotations := map[string]string{}
		for key, val := range rule.Annotations {
			annotations[key] = val
		}
		lastSeen := now
		alert := Alert{
			Fingerprint: Fingerprint("sentinel", group.Tenant, group.Partition, labels),
			Source:      "sentinel",
			Tenant:      group.Tenant,
			Partition:   group.Partition,
			Status:      status,
			Severity:    severity,
			Labels:      labels,
			Annotations: annotations,
			StartsAt:    startsAt,
			Value:       strconv.FormatFloat(value, 'g', -1, 64),
			Delivery:    DeliverySentinel,
			LastSeenAt:  &lastSeen,
		}
		alert.ID = alert.Fingerprint
		if _, err := s.Apply(ctx, alert); err != nil {
			result.Error = err.Error()
			return result
		}
		result.State = status
	}
	return result
}

func (s *Service) resolveRuleAlerts(ctx context.Context, group RuleGroup, rule AlertRule, now time.Time) {
	alerts, err := s.Store.ListAlerts(ctx, group.Tenant, group.Partition)
	if err != nil {
		return
	}
	for _, alert := range alerts {
		if alert.Labels["rule"] != rule.Alert || alert.Labels["rule_group"] != group.Name {
			continue
		}
		if alert.Status == StatusResolved {
			continue
		}
		if rule.KeepFiringFor > 0 && alert.LastSeenAt != nil && now.Sub(*alert.LastSeenAt) <= time.Duration(rule.KeepFiringFor) {
			continue
		}
		alert.Status = StatusResolved
		alert.EndsAt = &now
		_, _ = s.Apply(ctx, alert)
	}
}

func prometheusResultSlice(data map[string]any) []any {
	if data == nil {
		return nil
	}
	inner, _ := data["data"].(map[string]any)
	result, _ := inner["result"].([]any)
	return result
}

func promSampleValue(item map[string]any) (float64, float64, error) {
	value, ok := item["value"].([]any)
	if !ok || len(value) != 2 {
		return 0, 0, fmt.Errorf("alerting: malformed sample %#v", item["value"])
	}
	ts, err := numericValue(value[0])
	if err != nil {
		return 0, 0, err
	}
	sample, err := numericValue(value[1])
	if err != nil {
		return 0, 0, err
	}
	return sample, ts, nil
}

func numericValue(value any) (float64, error) {
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case string:
		return strconv.ParseFloat(typed, 64)
	default:
		return 0, fmt.Errorf("alerting: invalid numeric value %v", value)
	}
}

// RuleScheduler periodically evaluates all stored rule groups.
type RuleScheduler struct {
	Load       func(context.Context) ([]RuleGroupRef, error)
	Evaluate   func(context.Context, RuleGroup) []RuleEvalResult
	Interval   time.Duration
	ShardIndex int
	ShardCount int
	Logger     *log.Logger
	mu         sync.Mutex
	groups     int
	lastLoad   time.Time
	lastError  string
}

// RuleSchedulerStats reports scheduler state.
type RuleSchedulerStats struct {
	Groups    int       `json:"groups"`
	LastLoad  time.Time `json:"lastLoad"`
	LastError string    `json:"lastError,omitempty"`
}

// Stats returns a snapshot.
func (s *RuleScheduler) Stats() RuleSchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RuleSchedulerStats{Groups: s.groups, LastLoad: s.lastLoad, LastError: s.lastError}
}

// Run evaluates rule groups until ctx is cancelled.
func (s *RuleScheduler) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	s.evaluateAll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.evaluateAll(ctx)
		}
	}
}

func (s *RuleScheduler) evaluateAll(ctx context.Context) {
	refs, err := s.Load(ctx)
	s.mu.Lock()
	s.lastLoad = time.Now().UTC()
	if err != nil {
		s.lastError = err.Error()
	} else {
		s.lastError = ""
		s.groups = len(refs)
	}
	s.mu.Unlock()
	if err != nil {
		s.logf("rule load: %v", err)
		return
	}
	for _, ref := range refs {
		if ref.File == nil {
			continue
		}
		if ShardFor(ref.Tenant, ref.Partition, ref.Group, s.ShardCount) != s.ShardIndex {
			continue
		}
		for _, group := range ref.File.Groups {
			for _, result := range s.Evaluate(ctx, group) {
				if result.Error != "" {
					s.logf("rule group=%s rule=%s: %s", group.Name, result.Rule, result.Error)
				}
			}
		}
	}
}

func (s *RuleScheduler) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf("sentinel rules: "+format, args...)
	}
}

func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
