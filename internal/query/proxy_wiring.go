package query

import (
	"context"
	"fmt"
	"strconv"
	"time"

	promstorage "github.com/prometheus/prometheus/storage"
	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/tempo"
)

// SetProxyRouter installs the multi-backend router. When nil, handlers use the
// legacy single-backend plus AWS path.
func (s *Service) SetProxyRouter(router *proxy.Router) { s.proxyRouter = router }

// ProxyRouter returns the installed router, if any.
func (s *Service) ProxyRouter() *proxy.Router { return s.proxyRouter }

// LocalMetricsUpstream exposes the local PromQL engine and discovery as a
// proxy.MetricsUpstream so it can be registered as the "doctor" backend.
func (s *Service) LocalMetricsUpstream() proxy.MetricsUpstream {
	return localMetricsUpstream{svc: s}
}

// LocalLogsUpstream exposes the local logengine as a proxy.LogsUpstream.
func (s *Service) LocalLogsUpstream() proxy.LogsUpstream {
	return localLogsUpstream{svc: s}
}

type localMetricsUpstream struct {
	svc *Service
}

func (l localMetricsUpstream) Name() string { return proxy.SourceDoctor }

func (l localMetricsUpstream) PromInstant(ctx context.Context, tenant, query string, at time.Time) (map[string]any, error) {
	data, err, parse := l.svc.execPrometheusInstant(ctx, tenant, query, at)
	if err != nil {
		return nil, &proxy.QueryError{Err: err, Parse: parse}
	}
	return data, nil
}

func (l localMetricsUpstream) PromRange(ctx context.Context, tenant, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	data, err, parse := l.svc.execPrometheusRange(ctx, tenant, query, start, end, step)
	if err != nil {
		return nil, &proxy.QueryError{Err: err, Parse: parse}
	}
	return data, nil
}

func (l localMetricsUpstream) PromSeries(ctx context.Context, tenant string, match []string, start, end time.Time) ([]map[string]string, error) {
	selectorSets, err := parsePrometheusSelectorSets(match)
	if err != nil {
		return nil, err
	}
	querier, err := l.svc.localQuerier(tenant, start, end)
	if err != nil {
		return nil, err
	}
	defer func() { _ = querier.Close() }()
	doctorQuerier, ok := querier.(*doctorPromQuerier)
	if !ok {
		return nil, fmt.Errorf("unexpected querier type")
	}
	seen := make(map[string]map[string]string)
	for _, selectorSet := range selectorSets {
		labelMaps, seriesErr := doctorQuerier.seriesLabelMaps(ctx, selectorSet)
		if seriesErr != nil {
			return nil, seriesErr
		}
		for _, labels := range labelMaps {
			seen[metricLabelMapString(labels)] = labels
		}
	}
	out := make([]map[string]string, 0, len(seen))
	for _, labels := range seen {
		out = append(out, labels)
	}
	return out, nil
}

func (l localMetricsUpstream) PromLabels(ctx context.Context, tenant string, match []string, start, end time.Time) ([]string, error) {
	selectorSets, err := parsePrometheusSelectorSets(match)
	if err != nil {
		return nil, err
	}
	querier, err := l.svc.localQuerier(tenant, start, end)
	if err != nil {
		return nil, err
	}
	defer func() { _ = querier.Close() }()

	names := make(map[string]struct{})
	if len(selectorSets) == 0 {
		values, _, labelErr := querier.LabelNames(ctx, nil)
		if labelErr != nil {
			return nil, labelErr
		}
		for _, value := range values {
			names[value] = struct{}{}
		}
		return sortedStrings(names), nil
	}
	for _, selectorSet := range selectorSets {
		values, _, labelErr := querier.LabelNames(ctx, nil, selectorSet...)
		if labelErr != nil {
			return nil, labelErr
		}
		for _, value := range values {
			names[value] = struct{}{}
		}
	}
	return sortedStrings(names), nil
}

func (l localMetricsUpstream) PromLabelValues(ctx context.Context, tenant, label string, match []string, start, end time.Time) ([]string, error) {
	selectorSets, err := parsePrometheusSelectorSets(match)
	if err != nil {
		return nil, err
	}
	querier, err := l.svc.localQuerier(tenant, start, end)
	if err != nil {
		return nil, err
	}
	defer func() { _ = querier.Close() }()

	values := make(map[string]struct{})
	if len(selectorSets) == 0 {
		results, _, labelErr := querier.LabelValues(ctx, label, nil)
		if labelErr != nil {
			return nil, labelErr
		}
		for _, value := range results {
			values[value] = struct{}{}
		}
		return sortedStrings(values), nil
	}
	for _, selectorSet := range selectorSets {
		results, _, labelErr := querier.LabelValues(ctx, label, nil, selectorSet...)
		if labelErr != nil {
			return nil, labelErr
		}
		for _, value := range results {
			values[value] = struct{}{}
		}
	}
	return sortedStrings(values), nil
}

func (s *Service) localQuerier(tenant string, start, end time.Time) (promstorage.Querier, error) {
	start, end = clampDiscoveryWindow(start, end)
	return doctorPromQueryable{backend: s.logEngine, tenant: tenant}.Querier(start.UnixMilli(), end.UnixMilli())
}

type localLogsUpstream struct {
	svc *Service
}

func (l localLogsUpstream) Name() string { return proxy.SourceDoctor }

