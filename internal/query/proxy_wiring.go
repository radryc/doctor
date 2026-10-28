package query

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	promstorage "github.com/prometheus/prometheus/storage"
	"github.com/radryc/doctor/internal/proxy"
	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/doctor/internal/tempo"
	"github.com/radryc/doctor/internal/tracecontext"
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
	data, err, parse := l.svc.execPrometheusInstant(ctx, tenant, "", query, at)
	if err != nil {
		return nil, &proxy.QueryError{Err: err, Parse: parse}
	}
	return data, nil
}

func (l localMetricsUpstream) PromRange(ctx context.Context, tenant, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	data, err, parse := l.svc.execPrometheusRange(ctx, tenant, "", query, start, end, step)
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
	if limit <= 0 || limit > logQueryRecordLimit {
		limit = logQueryRecordLimit
	}
	if backend, ok := l.svc.logEngine.(rawLogQueryBackend); ok {
		return backend.QueryLogsRaw(ctx, tenant, query, from, to, limit)
	}
	return l.svc.logEngine.QueryLogs(ctx, tenant, query, "", from, to, limit)
}

// LocalTracesUpstream exposes the local trace storage as a proxy.TracesUpstream.
func (s *Service) LocalTracesUpstream() proxy.TracesUpstream {
	return localTracesUpstream{svc: s}
}

const (
	traceDiscoverySpanLimit = 5000
	traceGetSpanLimit       = 20000
	logQueryRecordLimit     = 5000
)

type traceDiscoverySource interface {
	QueryTraceDiscovery(ctx context.Context, tenant, discovery, tag string, from, to time.Time) ([]string, error)
}

type localTracesUpstream struct {
	svc *Service
}

func (l localTracesUpstream) discovery(ctx context.Context, tenant, kind, tag string) ([]string, bool, error) {
	source, ok := l.svc.logEngine.(traceDiscoverySource)
	if !ok {
		return nil, false, nil
	}
	end := time.Now().UTC()
	values, err := source.QueryTraceDiscovery(ctx, tenant, kind, tag, end.Add(-l.svc.traceWindow()), end)
	if err != nil {
		// During a rolling MonoFS upgrade the client may be newer than the
		// server. Fall back to the legacy span scan rather than breaking the
		// service/tag picker.
		proxy.AddWarning(ctx, proxy.Warning{Backend: "monofs", Signal: proxy.SignalTraces, Message: "trace discovery unavailable: " + err.Error()})
		return nil, false, nil
	}
	return values, true, nil
}

func (l localTracesUpstream) Name() string { return proxy.SourceDoctor }

func (l localTracesUpstream) ListServices(ctx context.Context, tenant string) ([]string, error) {
	if services, ok, err := l.discovery(ctx, tenant, "services", ""); ok {
		return services, err
	}
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
	limit := search.Limit
	if limit <= 0 || limit > traceDiscoverySpanLimit {
		limit = traceDiscoverySpanLimit
	}
	records, err := l.svc.logEngine.QueryTraces(ctx, tenant, "", search.Service, search.From, search.To, limit)
	if err != nil {
		return nil, err
	}
	records = canonicalizeTraceRecords(records)

	// First pass: find the traces that have at least one matching span. The
	// summaries are then built from every span of those traces so root service
	// and duration reflect the trace, not the matched subset.
	matchedTraces := make(map[string]struct{})
	for _, record := range records {
		if proxy.MatchTraceRecord(record, search) {
			matchedTraces[record.TraceID] = struct{}{}
		}
	}
	if len(matchedTraces) == 0 {
		return nil, nil
	}
	full := make([]segment.TraceRecord, 0, len(records))
	for _, record := range records {
		if _, ok := matchedTraces[record.TraceID]; ok {
			full = append(full, record)
		}
	}

	summaries := tempo.SummariesFromRecords(full, nil)
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
	return l.GetTraceWindow(ctx, tenant, traceID, time.Time{}, time.Time{})
}

// GetTraceWindow queries the local store for one trace inside an explicit
// time window. Callers that know the trace window should use this method; the
// zero-window compatibility path remains for older callers.
func (l localTracesUpstream) GetTraceWindow(ctx context.Context, tenant, traceID string, from, to time.Time) ([]segment.TraceRecord, bool, error) {
	// X-Ray trace IDs are served by the AWS upstream. Querying every local
	// chunk for an ID that cannot exist there turns a federated trace detail
	// request into a full local scan and can exceed the HTTP deadline.
	if isXRayTraceID(traceID) {
		return nil, false, nil
	}
	records, err := l.svc.logEngine.QueryTraces(ctx, tenant, traceID, "", from, to, traceGetSpanLimit)
	if err != nil {
		return nil, false, err
	}
	if len(records) == 0 {
		return nil, false, nil
	}
	return canonicalizeTraceRecords(records), true, nil
}

