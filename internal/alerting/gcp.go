package alerting

import (
	"context"
	"encoding/json"
	"errors"
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
		var authErr *gcpReadAuthError
		if errors.As(err, &authErr) {
			if applyErr := s.applyAuthFailure(ctx, authErr); applyErr != nil {
				return summary, applyErr
			}
			summary.Imported = 1
		}
		return summary, err
	}
	// Resolve on every successful poll, not only after observing a failure in
	// this process: Sentinel restarts are common, and an existing firing
	// GCPBackendAuthFailure must clear even when the in-memory state is fresh.
	if resolveErr := s.resolveAuthFailure(ctx); resolveErr != nil {
		return summary, resolveErr
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
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &gcpReadAuthError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
		}
		return nil, fmt.Errorf("gcp read API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope gcpAlertEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode gcp alerts: %w", err)
	}
	return envelope.Alerts, nil
}

// gcpReadAuthError marks a bearer-token/scope rejection from the GCP read
// API. Unlike a transient network or upstream error, this is actionable and
// should become an alert in Sentinel even though the source poll failed.
type gcpReadAuthError struct {
	StatusCode int
	Body       string
}

func (e *gcpReadAuthError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("gcp read API HTTP %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
}

func (s *GCPSource) applyAuthFailure(ctx context.Context, authErr *gcpReadAuthError) error {
	if s.Service == nil {
		return nil
	}
	_, err := s.Service.Apply(ctx, s.authFailureAlert(StatusFiring, authErr))
	return err
}

func (s *GCPSource) resolveAuthFailure(ctx context.Context) error {
	if s.Service == nil || s.Store == nil {
		return nil
	}
	alert := s.authFailureAlert(StatusResolved, nil)
	alerts, err := s.Store.ListAlerts(ctx, alert.Tenant, alert.Partition)
	if err != nil {
		return err
	}
	firing := false
	for i := range alerts {
		if alerts[i].Fingerprint == alert.Fingerprint && alerts[i].Status == StatusFiring {
			firing = true
			break
		}
	}
	if !firing {
		return nil
	}
	_, err = s.Service.Apply(ctx, alert)
	return err
}

func (s *GCPSource) authFailureAlert(status string, authErr *gcpReadAuthError) Alert {
	tenant := s.Tenant
	if tenant == "" && s.Service != nil {
		tenant = s.Service.DefaultTenant
	}
	if tenant == "" {
		tenant = "default"
	}
	partition := firstNonEmpty(s.Partition, "doctor")
	labels := map[string]string{
		"alertname": "GCPBackendAuthFailure",
		"source":    "gcp",
		"provider":  "gcp",
		"component": "doctor",
		"partition": partition,
		"severity":  "critical",
	}
	summary := "Doctor cannot authenticate to the GCP read API."
	description := "Sentinel received an authentication failure while polling the GCP read API. Doctor's GCP backend token does not match guardian-pusher-gcp's read token, or its scope is insufficient. GCP metrics, logs, and traces are unavailable until the token is re-synced."
	if authErr != nil {
		description = fmt.Sprintf("Sentinel received HTTP %d from %s while polling /v1/gcp/alerts. Doctor's GCP backend token does not match guardian-pusher-gcp's read token, or its scope is insufficient. GCP metrics, logs, and traces are unavailable until the token is re-synced.", authErr.StatusCode, s.BaseURL)
	}
	action := `1. Compare Doctor's token with the platform secret:
   TOKEN=$(kubectl -n doctor get deploy -o name | grep query | head -1 | xargs -I{} kubectl -n doctor get {} -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="DOCTOR_GCP_BACKEND_TOKEN")].value}')
   SECRET=$(cat stratatools/partitions/shared/secrets/gcp-pusher-token)
   test "$TOKEN" = "$SECRET" || echo "token drift: Doctor != shared secret"
2. Re-sync the pusher and Doctor/Sentinel wiring:
   FORCE_GCP_PUSHER_BUILD=true ./demo.sh prepare --skip-aws
3. Clear the query circuit breaker:
   kubectl -n doctor rollout restart deploy/$(kubectl -n doctor get deploy -o name | grep -m1 query | sed 's|.*/||')
4. Verify:
   curl -fsS -H "Authorization: Bearer $SECRET" http://172.18.0.1:19091/healthz`
	if status == StatusResolved {
		summary = "Doctor's GCP read API authentication recovered."
	}
	annotations := map[string]string{
		"summary":     summary,
		"description": description,
		"action":      action,
		"runbook":     "https://strata.local/runbooks/doctor-gcp-token-mismatch",
	}
	alert := Alert{
		Source:      "gcp",
		Tenant:      tenant,
		Partition:   partition,
		Status:      status,
		Severity:    "critical",
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    s.now(),
		Delivery:    DeliverySentinel,
	}
	alert.Fingerprint = Fingerprint("gcp", tenant, partition, labels)
	alert.ID = alert.Fingerprint
	if status == StatusResolved {
		now := s.now()
		alert.EndsAt = &now
	}
	return alert
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
