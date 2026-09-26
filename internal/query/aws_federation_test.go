package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

type fakeAWSBackend struct {
	logs          []awsstore.LogRecord
	services      []string
	traces        []awsstore.TraceSummary
	spans         []awsstore.Span
	promResponse  map[string]any
	lastPromQuery string
	lastLogQuery  awsstore.LogQuery
}

func (f *fakeAWSBackend) Enabled() bool { return true }

func (f *fakeAWSBackend) ListServices(context.Context) ([]string, error) { return f.services, nil }

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
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: &fakeAWSBackend{services: []string{"xray-api", "xray-worker"}}}

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
