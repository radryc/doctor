package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

type fakeAWSBackend struct {
	logs              []awsstore.LogRecord
	services          []awsstore.ServiceInfo
	metricDescriptors []awsstore.MetricDescriptor
	traces            []awsstore.TraceSummary
	spans             []awsstore.Span
	promResponse      map[string]any
	lastPromQuery     string
	lastLogQuery      awsstore.LogQuery
}

func (f *fakeAWSBackend) Enabled() bool { return true }

func (f *fakeAWSBackend) ListMetrics(context.Context, string, string) ([]awsstore.MetricDescriptor, error) {
	return f.metricDescriptors, nil
}

func (f *fakeAWSBackend) ListServices(context.Context) ([]awsstore.ServiceInfo, error) {
	return f.services, nil
}

func (f *fakeAWSBackend) QueryLogs(_ context.Context, q awsstore.LogQuery) ([]awsstore.LogRecord, error) {
	f.lastLogQuery = q
	return f.logs, nil
}

func (f *fakeAWSBackend) QueryTraces(context.Context, awsstore.TraceQuery) ([]awsstore.TraceSummary, error) {
	return f.traces, nil
}

func (f *fakeAWSBackend) GetTrace(context.Context, string) ([]awsstore.Span, error) {
	return f.spans, nil
}

func (f *fakeAWSBackend) PrometheusQuery(_ context.Context, query string, _ time.Time) (map[string]any, error) {
	f.lastPromQuery = query
	return f.promResponse, nil
}

func (f *fakeAWSBackend) PrometheusQueryRange(_ context.Context, query string, _, _ time.Time, _ time.Duration) (map[string]any, error) {
	f.lastPromQuery = query
	return f.promResponse, nil
}

func (f *fakeAWSBackend) PrometheusSeries(context.Context, []string, time.Time, time.Time) (map[string]any, error) {
	return f.promResponse, nil
}

func (f *fakeAWSBackend) PrometheusLabels(context.Context, []string, time.Time, time.Time) (map[string]any, error) {
	return f.promResponse, nil
}

func (f *fakeAWSBackend) PrometheusLabelValues(context.Context, string, []string, time.Time, time.Time) (map[string]any, error) {
	return f.promResponse, nil
}

func TestPrometheusQueryFederatesAWSMetrics(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	aws := &fakeAWSBackend{
		promResponse: map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []any{
					map[string]any{
						"metric": map[string]any{"__name__": "CPUUtilization", "InstanceId": "i-1"},
						"value":  []any{float64(1780000000), "42"},
					},
				},
			},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: aws}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query?query=CPUUtilization%7Bsource%3D%22aws%22%7D&time="+strconv.FormatInt(base.Unix(), 10),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if aws.lastPromQuery != "CPUUtilization" {
		t.Fatalf("forwarded promql query = %q, want CPUUtilization", aws.lastPromQuery)
	}

	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result length = %d, want 1 (body=%s)", len(resp.Data.Result), rec.Body.String())
	}
	if got := resp.Data.Result[0].Metric["source"]; got != "aws" {
		t.Fatalf("source = %q, want aws", got)
	}
	if got := resp.Data.Result[0].Metric["InstanceId"]; got != "i-1" {
		t.Fatalf("InstanceId = %q, want i-1", got)
	}
}

func TestPrometheusSourceLabelValuesIncludeAWS(t *testing.T) {
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: &fakeAWSBackend{}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/label/source/values", nil)
	rec := httptest.NewRecorder()
	svc.handlePrometheusLabelValues(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, value := range resp.Data {
		if value == awsSourceLabel {
			found = true
		}
	}
	if !found {
		t.Fatalf("source label values = %#v, want %q", resp.Data, awsSourceLabel)
	}
}

func TestPrometheusMetricNamesIncludeAWS(t *testing.T) {
	aws := &fakeAWSBackend{metricDescriptors: []awsstore.MetricDescriptor{
		{Namespace: "AWS/EC2", MetricName: "CPUUtilization"},
		{Namespace: "AWS/EC2", MetricName: "CPUUtilization"},
		{Namespace: "AWS/EC2", MetricName: "MemoryUtilization"},
	}}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		defaultTenant: "default",
		logEngine: &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{{
			MetricName: "local_metric",
			Timestamp:  base.Add(-time.Minute),
			HasValue:   true,
			Value:      1,
		}}},
		aws: aws,
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/label/__name__/values?start="+url.QueryEscape(base.Add(-time.Hour).Format(time.RFC3339))+
			"&end="+url.QueryEscape(base.Format(time.RFC3339)),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusLabelValues(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	values := make(map[string]bool)
	for _, value := range resp.Data {
		values[value] = true
	}
	for _, want := range []string{"local_metric", "CPUUtilization", "MemoryUtilization"} {
		if !values[want] {
			t.Fatalf("metric names = %#v, want %q", resp.Data, want)
		}
	}
}

func TestPrometheusSeriesIncludeAWSSource(t *testing.T) {
	aws := &fakeAWSBackend{promResponse: map[string]any{
		"status": "success",
		"data": []any{
			map[string]any{"__name__": "CPUUtilization", "InstanceId": "i-1"},
		},
	}}
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: aws}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/series?match[]="+url.QueryEscape(`{source="aws"}`),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusSeries(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("series = %#v, want 1", resp.Data)
	}
	if got := resp.Data[0]["source"]; got != awsSourceLabel {
		t.Fatalf("series source = %q, want %q", got, awsSourceLabel)
	}
}

