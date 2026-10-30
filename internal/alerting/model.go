// Package alerting implements Doctor Sentinel: alert intake, normalization,
// state, routing, delivery, silences, and alert-rule evaluation.
package alerting

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Alert status values.
const (
	StatusPending  = "pending"
	StatusFiring   = "firing"
	StatusResolved = "resolved"
	StatusNoData   = "no_data"
	StatusError    = "error"
)

// Delivery ownership values.
const (
	DeliverySentinel = "sentinel"
	DeliveryNative   = "native"
)

// Alert is the canonical alert instance.
type Alert struct {
	ID             string            `json:"id"`
	Fingerprint    string            `json:"fingerprint"`
	Source         string            `json:"source"`
	Tenant         string            `json:"tenant"`
	Partition      string            `json:"partition"`
	Status         string            `json:"status"`
	Severity       string            `json:"severity"`
	Labels         map[string]string `json:"labels"`
	Annotations    map[string]string `json:"annotations,omitempty"`
	StartsAt       time.Time         `json:"startsAt"`
	EndsAt         *time.Time        `json:"endsAt,omitempty"`
	Value          string            `json:"value,omitempty"`
	Delivery       string            `json:"delivery"`
	Acked          bool              `json:"acked,omitempty"`
	AckedBy        string            `json:"ackedBy,omitempty"`
	AckedAt        *time.Time        `json:"ackedAt,omitempty"`
	SilenceIDs     []string          `json:"silenceIds,omitempty"`
	LastNotifiedAt *time.Time        `json:"lastNotifiedAt,omitempty"`
	CreatedAt      time.Time         `json:"createdAt"`
	UpdatedAt      time.Time         `json:"updatedAt"`
}

// Validate checks the fields every source must provide.
func (a *Alert) Validate() error {
	if a == nil {
		return fmt.Errorf("alerting: alert is nil")
	}
	if strings.TrimSpace(a.Source) == "" {
		return fmt.Errorf("alerting: alert source is required")
	}
	if strings.TrimSpace(a.Tenant) == "" {
		return fmt.Errorf("alerting: alert tenant is required")
	}
	if strings.TrimSpace(a.Partition) == "" {
		return fmt.Errorf("alerting: alert partition is required")
	}
	if strings.TrimSpace(a.Status) == "" {
		return fmt.Errorf("alerting: alert status is required")
	}
	if a.Labels == nil {
		a.Labels = map[string]string{}
	}
	if a.Annotations == nil {
		a.Annotations = map[string]string{}
	}
	if a.Delivery == "" {
		a.Delivery = DeliverySentinel
	}
	return nil
}

// Matcher is one silence matcher.
type Matcher struct {
	Name     string `json:"name" yaml:"name"`
	Operator string `json:"operator" yaml:"operator"`
	Value    string `json:"value" yaml:"value"`
}

// Matches reports whether the matcher matches the label set.
func (m Matcher) Matches(labels map[string]string) bool {
	value, ok := labels[m.Name]
	switch strings.TrimSpace(m.Operator) {
	case "", "=", "==":
		return ok && value == m.Value
	case "!=":
		return !ok || value != m.Value
	case "=~":
		return regexMatch(m.Value, value)
	case "!~":
		return !regexMatch(m.Value, value)
	default:
		return false
	}
}

