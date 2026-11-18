package query

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/access"
	"github.com/radryc/doctor/internal/proxy"
	"github.com/radryc/doctor/internal/segment"
)

// handlePrometheusQueryViaRouter executes an instant query through the
// multi-backend router, surfacing partial failures as a response header.
type proxyQueryResult struct {
	data     map[string]any
	warnings []proxy.Warning
	meta     proxy.QueryMeta
}

// metricsInstantViaRouter coalesces identical in-flight instant queries. The
// key includes the identity filters because the result is filtered in place
// per request. A one-second bucket lets concurrent clients asking for "now"
// share a single backend fan-out without making the result visibly stale.
func (s *Service) metricsInstantViaRouter(ctx context.Context, tenant, query string, at time.Time, identity signalIdentity) (map[string]any, []proxy.Warning, proxy.QueryMeta, error) {
	key := fmt.Sprintf("instant|%s|%s|%s|%s|%s|%d", tenant, identity.Partition, identity.Intent, identity.Asset, query, at.Unix())
	result, err := s.queryFlight.Do(ctx, key, func(callCtx context.Context) (proxyQueryResult, error) {
		data, warnings, meta, err := s.proxyRouter.MetricsInstantForPartitionWithMeta(callCtx, tenant, query, at, identity.Partition)
		if err != nil {
			return proxyQueryResult{}, err
		}
		return proxyQueryResult{data: data, warnings: warnings, meta: meta}, nil
	})
	if err != nil {
		return nil, nil, proxy.QueryMeta{}, err
	}
	return result.data, result.warnings, result.meta, nil
}

func (s *Service) metricsRangeViaRouter(ctx context.Context, tenant, query string, start, end time.Time, step time.Duration, identity signalIdentity) (map[string]any, []proxy.Warning, proxy.QueryMeta, error) {
	key := fmt.Sprintf("range|%s|%s|%s|%s|%s|%d|%d|%d", tenant, identity.Partition, identity.Intent, identity.Asset, query, start.Unix(), end.Unix(), step.Nanoseconds())
	result, err := s.queryFlight.Do(ctx, key, func(callCtx context.Context) (proxyQueryResult, error) {
		data, warnings, meta, err := s.proxyRouter.MetricsRangeForPartitionWithMeta(callCtx, tenant, query, start, end, step, identity.Partition)
		if err != nil {
			return proxyQueryResult{}, err
		}
		return proxyQueryResult{data: data, warnings: warnings, meta: meta}, nil
	})
	if err != nil {
		return nil, nil, proxy.QueryMeta{}, err
	}
	return result.data, result.warnings, result.meta, nil
}

func (s *Service) handlePrometheusQueryViaRouter(w http.ResponseWriter, r *http.Request, query string, at time.Time) {
	data, warnings, meta, err := s.metricsInstantViaRouter(r.Context(), s.tenantFromRequest(r), query, at, identityFromRequest(r))
	if err != nil {
		switch {
		case errors.Is(err, proxy.ErrScopeRequired), errors.Is(err, proxy.ErrTooBroad):
			writePrometheusError(w, http.StatusUnprocessableEntity, "bad_data", err.Error())
		case proxy.IsParseError(err):
			writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		default:
			writePrometheusExecutionError(w, err)
		}
		return
	}
	writeProxyWarnings(w, warnings)
	writeAggregationHeader(w, meta)
	filtered := s.filterPrometheusDataForRequest(r, data)
	if !s.enforceSeriesLimit(w, r, filtered) {
		return
	}
	writePrometheusSuccess(w, filtered)
}

// handlePrometheusQueryRangeViaRouter executes a range query through the
// multi-backend router.
func (s *Service) handlePrometheusQueryRangeViaRouter(w http.ResponseWriter, r *http.Request, query string, start, end time.Time, step time.Duration) {
	data, warnings, meta, err := s.metricsRangeViaRouter(r.Context(), s.tenantFromRequest(r), query, start, end, step, identityFromRequest(r))
	if err != nil {
		switch {
		case errors.Is(err, proxy.ErrScopeRequired), errors.Is(err, proxy.ErrTooBroad):
			writePrometheusError(w, http.StatusUnprocessableEntity, "bad_data", err.Error())
		case proxy.IsParseError(err):
			writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		default:
			writePrometheusExecutionError(w, err)
		}
		return
	}
	writeProxyWarnings(w, warnings)
	writeAggregationHeader(w, meta)
	filtered := s.filterPrometheusDataForRequest(r, data)
	if !s.enforceSeriesLimit(w, r, filtered) {
		return
	}
	writePrometheusSuccess(w, filtered)
}

