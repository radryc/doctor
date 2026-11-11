package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// GCPAlert mirrors the pusher /v1/gcp/alerts payload.
type GCPAlert struct {
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName,omitempty"`
	Policy      string            `json:"policy,omitempty"`
	State       string            `json:"state"`
	Severity    string            `json:"severity,omitempty"`
	Summary     string            `json:"summary,omitempty"`
	Partition   string            `json:"partition,omitempty"`
	Project     string            `json:"project,omitempty"`
	Region      string            `json:"region,omitempty"`
	StartedAt   time.Time         `json:"startedAt,omitempty"`
	UpdatedAt   time.Time         `json:"updatedAt,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type gcpAlertEnvelope struct {
	Alerts []GCPAlert `json:"alerts"`
}

// GCPPolicyMode controls the default Cloud Monitoring policy selection.
type GCPPolicyMode string

const (
	GCPPolicyImportAll GCPPolicyMode = "import-all"
	GCPPolicyIgnoreAll GCPPolicyMode = "ignore-all"
)

// GCPPolicyRule matches alert policies by name, severity, or label.
type GCPPolicyRule struct {
	NameRegex string `json:"nameRegex,omitempty" yaml:"nameRegex,omitempty"`
	Severity  string `json:"severity,omitempty" yaml:"severity,omitempty"`
	Label     string `json:"label,omitempty" yaml:"label,omitempty"`
	Equals    string `json:"equals,omitempty" yaml:"equals,omitempty"`
}

// GCPPolicy selects which Cloud Monitoring policies Sentinel imports.
type GCPPolicy struct {
	Default GCPPolicyMode   `json:"default" yaml:"default"`
	Include []GCPPolicyRule `json:"include,omitempty" yaml:"include,omitempty"`
	Exclude []GCPPolicyRule `json:"exclude,omitempty" yaml:"exclude,omitempty"`
}

// DefaultGCPPolicy imports all enabled policies unless excluded.
func DefaultGCPPolicy() *GCPPolicy {
	return &GCPPolicy{Default: GCPPolicyImportAll}
}

// Validate checks the policy.
func (p GCPPolicy) Validate() error {
	switch p.Default {
	case "", GCPPolicyImportAll, GCPPolicyIgnoreAll:
		return nil
	default:
		return fmt.Errorf("alerting: invalid GCP policy selection mode %q", p.Default)
	}
}

// Select evaluates the policy. Exclude always wins.
func (p GCPPolicy) Select(alert GCPAlert) Selection {
	if strings.EqualFold(alert.Labels["sentinel.io/import"], "false") {
		return Selection{Reason: "sentinel.io/import=false"}
	}
	for _, rule := range p.Exclude {
		if rule.matches(alert) {
			return Selection{Reason: "exclude:" + rule.describe()}
		}
	}
	if strings.EqualFold(alert.Labels["sentinel.io/import"], "true") {
		return Selection{Included: true, Reason: "sentinel.io/import=true"}
	}
	for _, rule := range p.Include {
		if rule.matches(alert) {
			return Selection{Included: true, Reason: "include:" + rule.describe()}
		}
	}
	if p.Default == GCPPolicyIgnoreAll {
		return Selection{Reason: "default:ignore-all"}
	}
	return Selection{Included: true, Reason: "default:import-all"}
}

func (r GCPPolicyRule) matches(alert GCPAlert) bool {
	if r.NameRegex != "" {
		re, err := regexp.Compile(r.NameRegex)
		if err != nil || !re.MatchString(alert.DisplayName) && !re.MatchString(alert.Name) {
			return false
		}
	}
	if r.Severity != "" && !strings.EqualFold(r.Severity, alert.Severity) {
		return false
	}
	if r.Label != "" {
		if alert.Labels[r.Label] != r.Equals {
			return false
		}
	}
	return true
}

func (r GCPPolicyRule) describe() string {
	parts := make([]string, 0, 3)
	if r.NameRegex != "" {
		parts = append(parts, "name~="+r.NameRegex)
	}
	if r.Severity != "" {
		parts = append(parts, "severity="+r.Severity)
	}
	if r.Label != "" {
		parts = append(parts, r.Label+"="+r.Equals)
	}
	return strings.Join(parts, ",")
}

// GCPSource polls the guardians-pusher-gcp read API and imports selected
// Cloud Monitoring alert policies into Sentinel.
type GCPSource struct {
	BaseURL     string
	Token       string
	Project     string
	Region      string
	Partition   string
	Tenant      string
	Service     *Service
	Store       *Store
	HTTP        *http.Client
	Logger      *log.Logger
	Interval    time.Duration
	Policy      *GCPPolicy
	DefaultMode GCPPolicyMode
	Now         func() time.Time
}

// Run polls until ctx is cancelled.
func (s *GCPSource) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if summary, err := s.PollOnce(ctx); err != nil {
		s.logf("poll: %v", err)
	} else {
		s.logf("poll: discovered=%d included=%d excluded=%d imported=%d unscoped=%d", summary.Discovered, summary.Included, summary.Excluded, summary.Imported, summary.Unscoped)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if summary, err := s.PollOnce(ctx); err != nil {
				s.logf("poll: %v", err)
			} else {
				s.logf("poll: discovered=%d included=%d excluded=%d imported=%d unscoped=%d", summary.Discovered, summary.Included, summary.Excluded, summary.Imported, summary.Unscoped)
			}
		}
	}
}

// PollOnce fetches and imports selected alert policies.
func (s *GCPSource) PollOnce(ctx context.Context) (SourceSummary, error) {
	var summary SourceSummary
	if strings.TrimSpace(s.BaseURL) == "" {
		return summary, fmt.Errorf("alerting: gcp read URL is not configured")
	}
	policy := s.Policy
	if policy == nil && s.Store != nil {
		loaded, err := s.Store.GetGCPPolicy(ctx)
		if err != nil {
			return summary, err
		}
		policy = loaded
	}
	if policy == nil {
		policy = DefaultGCPPolicy()
	}
	if s.DefaultMode != "" {
		policy.Default = s.DefaultMode
	}
	alerts, err := s.fetchAlerts(ctx)
	if err != nil {
		return summary, err
	}
	summary.Discovered = len(alerts)
	for _, alert := range alerts {
		if strings.EqualFold(alert.State, "disabled") {
			summary.Excluded++
			continue
		}
		if !policy.Select(alert).Included {
			summary.Excluded++
			continue
		}
		normalized, ok := s.normalize(alert)
		if !ok {
			summary.Unscoped++
			continue
		}
		summary.Included++
		if s.Service == nil {
			continue
		}
		if _, err := s.Service.Apply(ctx, normalized); err != nil {
			return summary, err
		}
		summary.Imported++
	}
	return summary, nil
}

func (s *GCPSource) fetchAlerts(ctx context.Context) ([]GCPAlert, error) {
	endpoint := strings.TrimRight(s.BaseURL, "/") + "/v1/gcp/alerts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(s.Token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.Token))
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gcp read API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope gcpAlertEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode gcp alerts: %w", err)
	}
	return envelope.Alerts, nil
}

func (s *GCPSource) normalize(alert GCPAlert) (Alert, bool) {
	partition := firstNonEmpty(alert.Partition, alert.Labels["guardian_partition"], alert.Labels["guardian.io/partition"], s.Partition)
	if partition == "" {
		return Alert{}, false
	}
	status := StatusNoData
	switch strings.ToLower(alert.State) {
	case "firing":
		status = StatusFiring
	case "ok", "resolved":
		status = StatusResolved
	}
	severity := strings.ToLower(strings.TrimSpace(alert.Severity))
	if severity == "" {
		if status == StatusFiring {
			severity = "warning"
		} else {
			severity = "info"
		}
	}
	name := firstNonEmpty(alert.DisplayName, alert.Name)
	labels := map[string]string{
		"alertname": name,
		"source":    "gcp",
		"project":   firstNonEmpty(alert.Project, s.Project),
		"region":    firstNonEmpty(alert.Region, s.Region),
		"partition": partition,
	}
	for key, value := range alert.Labels {
		if value != "" {
			labels[key] = value
		}
	}
	annotations := map[string]string{}
	if summary := strings.TrimSpace(alert.Summary); summary != "" {
		annotations["summary"] = summary
	}
	if alert.Name != "" {
		annotations["console_url"] = gcpConsoleURL(firstNonEmpty(alert.Project, s.Project), alert.Name)
	}
	tenant := s.Tenant
	if tenant == "" && s.Service != nil {
		tenant = s.Service.DefaultTenant
	}
	if tenant == "" {
		tenant = "default"
	}
	startsAt := alert.StartedAt
	if startsAt.IsZero() {
		startsAt = s.now()
	}
	alertOut := Alert{
		Fingerprint: Fingerprint("gcp", tenant, partition, labels),
		Source:      "gcp",
		Tenant:      tenant,
		Partition:   partition,
		Status:      status,
		Severity:    severity,
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    startsAt.UTC(),
		Delivery:    DeliverySentinel,
	}
	alertOut.ID = alertOut.Fingerprint
	if status == StatusResolved {
		now := s.now()
		alertOut.EndsAt = &now
	}
	return alertOut, true
}

func gcpConsoleURL(project, policyName string) string {
	id := policyName
	if idx := strings.LastIndex(policyName, "/"); idx >= 0 {
		id = policyName[idx+1:]
	}
	project = firstNonEmpty(project, "_")
	return "https://console.cloud.google.com/monitoring/alerting/policies/" + url.PathEscape(id) + "?project=" + url.QueryEscape(project)
}

func (s *GCPSource) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *GCPSource) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf("sentinel gcp: "+format, args...)
	}
}
