package query

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

// This file pins the Grafana datasource contract: the exact endpoints and query
// shapes Grafana 12's Prometheus, Loki, and Tempo datasources (including the
// Traces/Logs/Metrics Drilldown apps) send. Regressions here are what made the
// Doctor datasources unusable.

func grafanaContractBackend() (*fakeLogEngineBackend, time.Time) {
	base := time.Now().UTC().Truncate(time.Second)
	return &fakeLogEngineBackend{
		traces: []segment.TraceRecord{
			{
				TraceID: "trace-a", SpanID: "span-root", ParentSpanID: "",
				Name: "GET /health", Kind: "SPAN_KIND_SERVER", Service: "guardian",
				StatusCode: "STATUS_CODE_OK", StartTime: base, EndTime: base.Add(200 * time.Millisecond),
				ResourceAttributes: map[string]string{"k8s.pod.name": "guardian-0"},
			},
			{
				TraceID: "trace-a", SpanID: "span-child", ParentSpanID: "span-root",
				Name: "SELECT", Kind: "SPAN_KIND_CLIENT", Service: "guardian",
				StatusCode: "STATUS_CODE_ERROR", StartTime: base.Add(10 * time.Millisecond), EndTime: base.Add(100 * time.Millisecond),
				Attributes: map[string]string{"db.system.name": "postgresql"},
			},
			{
				TraceID: "trace-b", SpanID: "span-b", ParentSpanID: "",
				Name: "GET /x", Kind: "SPAN_KIND_SERVER", Service: "worker",
				StatusCode: "STATUS_CODE_OK", StartTime: base, EndTime: base.Add(50 * time.Millisecond),
			},
		},
		logs: []segment.LogRecord{
			{Service: "guardian", Timestamp: base, SeverityText: "INFO", Body: "hello world", Partition: "edge"},
			{Service: "guardian", Timestamp: base.Add(time.Second), SeverityText: "ERROR", Body: "boom", Partition: "edge"},
			{Service: "worker", Timestamp: base.Add(2 * time.Second), SeverityText: "INFO", Body: "working", Partition: "edge"},
		},
		metrics: []segment.MetricPointRecord{
			{MetricName: "guardian_requests_total", Service: "guardian", HasValue: true, Value: 5, Timestamp: base, TraceID: "trace-a", SpanID: "span-root"},
			{MetricName: "guardian_requests_total", Service: "guardian", HasValue: true, Value: 6, Timestamp: base.Add(15 * time.Second), TraceID: "trace-b"},
			{MetricName: "worker_requests_total", Service: "worker", HasValue: true, Value: 1, Timestamp: base.Add(30 * time.Second)},
		},
	}, base
}

func grafanaContractService(t *testing.T) (*Service, time.Time) {
	t.Helper()
	backend, base := grafanaContractBackend()
	svc := &Service{defaultTenant: "default", logEngine: backend}
	svc.SetProxyRouter(newProxyTestRouter(t, svc))
	return svc, base
}

func doRequest(t *testing.T, handler http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// --- Tempo search -----------------------------------------------------------

func TestGrafanaTempoSearchAcceptsDrilldownQueries(t *testing.T) {
	svc, base := grafanaContractService(t)
	window := url.Values{}
	window.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	window.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))

	cases := []struct {
		name      string
		query     string
		wantTrace bool
	}{
		{"match all", `{}`, true},
		{"root spans", `{nestedSetParent<0 && true}`, true},
		{"service present", `{resource.service.name != nil}`, true},
		{"root with service", `{nestedSetParent<0 && true && resource.service.name != nil}`, true},
		{"kind enum", `{kind=server}`, true},
		{"status enum", `{status=error}`, true},
		{"database calls", `{span.db.system.name!=""}`, true},
		{"service exact", `{service.name="guardian"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := url.Values{}
			params.Set("q", tc.query)
			for key, values := range window {
				for _, value := range values {
					params.Set(key, value)
				}
			}
			rec := doRequest(t, svc.handleTempoSearch, "/api/search?"+params.Encode())
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var response struct {
				Traces []json.RawMessage `json:"traces"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
			}
			if tc.wantTrace && len(response.Traces) == 0 {
				t.Fatalf("expected traces for %q, got none", tc.query)
			}
		})
	}
}

