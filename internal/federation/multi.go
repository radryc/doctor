package federation

import (
	"context"
	"sort"
	"strings"
	"time"

	proxy "github.com/radryc/doctor/internal/proxy"
	awsstore "github.com/radryc/doctor/internal/store/aws"
)

// Multi combines one manager per provider for status and target listings while
// delegating AWS queries to the AWS manager. It lets the federation UI and
// /v1/federation endpoints show AWS and GCP targets from a single snapshot.
type Multi struct {
	AWS      proxy.AWSClient
	Managers []*Manager
}

// NewMulti builds a multi-provider facade.
func NewMulti(aws proxy.AWSClient, managers ...*Manager) *Multi {
	return &Multi{AWS: aws, Managers: managers}
}

// Enabled reports whether any provider is configured.
func (m *Multi) Enabled() bool {
	if m == nil {
		return false
	}
	if m.AWS != nil && m.AWS.Enabled() {
		return true
	}
	for _, manager := range m.Managers {
		if manager != nil && manager.Enabled() {
			return true
		}
	}
	return false
}

// CheckScope implements ScopeChecker (used by the query service).
func (m *Multi) CheckScope(ctx context.Context, q Query) error {
	if m == nil || m.AWS == nil {
		return nil
	}
	if checker, ok := m.AWS.(ScopeChecker); ok {
		return checker.CheckScope(ctx, q)
	}
	return nil
}

func (m *Multi) ListMetrics(ctx context.Context, namespace, metric string) ([]awsstore.MetricDescriptor, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.ListMetrics(ctx, namespace, metric)
}

func (m *Multi) ListServices(ctx context.Context) ([]awsstore.ServiceInfo, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.ListServices(ctx)
}

func (m *Multi) ListLogGroups(ctx context.Context) ([]string, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.ListLogGroups(ctx)
}

func (m *Multi) QueryLogs(ctx context.Context, q awsstore.LogQuery) ([]awsstore.LogRecord, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.QueryLogs(ctx, q)
}

func (m *Multi) QueryTraces(ctx context.Context, q awsstore.TraceQuery) ([]awsstore.TraceSummary, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.QueryTraces(ctx, q)
}

func (m *Multi) GetTrace(ctx context.Context, traceID string) ([]awsstore.Span, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.GetTrace(ctx, traceID)
}

func (m *Multi) PrometheusQuery(ctx context.Context, query string, at time.Time) (map[string]any, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.PrometheusQuery(ctx, query, at)
}

func (m *Multi) PrometheusQueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.PrometheusQueryRange(ctx, query, start, end, step)
}

func (m *Multi) PrometheusSeries(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.PrometheusSeries(ctx, match, start, end)
}

func (m *Multi) PrometheusLabels(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.PrometheusLabels(ctx, match, start, end)
}

func (m *Multi) PrometheusLabelValues(ctx context.Context, label string, match []string, start, end time.Time) (map[string]any, error) {
	if m == nil || m.AWS == nil {
		return nil, nil
	}
	return m.AWS.PrometheusLabelValues(ctx, label, match, start, end)
}

// Status merges provider status maps.
func (m *Multi) Status() map[string]any { return m.StatusFor(nil) }

