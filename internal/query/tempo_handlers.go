package query

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/proxy/translate"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/tempo"
)

// handleTempoEcho reports readiness for the Tempo datasource.
func (s *Service) handleTempoEcho(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleTempoBuildInfo serves the Tempo /api/status/buildinfo probe. Grafana
// calls it while initializing the datasource; advertising the oldest supported
// API version keeps feature detection (for example streaming search) off.
func (s *Service) handleTempoBuildInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version":   "2.1.0",
		"revision":  "doctor",
		"branch":    "",
		"buildDate": "",
		"buildUser": "",
		"goVersion": "",
	})
}

// handleTempoTraceByID serves GET /api/traces/{traceID} (Tempo v1). Grafana
// requests application/protobuf; JSON remains the default for other clients.
func (s *Service) handleTempoTraceByID(w http.ResponseWriter, r *http.Request) {
	s.serveTempoTraceByID(w, r, "/api/traces/", false)
}

// handleTempoTraceByIDV2 serves GET /api/v2/traces/{traceID} (Tempo v2,
// TraceByIDResponse). Grafana v12 tries this route before the v1 one.
func (s *Service) handleTempoTraceByIDV2(w http.ResponseWriter, r *http.Request) {
	s.serveTempoTraceByID(w, r, "/api/v2/traces/", true)
}

