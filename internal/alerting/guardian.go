package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Guardian partition alert names. The alerts are partition-scoped so Nightwatch
// can correlate them with the partition it already watches and investigate the
// failing intent/asset through Guardian topology.
const (
	GuardianPartitionErrorAlert     = "GuardianPartitionError"
	GuardianPartitionAttentionAlert = "GuardianPartitionAttention"
)

// GuardianPartitionSummary mirrors the fields Doctor needs from Guardian's
// GET /api/partitions response. Guardian reports the same status through
// Status, Health, and DisplayStatus depending on whether runtime state exists,
// so the source normalizes all of them.
type GuardianPartitionSummary struct {
	Name              string   `json:"name"`
	Status            string   `json:"status,omitempty"`
	DisplayStatus     string   `json:"displayStatus,omitempty"`
	Health            string   `json:"health,omitempty"`
	LastHealth        string   `json:"lastHealth,omitempty"`
	LastDisplayStatus string   `json:"lastDisplayStatus,omitempty"`
	Errors            []string `json:"errors,omitempty"`
	FailingAssets     int      `json:"failingAssets,omitempty"`
	FailedIntents     int      `json:"failedIntents,omitempty"`
	DriftedIntents    int      `json:"driftedIntents,omitempty"`
}

type guardianPartitionEnvelope struct {
	Partitions []GuardianPartitionSummary `json:"partitions"`
}

// GuardianSource polls Guardian partition state and turns failing/attention
// partitions into Sentinel alerts. Those alerts are visible through the
// Sentinel API that Nightwatch polls, so Guardian errors and attentions show up
// as alarms without relying on Guardian's own webhook configuration.
type GuardianSource struct {
	BaseURL  string
	Token    string
	Tenant   string
	Service  *Service
	Store    *Store
	HTTP     *http.Client
	Logger   *log.Logger
	Interval time.Duration
	Now      func() time.Time
	// StateTTL bounds how often the tenant alert snapshot is reloaded. Between
	// reloads the source keeps the state it has already observed or written.
	StateTTL time.Duration

	pollMu        sync.Mutex
	stateMu       sync.Mutex
	stateCache    map[string]Alert
	stateLoadedAt time.Time
}

// Run polls until ctx is cancelled.
func (s *GuardianSource) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if summary, err := s.PollOnce(ctx); err != nil {
		s.logf("poll: %v", err)
	} else {
		s.logSummary(summary)
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
				s.logSummary(summary)
			}
		}
	}
}

func (s *GuardianSource) logSummary(summary SourceSummary) {
	s.logf("poll: discovered=%d unhealthy=%d healthy=%d imported=%d resolved=%d", summary.Discovered, summary.Included, summary.Healthy, summary.Imported, summary.Resolved)
}

// PollOnce fetches current Guardian partitions and reconciles their Sentinel
// alerts. Partitions that are healthy (or no longer unhealthy) resolve the
// alerts previously created for them.
func (s *GuardianSource) PollOnce(ctx context.Context) (SourceSummary, error) {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()

	var summary SourceSummary
	if strings.TrimSpace(s.BaseURL) == "" {
		return summary, fmt.Errorf("alerting: guardian API URL is not configured")
	}
	if s.Service == nil || s.Store == nil {
		return summary, fmt.Errorf("alerting: guardian source requires a service and store")
	}
	partitions, err := s.fetchPartitions(ctx)
	if err != nil {
		return summary, err
	}
	// Load the tenant's alert state once instead of issuing two reads for every
	// healthy partition on every poll. The snapshot is cached for StateTTL and
	// updated by every transition this source applies.
	state, err := s.loadState(ctx)
	if err != nil {
		return summary, fmt.Errorf("alerting: load guardian alert state: %w", err)
	}
	summary.Discovered = len(partitions)
	now := s.now()
	for _, partition := range partitions {
		name := strings.TrimSpace(partition.Name)
		if name == "" {
			continue
		}
		partition.Name = name
		alertName := guardianPartitionAlertName(partition)
		switch alertName {
		case GuardianPartitionErrorAlert:
			summary.Included++
			applied, err := s.applyPartitionAlert(ctx, partition, GuardianPartitionErrorAlert, "critical", now)
			if err != nil {
				return summary, err
			}
			state[alertStateKey(applied.Partition, applied.Fingerprint)] = *applied
			summary.Imported++
			resolved, err := s.resolvePartitionAlert(ctx, state, partition, GuardianPartitionAttentionAlert, now)
			if err != nil {
				return summary, err
			}
			if resolved {
				summary.Resolved++
			}
		case GuardianPartitionAttentionAlert:
			summary.Included++
			applied, err := s.applyPartitionAlert(ctx, partition, GuardianPartitionAttentionAlert, "warning", now)
			if err != nil {
				return summary, err
			}
			state[alertStateKey(applied.Partition, applied.Fingerprint)] = *applied
			summary.Imported++
			resolved, err := s.resolvePartitionAlert(ctx, state, partition, GuardianPartitionErrorAlert, now)
			if err != nil {
				return summary, err
			}
			if resolved {
				summary.Resolved++
			}
		default:
			summary.Healthy++
			for _, name := range []string{GuardianPartitionErrorAlert, GuardianPartitionAttentionAlert} {
				resolved, err := s.resolvePartitionAlert(ctx, state, partition, name, now)
				if err != nil {
					return summary, err
				}
				if resolved {
					summary.Resolved++
				}
			}
		}
	}
	return summary, nil
}