// StatusFor merges provider status maps with the caller's visibility filter.
func (m *Multi) StatusFor(visible func(string) bool) map[string]any {
	merged := map[string]any{
		"enabled":              false,
		"targets":              0,
		"account_count":        0,
		"partition_count":      0,
		"open_circuits":        0,
		"accounts":             map[string]int{},
		"regions":              map[string]int{},
		"partitions":           map[string]int{},
		"circuits":             []breakerSnapshot{},
		"recent_errors":        []string{},
		"accounts_truncated":   false,
		"partitions_truncated": false,
		"client_pool_size":     0,
		"max_fanout":           0,
	}
	if m == nil {
		return merged
	}
	var versions, sources []string
	var loadedAt time.Time
	for _, manager := range m.Managers {
		if manager == nil {
			continue
		}
		status := manager.StatusFor(visible)
		merged["enabled"] = toBool(merged["enabled"]) || toBool(status["enabled"])
		merged["targets"] = toInt(merged["targets"]) + toInt(status["targets"])
		merged["account_count"] = toInt(merged["account_count"]) + toInt(status["account_count"])
		merged["partition_count"] = toInt(merged["partition_count"]) + toInt(status["partition_count"])
		merged["open_circuits"] = toInt(merged["open_circuits"]) + toInt(status["open_circuits"])
		merged["client_pool_size"] = toInt(merged["client_pool_size"]) + toInt(status["client_pool_size"])
		merged["max_fanout"] = maxInt(toInt(merged["max_fanout"]), toInt(status["max_fanout"]))
		merged["accounts_truncated"] = toBool(merged["accounts_truncated"]) || toBool(status["accounts_truncated"])
		merged["partitions_truncated"] = toBool(merged["partitions_truncated"]) || toBool(status["partitions_truncated"])
		mergeCountMap(merged["accounts"], status["accounts"])
		mergeCountMap(merged["regions"], status["regions"])
		mergeCountMap(merged["partitions"], status["partitions"])
		merged["circuits"] = append(toCircuitList(merged["circuits"]), toCircuitList(status["circuits"])...)
		merged["recent_errors"] = appendLimited(toStringList(merged["recent_errors"]), toStringList(status["recent_errors"]), 16)
		merged["stale"] = toBool(merged["stale"]) || toBool(status["stale"])
		if version := strings.TrimSpace(toString(status["version"])); version != "" {
			versions = append(versions, version)
		}
		if source := strings.TrimSpace(toString(status["source"])); source != "" {
			sources = append(sources, source)
		}
		if loaded, ok := status["loaded_at"].(time.Time); ok && !loaded.IsZero() {
			if loadedAt.IsZero() || loaded.Before(loadedAt) {
				loadedAt = loaded
			}
		}
	}
	if len(versions) > 0 {
		merged["version"] = strings.Join(versions, "+")
	}
	if len(sources) > 0 {
		merged["source"] = strings.Join(sources, ",")
	}
	if !loadedAt.IsZero() {
		merged["loaded_at"] = loadedAt
	}
	return merged
}

// TargetsPage delegates to TargetsPageFor.
func (m *Multi) TargetsPage(ctx context.Context, account, region, partition, query, cursor string, limit int) TargetPage {
	return m.TargetsPageFor(ctx, TargetPageOptions{
		Account:   account,
		Region:    region,
		Partition: partition,
		Query:     query,
		Cursor:    cursor,
		Limit:     limit,
	})
}

// TargetsPageFor merges provider target pages and re-applies pagination.
func (m *Multi) TargetsPageFor(ctx context.Context, opts TargetPageOptions) TargetPage {
	if m == nil {
		return TargetPage{Targets: []map[string]any{}}
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	all := make([]map[string]any, 0)
	total := 0
	for _, manager := range m.Managers {
		if manager == nil {
			continue
		}
		pageOpts := opts
		pageOpts.Cursor = ""
		pageOpts.Limit = 500
		page := manager.TargetsPageFor(ctx, pageOpts)
		all = append(all, page.Targets...)
		total += page.Total
	}
	sort.SliceStable(all, func(i, j int) bool {
		return toString(all[i]["id"]) < toString(all[j]["id"])
	})
	start := 0
	if opts.Cursor != "" {
		start = sort.Search(len(all), func(i int) bool { return toString(all[i]["id"]) >= opts.Cursor })
		for start < len(all) && toString(all[start]["id"]) == opts.Cursor {
			start++
		}
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	page := TargetPage{Targets: []map[string]any{}, Total: total, Matched: len(all)}
	if end < len(all) {
		page.HasMore = true
		page.NextCursor = toString(all[end-1]["id"])
	}
	if start < end {
		page.Targets = all[start:end]
	}
	return page
}

// DebugPlan delegates to the first manager; provider-specific detail is
// available from each manager's own status.
func (m *Multi) DebugPlan(ctx context.Context, query string, signal Signal, account, region, partition string) map[string]any {
	if m == nil {
		return nil
	}
	for _, manager := range m.Managers {
		if manager != nil {
			return manager.DebugPlan(ctx, query, signal, account, region, partition)
		}
	}
	return nil
}

func mergeCountMap(dst, src any) {
	target, ok := dst.(map[string]int)
	if !ok {
		return
	}
	incoming, ok := src.(map[string]int)
	if !ok {
		return
	}
	for key, value := range incoming {
		target[key] += value
	}
}

func toBool(value any) bool {
	typed, _ := value.(bool)
	return typed
}

func toInt(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func toString(value any) string {
	typed, _ := value.(string)
	return typed
}

func toCircuitList(value any) []breakerSnapshot {
	typed, _ := value.([]breakerSnapshot)
	return typed
}

func toStringList(value any) []string {
	typed, _ := value.([]string)
	return typed
}

func appendLimited(left, right []string, limit int) []string {
	out := append(append([]string(nil), left...), right...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
