package alerting

import (
	"fmt"
	"regexp"
	"strings"
)

// AWSPolicyMode controls the default CloudWatch alarm selection.
type AWSPolicyMode string

const (
	AWSPolicyManagedOnly AWSPolicyMode = "managed-only"
	AWSPolicyImportAll   AWSPolicyMode = "import-all"
	AWSPolicyIgnoreAll   AWSPolicyMode = "ignore-all"
)

// AWSPolicyRule matches alarms by account, region, name, tag, or type.
type AWSPolicyRule struct {
	Account   string `json:"account,omitempty" yaml:"account,omitempty"`
	Region    string `json:"region,omitempty" yaml:"region,omitempty"`
	NameRegex string `json:"nameRegex,omitempty" yaml:"nameRegex,omitempty"`
	Tag       string `json:"tag,omitempty" yaml:"tag,omitempty"`
	Equals    string `json:"equals,omitempty" yaml:"equals,omitempty"`
	AlarmType string `json:"alarmType,omitempty" yaml:"alarmType,omitempty"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Metric    string `json:"metric,omitempty" yaml:"metric,omitempty"`
}

// AWSPolicy selects which CloudWatch alarms Sentinel processes.
type AWSPolicy struct {
	Source   string          `json:"source,omitempty" yaml:"source,omitempty"`
	Default  AWSPolicyMode   `json:"default" yaml:"default"`
	Include  []AWSPolicyRule `json:"include,omitempty" yaml:"include,omitempty"`
	Exclude  []AWSPolicyRule `json:"exclude,omitempty" yaml:"exclude,omitempty"`
	Unscoped string          `json:"unscoped,omitempty" yaml:"unscoped,omitempty"`
}

// DefaultAWSPolicy is the safe default: only managed/opted-in alarms.
func DefaultAWSPolicy() *AWSPolicy {
	return &AWSPolicy{
		Source:   "aws",
		Default:  AWSPolicyManagedOnly,
		Unscoped: "ignore",
	}
}

// Validate checks the policy.
func (p AWSPolicy) Validate() error {
	switch p.Default {
	case "", AWSPolicyManagedOnly, AWSPolicyImportAll, AWSPolicyIgnoreAll:
	default:
		return fmt.Errorf("alerting: invalid AWS alarm selection mode %q", p.Default)
	}
	return nil
}

// AlarmMeta is the subset of CloudWatch alarm metadata used for selection.
type AlarmMeta struct {
	Name      string
	ARN       string
	Account   string
	Region    string
	Type      string
	Namespace string
	Metric    string
	Tags      map[string]string
}

// Selection is the result of evaluating the policy.
type Selection struct {
	Included bool
	Reason   string
}

// Select evaluates the policy. Exclude always wins.
func (p AWSPolicy) Select(meta AlarmMeta) Selection {
	if strings.EqualFold(meta.Tags["sentinel.io/import"], "false") {
		return Selection{Reason: "sentinel.io/import=false"}
	}
	for _, rule := range p.Exclude {
		if rule.matches(meta) {
			return Selection{Reason: "exclude:" + rule.describe()}
		}
	}
	if strings.EqualFold(meta.Tags["sentinel.io/import"], "true") {
		return Selection{Included: true, Reason: "sentinel.io/import=true"}
	}
	for _, rule := range p.Include {
		if rule.matches(meta) {
			return Selection{Included: true, Reason: "include:" + rule.describe()}
		}
	}
	switch p.effectiveDefault() {
	case AWSPolicyImportAll:
		return Selection{Included: true, Reason: "default:import-all"}
	case AWSPolicyIgnoreAll:
		return Selection{Reason: "default:ignore-all"}
	default:
		if strings.EqualFold(meta.Tags["guardian.io/managed-by"], "guardian") ||
			strings.EqualFold(meta.Tags["sentinel.io/rule-pack"], "default") {
			return Selection{Included: true, Reason: "managed-only"}
		}
		return Selection{Reason: "default:managed-only"}
	}
}

func (p AWSPolicy) effectiveDefault() AWSPolicyMode {
	if p.Default == "" {
		return AWSPolicyManagedOnly
	}
	return p.Default
}

func (r AWSPolicyRule) matches(meta AlarmMeta) bool {
	if r.Account != "" && r.Account != meta.Account {
		return false
	}
	if r.Region != "" && r.Region != meta.Region {
		return false
	}
	if r.AlarmType != "" && !strings.EqualFold(r.AlarmType, meta.Type) {
		return false
	}
	if r.Namespace != "" && r.Namespace != meta.Namespace {
		return false
	}
	if r.Metric != "" && r.Metric != meta.Metric {
		return false
	}
	if r.NameRegex != "" {
		re, err := regexp.Compile(r.NameRegex)
		if err != nil || !re.MatchString(meta.Name) {
			return false
		}
	}
	if r.Tag != "" {
		value, ok := meta.Tags[r.Tag]
		if !ok {
			return false
		}
		if r.Equals != "" && value != r.Equals {
			return false
		}
	}
	return true
}

func (r AWSPolicyRule) describe() string {
	parts := make([]string, 0, 5)
	for _, value := range []string{r.Account, r.Region, r.NameRegex, r.Tag, r.Equals, r.AlarmType} {
		if value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "/")
}
