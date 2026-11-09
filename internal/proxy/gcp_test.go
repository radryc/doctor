package proxy

import (
	"context"
	"testing"
	"time"

	gcpstore "github.com/radryc/doctor/internal/store/gcp"
)

type fakeGCPClient struct {
	logs      []gcpstore.LogRecord
	traces    []gcpstore.TraceSummary
	spans     []gcpstore.Span
	lastLog   gcpstore.LogQuery
	lastTrace gcpstore.TraceQuery
}

func (f *fakeGCPClient) Enabled() bool { return true }

func (f *fakeGCPClient) ListMetrics(context.Context, string, string) ([]gcpstore.MetricDescriptor, error) {
	return []gcpstore.MetricDescriptor{{Namespace: "run.googleapis.com", MetricName: "request_count"}}, nil
}

func (f *fakeGCPClient) QueryMetric(context.Context, gcpstore.MetricQuery) ([]gcpstore.MetricSeries, error) {
	return nil, nil
}

func (f *fakeGCPClient) ListServices(context.Context) ([]gcpstore.ServiceInfo, error) {
	return []gcpstore.ServiceInfo{{Name: "demo-api", Region: "europe-west1", Project: "demo"}}, nil
}

func (f *fakeGCPClient) ListLogNames(context.Context) ([]string, error) { return []string{"app"}, nil }

func (f *fakeGCPClient) QueryLogs(_ context.Context, q gcpstore.LogQuery) ([]gcpstore.LogRecord, error) {
	f.lastLog = q
	return f.logs, nil
}

func (f *fakeGCPClient) QueryTraces(_ context.Context, q gcpstore.TraceQuery) ([]gcpstore.TraceSummary, error) {
	f.lastTrace = q
	return f.traces, nil
}

func (f *fakeGCPClient) GetTrace(context.Context, string) ([]gcpstore.Span, error) {
	return f.spans, nil
}

func (f *fakeGCPClient) PrometheusQuery(context.Context, string, time.Time) (map[string]any, error) {
	return map[string]any{"status": "success"}, nil
}
func (f *fakeGCPClient) PrometheusQueryRange(context.Context, string, time.Time, time.Time, time.Duration) (map[string]any, error) {
	return map[string]any{"status": "success"}, nil
}
func (f *fakeGCPClient) PrometheusSeries(context.Context, []string, time.Time, time.Time) (map[string]any, error) {
	return map[string]any{"status": "success"}, nil
}
func (f *fakeGCPClient) PrometheusLabels(context.Context, []string, time.Time, time.Time) (map[string]any, error) {
	return map[string]any{"status": "success"}, nil
}
func (f *fakeGCPClient) PrometheusLabelValues(context.Context, string, []string, time.Time, time.Time) (map[string]any, error) {
	return map[string]any{"status": "success"}, nil
}

func TestGCPQueryMode(t *testing.T) {
	stripped, mode := GCPQueryMode(`rate(run_googleapis_com_request_count{source="gcp"}[5m])`)
	if mode != AWSQueryOnly {
		t.Fatalf("mode = %d", mode)
	}
	if stripped != `rate(run_googleapis_com_request_count[5m])` {
		t.Fatalf("stripped = %q", stripped)
	}

	_, mode = GCPQueryMode(`foo{source=~"gcp|doctor"}`)
	if mode != AWSQueryMixed {
		t.Fatalf("mixed mode = %d", mode)
	}

	_, mode = GCPQueryMode(`foo{job="api"}`)
	if mode != AWSQueryNone {
		t.Fatalf("plain mode = %d", mode)
	}
}

func TestProviderSelectorMode(t *testing.T) {
	stripped, kind, ok := ProviderSelectorMode(`{source="gcp", gcp_log_name="app"} |= "error"`)
	if !ok || kind != KindGCP {
		t.Fatalf("kind = %q ok=%v", kind, ok)
	}
	if stripped != `{ gcp_log_name="app"} |= "error"` && stripped != `{gcp_log_name="app"} |= "error"` {
		t.Fatalf("stripped = %q", stripped)
	}
	if _, kind, ok := ProviderSelectorMode(`{job="api"}`); ok || kind != "" {
		t.Fatalf("plain selector should not be provider-scoped: %q %v", kind, ok)
	}
}