// handleProxyEndpoints lists the configured backends, fleet summary, and their health.
func (s *Service) handleProxyEndpoints(w http.ResponseWriter, r *http.Request) {
	if s.proxyRouter == nil {
		writeJSON(w, http.StatusOK, map[string]any{"endpoints": []proxy.EndpointInfo{}})
		return
	}
	if s.awsReady() {
		if prober, ok := s.aws.(awsHealthProber); ok {
			s.proxyRouter.ReportBackendHealth("aws", prober.Health(r.Context()))
		}
	}
	if s.gcp != nil {
		if prober, ok := s.gcp.(interface{ Health(context.Context) error }); ok {
			s.proxyRouter.ReportBackendHealth("gcp", prober.Health(r.Context()))
		}
	}
	federation := map[string]any{}
	if s.federationHealth != nil {
		federation = s.federationHealth()
		for provider, raw := range federation {
			summary, _ := raw.(map[string]any)
			total, _ := summary["total"].(int)
			open, _ := summary["open"].(int)
			if total > 0 && open >= total {
				s.proxyRouter.ReportBackendHealth(provider, fmt.Errorf("all %d federation targets are circuit-open", total))
			} else {
				s.proxyRouter.ReportBackendHealth(provider, nil)
			}
		}
	}
	q := r.URL.Query()
	if q.Get("summary_only") == "1" || q.Get("summary_only") == "true" {
		summary := s.proxyRouter.SummarizeEndpoints()
		writeJSON(w, http.StatusOK, map[string]any{
			"summary":   summary,
			"endpoints": []proxy.EndpointInfo{},
		})
		return
	}

	limitParam := strings.TrimSpace(q.Get("limit"))
	cursorParam := strings.TrimSpace(q.Get("cursor"))
	kindParam := strings.TrimSpace(q.Get("kind"))
	statusParam := strings.TrimSpace(q.Get("status"))
	regionParam := strings.TrimSpace(q.Get("region"))
	accountParam := strings.TrimSpace(q.Get("account"))
	clusterParam := strings.TrimSpace(q.Get("cluster"))
	versionParam := strings.TrimSpace(q.Get("version"))
	queryParam := strings.TrimSpace(q.Get("q"))
	outliersParam := q.Get("outliers") == "1" || q.Get("outliers") == "true"
	unhealthyParam := q.Get("unhealthy") == "1" || q.Get("unhealthy") == "true"
	summaryParam := q.Get("summary") == "1" || q.Get("summary") == "true"

	hasFilterOrPage := limitParam != "" || cursorParam != "" || kindParam != "" ||
		statusParam != "" || regionParam != "" || accountParam != "" ||
		clusterParam != "" || versionParam != "" || queryParam != "" ||
		outliersParam || unhealthyParam

	if hasFilterOrPage {
		limit := 200
		if limitParam != "" {
			if parsed, err := strconv.Atoi(limitParam); err == nil && parsed > 0 {
				limit = parsed
			}
		}
		page := s.proxyRouter.EndpointsPage(r.Context(), proxy.EndpointPageFilter{
			Kind:      kindParam,
			Status:    statusParam,
			Region:    regionParam,
			Account:   accountParam,
			Cluster:   clusterParam,
			Version:   versionParam,
			Query:     queryParam,
			Outliers:  outliersParam,
			Unhealthy: unhealthyParam,
			Limit:     limit,
			Cursor:    cursorParam,
		})
		writeJSON(w, http.StatusOK, page)
		return
	}

	endpoints := s.proxyRouter.Endpoints(r.Context())
	resp := map[string]any{"endpoints": endpoints, "federation": federation}
	if summaryParam {
		resp["summary"] = s.proxyRouter.SummarizeEndpoints()
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeProxyWarnings reports partial backend failures without failing the
// response. Observed access denials are surfaced separately so operators can
// audit the observe-first rollout.
func writeAggregationHeader(w http.ResponseWriter, meta proxy.QueryMeta) {
	if meta.Aggregation == "" {
		return
	}
	w.Header().Set("X-Doctor-Federation-Aggregation", meta.Aggregation)
}

func writeProxyWarnings(w http.ResponseWriter, warnings []proxy.Warning) {
	if len(warnings) == 0 {
		return
	}
	parts := make([]string, 0, len(warnings))
	observed := 0
	for _, warning := range warnings {
		parts = append(parts, warning.Backend+": "+warning.Message)
		if strings.Contains(warning.Message, "access denied (observed)") {
			observed++
		}
	}
	w.Header().Set("X-Doctor-Partial", strings.Join(parts, "; "))
	if observed > 0 {
		w.Header().Set("X-Doctor-Access-Observed", strconv.Itoa(observed))
	}
}

// isAccessDenied reports whether an upstream error is an access enforcement
// denial.
func isAccessDenied(err error) bool {
	return errors.Is(err, access.ErrAccessDenied)
}

// logSearchPushdown converts the native log-search parameters into the reduced
// LogQL form used by proxy upstreams.
func isGCPLogName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	return strings.Contains(value, ".googleapis.com/") ||
		strings.Contains(value, ".googleapis.com%2f") ||
		strings.Contains(value, "/logs/")
}

func logSearchPushdown(service, text, logGroup string, identity signalIdentity) proxy.LogPushdown {
	selector := identity.logSelector(service)
	switch {
	case isGCPLogName(logGroup):
		selector = `{source="gcp", gcp_log_name="` + escapeLogQL(strings.TrimSpace(logGroup)) + `"}`
	case strings.TrimSpace(logGroup) != "":
		// CloudWatch log groups are not labelled with Doctor identity. Filter
		// the fetched records after the upstream returns them.
		selector = `{source="aws", aws_log_group="` + escapeLogQL(strings.TrimSpace(logGroup)) + `"}`
	default:
		if _, mode := proxy.AWSQueryMode(text); mode != proxy.AWSQueryNone {
			// An AWS/X-Ray upstream may not carry guardian_* labels, so keep
			// the selector broad and apply identity filters to the result.
			selector = `{service=~".+"}`
			if service != "" {
				selector = `{service="` + escapeLogQL(service) + `"}`
			}
		}
	}
	pushdown := proxy.LogPushdown{Selector: selector}
	if text != "" {
		pushdown.LineFilters = []proxy.LogLineFilter{{Op: "|=", Value: text}}
	}
	return pushdown
}

func escapeLogQL(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}

// searchSpansViaRouter implements the native span search over the proxy. When
// a trace ID is given it fetches that trace; otherwise it searches trace
// summaries and hydrates spans up to the limit.
func (s *Service) searchSpansViaRouter(r *http.Request, tenant, service, traceID string, from, to time.Time, limit int) ([]segment.TraceRecord, error) {
	if traceID != "" {
		records, _, err := s.proxyRouter.GetTrace(r.Context(), tenant, traceID)
		if err != nil {
			return nil, err
		}
		return filterSpansByService(records, service), nil
	}
	summaries, warnings, err := s.proxyRouter.SearchTraces(r.Context(), tenant, proxy.TraceSearch{
		Service: service,
		From:    from,
		To:      to,
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}
	_ = warnings
	spans := make([]segment.TraceRecord, 0, limit)
	for _, summary := range summaries {
		if len(spans) >= limit {
			break
		}
		records, _, err := s.proxyRouter.GetTrace(r.Context(), tenant, summary.TraceID)
		if err != nil {
			if isAccessDenied(err) {
				return nil, err
			}
			continue
		}
		spans = append(spans, filterSpansByService(records, service)...)
	}
	if len(spans) > limit {
		spans = spans[:limit]
	}
	return spans, nil
}

func filterSpansByService(records []segment.TraceRecord, service string) []segment.TraceRecord {
	if service == "" {
		return records
	}
	out := make([]segment.TraceRecord, 0, len(records))
	for _, record := range records {
		if record.Service == service {
			out = append(out, record)
		}
	}
	return out
}

// handlePrometheusLabelsViaRouter implements /api/v1/labels through the
// multi-backend router, so static and discovered Prometheus/Doctor backends
// are visible to Grafana's label autocomplete.
func (s *Service) handlePrometheusLabelsViaRouter(w http.ResponseWriter, r *http.Request) {
	match := requestParams(r, "match[]")
	start, end := prometheusDiscoveryRange(r)
	values, warnings, err := s.proxyRouter.MetricsLabels(
		r.Context(), s.tenantFromRequest(r), match, start, end, partitionFromRequest(r))
	if err != nil {
		writePrometheusDiscoveryError(w, err)
		return
	}
	writeProxyWarnings(w, warnings)
	out := append([]string(nil), values...)
	found := false
	for _, value := range out {
		if value == labelSource {
			found = true
			break
		}
	}
	if !found {
		out = append(out, labelSource)
	}
	sort.Strings(out)
	writePrometheusSuccess(w, out)
}

// handlePrometheusLabelValuesViaRouter implements
// /api/v1/label/{name}/values through the router. The metric catalog keeps its
// provider-priority and truncation behaviour for __name__.
func (s *Service) handlePrometheusLabelValuesViaRouter(w http.ResponseWriter, r *http.Request) {
	rawLabelName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/label/"), "/values")
	if rawLabelName == "" {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "label name is required")
		return
	}
	labelName, err := url.PathUnescape(rawLabelName)
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}

	sources, err := parseMetricCatalogSource(r.URL.Query().Get("source"))
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	rawSource := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	includeOther := rawSource == "" || rawSource == "all"
	for _, part := range strings.Split(rawSource, ",") {
		if strings.TrimSpace(part) == "all" {
			includeOther = true
		}
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := parseMetricCatalogLimit(r.URL.Query().Get("limit"))
	if limit == 0 {
		// Bound discovery even when the caller does not send ?limit=. A 50k
		// target catalog must not serialize every cached label value.
		limit = metricCatalogLimit
	}

	match := requestParams(r, "match[]")
	start, end := prometheusDiscoveryRange(r)
	results, warnings, err := s.proxyRouter.MetricsLabelValues(
		r.Context(), s.tenantFromRequest(r), labelName, match, start, end, partitionFromRequest(r))
	if err != nil {
		writePrometheusDiscoveryError(w, err)
		return
	}
	writeProxyWarnings(w, warnings)

	values := make(map[string]int)
	add := func(priority int, value string) {
		if value == "" {
			return
		}
		if current, ok := values[value]; !ok || priority < current {
			values[value] = priority
		}
	}
	for _, result := range results {
		if !metricSourceEnabled(sources, result.Source, includeOther) {
			continue
		}
		add(metricSourcePriority(result.Source), result.Value)
	}

	if labelName == labelSource {
		available := map[string]struct{}{}
		for _, source := range s.proxyRouter.MetricSources() {
			available[source] = struct{}{}
		}
		if sources.doctor {
			if _, ok := available[proxy.SourceDoctor]; ok {
				add(metricCatalogDoctorPriority, sourceDoctor)
			}
		}
		if sources.aws {
			if _, ok := available[proxy.SourceAWS]; ok {
				add(metricCatalogAWSPriority, awsSourceLabel)
			}
		}
		if sources.gcp {
			if _, ok := available[proxy.SourceGCP]; ok {
				add(metricCatalogGCPPriority, proxy.SourceGCP)
			}
		}
	}

	if labelName != "__name__" {
		returned := sortedPriorityValues(values)
		if limit > 0 && len(returned) > limit {
			w.Header().Set("X-Doctor-Metric-Truncated", "true")
			returned = returned[:limit]
		}
		writePrometheusSuccess(w, returned)
		return
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	names = filterMetricCatalog(names, search)
	w.Header().Set("X-Doctor-Metric-Total", strconv.Itoa(len(names)))
	out := arrangeMetricCatalog(names, values, limit)
	if limit > 0 && len(names) > limit {
		w.Header().Set("X-Doctor-Metric-Truncated", "true")
	}
	writePrometheusSuccess(w, out)
}

// handlePrometheusSeriesViaRouter implements /api/v1/series through the router.
func (s *Service) handlePrometheusSeriesViaRouter(w http.ResponseWriter, r *http.Request) {
	match := requestParams(r, "match[]")
	start, end := prometheusDiscoveryRange(r)
	series, warnings, err := s.proxyRouter.MetricsSeries(
		r.Context(), s.tenantFromRequest(r), match, start, end, partitionFromRequest(r))
	if err != nil {
		writePrometheusDiscoveryError(w, err)
		return
	}
	writeProxyWarnings(w, warnings)
	if len(series) > maxPrometheusSeries {
		writePrometheusError(w, http.StatusUnprocessableEntity, "execution", fmt.Sprintf(
			"series result exceeds %d; narrow the selector or time range", maxPrometheusSeries))
		return
	}
	writePrometheusSuccess(w, series)
}

func metricSourcePriority(source string) int {
	switch source {
	case proxy.SourceDoctor:
		return metricCatalogDoctorPriority
	case proxy.SourceAWS:
		return metricCatalogAWSPriority
	case proxy.SourceGCP:
		return metricCatalogGCPPriority
	default:
		return metricCatalogGCPPriority
	}
}

func metricSourceEnabled(sources metricCatalogSource, source string, includeOther bool) bool {
	switch source {
	case proxy.SourceDoctor:
		return sources.doctor
	case proxy.SourceAWS:
		return sources.aws
	case proxy.SourceGCP:
		return sources.gcp
	default:
		return includeOther
	}
}