func (s *Service) serveTempoTraceByID(w http.ResponseWriter, r *http.Request, prefix string, v2 bool) {
	traceID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, prefix))
	if traceID == "" {
		http.Error(w, "trace id is required", http.StatusBadRequest)
		return
	}
	tenant := s.tenantFromRequest(r)
	from, to := traceLatestWindow(r)
	traceID, err := s.resolveTraceID(r.Context(), tenant, traceID, from, to)
	if err != nil {
		if isAccessDenied(err) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if traceID == "" {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}

	var (
		records  []segment.TraceRecord
		found    bool
		warnings []proxy.Warning
		queryErr error
	)
	if s.proxyRouter != nil {
		records, warnings, queryErr = s.proxyRouter.GetTrace(r.Context(), tenant, traceID)
		found = len(records) > 0
	} else {
		local := localTracesUpstream{svc: s}
		records, found, queryErr = local.GetTrace(r.Context(), tenant, traceID)
		if queryErr == nil && !found && s.awsReady() {
			records, found, queryErr = (s.AWSUpstream()).GetTrace(r.Context(), tenant, traceID)
		}
	}
	if queryErr != nil {
		if isAccessDenied(queryErr) {
			http.Error(w, queryErr.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, queryErr.Error(), http.StatusInternalServerError)
		return
	}
	if !found || len(records) == 0 {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}
	writeProxyWarnings(w, warnings)

	if acceptsProtobuf(r) {
		var (
			body []byte
			perr error
		)
		if v2 {
			body, perr = tempo.ProtoTraceByIDResponse(records)
		} else {
			body, perr = tempo.ProtoFromRecords(records)
		}
		if perr != nil {
			http.Error(w, perr.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/protobuf")
		_, _ = w.Write(body)
		return
	}

	if v2 {
		writeJSON(w, http.StatusOK, map[string]any{
			"trace":  tempo.TraceFromRecords(records),
			"status": "COMPLETE",
		})
		return
	}
	writeJSON(w, http.StatusOK, tempo.TraceFromRecords(records))
}

// acceptsProtobuf reports whether the client asked for the OTLP protobuf body.
func acceptsProtobuf(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "protobuf")
}

// latestTraceID is the magic trace ID the provisioned Grafana dashboards use to
// pin their trace waterfall panels to the newest trace. Grafana's Tempo
// datasource validates trace IDs before sending them, so the dashboards use the
// magic hex aliases (valid 32-hex strings); the plain names stay supported for
// the doctor UI, deep links, and direct API use. latest-service:<prefix> (or its
// magic hex alias) scopes the lookup to services matching the prefix.
const (
	latestTraceID       = "latest"
	latestAWSTraceID    = "latest-aws"
	latestDoctorTraceID = "latest-doctor"
	latestServicePrefix = "latest-service:"

	magicLatestTraceID        = "00000000000000000000000000000000"
	magicLatestAWSTraceID     = "00000000000000000000000000000001"
	magicLatestDoctorTraceID  = "00000000000000000000000000000002"
	magicLatestServicePrefix  = "ad00"
	magicLatestTraceHexLength = 32
)

// normalizeLatestMagic maps the supported latest* aliases (plain or hex) to a
// lookup kind and optional service prefix.
func normalizeLatestMagic(traceID string) (kind, service string, ok bool) {
	trimmed := strings.TrimSpace(traceID)
	lower := strings.ToLower(trimmed)
	switch lower {
	case latestTraceID, magicLatestTraceID:
		return "all", "", true
	case latestAWSTraceID, magicLatestAWSTraceID:
		return "aws", "", true
	case latestDoctorTraceID, magicLatestDoctorTraceID:
		return "doctor", "", true
	}
	if strings.HasPrefix(lower, latestServicePrefix) {
		if scoped := strings.TrimSpace(trimmed[len(latestServicePrefix):]); scoped != "" {
			return "service", scoped, true
		}
	}
	if scoped, ok := decodeLatestServiceMagic(lower); ok {
		return "service", scoped, true
	}
	return "", "", false
}

// decodeLatestServiceMagic decodes a 32-hex magic ID into a service prefix.
func decodeLatestServiceMagic(id string) (string, bool) {
	if len(id) != magicLatestTraceHexLength || !strings.HasPrefix(id, magicLatestServicePrefix) {
		return "", false
	}
	raw, err := hex.DecodeString(id[len(magicLatestServicePrefix):])
	if err != nil {
		return "", false
	}
	service := strings.TrimRight(string(raw), "\x00")
	if service == "" {
		return "", false
	}
	return service, true
}

// traceLatestWindow returns the search window for latestTraceID. Grafana sends
// the dashboard range as start/end; otherwise look back one hour.
func traceLatestWindow(r *http.Request) (time.Time, time.Time) {
	now := time.Now().UTC()
	end, err := parseLokiTime(r.URL.Query().Get("end"), now)
	if err != nil {
		end = now
	}
	start, err := parseLokiTime(r.URL.Query().Get("start"), end.Add(-time.Hour))
	if err != nil || !end.After(start) {
		start = end.Add(-time.Hour)
	}
	return start, end
}

// resolveTraceID maps the latest* magics to the most recent trace in the
// window, optionally scoped to one federation source or service prefix. Other
// IDs pass through untouched.
func (s *Service) resolveTraceID(ctx context.Context, tenant, traceID string, from, to time.Time) (string, error) {
	kind, service, ok := normalizeLatestMagic(traceID)
	if !ok {
		return traceID, nil
	}
	if kind == "service" {
		return s.latestServiceTraceID(ctx, tenant, service, from, to)
	}
	search := proxy.TraceSearch{From: from, To: to, Limit: 1}
	var summaries []proxy.TraceSummary
	switch {
	case kind == "doctor":
		localSummaries, err := (localTracesUpstream{svc: s}).SearchTraces(ctx, tenant, search)
		if err != nil {
			return "", err
		}
		summaries = localSummaries
	case kind == "aws":
		if !s.awsReady() {
			return "", nil
		}
		awsSummaries, err := (s.AWSUpstream()).SearchTraces(ctx, tenant, search)
		if err != nil {
			return "", err
		}
		summaries = awsSummaries
	case s.proxyRouter != nil:
		merged, _, err := s.proxyRouter.SearchTraces(ctx, tenant, search)
		if err != nil {
			return "", err
		}
		summaries = merged
	default:
		localSummaries, err := (localTracesUpstream{svc: s}).SearchTraces(ctx, tenant, search)
		if err != nil {
			return "", err
		}
		groups := [][]proxy.TraceSummary{localSummaries}
		if s.awsReady() {
			if awsSummaries, awsErr := (s.AWSUpstream()).SearchTraces(ctx, tenant, search); awsErr == nil {
				groups = append(groups, awsSummaries)
			}
		}
		summaries = proxy.MergeTraceSummaries(groups...)
	}
	if len(summaries) == 0 {
		return "", nil
	}
	return summaries[0].TraceID, nil
}

// latestServiceTraceID resolves the newest trace whose root or leaf service
// starts with the requested prefix, searching both native and federated AWS
// traces.
func (s *Service) latestServiceTraceID(ctx context.Context, tenant, service string, from, to time.Time) (string, error) {
	search := proxy.TraceSearch{From: from, To: to, Limit: traceDiscoverySpanLimit}
	localSummaries, err := (localTracesUpstream{svc: s}).SearchTraces(ctx, tenant, search)
	if err != nil {
		return "", err
	}
	groups := [][]proxy.TraceSummary{localSummaries}
	if s.awsReady() {
		if awsSummaries, awsErr := (s.AWSUpstream()).SearchTraces(ctx, tenant, search); awsErr == nil {
			groups = append(groups, awsSummaries)
		}
	}
	matched := make([]proxy.TraceSummary, 0, 4)
	for _, summary := range proxy.MergeTraceSummaries(groups...) {
		if strings.HasPrefix(summary.RootServiceName, service) || strings.HasPrefix(summary.Service, service) {
			matched = append(matched, summary)
		}
	}
	if len(matched) == 0 {
		return "", nil
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].StartTime.After(matched[j].StartTime) })
	return matched[0].TraceID, nil
}