func (s *GuardianSource) loadState(ctx context.Context) (map[string]Alert, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	ttl := s.StateTTL
	if ttl <= 0 {
		// Re-scan at most every few polls. Bound staleness to roughly one
		// interval so a resolved alert is not held for minutes.
		ttl = 2 * s.Interval
		if ttl < time.Minute {
			ttl = time.Minute
		}
		if ttl > 5*time.Minute {
			ttl = 5 * time.Minute
		}
	}
	now := s.now()
	if s.stateCache != nil && now.Sub(s.stateLoadedAt) < ttl {
		return s.stateCache, nil
	}
	state, err := s.Store.ListTenantAlerts(ctx, s.tenant())
	if err != nil {
		return nil, err
	}
	s.stateCache = state
	s.stateLoadedAt = now
	return state, nil
}

func (s *GuardianSource) fetchPartitions(ctx context.Context) ([]GuardianPartitionSummary, error) {
	paths := []string{"/api/partitions", "/api/m2m/partitions"}
	var lastErr error
	for _, path := range paths {
		body, status, err := s.get(ctx, path)
		if err != nil {
			lastErr = err
			continue
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			lastErr = fmt.Errorf("guardian %s HTTP %d: %s", path, status, strings.TrimSpace(string(body)))
			continue
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("guardian %s HTTP %d: %s", path, status, strings.TrimSpace(string(body)))
		}
		partitions, err := decodeGuardianPartitions(body)
		if err != nil {
			lastErr = err
			continue
		}
		return partitions, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("alerting: guardian API URL is not configured")
	}
	return nil, lastErr
}