func TestLogServicesIncludeAWS(t *testing.T) {
	aws := &fakeAWSBackend{services: []awsstore.ServiceInfo{{
		Name:    "xray-api",
		Region:  "eu-west-1",
		Account: "123456789012",
	}}}
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: aws}

	req := httptest.NewRequest(http.MethodGet, "/v1/logs/services", nil)
	rec := httptest.NewRecorder()
	svc.handleLogServices(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Services   []string `json:"services"`
		Partitions []string `json:"partitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !containsString(resp.Services, "xray-api") {
		t.Fatalf("services = %#v, want xray-api", resp.Services)
	}
	if !containsString(resp.Partitions, "eu-west-1") {
		t.Fatalf("partitions = %#v, want eu-west-1", resp.Partitions)
	}
}

func TestRecentTracesIncludeAWS(t *testing.T) {
	base := time.Now().UTC()
	aws := &fakeAWSBackend{traces: []awsstore.TraceSummary{{
		TraceID:   "1-aws",
		Service:   "xray-api",
		StartTime: base.Add(-time.Minute),
		EndTime:   base,
		Duration:  time.Second,
		Region:    "eu-west-1",
		Account:   "123456789012",
	}}}
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: aws}

	req := httptest.NewRequest(http.MethodGet, "/v1/traces/recent", nil)
	rec := httptest.NewRecorder()
	svc.handleRecentTraces(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Traces []struct {
			TraceID   string `json:"trace_id"`
			Partition string `json:"partition"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, trace := range resp.Traces {
		if trace.TraceID == "1-aws" {
			found = true
			if trace.Partition != "eu-west-1" {
				t.Fatalf("partition = %q, want eu-west-1", trace.Partition)
			}
		}
	}
	if !found {
		t.Fatalf("traces = %#v, want 1-aws", resp.Traces)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Regression: with the AWS backend attached, an instant query for a metric that
// resolves locally must keep its local series. The transparent CloudWatch
// fallback must not fire, and the local/AWS merge must preserve local results.
// Label matchers use the normalized Prometheus vocabulary, but the storage
// layer keys labels by their raw (dotted) names. They must therefore be applied
// locally against the normalized series labels rather than pushed to storage,
// otherwise `{guardian_partition="doctor"}` matches nothing.
func TestPrometheusLabelMatcherAppliedLocallyNotPushedToStorage(t *testing.T) {
	base := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{
		{Service: "guardian", MetricName: "http_requests_total", Partition: "doctor", Timestamp: base, HasValue: true, Value: 1},
		{Service: "guardian", MetricName: "http_requests_total", Partition: "opentelemetry", Timestamp: base, HasValue: true, Value: 2},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query?query="+url.QueryEscape(`http_requests_total{guardian_partition="doctor"}`)+"&time="+url.QueryEscape(base.Format(time.RFC3339)),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result = %d series, want 1 (body=%s)", len(resp.Data.Result), rec.Body.String())
	}
	if got := resp.Data.Result[0].Metric["guardian_partition"]; got != "doctor" {
		t.Fatalf("guardian_partition = %q, want doctor", got)
	}
	if len(backend.lastMetricQuery.LabelMatchers) != 0 {
		t.Fatalf("label matchers must not be pushed to storage: %#v", backend.lastMetricQuery.LabelMatchers)
	}
}

func TestPrometheusQueryKeepsLocalSeriesWhenAWSEnabled(t *testing.T) {
	base := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{{
		Tenant:     "default",
		Service:    "api",
		MetricName: "http_requests_total",
		Timestamp:  base,
		Attributes: map[string]string{"instance": "pod-a"},
		HasValue:   true,
		Value:      42,
	}}}
	aws := &fakeAWSBackend{promResponse: map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": []any{}},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend, aws: aws}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query?tenant=default&query=http_requests_total&time="+url.QueryEscape(base.Format(time.RFC3339)),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)

	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result length = %d, want 1; local series were dropped (body=%s)", len(resp.Data.Result), rec.Body.String())
	}
	if aws.lastPromQuery != "" {
		t.Fatalf("unexpected AWS fallback for a locally-resolved metric: %q", aws.lastPromQuery)
	}
}