// handleTempoSearch serves GET /api/search (tag search and TraceQL). When the
// client requests span sets (spss), the matched spans are attached so Grafana's
// span list renders.
func (s *Service) handleTempoSearch(w http.ResponseWriter, r *http.Request) {
	search, err := tempoSearchFromRequest(r)
	if err != nil {
		writeTempoError(w, http.StatusBadRequest, err.Error())
		return
	}
	tenant := s.tenantFromRequest(r)
	spss := parseLimit(r.URL.Query().Get("spss"), 0, 1000)

	var (
		summaries []proxy.TraceSummary
		warnings  []proxy.Warning
	)
	if s.proxyRouter != nil {
		summaries, warnings, err = s.proxyRouter.SearchTraces(r.Context(), tenant, search)
		if err != nil {
			if errors.Is(err, proxy.ErrUnsupported) {
				writeTempoError(w, http.StatusBadRequest, err.Error())
				return
			}
			if isAccessDenied(err) {
				writeTempoError(w, http.StatusForbidden, err.Error())
				return
			}
			writeTempoError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		// The local/AWS fallback does not speak TraceQL directly; fold the
		// supported subset into tag matchers before searching.
		if err := search.ApplyTraceQL(); err != nil {
			writeTempoError(w, http.StatusBadRequest, err.Error())
			return
		}
		if search.MatchNone {
			writeJSON(w, http.StatusOK, tempoSearchResponse(nil, nil))
			return
		}
		local := localTracesUpstream{svc: s}
		localSummaries, localErr := local.SearchTraces(r.Context(), tenant, search)
		if localErr != nil {
			writeTempoError(w, http.StatusInternalServerError, localErr.Error())
			return
		}
		groups := [][]proxy.TraceSummary{localSummaries}
		if s.awsReady() {
			if awsSummaries, awsErr := (s.AWSUpstream()).SearchTraces(r.Context(), tenant, search); awsErr == nil {
				groups = append(groups, awsSummaries)
			}
		}
		summaries = proxy.MergeTraceSummaries(groups...)
	}

	var spanSets map[string]tempo.SpanSet
	if spss > 0 {
		spanSets, err = s.searchSpanSets(r.Context(), tenant, search, spss)
		if err != nil {
			if isAccessDenied(err) {
				writeTempoError(w, http.StatusForbidden, err.Error())
				return
			}
			// Span sets are an enrichment; a failure must not fail the search.
			spanSets = nil
		}
	}

	writeProxyWarnings(w, warnings)
	writeJSON(w, http.StatusOK, tempoSearchResponse(summaries, spanSets))
}

// searchSpanSets resolves matched spans for the search. The TraceQL filters are
// folded into the tag form first; unsupported constructs skip the enrichment.
func (s *Service) searchSpanSets(ctx context.Context, tenant string, search proxy.TraceSearch, spss int) (map[string]tempo.SpanSet, error) {
	spanSearch := search
	spanSearch.Limit = 0
	if err := spanSearch.ApplyTraceQL(); err != nil {
		return nil, nil
	}
	if spanSearch.MatchNone {
		return nil, nil
	}
	if s.proxyRouter != nil {
		sets, _, err := s.proxyRouter.SearchSpanSets(ctx, tenant, spanSearch, spss)
		return sets, err
	}
	return (localTracesUpstream{svc: s}).SearchSpanSets(ctx, tenant, spanSearch, spss)
}

// traceQLMetricsRequest parses the shared metrics query parameters.
func (s *Service) traceQLMetricsRequest(r *http.Request) (traceQLMetricsQuery, time.Time, time.Time, time.Duration, error) {
	values := r.URL.Query()
	raw := strings.TrimSpace(values.Get("q"))
	if raw == "" {
		raw = strings.TrimSpace(values.Get("query"))
	}
	query, err := parseTraceQLMetricsQuery(raw)
	if err != nil {
		return traceQLMetricsQuery{}, time.Time{}, time.Time{}, 0, err
	}

	now := time.Now().UTC()
	end, err := parseLokiTime(values.Get("end"), now)
	if err != nil {
		return traceQLMetricsQuery{}, time.Time{}, time.Time{}, 0, err
	}
	start, err := parseLokiTime(values.Get("start"), end.Add(-time.Hour))
	if err != nil {
		return traceQLMetricsQuery{}, time.Time{}, time.Time{}, 0, err
	}
	if end.Before(start) {
		return traceQLMetricsQuery{}, time.Time{}, time.Time{}, 0, errors.New("end must be after start")
	}
	step, err := parseTraceQLMetricsStep(values.Get("step"))
	if err != nil {
		return traceQLMetricsQuery{}, time.Time{}, time.Time{}, 0, err
	}
	if step <= 0 {
		step = defaultTraceQLMetricsStep(start, end)
	}
	return query, start, end, step, nil
}

// traceMetricsAccessError applies the trace access gate to direct store reads.
func (s *Service) traceMetricsAccessError(ctx context.Context) error {
	if s.proxyRouter == nil {
		return nil
	}
	return s.proxyRouter.LocalTraceAccessError(ctx)
}

// writeTempoMetricsError maps a metrics evaluation error onto an HTTP status.
func writeTempoMetricsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, translate.ErrUnsupported):
		writeTempoError(w, http.StatusBadRequest, err.Error())
	case isAccessDenied(err):
		writeTempoError(w, http.StatusForbidden, err.Error())
	default:
		writeTempoError(w, http.StatusInternalServerError, err.Error())
	}
}