// Silence suppresses notifications for matching alerts in one partition.
type Silence struct {
	ID        string    `json:"id" yaml:"id"`
	Tenant    string    `json:"tenant" yaml:"tenant"`
	Partition string    `json:"partition" yaml:"partition"`
	Matchers  []Matcher `json:"matchers" yaml:"matchers"`
	StartsAt  time.Time `json:"startsAt" yaml:"startsAt"`
	EndsAt    time.Time `json:"endsAt" yaml:"endsAt"`
	Scope     string    `json:"scope,omitempty" yaml:"scope,omitempty"`
	CreatedBy string    `json:"createdBy,omitempty" yaml:"createdBy,omitempty"`
	Comment   string    `json:"comment,omitempty" yaml:"comment,omitempty"`
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// Validate checks a silence document.
func (s *Silence) Validate(now time.Time) error {
	if s == nil {
		return fmt.Errorf("alerting: silence is nil")
	}
	if strings.TrimSpace(s.Tenant) == "" {
		return fmt.Errorf("alerting: silence tenant is required")
	}
	if strings.TrimSpace(s.Partition) == "" {
		return fmt.Errorf("alerting: silence partition is required")
	}
	if len(s.Matchers) == 0 {
		return fmt.Errorf("alerting: silence must have at least one matcher")
	}
	for i := range s.Matchers {
		if strings.TrimSpace(s.Matchers[i].Name) == "" {
			return fmt.Errorf("alerting: silence matcher %d has no name", i)
		}
	}
	if s.StartsAt.IsZero() {
		s.StartsAt = now
	}
	if s.EndsAt.IsZero() {
		s.EndsAt = now.Add(time.Hour)
	}
	if !s.EndsAt.After(s.StartsAt) {
		return fmt.Errorf("alerting: silence end must be after start")
	}
	return nil
}

// Active reports whether the silence is active at now.
func (s Silence) Active(now time.Time) bool {
	return !now.Before(s.StartsAt) && now.Before(s.EndsAt)
}

// Matches reports whether every matcher matches the alert labels.
func (s Silence) Matches(alert Alert) bool {
	if s.Tenant != alert.Tenant || s.Partition != alert.Partition {
		return false
	}
	for _, matcher := range s.Matchers {
		if !matcher.Matches(alert.Labels) {
			return false
		}
	}
	return true
}

// Receiver describes one notification channel.
type Receiver struct {
	Name      string            `json:"name" yaml:"name"`
	Type      string            `json:"type" yaml:"type"`
	URL       string            `json:"url,omitempty" yaml:"url,omitempty"`
	SecretEnv string            `json:"secretEnv,omitempty" yaml:"secretEnv,omitempty"`
	Headers   map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Timeout   string            `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// Route selects receivers for matching alerts.
type Route struct {
	Name           string            `json:"name" yaml:"name"`
	Matchers       map[string]string `json:"matchers,omitempty" yaml:"matchers,omitempty"`
	Receivers      []string          `json:"receivers" yaml:"receivers"`
	RepeatInterval string            `json:"repeatInterval,omitempty" yaml:"repeatInterval,omitempty"`
	Continue       bool              `json:"continue,omitempty" yaml:"continue,omitempty"`
}

// ReceiverConfig is the on-disk notification configuration.
type ReceiverConfig struct {
	Receivers []Receiver `json:"receivers" yaml:"receivers"`
	Routes    []Route    `json:"routes" yaml:"routes"`
}

// Validate checks the receiver configuration.
func (c *ReceiverConfig) Validate() error {
	seen := map[string]struct{}{}
	for i := range c.Receivers {
		r := &c.Receivers[i]
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("alerting: receiver[%d] has no name", i)
		}
		if _, ok := seen[r.Name]; ok {
			return fmt.Errorf("alerting: duplicate receiver %q", r.Name)
		}
		seen[r.Name] = struct{}{}
		if r.Type == "" {
			r.Type = "webhook"
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("alerting: route[%d] has no name", i)
		}
		for _, name := range r.Receivers {
			if _, ok := seen[name]; !ok {
				return fmt.Errorf("alerting: route %q references unknown receiver %q", r.Name, name)
			}
		}
	}
	return nil
}

// OutboxRecord is a pending notification.
type OutboxRecord struct {
	ID          string    `json:"id"`
	AlertID     string    `json:"alertId"`
	Fingerprint string    `json:"fingerprint"`
	Tenant      string    `json:"tenant"`
	Partition   string    `json:"partition"`
	Channel     string    `json:"channel"`
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts"`
	NextAttempt time.Time `json:"nextAttempt"`
	LastError   string    `json:"lastError,omitempty"`
	Payload     []byte    `json:"payload,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// MarshalPayload stores the notification payload as JSON.
func (r *OutboxRecord) MarshalPayload(alert Alert) error {
	payload, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	r.Payload = payload
	return nil
}
