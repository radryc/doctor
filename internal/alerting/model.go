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
	ID              string            `json:"id"`
	Fingerprint     string            `json:"fingerprint"`
	Scope           string            `json:"scope,omitempty"`
	Source          string            `json:"source"`
	Tenant          string            `json:"tenant"`
	Partition       string            `json:"partition"`
	SourcePartition string            `json:"sourcePartition,omitempty"`
	Audience        *Audience         `json:"audience,omitempty"`
	DedupeKey       string            `json:"dedupeKey,omitempty"`
	ExpiresAt       *time.Time        `json:"expiresAt,omitempty"`
	Status          string            `json:"status"`
	Severity        string            `json:"severity"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	StartsAt        time.Time         `json:"startsAt"`
	EndsAt          *time.Time        `json:"endsAt,omitempty"`
	Value           string            `json:"value,omitempty"`
	Delivery        string            `json:"delivery"`
	// Notify lists explicit receiver names chosen for this alert (for example
	// by dragging a rule onto a destination in the UI). It takes precedence
	// over label routes and the default receiver.
	Notify         []string   `json:"notify,omitempty"`
	Acked          bool       `json:"acked,omitempty"`
	AckedBy        string     `json:"ackedBy,omitempty"`
	AckedAt        *time.Time `json:"ackedAt,omitempty"`
	SilenceIDs     []string   `json:"silenceIds,omitempty"`
	LastSeenAt     *time.Time `json:"lastSeenAt,omitempty"`
	LastNotifiedAt *time.Time `json:"lastNotifiedAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
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

