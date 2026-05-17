package query

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

type rawLogQueryBackend interface {
	QueryLogsRaw(ctx context.Context, tenant, query string, from, to time.Time, limit int) ([]segment.LogRecord, error)
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

type lokiVectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

type zipkinEndpoint struct {
	ServiceName string `json:"serviceName,omitempty"`
}

type zipkinSpan struct {
	TraceID       string            `json:"traceId"`
	ID            string            `json:"id"`
	ParentID      string            `json:"parentId,omitempty"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind,omitempty"`
	Timestamp     int64             `json:"timestamp"`
	Duration      int64             `json:"duration"`
	LocalEndpoint zipkinEndpoint    `json:"localEndpoint,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
}

const zipkinDiscoverySpanLimit = 5000

func (s *Service) handleLokiReady(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *Service) handleLokiQuery(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	rawQuery := strings.TrimSpace(r.URL.Query().Get("query"))
	if isLokiGrafanaHealthcheck(rawQuery) {
		timestamp := now.UnixNano()
		if rawTime := strings.TrimSpace(r.URL.Query().Get("time")); rawTime != "" {
			if parsed, parseErr := strconv.ParseInt(rawTime, 10, 64); parseErr == nil && parsed > 0 {
				timestamp = parsed
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []lokiVectorSample{{
					Metric: map[string]string{},
					Value:  []any{timestamp, "2"},
				}},
			},
		})
		return
	}

	queryTime, err := parseLokiTime(r.URL.Query().Get("time"), now)
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}

	since := time.Hour
	if rawSince := strings.TrimSpace(r.URL.Query().Get("since")); rawSince != "" {
		parsedSince, parseErr := time.ParseDuration(rawSince)
		if parseErr != nil || parsedSince <= 0 {
			writeLokiError(w, http.StatusBadRequest, fmt.Sprintf("invalid since %q", rawSince))
			return
		}
		since = parsedSince
	}

	queryRange := r.Clone(r.Context())
	params := queryRange.URL.Query()
	params.Set("start", strconv.FormatInt(queryTime.Add(-since).UnixNano(), 10))
	params.Set("end", strconv.FormatInt(queryTime.UnixNano(), 10))
	queryRange.URL.RawQuery = params.Encode()
	s.handleLokiQueryRange(w, queryRange)
}

func (s *Service) handleLokiQueryRange(w http.ResponseWriter, r *http.Request) {
	end, err := parseLokiTime(r.URL.Query().Get("end"), time.Now().UTC())
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}
	start, err := parseLokiTime(r.URL.Query().Get("start"), end.Add(-1*time.Hour))
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if end.Before(start) {
		writeLokiError(w, http.StatusBadRequest, "end must be after start")
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("query"))
	limit := parseLimit(r.URL.Query().Get("limit"), 1000, 5000)
	direction := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("direction")))
	if direction == "" {
		direction = "backward"
	}

	logs, err := s.queryLogsForLoki(r.Context(), s.tenantFromRequest(r), query, start, end, limit)
	if err != nil {
		writeLokiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "streams",
			"result":     buildLokiStreams(logs, direction),
			"stats":      map[string]any{},
		},
	})
}

func (s *Service) handleLokiLabels(w http.ResponseWriter, r *http.Request) {
	start, end, err := lokiDiscoveryRange(r)
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}

	logs, err := s.queryLogsForLoki(r.Context(), s.tenantFromRequest(r), strings.TrimSpace(r.URL.Query().Get("query")), start, end, 5000)
	if err != nil {
		writeLokiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	labels := make(map[string]struct{})
	for _, record := range logs {
		for name := range lokiLabelsFromRecord(record) {
			labels[name] = struct{}{}
		}
	}

	out := make([]string, 0, len(labels))
	for name := range labels {
		out = append(out, name)
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": out})
}

func (s *Service) handleLokiLabelValues(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/values") {
		http.NotFound(w, r)
		return
	}
	labelName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/loki/api/v1/label/"), "/values")
	if labelName == "" {
		writeLokiError(w, http.StatusBadRequest, "label name is required")
		return
	}

	start, end, err := lokiDiscoveryRange(r)
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}

	logs, err := s.queryLogsForLoki(r.Context(), s.tenantFromRequest(r), strings.TrimSpace(r.URL.Query().Get("query")), start, end, 5000)
	if err != nil {
		writeLokiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	values := make(map[string]struct{})
	for _, record := range logs {
		if value := lokiLabelsFromRecord(record)[labelName]; value != "" {
			values[value] = struct{}{}
		}
	}

	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": out})
}

func (s *Service) handleLokiSeries(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		selectors := r.URL.Query()["match[]"]
		if len(selectors) > 0 {
			query = selectors[0]
		}
	}

	start, end, err := lokiDiscoveryRange(r)
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}

	logs, err := s.queryLogsForLoki(r.Context(), s.tenantFromRequest(r), query, start, end, 5000)
	if err != nil {
		writeLokiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	series := make(map[string]map[string]string)
	for _, record := range logs {
		labels := lokiLabelsFromRecord(record)
		series[metricLabelMapString(labels)] = labels
	}

	out := make([]map[string]string, 0, len(series))
	for _, labels := range series {
		out = append(out, labels)
	}
	sort.Slice(out, func(i, j int) bool {
		return metricLabelMapString(out[i]) < metricLabelMapString(out[j])
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": out})
}

func (s *Service) handleZipkinServices(w http.ResponseWriter, r *http.Request) {
	start, end := zipkinDiscoveryRange(r)
	spans, err := s.logEngine.QueryTraces(r.Context(), s.tenantFromRequest(r), "", "", start, end, zipkinDiscoverySpanLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	services := make(map[string]struct{})
	for _, span := range spans {
		if span.Service != "" {
			services[span.Service] = struct{}{}
		}
	}

	out := make([]string, 0, len(services))
	for service := range services {
		out = append(out, service)
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleZipkinSpans(w http.ResponseWriter, r *http.Request) {
	serviceName := strings.TrimSpace(r.URL.Query().Get("serviceName"))
	if serviceName == "" {
		writeJSON(w, http.StatusOK, []string{})
		return
	}

	start, end := zipkinDiscoveryRange(r)
	spans, err := s.logEngine.QueryTraces(r.Context(), s.tenantFromRequest(r), "", serviceName, start, end, zipkinDiscoverySpanLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	names := make(map[string]struct{})
	for _, span := range spans {
		if span.Name != "" {
			names[span.Name] = struct{}{}
		}
	}

	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleZipkinTraces(w http.ResponseWriter, r *http.Request) {
	serviceName := strings.TrimSpace(r.URL.Query().Get("serviceName"))
	limit := parseLimit(r.URL.Query().Get("limit"), 10, 100)
	end := parseZipkinMilliseconds(r.URL.Query().Get("endTs"), time.Now().UTC())
	lookback := parseZipkinDuration(r.URL.Query().Get("lookback"), time.Hour)
	start := end.Add(-lookback)

	spans, err := s.logEngine.QueryTraces(r.Context(), s.tenantFromRequest(r), "", serviceName, start, end, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	traces := groupZipkinTraces(spans)
	if len(traces) > limit {
		traces = traces[:limit]
	}
	writeJSON(w, http.StatusOK, traces)
}

func (s *Service) handleZipkinTraceByID(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimPrefix(r.URL.Path, "/api/v2/trace/")
	if traceID == "" {
		http.Error(w, "trace id is required", http.StatusBadRequest)
		return
	}
	if traceID == "latest" {
		start, end := zipkinDiscoveryRange(r)
		recent, err := s.logEngine.QueryTraces(r.Context(), s.tenantFromRequest(r), "", "", start, end, 1)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(recent) == 0 || recent[0].TraceID == "" {
			http.Error(w, "trace not found", http.StatusNotFound)
			return
		}
		traceID = recent[0].TraceID
	}

	spans, err := s.logEngine.QueryTraces(r.Context(), s.tenantFromRequest(r), traceID, "", time.Time{}, time.Time{}, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(spans) == 0 {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}

	out := make([]zipkinSpan, 0, len(spans))
	for _, span := range spans {
		out = append(out, zipkinSpanFromRecord(span))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) queryLogsForLoki(ctx context.Context, tenant, query string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	if backend, ok := s.logEngine.(rawLogQueryBackend); ok {
		return backend.QueryLogsRaw(ctx, tenant, query, from, to, limit)
	}
	return s.logEngine.QueryLogs(ctx, tenant, query, "", from, to, limit)
}

func buildLokiStreams(logs []segment.LogRecord, direction string) []lokiStream {
	grouped := make(map[string]*lokiStream)
	for _, record := range logs {
		labels := lokiLabelsFromRecord(record)
		key := metricLabelMapString(labels)
		stream := grouped[key]
		if stream == nil {
			stream = &lokiStream{Stream: labels}
			grouped[key] = stream
		}
		stream.Values = append(stream.Values, []string{strconv.FormatInt(record.Timestamp.UTC().UnixNano(), 10), record.Body})
	}

	out := make([]lokiStream, 0, len(grouped))
	for _, stream := range grouped {
		sort.Slice(stream.Values, func(i, j int) bool {
			if direction == "forward" {
				return stream.Values[i][0] < stream.Values[j][0]
			}
			return stream.Values[i][0] > stream.Values[j][0]
		})
		out = append(out, *stream)
	}
	sort.Slice(out, func(i, j int) bool {
		return metricLabelMapString(out[i].Stream) < metricLabelMapString(out[j].Stream)
	})
	return out
}

func lokiLabelsFromRecord(record segment.LogRecord) map[string]string {
	labels := make(map[string]string, 4)
	if record.Service != "" {
		labels["service"] = record.Service
	}
	if record.SeverityText != "" {
		labels["level"] = strings.ToLower(record.SeverityText)
	}
	if record.TraceID != "" {
		labels["trace_id"] = record.TraceID
	}
	if record.SpanID != "" {
		labels["span_id"] = record.SpanID
	}
	return labels
}

func writeLokiError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]any{
		"status": "error",
		"error":  message,
	})
}

func lokiDiscoveryRange(r *http.Request) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	end, err := parseLokiTime(r.URL.Query().Get("end"), now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start, err := parseLokiTime(r.URL.Query().Get("start"), end.Add(-doctorDiscoveryWindow))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end must be after start")
	}
	return start, end, nil
}

func parseLokiTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback.UTC(), nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	if integer, err := strconv.ParseInt(value, 10, 64); err == nil {
		switch {
		case integer >= 1_000_000_000_000_000_000:
			return time.Unix(0, integer).UTC(), nil
		case integer >= 1_000_000_000_000:
			return time.UnixMilli(integer).UTC(), nil
		default:
			return time.Unix(integer, 0).UTC(), nil
		}
	}
	return parsePrometheusTime(value, fallback)
}

func isLokiGrafanaHealthcheck(query string) bool {
	return strings.Join(strings.Fields(strings.ToLower(query)), "") == "vector(1)+vector(1)"
}

func parseZipkinMilliseconds(value string, fallback time.Time) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback.UTC()
	}
	if milliseconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.UnixMilli(milliseconds).UTC()
	}
	return fallback.UTC()
}

func parseZipkinDuration(value string, fallback time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if milliseconds, err := strconv.ParseInt(value, 10, 64); err == nil && milliseconds > 0 {
		return time.Duration(milliseconds) * time.Millisecond
	}
	if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
		return duration
	}
	return fallback
}

func zipkinDiscoveryRange(r *http.Request) (time.Time, time.Time) {
	end := parseZipkinMilliseconds(r.URL.Query().Get("endTs"), time.Now().UTC())
	lookback := parseZipkinDuration(r.URL.Query().Get("lookback"), doctorDiscoveryWindow)
	return end.Add(-lookback), end
}

func groupZipkinTraces(spans []segment.TraceRecord) [][]zipkinSpan {
	type traceGroup struct {
		latest time.Time
		spans  []zipkinSpan
	}

	groups := make(map[string]*traceGroup)
	for _, span := range spans {
		if span.TraceID == "" {
			continue
		}
		group := groups[span.TraceID]
		if group == nil {
			group = &traceGroup{}
			groups[span.TraceID] = group
		}
		if span.EndTime.After(group.latest) {
			group.latest = span.EndTime
		}
		group.spans = append(group.spans, zipkinSpanFromRecord(span))
	}

	ordered := make([]*traceGroup, 0, len(groups))
	for _, group := range groups {
		sort.Slice(group.spans, func(i, j int) bool { return group.spans[i].Timestamp < group.spans[j].Timestamp })
		ordered = append(ordered, group)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].latest.After(ordered[j].latest) })

	out := make([][]zipkinSpan, 0, len(ordered))
	for _, group := range ordered {
		out = append(out, group.spans)
	}
	return out
}

func zipkinSpanFromRecord(span segment.TraceRecord) zipkinSpan {
	tags := make(map[string]string, len(span.Attributes)+1)
	for key, value := range span.Attributes {
		if value == "" {
			continue
		}
		tags[key] = value
	}
	if span.StatusCode != "" {
		tags["status.code"] = span.StatusCode
	}
	duration := span.EndTime.Sub(span.StartTime).Microseconds()
	if duration < 0 {
		duration = 0
	}
	return zipkinSpan{
		TraceID:       span.TraceID,
		ID:            span.SpanID,
		ParentID:      span.ParentSpanID,
		Name:          span.Name,
		Kind:          strings.ToLower(span.Kind),
		Timestamp:     span.StartTime.UTC().UnixMicro(),
		Duration:      duration,
		LocalEndpoint: zipkinEndpoint{ServiceName: span.Service},
		Tags:          tags,
	}
}