// isXRayTraceID reports whether id uses the X-Ray 1-<epoch>-<hex> format.
func isXRayTraceID(id string) bool {
	return tracecontext.IsXRayTraceID(id)
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
	if tags, ok, err := l.discovery(ctx, tenant, "tags", ""); ok {
		return tags, err
	}
	records, err := l.recentRecords(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return tempo.TagKeysFromRecords(records), nil
}

// TraceTagScopes implements proxy.TraceTagScopeSource (Tempo v2 tags).
func (l localTracesUpstream) TraceTagScopes(ctx context.Context, tenant string) ([]tempo.TagScope, error) {
	if tags, ok, err := l.discovery(ctx, tenant, "tags", ""); ok {
		if err != nil {
			return nil, err
		}
		return tempo.TagScopesFromNames(tags), nil
	}
	records, err := l.recentRecords(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return tempo.TagScopesFromRecords(records), nil
}

// SearchSpanSets implements proxy.TraceSpanSource: it returns the matched spans
// of a search grouped by trace ID, capped at spss spans per trace.
func (l localTracesUpstream) SearchSpanSets(ctx context.Context, tenant string, search proxy.TraceSearch, spss int) (map[string]tempo.SpanSet, error) {
	if spss <= 0 {
		return nil, nil
	}
	records, err := l.svc.logEngine.QueryTraces(ctx, tenant, "", search.Service, search.From, search.To, traceDiscoverySpanLimit)
	if err != nil {
		return nil, err
	}
	records = canonicalizeTraceRecords(records)

	sets := make(map[string]tempo.SpanSet)
	resourceAttrs := make(map[string]map[string]string)
	for _, record := range records {
		if !proxy.MatchTraceRecord(record, search) {
			continue
		}
		set := sets[record.TraceID]
		set.Matched++
		if len(set.Spans) < spss {
			set.Spans = append(set.Spans, searchSpanFromRecord(record))
		}
		sets[record.TraceID] = set

		attrs := resourceAttrs[record.TraceID]
		if attrs == nil {
			attrs = make(map[string]string)
			resourceAttrs[record.TraceID] = attrs
		}
		for key, value := range record.ResourceAttributes {
			if _, exists := attrs[key]; !exists {
				attrs[key] = value
			}
		}
	}
	for traceID, attrs := range resourceAttrs {
		set := sets[traceID]
		set.Attributes = stringAttributes(attrs)
		sets[traceID] = set
	}
	return sets, nil
}

// searchSpanFromRecord converts a span record into a Tempo search span.
func searchSpanFromRecord(record segment.TraceRecord) tempo.SearchSpan {
	duration := int64(0)
	if !record.StartTime.IsZero() && !record.EndTime.IsZero() {
		if delta := record.EndTime.Sub(record.StartTime); delta > 0 {
			duration = delta.Nanoseconds()
		}
	}
	return tempo.SearchSpan{
		SpanID:            record.SpanID,
		Name:              record.Name,
		StartTimeUnixNano: strconv.FormatInt(record.StartTime.UTC().UnixNano(), 10),
		DurationNanos:     strconv.FormatInt(duration, 10),
		Attributes:        stringAttributes(record.Attributes),
	}
}

// stringAttributes renders a string map as sorted OTLP-JSON key/value pairs.
func stringAttributes(attrs map[string]string) []tempo.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]tempo.KeyValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, tempo.KeyValue{Key: key, Value: tempo.Value{StringValue: attrs[key]}})
	}
	return out
}

func (l localTracesUpstream) TraceTagValues(ctx context.Context, tenant, tag string) ([]string, error) {
	if values, ok, err := l.discovery(ctx, tenant, "tag_values", tag); ok {
		return values, err
	}
	records, err := l.recentRecords(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return tempo.TagValuesFromRecords(records, tag), nil
}

func (l localTracesUpstream) recentRecords(ctx context.Context, tenant string) ([]segment.TraceRecord, error) {
	end := time.Now().UTC()
	start := end.Add(-l.svc.traceWindow())
	records, err := l.svc.logEngine.QueryTraces(ctx, tenant, "", "", start, end, traceDiscoverySpanLimit)
	if err != nil {
		return nil, err
	}
	return canonicalizeTraceRecords(records), nil
}
