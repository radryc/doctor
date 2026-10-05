package query

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
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

	plan, err := parseLokiPlan(rawQuery)
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if plan.metric != nil {
		window := plan.metric.rangeDur
		if window <= 0 {
			window = time.Hour
		}
		records, fetchErr := s.fetchLokiRecords(r.Context(), s.tenantFromRequest(r), plan, queryTime.Add(-window), queryTime, 5000)
		if fetchErr != nil {
			writeLokiQueryError(w, fetchErr)
			return
		}
		records = plan.applyStages(records)
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     plan.evaluateInstant(records, queryTime),
				"stats":      map[string]any{},
			},
		})
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
	direction := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("direction")))
	if direction == "" {
		direction = "backward"
	}
	tenant := s.tenantFromRequest(r)

	plan, err := parseLokiPlan(query)
	if err != nil {
		writeLokiError(w, http.StatusBadRequest, err.Error())
		return
	}

	if plan.metric != nil {
		limit := parseLimit(r.URL.Query().Get("limit"), 5000, 20000)
		records, fetchErr := s.fetchLokiRecords(r.Context(), tenant, plan, start, end, limit)
		if fetchErr != nil {
			writeLokiQueryError(w, fetchErr)
			return
		}
		records = plan.applyStages(records)
		step := parseLokiStep(r.URL.Query().Get("step"), start, end)
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "matrix",
				"result":     plan.evaluateRange(records, start, end, step),
				"stats":      map[string]any{},
			},
		})
		return
	}

	limit := parseLimit(r.URL.Query().Get("limit"), 1000, 5000)
	logs, err := s.fetchLokiRecords(r.Context(), tenant, plan, start, end, limit)
	if err != nil {
		writeLokiQueryError(w, err)
		return
	}
	logs = plan.applyStages(logs)
	if len(logs) > limit {
		logs = logs[:limit]
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
		writeLokiQueryError(w, err)
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
		writeLokiQueryError(w, err)
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
		writeLokiQueryError(w, err)
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

func (s *Service) queryLogsForLoki(ctx context.Context, tenant, query string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	if strings.TrimSpace(query) == "" {
		query = `{service=~".+"}`
	}
	plan, err := parseLokiPlan(query)
	if err != nil {
		return nil, err
	}
	return s.fetchLokiRecords(ctx, tenant, plan, from, to, limit)
}

// fetchLokiRecords reduces a parsed LogQL plan to the storage subset, queries
// local storage plus any federated AWS backend, and returns the raw records.
func (s *Service) fetchLokiRecords(ctx context.Context, tenant string, plan lokiPlan, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	if s.proxyRouter != nil {
		records, _, err := s.proxyRouter.Logs(ctx, tenant, logPushdown(plan), from, to, limit)
		return records, err
	}
	storageQuery := plan.storageQuery()
	var local []segment.LogRecord
	var err error
	if backend, ok := s.logEngine.(rawLogQueryBackend); ok {
		local, err = backend.QueryLogsRaw(ctx, tenant, storageQuery, from, to, limit)
	} else {
		local, err = s.logEngine.QueryLogs(ctx, tenant, storageQuery, "", from, to, limit)
	}
	if err != nil {
		return nil, err
	}
	if awsLogs := s.queryAWSLogsForLoki(ctx, tenant, plan, from, to, limit); len(awsLogs) > 0 {
		local = proxy.MergeLogs(local, proxy.LogRemote{Source: proxy.SourceAWS, Records: awsLogs})
	}
	return local, nil
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
	attrs := mergeStringMaps(record.ResourceAttributes, record.Attributes)
	labels := canonicalLabels(record.Service, record.Tenant, record.Partition, record.Intent, record.Asset, attrs)
	if record.SeverityText != "" {
		labels["level"] = strings.ToLower(record.SeverityText)
	}
	// Typed correlation IDs are authoritative even when the attribute map is
	// sparse (e.g. records replayed from pre-extension chunks).
	if record.TraceID != "" {
		labels["trace_id"] = record.TraceID
	}
	if record.SpanID != "" {
		labels["span_id"] = record.SpanID
	}
	return filterLokiLabels(labels)
}

// writeLokiQueryError maps access enforcement denials to 403 and other fetch
// failures to 500.
func writeLokiQueryError(w http.ResponseWriter, err error) {
	if isAccessDenied(err) {
		writeLokiError(w, http.StatusForbidden, err.Error())
		return
	}
	writeLokiError(w, http.StatusInternalServerError, err.Error())
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