// Audience targets a subspace alert.
type Audience struct {
	Type       string   `json:"type" yaml:"type"`
	Partitions []string `json:"partitions,omitempty" yaml:"partitions,omitempty"`
	Groups     []string `json:"groups,omitempty" yaml:"groups,omitempty"`
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
	Name        string            `json:"name" yaml:"name"`
	Type        string            `json:"type" yaml:"type"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	URL         string            `json:"url,omitempty" yaml:"url,omitempty"`
	SecretEnv   string            `json:"secretEnv,omitempty" yaml:"secretEnv,omitempty"`
	Headers     map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Timeout     string            `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// Route selects receivers for matching alerts.
//
// Tenant and Partition narrow a route to one partition, which makes it easy to
// send a specific partition to its own PagerDuty service. Match supports the
// matcher operators used by silences (=, !=, =~, !~), while Matchers remains
// for backwards-compatible exact label matching.
type Route struct {
	Name           string            `json:"name" yaml:"name"`
	Tenant         string            `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	Partition      string            `json:"partition,omitempty" yaml:"partition,omitempty"`
	Matchers       map[string]string `json:"matchers,omitempty" yaml:"matchers,omitempty"`
	Match          []Matcher         `json:"match,omitempty" yaml:"match,omitempty"`
	Receivers      []string          `json:"receivers" yaml:"receivers"`
	RepeatInterval string            `json:"repeatInterval,omitempty" yaml:"repeatInterval,omitempty"`
	Continue       bool              `json:"continue,omitempty" yaml:"continue,omitempty"`
	// Default marks a catch-all route. It is only consulted when no other
	// route matched an alert.
	Default bool `json:"default,omitempty" yaml:"default,omitempty"`
}

// ReceiverConfig is the on-disk notification configuration.
type ReceiverConfig struct {
	Receivers []Receiver `json:"receivers" yaml:"receivers"`
	Routes    []Route    `json:"routes" yaml:"routes"`
	// DefaultReceiver receives alerts that match no route and carry no
	// explicit rule-level destination. It is the "default destination" in the
	// UI.
	DefaultReceiver string `json:"defaultReceiver,omitempty" yaml:"defaultReceiver,omitempty"`
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
		if strings.EqualFold(r.Type, "pagerduty") && strings.TrimSpace(r.URL) == "" {
			r.URL = "https://events.pagerduty.com/v2/enqueue"
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
	if c.DefaultReceiver != "" {
		if _, ok := seen[c.DefaultReceiver]; !ok {
			return fmt.Errorf("alerting: default receiver %q is not defined", c.DefaultReceiver)
		}
	}
	return nil
}

// Applies reports whether the route selects this alert. Default routes are
// handled by the router only after every regular route has been considered.
func (r Route) Applies(alert Alert) bool {
	if r.Default {
		return false
	}
	if r.Tenant != "" && r.Tenant != alert.Tenant {
		return false
	}
	if r.Partition != "" && r.Partition != alert.Partition {
		return false
	}
	for key, want := range r.Matchers {
		if alert.Labels[key] != want {
			return false
		}
	}
	for _, matcher := range r.Match {
		if !matcher.Matches(alert.Labels) {
			return false
		}
	}
	return true
}

// AlertRoute assigns an explicit destination to a source-generated alert.
// Source alerts (for example GuardianPartitionAttention or a CloudWatch alarm)
// are not defined by a Sentinel rule, so they cannot store Notify on the rule.
// AlertRoutes give those alerts the same "explicit destination wins" behaviour
// that rule destinations get, without turning every source alert into a rule.
type AlertRoute struct {
	Source    string   `json:"source" yaml:"source"`
	Alert     string   `json:"alertname" yaml:"alertname"`
	Receivers []string `json:"receivers" yaml:"receivers"`
}

// PartitionRouting is the notification configuration owned by one partition.
// Receivers, their credentials, routes, and fallbacks are all partition-local.
type PartitionRouting struct {
	Version   int        `json:"version" yaml:"version"`
	Tenant    string     `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	Partition string     `json:"partition,omitempty" yaml:"partition,omitempty"`
	Receivers []Receiver `json:"receivers" yaml:"receivers"`
	Routes    []Route    `json:"routes" yaml:"routes"`
	// AlertRoutes target source-generated alerts by source and alertname. They
	// sit between an alert's own Notify list and the partition destination.
	AlertRoutes []AlertRoute `json:"alertRoutes,omitempty" yaml:"alertRoutes,omitempty"`
	// PartitionReceiver receives every alert in the partition that has no
	// explicit per-alert destination.
	PartitionReceiver string `json:"partitionReceiver,omitempty" yaml:"partitionReceiver,omitempty"`
	// DefaultReceiver is the final fallback when no label route matches.
	DefaultReceiver string `json:"defaultReceiver,omitempty" yaml:"defaultReceiver,omitempty"`
}

// Validate checks the partition routing configuration.
func (c *PartitionRouting) Validate() error {
	if c.Receivers == nil {
		c.Receivers = []Receiver{}
	}
	if c.Routes == nil {
		c.Routes = []Route{}
	}
	if c.AlertRoutes == nil {
		c.AlertRoutes = []AlertRoute{}
	}
	cfg := ReceiverConfig{Receivers: c.Receivers, Routes: c.Routes, DefaultReceiver: c.DefaultReceiver}
	if err := cfg.Validate(); err != nil {
		return err
	}
	known := make(map[string]struct{}, len(c.Receivers))
	for _, receiver := range c.Receivers {
		known[receiver.Name] = struct{}{}
	}
	for i := range c.AlertRoutes {
		route := &c.AlertRoutes[i]
		if strings.TrimSpace(route.Source) == "" {
			return fmt.Errorf("alerting: alertRoute[%d] has no source", i)
		}
		if strings.TrimSpace(route.Alert) == "" {
			return fmt.Errorf("alerting: alertRoute[%d] has no alertname", i)
		}
		if len(route.Receivers) == 0 {
			return fmt.Errorf("alerting: alertRoute[%d] has no receivers", i)
		}
		for _, name := range route.Receivers {
			if _, ok := known[name]; !ok {
				return fmt.Errorf("alerting: alertRoute %q references unknown receiver %q", route.Alert, name)
			}
		}
	}
	if c.PartitionReceiver != "" {
		found := false
		for _, receiver := range c.Receivers {
			if receiver.Name == c.PartitionReceiver {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("alerting: partition receiver %q is not defined", c.PartitionReceiver)
		}
	}
	return nil
}

// PartitionAlertSummary is a compact alert rollup used by the Fleet view.
type PartitionAlertSummary struct {
	Tenant    string `json:"tenant"`
	Partition string `json:"partition"`
	Firing    int    `json:"firing"`
	Pending   int    `json:"pending"`
	Critical  int    `json:"critical"`
	Total     int    `json:"total"`
}

// OutboxRecord is a pending notification.
type OutboxRecord struct {
	ID           string    `json:"id"`
	AlertID      string    `json:"alertId"`
	Fingerprint  string    `json:"fingerprint"`
	Tenant       string    `json:"tenant"`
	Partition    string    `json:"partition"`
	Channel      string    `json:"channel"`
	Status       string    `json:"status"`
	Attempts     int       `json:"attempts"`
	NextAttempt  time.Time `json:"nextAttempt"`
	LastError    string    `json:"lastError,omitempty"`
	Payload      []byte    `json:"payload,omitempty"`
	ClaimedBy    string    `json:"claimedBy,omitempty"`
	ClaimedUntil time.Time `json:"claimedUntil,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
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