func TestGCPUpstreamQueryLogs(t *testing.T) {
	client := &fakeGCPClient{logs: []gcpstore.LogRecord{{
		Timestamp: time.Now(),
		LogGroup:  "app",
		Message:   "boom",
		Severity:  "ERROR",
		Region:    "europe-west1",
		Project:   "demo",
		Fields:    map[string]string{"service.name": "demo-api"},
	}}}
	upstream := &GCPUpstream{Client: client}
	records, err := upstream.QueryLogs(context.Background(), "default", LogPushdown{
		Selector:    `{source="gcp", gcp_log_name="app"}`,
		LineFilters: []LogLineFilter{{Op: "|=", Value: "boom"}},
	}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if client.lastLog.LogName != "app" || client.lastLog.Query != `"boom"` {
		t.Fatalf("query = %+v", client.lastLog)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	record := records[0]
	if record.Body != "boom" || record.Partition != "europe-west1" {
		t.Errorf("record = %+v", record)
	}
	if record.Attributes[LabelSource] != SourceGCP || record.Attributes[AttrGCPProject] != "demo" {
		t.Errorf("attributes = %v", record.Attributes)
	}
}

func TestGCPUpstreamSearchTraces(t *testing.T) {
	client := &fakeGCPClient{traces: []gcpstore.TraceSummary{{
		TraceID:   "abc",
		Service:   "demo-api",
		StartTime: time.Now().Add(-time.Minute),
		Duration:  time.Second,
		SpanCount: 2,
		Region:    "europe-west1",
		Project:   "demo",
	}}}
	upstream := &GCPUpstream{Client: client}
	search := TraceSearch{
		Tags: []TraceTag{{Key: "source", Value: "gcp"}},
		From: time.Now().Add(-time.Hour),
		To:   time.Now(),
	}
	summaries, err := upstream.SearchTraces(context.Background(), "default", search)
	if err != nil {
		t.Fatalf("SearchTraces: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v", summaries)
	}
	summary := summaries[0]
	if summary.Account != "demo" || summary.Partition != "europe-west1" || summary.Service != "demo-api" {
		t.Errorf("summary = %+v", summary)
	}
	if TraceSearchProviderKind(search) != KindGCP {
		t.Errorf("provider kind = %q", TraceSearchProviderKind(search))
	}
}

func TestGCPUpstreamGetTrace(t *testing.T) {
	client := &fakeGCPClient{spans: []gcpstore.Span{{
		TraceID:   "abc",
		SpanID:    "root",
		Name:      "demo-api",
		Service:   "demo-api",
		StartTime: time.Now().Add(-time.Second),
		EndTime:   time.Now(),
		Region:    "europe-west1",
		Project:   "demo",
	}}}
	upstream := &GCPUpstream{Client: client}
	records, found, err := upstream.GetTrace(context.Background(), "default", "abc")
	if err != nil || !found {
		t.Fatalf("GetTrace: found=%v err=%v", found, err)
	}
	if len(records) != 1 || records[0].Attributes[LabelSource] != SourceGCP {
		t.Fatalf("records = %+v", records)
	}
	if records[0].Partition != "europe-west1" {
		t.Errorf("partition = %q", records[0].Partition)
	}
}

func TestGCPPartition(t *testing.T) {
	if got := GCPPartition("europe-west1", "demo"); got != "europe-west1" {
		t.Errorf("region preferred: %q", got)
	}
	if got := GCPPartition("", "demo"); got != "demo" {
		t.Errorf("project fallback: %q", got)
	}
	if got := GCPPartition("", ""); got != SourceGCP {
		t.Errorf("default: %q", got)
	}
}

func TestRouterMetricsGCPOnlyStripsSource(t *testing.T) {
	local := &fakeMetricsUpstream{name: "doctor", instant: vectorWithSeries(SourceDoctor)}
	gcp := &fakeMetricsUpstream{name: "gcp", instant: vectorWithSeries("")}
	prom := &fakeMetricsUpstream{name: "prom", instant: vectorWithSeries("")}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
			"gcp":    {Name: "gcp", Kind: KindGCP, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
			"prom":   {Name: "prom", Kind: KindPrometheus, BaseURL: "http://prom", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
		},
		map[string]Upstreams{
			"doctor": {Metrics: local},
			"gcp":    {Metrics: gcp},
			"prom":   {Metrics: prom},
		},
	)

	data, _, err := router.MetricsInstant(context.Background(), "default", `request_count{source="gcp"}`, time.Now())
	if err != nil {
		t.Fatalf("MetricsInstant: %v", err)
	}
	if local.callCount() != 0 || prom.callCount() != 0 {
		t.Fatalf("non-GCP backends called: local=%d prom=%d", local.callCount(), prom.callCount())
	}
	if gcp.callCount() != 1 || gcp.lastQuery != "request_count" {
		t.Fatalf("gcp calls=%d query=%q", gcp.callCount(), gcp.lastQuery)
	}
	if items := PrometheusResultSlice(data); len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
}