func TestGrafanaTempoSearchSpanSets(t *testing.T) {
	svc, base := grafanaContractService(t)
	params := url.Values{}
	params.Set("q", `{nestedSetParent<0 && true}`)
	params.Set("spss", "3")
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))

	rec := doRequest(t, svc.handleTempoSearch, "/api/search?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Traces []struct {
			TraceID  string `json:"traceID"`
			SpanSets []struct {
				Matched int `json:"matched"`
				Spans   []struct {
					SpanID string `json:"spanID"`
					Name   string `json:"name"`
				} `json:"spans"`
			} `json:"spanSets"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Traces) != 2 {
		t.Fatalf("traces = %d, want 2 (body=%s)", len(response.Traces), rec.Body.String())
	}
	for _, trace := range response.Traces {
		if len(trace.SpanSets) != 1 || trace.SpanSets[0].Matched != 1 || len(trace.SpanSets[0].Spans) != 1 {
			t.Fatalf("trace %s spanSets = %#v", trace.TraceID, trace.SpanSets)
		}
		if trace.SpanSets[0].Spans[0].SpanID == "" || trace.SpanSets[0].Spans[0].Name == "" {
			t.Fatalf("trace %s span missing fields: %#v", trace.TraceID, trace.SpanSets[0].Spans[0])
		}
	}
}

func TestGrafanaTempoTagsV2Scopes(t *testing.T) {
	svc, _ := grafanaContractService(t)
	rec := doRequest(t, svc.handleTempoTagsV2, "/api/v2/search/tags")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Scopes []struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	scopes := map[string][]string{}
	for _, scope := range response.Scopes {
		scopes[scope.Name] = scope.Tags
	}
	for _, want := range []string{"intrinsic", "resource", "span"} {
		if _, ok := scopes[want]; !ok {
			t.Fatalf("missing scope %q in %#v", want, response.Scopes)
		}
	}
	if !contractContainsString(scopes["resource"], "service.name") || !contractContainsString(scopes["span"], "db.system.name") {
		t.Fatalf("scopes = %#v", scopes)
	}
}

func TestGrafanaTempoTagValuesV2(t *testing.T) {
	svc, _ := grafanaContractService(t)
	rec := doRequest(t, svc.handleTempoTagValuesV2, "/api/v2/search/tag/service.name/values")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		TagValues []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"tagValues"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.TagValues) != 2 || response.TagValues[0].Type != "string" {
		t.Fatalf("tagValues = %#v", response.TagValues)
	}
}

// --- Tempo metrics ----------------------------------------------------------

func TestGrafanaMetricsQueryRangeRateByService(t *testing.T) {
	svc, base := grafanaContractService(t)
	params := url.Values{}
	params.Set("q", `{nestedSetParent<0 && true && resource.service.name != nil} | rate() by(resource.service.name)`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))
	params.Set("step", "15s")

	rec := doRequest(t, svc.handleTempoMetricsQueryRange, "/api/metrics/query_range?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Series []struct {
			Labels []struct {
				Key   string `json:"key"`
				Value struct {
					StringValue string `json:"stringValue"`
				} `json:"value"`
			} `json:"labels"`
			Samples []struct {
				TimestampMs int64   `json:"timestampMs"`
				Value       float64 `json:"value"`
			} `json:"samples"`
		} `json:"series"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if len(response.Series) != 2 {
		t.Fatalf("series = %d, want 2 (body=%s)", len(response.Series), rec.Body.String())
	}
	services := map[string]bool{}
	for _, series := range response.Series {
		if len(series.Labels) != 1 || series.Labels[0].Key != "resource.service.name" {
			t.Fatalf("labels = %#v (key must keep the query spelling)", series.Labels)
		}
		services[series.Labels[0].Value.StringValue] = true
		if len(series.Samples) == 0 {
			t.Fatalf("series has no samples: %#v", series)
		}
	}
	if !services["guardian"] || !services["worker"] {
		t.Fatalf("services = %#v", services)
	}
}

func TestGrafanaMetricsQueryRangeQuantile(t *testing.T) {
	svc, base := grafanaContractService(t)
	params := url.Values{}
	params.Set("q", `{resource.service.name="guardian"} | quantile_over_time(duration, 0.9)`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))
	params.Set("step", "15s")

	rec := doRequest(t, svc.handleTempoMetricsQueryRange, "/api/metrics/query_range?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Series []struct {
			Labels []struct {
				Key   string `json:"key"`
				Value struct {
					DoubleValue *float64 `json:"doubleValue"`
				} `json:"value"`
			} `json:"labels"`
			Samples []struct {
				Value float64 `json:"value"`
			} `json:"samples"`
		} `json:"series"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Series) != 1 {
		t.Fatalf("series = %d, want 1 (body=%s)", len(response.Series), rec.Body.String())
	}
	foundQuantile := false
	for _, label := range response.Series[0].Labels {
		if label.Key == "p" && label.Value.DoubleValue != nil && *label.Value.DoubleValue == 0.9 {
			foundQuantile = true
		}
	}
	if !foundQuantile {
		t.Fatalf("missing p=0.9 label: %#v", response.Series[0].Labels)
	}
	if len(response.Series[0].Samples) == 0 {
		t.Fatalf("no samples: %#v", response.Series[0])
	}
	// The p90 of the two guardian span durations (200ms, 90ms) is 189ms.
	if got := response.Series[0].Samples[0].Value; got < 180_000_000 || got > 200_000_000 {
		t.Fatalf("p90 = %v, want ~189ms", got)
	}
}

func TestGrafanaMetricsQueryInstant(t *testing.T) {
	svc, base := grafanaContractService(t)
	params := url.Values{}
	params.Set("q", `{} | rate()`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))

	rec := doRequest(t, svc.handleTempoMetricsQuery, "/api/metrics/query?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Series []struct {
			Value float64 `json:"value"`
		} `json:"series"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Series) != 1 || response.Series[0].Value <= 0 {
		t.Fatalf("series = %#v", response.Series)
	}
}