func (l localLogsUpstream) QueryLogs(ctx context.Context, tenant string, pushdown proxy.LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	query := pushdown.Query()
	if backend, ok := l.svc.logEngine.(rawLogQueryBackend); ok {
		return backend.QueryLogsRaw(ctx, tenant, query, from, to, limit)
	}
	return l.svc.logEngine.QueryLogs(ctx, tenant, query, "", from, to, limit)
}

// LocalTracesUpstream exposes the local trace storage as a proxy.TracesUpstream.
func (s *Service) LocalTracesUpstream() proxy.TracesUpstream {
	return localTracesUpstream{svc: s}
}

const traceDiscoverySpanLimit = 5000

type localTracesUpstream struct {
	svc *Service
}

func (l localTracesUpstream) Name() string { return proxy.SourceDoctor }

func (l localTracesUpstream) ListServices(ctx context.Context, tenant string) ([]string, error) {
	records, err := l.recentRecords(ctx, tenant)
	if err != nil {
		return nil, err
	}
	services := make(map[string]struct{})
	for _, record := range records {
		if record.Service != "" {
			services[record.Service] = struct{}{}
		}
	}
	return sortedStrings(services), nil
}

func (l localTracesUpstream) SearchTraces(ctx context.Context, tenant string, search proxy.TraceSearch) ([]proxy.TraceSummary, error) {
	records, err := l.svc.logEngine.QueryTraces(ctx, tenant, "", search.Service, search.From, search.To, search.Limit)
	if err != nil {
		return nil, err
	}
	records = canonicalizeTraceRecords(records)
	filtered := make([]segment.TraceRecord, 0, len(records))
	for _, record := range records {
		if traceRecordMatchesTags(record, search.Tags) {
			filtered = append(filtered, record)
		}
	}
	summaries := tempo.SummariesFromRecords(filtered, nil)
	out := make([]proxy.TraceSummary, 0, len(summaries))
	for _, summary := range summaries {
		start := time.Time{}
		if nanos, parseErr := strconv.ParseInt(summary.StartTimeUnixNano, 10, 64); parseErr == nil {
			start = time.Unix(0, nanos).UTC()
		}
		duration := time.Duration(summary.DurationMs) * time.Millisecond
		if search.MinDuration > 0 && duration < search.MinDuration {
			continue
		}
		if search.MaxDuration > 0 && duration > search.MaxDuration {
			continue
		}
		out = append(out, proxy.TraceSummary{
			TraceID:         summary.TraceID,
			RootServiceName: summary.RootServiceName,
			RootTraceName:   summary.RootTraceName,
			StartTime:       start,
			Duration:        duration,
			SpanCount:       summary.SpanCount,
		})
		if search.Limit > 0 && len(out) >= search.Limit {
			break
		}
	}
	return out, nil
}

func (l localTracesUpstream) GetTrace(ctx context.Context, tenant, traceID string) ([]segment.TraceRecord, bool, error) {
	records, err := l.svc.logEngine.QueryTraces(ctx, tenant, traceID, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		return nil, false, err
	}
	if len(records) == 0 {
		return nil, false, nil
	}
	return canonicalizeTraceRecords(records), true, nil
}

// canonicalizeTraceRecords merges the shared canonical label vocabulary into
// span attributes so Tempo search and Grafana correlation see the same names as
// logs and metrics.
func canonicalizeTraceRecords(records []segment.TraceRecord) []segment.TraceRecord {
	out := make([]segment.TraceRecord, 0, len(records))
	for _, record := range records {
		attrs := mergeStringMaps(record.ResourceAttributes, record.Attributes)
		for key, value := range canonicalLabels(record.Service, record.Tenant, record.Partition, record.Intent, record.Asset, attrs) {
			if record.Attributes == nil {
				record.Attributes = make(map[string]string)
			}
			if _, exists := record.Attributes[key]; !exists {
				record.Attributes[key] = value
			}
		}
		out = append(out, record)
	}
	return out
}

func (l localTracesUpstream) TraceTags(ctx context.Context, tenant string) ([]string, error) {
	records, err := l.recentRecords(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return tempo.TagKeysFromRecords(records), nil
}

func (l localTracesUpstream) TraceTagValues(ctx context.Context, tenant, tag string) ([]string, error) {
	records, err := l.recentRecords(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return tempo.TagValuesFromRecords(records, tag), nil
}

func (l localTracesUpstream) recentRecords(ctx context.Context, tenant string) ([]segment.TraceRecord, error) {
	end := time.Now().UTC()
	start := end.Add(-doctorDiscoveryWindow)
	records, err := l.svc.logEngine.QueryTraces(ctx, tenant, "", "", start, end, traceDiscoverySpanLimit)
	if err != nil {
		return nil, err
	}
	return canonicalizeTraceRecords(records), nil
}

func traceRecordMatchesTags(record segment.TraceRecord, tags []proxy.TraceTag) bool {
	for _, tag := range tags {
		var value string
		switch tag.Key {
		case "service.name", "service":
			value = record.Service
		case "name":
			value = record.Name
		case "kind":
			value = record.Kind
		case "status", "status.code":
			value = record.StatusCode
		case "guardian_partition":
			value = record.Partition
		case "guardian_intent":
			value = record.Intent
		case "guardian_asset":
			value = record.Asset
		default:
			value = record.Attributes[tag.Key]
			if value == "" {
				value = record.ResourceAttributes[tag.Key]
			}
		}
		if value != tag.Value {
			return false
		}
	}
	return true
}
