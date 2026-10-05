package query

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/tempo"
)

// handleTempoEcho reports readiness for the Tempo datasource.
func (s *Service) handleTempoEcho(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleTempoTraceByID serves GET /api/traces/{traceID} in OTLP-JSON form.
func (s *Service) handleTempoTraceByID(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/traces/"))
	if traceID == "" {
		http.Error(w, "trace id is required", http.StatusBadRequest)
		return
	}
	tenant := s.tenantFromRequest(r)

	var (
		records  []segment.TraceRecord
		found    bool
		warnings []proxy.Warning
		err      error
	)
	if s.proxyRouter != nil {
		records, warnings, err = s.proxyRouter.GetTrace(r.Context(), tenant, traceID)
		found = len(records) > 0
	} else {
		local := localTracesUpstream{svc: s}
		records, found, err = local.GetTrace(r.Context(), tenant, traceID)
		if err == nil && !found && s.awsReady() {
			records, found, err = (&proxy.AWSUpstream{Client: s.aws}).GetTrace(r.Context(), tenant, traceID)
		}
	}
	if err != nil {
		if isAccessDenied(err) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found || len(records) == 0 {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}
	writeProxyWarnings(w, warnings)
	writeJSON(w, http.StatusOK, tempo.TraceFromRecords(records))
}

// handleTempoSearch serves GET /api/search (tag search and TraceQL).
func (s *Service) handleTempoSearch(w http.ResponseWriter, r *http.Request) {
	search, err := tempoSearchFromRequest(r)
	if err != nil {
		writeTempoError(w, http.StatusBadRequest, err.Error())
		return
	}
	tenant := s.tenantFromRequest(r)

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
		local := localTracesUpstream{svc: s}
		localSummaries, localErr := local.SearchTraces(r.Context(), tenant, search)
		if localErr != nil {
			writeTempoError(w, http.StatusInternalServerError, localErr.Error())
			return
		}
		groups := [][]proxy.TraceSummary{localSummaries}
		if s.awsReady() {
			if awsSummaries, awsErr := (&proxy.AWSUpstream{Client: s.aws}).SearchTraces(r.Context(), tenant, search); awsErr == nil {
				groups = append(groups, awsSummaries)
			}
		}
		summaries = proxy.MergeTraceSummaries(groups...)
	}

	writeProxyWarnings(w, warnings)
	writeJSON(w, http.StatusOK, tempoSearchResponse(summaries))
}

// handleTempoTags serves GET /api/search/tags.
func (s *Service) handleTempoTags(w http.ResponseWriter, r *http.Request) {
	tenant := s.tenantFromRequest(r)
	tags := make(map[string]struct{})

	if s.proxyRouter != nil {
		names, warnings, err := s.proxyRouter.TraceTags(r.Context(), tenant)
		if err != nil {
			if isAccessDenied(err) {
				writeTempoError(w, http.StatusForbidden, err.Error())
				return
			}
			writeTempoError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeProxyWarnings(w, warnings)
		for _, name := range names {
			tags[name] = struct{}{}
		}
	} else {
		local := localTracesUpstream{svc: s}
		names, err := local.TraceTags(r.Context(), tenant)
		if err != nil {
			writeTempoError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, name := range names {
			tags[name] = struct{}{}
		}
		if s.awsReady() {
			tags["service.name"] = struct{}{}
		}
	}

	writeJSON(w, http.StatusOK, tempo.TagNamesResponse{TagNames: sortedStrings(tags)})
}

// handleTempoTagValues serves GET /api/search/tag/{tag}/values.
func (s *Service) handleTempoTagValues(w http.ResponseWriter, r *http.Request) {
	tag := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/search/tag/"), "/values")
	if tag == "" || !strings.HasSuffix(r.URL.Path, "/values") {
		http.NotFound(w, r)
		return
	}
	tenant := s.tenantFromRequest(r)
	values := make(map[string]struct{})

	if s.proxyRouter != nil {
		found, warnings, err := s.proxyRouter.TraceTagValues(r.Context(), tenant, tag)
		if err != nil {
			if isAccessDenied(err) {
				writeTempoError(w, http.StatusForbidden, err.Error())
				return
			}
			writeTempoError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeProxyWarnings(w, warnings)
		for _, value := range found {
			values[value] = struct{}{}
		}
	} else {
		local := localTracesUpstream{svc: s}
		found, err := local.TraceTagValues(r.Context(), tenant, tag)
		if err != nil {
			writeTempoError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, value := range found {
			values[value] = struct{}{}
		}
		if s.awsReady() && (tag == "service.name" || tag == "service") {
			for _, service := range s.awsServices(r.Context()) {
				values[service] = struct{}{}
			}
		}
	}

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

func tempoSearchResponse(summaries []proxy.TraceSummary) tempo.SearchResponse {
	out := make([]tempo.SearchTrace, 0, len(summaries))
	for _, summary := range summaries {
		start := summary.StartTime.UTC()
		if start.IsZero() {
			start = time.Unix(0, 0).UTC()
		}
		out = append(out, tempo.SearchTrace{
			TraceID:           summary.TraceID,
			RootServiceName:   summary.RootServiceName,
			RootTraceName:     summary.RootTraceName,
			StartTimeUnixNano: strconv.FormatInt(start.UnixNano(), 10),
			DurationMs:        summary.Duration.Milliseconds(),
		})
	}
	return tempo.SearchResponse{Traces: out}
}

func writeTempoError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]any{"status": "error", "error": message})
}
