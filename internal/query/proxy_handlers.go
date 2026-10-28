package query

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/access"
	"github.com/radryc/doctor/internal/proxy"
	"github.com/radryc/doctor/internal/segment"
)

// handlePrometheusQueryViaRouter executes an instant query through the
// multi-backend router, surfacing partial failures as a response header.
func (s *Service) handlePrometheusQueryViaRouter(w http.ResponseWriter, r *http.Request, query string, at time.Time) {
	data, warnings, err := s.proxyRouter.MetricsInstant(r.Context(), s.tenantFromRequest(r), query, at)
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
	writePrometheusSuccess(w, filterPrometheusDataByIdentity(r, data))
}

// handlePrometheusQueryRangeViaRouter executes a range query through the
// multi-backend router.
func (s *Service) handlePrometheusQueryRangeViaRouter(w http.ResponseWriter, r *http.Request, query string, start, end time.Time, step time.Duration) {
	data, warnings, err := s.proxyRouter.MetricsRange(r.Context(), s.tenantFromRequest(r), query, start, end, step)
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
	writePrometheusSuccess(w, filterPrometheusDataByIdentity(r, data))
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
	resp := map[string]any{"endpoints": endpoints}
	if summaryParam {
		resp["summary"] = s.proxyRouter.SummarizeEndpoints()
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeProxyWarnings reports partial backend failures without failing the
// response. Observed access denials are surfaced separately so operators can
// audit the observe-first rollout.
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
func logSearchPushdown(service, text, logGroup string, identity signalIdentity) proxy.LogPushdown {
	selector := identity.logSelector(service)
	switch {
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