func TestPrometheusQueryFallsBackToAWSWithoutSourceMatcher(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	aws := &fakeAWSBackend{
		promResponse: map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []any{
					map[string]any{
						"metric": map[string]any{"__name__": "CPUUtilization", "Region": "eu-west-1"},
						"value":  []any{float64(1780000000), "7"},
					},
				},
			},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: aws}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query?query=CPUUtilization&time="+strconv.FormatInt(base.Unix(), 10),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if aws.lastPromQuery != "CPUUtilization" {
		t.Fatalf("forwarded promql query = %q, want CPUUtilization", aws.lastPromQuery)
	}
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result length = %d, want 1 (body=%s)", len(resp.Data.Result), rec.Body.String())
	}
	if got := resp.Data.Result[0].Metric["source"]; got != awsSourceLabel {
		t.Fatalf("source = %q, want %q", got, awsSourceLabel)
	}
}

func TestAWSQueryModeAndStripping(t *testing.T) {
	cases := []struct {
		in        string
		wantQuery string
		wantMode  int
	}{
		{`CPUUtilization`, `CPUUtilization`, awsQueryNone},
		{`CPUUtilization{source="aws"}`, `CPUUtilization`, awsQueryOnly},
		{`sum(rate({CPUUtilization, source="aws"}[5m]))`, `sum(rate({CPUUtilization}[5m]))`, awsQueryOnly},
		{`X{source="aws", InstanceId="i-1"}`, `X{InstanceId="i-1"}`, awsQueryOnly},
		{`X{source=~"aws|doctor"}`, `X`, awsQueryMixed},
	}
	for _, tc := range cases {
		forwarded, mode := awsQueryMode(tc.in)
		if forwarded != tc.wantQuery || mode != tc.wantMode {
			t.Fatalf("awsQueryMode(%q) = %q, %d; want %q, %d", tc.in, forwarded, mode, tc.wantQuery, tc.wantMode)
		}
	}
}

func TestLokiQueryFederatesAWSLogs(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	aws := &fakeAWSBackend{
		logs: []awsstore.LogRecord{{
			Timestamp: base.Add(-time.Minute),
			LogGroup:  "/ecs/api",
			Message:   "boom: request failed",
			Fields:    map[string]string{"service": "api"},
		}},
	}
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: aws}

	req := httptest.NewRequest(
		http.MethodGet,
		"/loki/api/v1/query_range?query=%7Bsource%3D%22aws%22%2Caws_log_group%3D%22%2Fecs%2Fapi%22%7D%20%7C%3D%20%22boom%22&start="+strconv.FormatInt(base.Add(-5*time.Minute).UnixNano(), 10)+"&end="+strconv.FormatInt(base.UnixNano(), 10),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handleLokiQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(aws.lastLogQuery.LogGroups) != 1 || aws.lastLogQuery.LogGroups[0] != "/ecs/api" {
		t.Fatalf("aws log groups = %#v", aws.lastLogQuery.LogGroups)
	}

	var resp struct {
		Data struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("streams = %d, want 1 (body=%s)", len(resp.Data.Result), rec.Body.String())
	}
	if got := resp.Data.Result[0].Stream["source"]; got != "aws" {
		t.Fatalf("stream source = %q, want aws", got)
	}
	if got := resp.Data.Result[0].Values[0][1]; got != "boom: request failed" {
		t.Fatalf("log body = %q", got)
	}
}

func TestZipkinServicesIncludesAWS(t *testing.T) {
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: &fakeAWSBackend{services: []awsstore.ServiceInfo{{Name: "xray-api"}, {Name: "xray-worker"}}}}

	req := httptest.NewRequest(http.MethodGet, "/api/v2/services", nil)
	rec := httptest.NewRecorder()
	svc.handleZipkinServices(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var services []string
	if err := json.Unmarshal(rec.Body.Bytes(), &services); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, service := range services {
		if service == "xray-api" {
			found = true
		}
	}
	if !found {
		t.Fatalf("services = %#v, want xray-api", services)
	}
}

func TestZipkinTraceByIDFallsBackToAWS(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	aws := &fakeAWSBackend{
		spans: []awsstore.Span{{
			TraceID:   "1-abc",
			SpanID:    "seg-1",
			Name:      "api",
			Service:   "xray-api",
			StartTime: base,
			EndTime:   base.Add(250 * time.Millisecond),
		}},
	}
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: aws}

	req := httptest.NewRequest(http.MethodGet, "/api/v2/trace/1-abc", nil)
	rec := httptest.NewRecorder()
	svc.handleZipkinTraceByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var spans []struct {
		TraceID string `json:"traceId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spans); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(spans) != 1 || spans[0].TraceID != "1-abc" {
		t.Fatalf("spans = %#v", spans)
	}
}