// handleTempoMetricsQueryRange serves GET /api/metrics/query_range, the TraceQL
// metrics endpoint Grafana's Traces Drilldown and node graph use.
func (s *Service) handleTempoMetricsQueryRange(w http.ResponseWriter, r *http.Request) {
	query, start, end, step, err := s.traceQLMetricsRequest(r)
	if err != nil {
		writeTempoMetricsError(w, err)
		return
	}
	if err := s.traceMetricsAccessError(r.Context()); err != nil {
		writeTempoError(w, http.StatusForbidden, err.Error())
		return
	}
	tenant := s.tenantFromRequest(r)
	response, err := s.evaluateTraceQLMetricsRange(r.Context(), tenant, query, start, end, step)
	if err != nil {
		writeTempoMetricsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleTempoMetricsQuery serves GET /api/metrics/query (instant metrics).
func (s *Service) handleTempoMetricsQuery(w http.ResponseWriter, r *http.Request) {
	query, start, end, _, err := s.traceQLMetricsRequest(r)
	if err != nil {
		writeTempoMetricsError(w, err)
		return
	}
	if err := s.traceMetricsAccessError(r.Context()); err != nil {
		writeTempoError(w, http.StatusForbidden, err.Error())
		return
	}
	tenant := s.tenantFromRequest(r)
	response, err := s.evaluateTraceQLMetricsInstant(r.Context(), tenant, query, start, end)
	if err != nil {
		writeTempoMetricsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// collectTraceTags gathers tag names from the router or the local/AWS path.
func (s *Service) collectTraceTags(ctx context.Context, tenant string) (map[string]struct{}, []proxy.Warning, error) {
	if tags, warnings, ok := s.cachedTraceTags(ctx, tenant); ok {
		return tags, warnings, nil
	}
	tags := make(map[string]struct{})
	if s.proxyRouter != nil {
		names, warnings, err := s.proxyRouter.TraceTags(ctx, tenant)
		if err != nil {
			return nil, warnings, err
		}
		for _, name := range names {
			tags[name] = struct{}{}
		}
		s.storeTraceTags(ctx, tenant, tags, warnings)
		return tags, warnings, nil
	}
	local := localTracesUpstream{svc: s}
	names, err := local.TraceTags(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range names {
		tags[name] = struct{}{}
	}
	if s.awsReady() {
		tags["service.name"] = struct{}{}
	}
	s.storeTraceTags(ctx, tenant, tags, nil)
	return tags, nil, nil
}

// collectTraceTagValues gathers the values of one tag.
func (s *Service) collectTraceTagValues(ctx context.Context, tenant, tag string) (map[string]struct{}, []proxy.Warning, error) {
	if values, warnings, ok := s.cachedTraceTagValues(ctx, tenant, tag); ok {
		return values, warnings, nil
	}
	values := make(map[string]struct{})
	if s.proxyRouter != nil {
		found, warnings, err := s.proxyRouter.TraceTagValues(ctx, tenant, tag)
		if err != nil {
			return nil, warnings, err
		}
		for _, value := range found {
			values[value] = struct{}{}
		}
		if tag == "service.name" || tag == "service" {
			s.filterTraceServiceValues(ctx, values)
		}
		s.storeTraceTagValues(ctx, tenant, tag, values, warnings)
		return values, warnings, nil
	}
	local := localTracesUpstream{svc: s}
	found, err := local.TraceTagValues(ctx, tenant, tag)
	if err != nil {
		return nil, nil, err
	}
	for _, value := range found {
		values[value] = struct{}{}
	}
	if s.awsReady() && (tag == "service.name" || tag == "service") {
		for _, service := range s.awsServices(ctx) {
			values[service] = struct{}{}
		}
		s.filterTraceServiceValues(ctx, values)
	}
	s.storeTraceTagValues(ctx, tenant, tag, values, nil)
	return values, nil, nil
}

// handleTempoTags serves GET /api/search/tags (Tempo v1).
func (s *Service) handleTempoTags(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	tags, warnings, err := s.collectTraceTags(r.Context(), tenant)
	if err != nil {
		if isAccessDenied(err) {
			writeTempoError(w, http.StatusForbidden, err.Error())
			return
		}
		writeTempoError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeProxyWarnings(w, warnings)
	writeJSON(w, http.StatusOK, tempo.TagNamesResponse{TagNames: sortedStrings(tags)})
}

// handleTempoTagsV2 serves GET /api/v2/search/tags (Tempo v2). Grafana 12's
// Tempo datasource proxies its `resources/tags` endpoint here for the Traces
// Drilldown tag pickers.
func (s *Service) handleTempoTagsV2(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	scopes, warnings, err := s.collectTraceTagScopes(r.Context(), tenant)
	if err != nil {
		if isAccessDenied(err) {
			writeTempoError(w, http.StatusForbidden, err.Error())
			return
		}
		writeTempoError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeProxyWarnings(w, warnings)
	writeJSON(w, http.StatusOK, tempo.TagScopesResponse{Scopes: scopes})
}

// collectTraceTagScopes gathers scoped tag names, falling back to classifying
// the v1 tag list when the router cannot report scopes.
func (s *Service) collectTraceTagScopes(ctx context.Context, tenant string) ([]tempo.TagScope, []proxy.Warning, error) {
	if scopes, warnings, ok := s.cachedTraceScopes(ctx, tenant); ok {
		return scopes, warnings, nil
	}
	if s.proxyRouter != nil {
		scopes, warnings, err := s.proxyRouter.TraceTagScopes(ctx, tenant)
		if err == nil {
			s.storeTraceScopes(ctx, tenant, scopes, warnings)
		}
		return scopes, warnings, err
	}
	local := localTracesUpstream{svc: s}
	scopes, err := local.TraceTagScopes(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}
	s.storeTraceScopes(ctx, tenant, scopes, nil)
	return scopes, nil, nil
}

// handleTempoTagValues serves GET /api/search/tag/{tag}/values (Tempo v1).
func (s *Service) handleTempoTagValues(w http.ResponseWriter, r *http.Request) {
	s.serveTempoTagValues(w, r, "/api/search/tag/")
}

// handleTempoTagValuesV2 serves GET /api/v2/search/tag/{tag}/values (Tempo v2),
// which Grafana 12 proxies its `resources/tag-values` endpoint to.
func (s *Service) handleTempoTagValuesV2(w http.ResponseWriter, r *http.Request) {
	s.serveTempoTagValues(w, r, "/api/v2/search/tag/")
}

func (s *Service) serveTempoTagValues(w http.ResponseWriter, r *http.Request, prefix string) {
	tag := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/values")
	if tag == "" || !strings.HasSuffix(r.URL.Path, "/values") {
		http.NotFound(w, r)
		return
	}
	// Tempo v2 accepts a scope query parameter; Doctor indexes scoped names
	// transparently, so the bare tag name resolves the same way.
	tenant := s.tenantFromRequest(r)
	values, warnings, err := s.collectTraceTagValues(r.Context(), tenant, tag)
	if err != nil {
		if isAccessDenied(err) {
			writeTempoError(w, http.StatusForbidden, err.Error())
			return
		}
		writeTempoError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeProxyWarnings(w, warnings)

	out := make([]tempo.TagValue, 0, len(values))
	for value := range values {
		out = append(out, tempo.TagValue{Type: "string", Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	writeJSON(w, http.StatusOK, tempo.TagValuesResponse{TagValues: out})
}

func tempoSearchFromRequest(r *http.Request) (proxy.TraceSearch, error) {
	query := r.URL.Query()
	now := time.Now().UTC()
	end, err := parseLokiTime(query.Get("end"), now)
	if err != nil {
		return proxy.TraceSearch{}, err
	}
	start, err := parseLokiTime(query.Get("start"), end.Add(-time.Hour))
	if err != nil {
		return proxy.TraceSearch{}, err
	}
	if end.Before(start) {
		return proxy.TraceSearch{}, errors.New("end must be after start")
	}

	search := proxy.TraceSearch{
		TraceQL: strings.TrimSpace(query.Get("q")),
		From:    start,
		To:      end,
		Limit:   parseLimit(query.Get("limit"), 20, 1000),
	}
	for _, raw := range query["tags"] {
		key, value, ok := strings.Cut(raw, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}
		if key == "service.name" && search.Service == "" {
			search.Service = value
			continue
		}
		search.Tags = append(search.Tags, proxy.TraceTag{Key: key, Value: value})
	}
	if raw := strings.TrimSpace(query.Get("minDuration")); raw != "" {
		duration, parseErr := parseTempoDuration(raw)
		if parseErr != nil {
			return proxy.TraceSearch{}, parseErr
		}
		search.MinDuration = duration
	}
	if raw := strings.TrimSpace(query.Get("maxDuration")); raw != "" {
		duration, parseErr := parseTempoDuration(raw)
		if parseErr != nil {
			return proxy.TraceSearch{}, parseErr
		}
		search.MaxDuration = duration
	}
	return search, nil
}

func parseTempoDuration(raw string) (time.Duration, error) {
	if duration, err := time.ParseDuration(raw); err == nil && duration >= 0 {
		return duration, nil
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds * float64(time.Second)), nil
	}
	return 0, errors.New("invalid duration " + raw)
}

func tempoSearchResponse(summaries []proxy.TraceSummary, spanSets map[string]tempo.SpanSet) tempo.SearchResponse {
	out := make([]tempo.SearchTrace, 0, len(summaries))
	for _, summary := range summaries {
		start := summary.StartTime.UTC()
		if start.IsZero() {
			start = time.Unix(0, 0).UTC()
		}
		entry := tempo.SearchTrace{
			TraceID:           summary.TraceID,
			RootServiceName:   summary.RootServiceName,
			RootTraceName:     summary.RootTraceName,
			StartTimeUnixNano: strconv.FormatInt(start.UnixNano(), 10),
			DurationMs:        summary.Duration.Milliseconds(),
		}
		if set, ok := spanSets[summary.TraceID]; ok {
			entry.SpanSets = []tempo.SpanSet{set}
		}
		out = append(out, entry)
	}
	return tempo.SearchResponse{Traces: out}
}

func writeTempoError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]any{"status": "error", "error": message})
}