func TestGrafanaMetricsQueryRejectsUnsupported(t *testing.T) {
	svc, base := grafanaContractService(t)
	params := url.Values{}
	params.Set("q", `{service.name="guardian"} | histogram_over_time(duration)`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Format(time.RFC3339Nano))

	rec := doRequest(t, svc.handleTempoMetricsQueryRange, "/api/metrics/query_range?"+params.Encode())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// --- Loki -------------------------------------------------------------------

func TestGrafanaLokiVolumeRange(t *testing.T) {
	svc, base := grafanaContractService(t)
	params := url.Values{}
	params.Set("query", `{service=~".+"}`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))
	params.Set("step", "30s")
	params.Set("targetLabels", "level")

	rec := doRequest(t, svc.handleLokiIndexVolumeRange, "/loki/api/v1/index/volume_range?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType == "" || contentType[:16] != "application/json" {
		t.Fatalf("content type = %q (must never be HTML)", contentType)
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if response.Status != "success" || response.Data.ResultType != "matrix" {
		t.Fatalf("response = %s", rec.Body.String())
	}
	levels := map[string]bool{}
	for _, series := range response.Data.Result {
		levels[series.Metric["level"]] = true
		if len(series.Values) == 0 {
			t.Fatalf("series without values: %#v", series)
		}
	}
	if !levels["info"] || !levels["error"] {
		t.Fatalf("levels = %#v (body=%s)", levels, rec.Body.String())
	}
}

func TestGrafanaLokiIndexStats(t *testing.T) {
	svc, base := grafanaContractService(t)
	params := url.Values{}
	params.Set("query", `{service=~".+"}`)
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))

	rec := doRequest(t, svc.handleLokiIndexStats, "/loki/api/v1/index/stats?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Streams int `json:"streams"`
		Entries int `json:"entries"`
		Bytes   int `json:"bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The fake backend stores three records across three stream identities.
	if response.Entries != 3 || response.Streams != 3 || response.Bytes == 0 {
		t.Fatalf("stats = %#v", response)
	}
}

func TestGrafanaLokiBuildInfo(t *testing.T) {
	svc := &Service{}
	rec := doRequest(t, svc.handleLokiBuildInfo, "/loki/api/v1/status/buildinfo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var response map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response["version"] == "" {
		t.Fatalf("missing version: %s", rec.Body.String())
	}
}

func TestGrafanaLokiEmptyFeatureEndpoints(t *testing.T) {
	svc := &Service{}
	for name, handler := range map[string]http.HandlerFunc{
		"patterns":        svc.handleLokiPatterns,
		"detected_fields": svc.handleLokiDetectedFields,
		"detected_labels": svc.handleLokiDetectedLabels,
	} {
		rec := doRequest(t, handler, "/loki/api/v1/"+name)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", name, rec.Code)
		}
		var decoded map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
	}
}

// --- Prometheus -------------------------------------------------------------

func TestGrafanaPrometheusRules(t *testing.T) {
	svc := &Service{}
	rec := doRequest(t, svc.handlePrometheusRules, "/api/v1/rules")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			Groups []any `json:"groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Status != "success" || response.Data.Groups == nil {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestGrafanaPrometheusQueryExemplars(t *testing.T) {
	svc, base := grafanaContractService(t)
	params := url.Values{}
	params.Set("query", "guardian_requests_total")
	params.Set("start", base.Add(-time.Minute).Format(time.RFC3339Nano))
	params.Set("end", base.Add(time.Minute).Format(time.RFC3339Nano))

	rec := doRequest(t, svc.handlePrometheusQueryExemplars, "/api/v1/query_exemplars?"+params.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Status string `json:"status"`
		Data   []struct {
			SeriesLabels map[string]string `json:"seriesLabels"`
			Exemplars    []struct {
				Labels map[string]string `json:"labels"`
				Value  string            `json:"value"`
			} `json:"exemplars"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if response.Status != "success" || len(response.Data) == 0 {
		t.Fatalf("response = %s", rec.Body.String())
	}
	foundTrace := false
	for _, series := range response.Data {
		for _, exemplar := range series.Exemplars {
			if exemplar.Labels["trace_id"] == "trace-a" {
				foundTrace = true
			}
		}
	}
	if !foundTrace {
		t.Fatalf("no exemplar with trace_id=trace-a: %s", rec.Body.String())
	}
}

func contractContainsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
