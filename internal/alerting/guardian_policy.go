package alerting

import (
	"fmt"
	"strings"
	"time"
)

// GuardianPolicy tunes Sentinel's Guardian partition source for one partition.
// Guardian partition alerts are source-generated rather than PromQL rules, so
// this document is where per-partition trigger timing lives.
type GuardianPolicy struct {
	Version   int    `json:"version" yaml:"version"`
	Tenant    string `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	Partition string `json:"partition,omitempty" yaml:"partition,omitempty"`
	// AlertOnLocked raises GuardianPartitionAttention for partitions whose only
	// problem is drift on a locked intent. Locked drift is intentional by
	// default, so this is false unless a partition explicitly opts in.
	AlertOnLocked bool `json:"alertOnLocked,omitempty" yaml:"alertOnLocked,omitempty"`
	// Alerts configures individual Guardian alert names.
	Alerts map[string]GuardianAlertPolicy `json:"alerts,omitempty" yaml:"alerts,omitempty"`
}

// GuardianAlertPolicy is the per-alert timing configuration.
type GuardianAlertPolicy struct {
	// For delays firing until the condition has been observed continuously for
	// this long. Zero fires on the first observation.
	For Duration `json:"for,omitempty" yaml:"for,omitempty"`
}

// DefaultGuardianPolicy is the safe default: locked drift is ignored and every
// alert fires on the first observation.
func DefaultGuardianPolicy() *GuardianPolicy {
	return &GuardianPolicy{
		Version: 1,
		Alerts:  map[string]GuardianAlertPolicy{},
	}
}

// Validate checks the policy document.
func (p *GuardianPolicy) Validate() error {
	if p == nil {
		return nil
	}
	for name, alert := range p.Alerts {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("alerting: guardian policy has an alert with no name")
		}
		if alert.For < 0 {
			return fmt.Errorf("alerting: guardian policy for %q has a negative duration", name)
		}
	}
	return nil
}

// For returns the configured trigger delay for one Guardian alert name.
func (p *GuardianPolicy) For(alertName string) time.Duration {
	if p == nil {
		return 0
	}
	alert, ok := p.Alerts[alertName]
	if !ok {
		return 0
	}
	return time.Duration(alert.For)
}