func (s *GuardianSource) get(ctx context.Context, path string) ([]byte, int, error) {
	endpoint := strings.TrimRight(s.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(s.Token); token != "" {
		// Support both the user-facing API and the service-to-service alias.
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Guardian-Internal-Token", token)
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func decodeGuardianPartitions(body []byte) ([]GuardianPartitionSummary, error) {
	var partitions []GuardianPartitionSummary
	if err := json.Unmarshal(body, &partitions); err == nil {
		return partitions, nil
	}
	var envelope guardianPartitionEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode guardian partitions: %w", err)
	}
	return envelope.Partitions, nil
}

// guardianPartitionAlertName returns the alert that should be firing for a
// partition, or "" when the partition is healthy enough to resolve its alerts.
func guardianPartitionAlertName(partition GuardianPartitionSummary) string {
	if len(partition.Errors) > 0 {
		return GuardianPartitionErrorAlert
	}
	primary := []string{partition.Status, partition.Health, partition.DisplayStatus}
	if name := guardianAlertNameForTokens(primary); name != "" {
		return name
	}
	if partition.FailingAssets > 0 || partition.FailedIntents > 0 {
		return GuardianPartitionErrorAlert
	}
	// Only fall back to the last resolved health when the live fields are
	// empty; otherwise a partition that is currently progressing would keep
	// flapping on a stale "last attention" value.
	if strings.TrimSpace(partition.Status) == "" && strings.TrimSpace(partition.Health) == "" && strings.TrimSpace(partition.DisplayStatus) == "" {
		if name := guardianAlertNameForTokens([]string{partition.LastHealth, partition.LastDisplayStatus}); name != "" {
			return name
		}
	}
	if partition.DriftedIntents > 0 {
		return GuardianPartitionAttentionAlert
	}
	return ""
}

func guardianAlertNameForTokens(tokens []string) string {
	for _, token := range tokens {
		switch strings.ToLower(strings.TrimSpace(token)) {
		case "invalid", "failing", "failed", "error", "unhealthy", "critical", "unavailable", "needs action":
			return GuardianPartitionErrorAlert
		}
	}
	for _, token := range tokens {
		switch strings.ToLower(strings.TrimSpace(token)) {
		case "attention", "degraded", "warning", "drifted", "drifted-locked":
			return GuardianPartitionAttentionAlert
		}
	}
	return ""
}

func (s *GuardianSource) applyPartitionAlert(ctx context.Context, partition GuardianPartitionSummary, alertName, severity string, now time.Time) (*Alert, error) {
	alert := s.normalizePartitionAlert(partition, alertName, severity, now)
	return s.Service.Apply(ctx, alert)
}

func (s *GuardianSource) resolvePartitionAlert(ctx context.Context, state map[string]Alert, partition GuardianPartitionSummary, alertName string, now time.Time) (bool, error) {
	fingerprint := s.fingerprint(partition.Name, alertName)
	existing, ok := state[alertStateKey(partition.Name, fingerprint)]
	if !ok || existing.Status == StatusResolved {
		return false, nil
	}
	if existing.Fingerprint == "" {
		existing.Fingerprint = fingerprint
	}
	if existing.ID == "" {
		existing.ID = existing.Fingerprint
	}
	existing.Status = StatusResolved
	existing.EndsAt = &now
	existing.UpdatedAt = now
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations["summary"] = fmt.Sprintf("Guardian partition %s recovered (%s).", partition.Name, guardianDisplayStatus(partition))
	applied, err := s.Service.Apply(ctx, existing)
	if err != nil {
		return false, err
	}
	state[alertStateKey(applied.Partition, applied.Fingerprint)] = *applied
	return true, nil
}

func (s *GuardianSource) normalizePartitionAlert(partition GuardianPartitionSummary, alertName, severity string, now time.Time) Alert {
	tenant := s.tenant()
	labels := map[string]string{
		"alertname":          alertName,
		"source":             "guardian",
		"partition":          partition.Name,
		"guardian_partition": partition.Name,
		"severity":           severity,
	}
	annotations := map[string]string{
		"summary": guardianAlertSummary(partition, alertName),
	}
	setAnnotation(annotations, "guardian.status", partition.Status)
	setAnnotation(annotations, "guardian.display_status", partition.DisplayStatus)
	setAnnotation(annotations, "guardian.health", partition.Health)
	setAnnotation(annotations, "guardian.errors", strings.Join(partition.Errors, "; "))
	setAnnotation(annotations, "guardian.failing_assets", intString(partition.FailingAssets))
	setAnnotation(annotations, "guardian.failed_intents", intString(partition.FailedIntents))
	setAnnotation(annotations, "guardian.drifted_intents", intString(partition.DriftedIntents))
	alert := Alert{
		Fingerprint: s.fingerprint(partition.Name, alertName),
		Source:      "guardian",
		Tenant:      tenant,
		Partition:   partition.Name,
		Status:      StatusFiring,
		Severity:    severity,
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    now,
		Delivery:    DeliverySentinel,
	}
	alert.ID = alert.Fingerprint
	return alert
}

func (s *GuardianSource) fingerprint(partition, alertName string) string {
	labels := map[string]string{
		"alertname":          alertName,
		"source":             "guardian",
		"partition":          partition,
		"guardian_partition": partition,
		"severity":           guardianAlertSeverity(alertName),
	}
	return Fingerprint("guardian", s.tenant(), partition, labels)
}

func guardianAlertSeverity(alertName string) string {
	if alertName == GuardianPartitionErrorAlert {
		return "critical"
	}
	return "warning"
}

func guardianAlertSummary(partition GuardianPartitionSummary, alertName string) string {
	status := guardianDisplayStatus(partition)
	if alertName == GuardianPartitionErrorAlert {
		if len(partition.Errors) > 0 {
			return fmt.Sprintf("Guardian partition %s has %d error(s): %s", partition.Name, len(partition.Errors), strings.Join(partition.Errors, "; "))
		}
		return fmt.Sprintf("Guardian partition %s needs action (%s).", partition.Name, status)
	}
	return fmt.Sprintf("Guardian partition %s needs attention (%s).", partition.Name, status)
}

func guardianDisplayStatus(partition GuardianPartitionSummary) string {
	for _, value := range []string{partition.DisplayStatus, partition.Status, partition.Health, partition.LastDisplayStatus, partition.LastHealth} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "unknown"
}

func setAnnotation(annotations map[string]string, key, value string) {
	if strings.TrimSpace(value) != "" {
		annotations[key] = value
	}
}

func intString(value int) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}

func (s *GuardianSource) tenant() string {
	tenant := strings.TrimSpace(s.Tenant)
	if tenant == "" && s.Service != nil {
		tenant = strings.TrimSpace(s.Service.DefaultTenant)
	}
	if tenant == "" {
		tenant = "default"
	}
	return tenant
}

func (s *GuardianSource) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *GuardianSource) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf("sentinel guardian: "+format, args...)
	}
}
