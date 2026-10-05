package tempo

import (
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

func TestTraceFromRecordsGroupsByService(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	trace := TraceFromRecords([]segment.TraceRecord{
		{
			TraceID: "trace-1", SpanID: "span-1", Name: "GET /a", Kind: "SPAN_KIND_SERVER",
			Service: "api", StartTime: base, EndTime: base.Add(time.Second),
			ResourceAttributes: map[string]string{"k8s.pod.name": "api-1"},
			Attributes:         map[string]string{"http.method": "GET"},
			StatusCode:         "STATUS_CODE_OK",
		},
		{
			TraceID: "trace-1", SpanID: "span-2", ParentSpanID: "span-1", Name: "SELECT",
			Service: "db", StartTime: base.Add(100 * time.Millisecond), EndTime: base.Add(200 * time.Millisecond),
		},
	})
	if len(trace.Batches) != 2 {
		t.Fatalf("batches = %d, want 2", len(trace.Batches))
	}
	if len(trace.Batches[0].ScopeSpans) != 1 || len(trace.Batches[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("unexpected span layout: %#v", trace.Batches[0])
	}
	span := trace.Batches[0].ScopeSpans[0].Spans[0]
	if span.TraceID != "trace-1" || span.Kind != "SPAN_KIND_SERVER" {
		t.Fatalf("span = %#v", span)
	}
	if span.Status == nil || span.Status.Code != "STATUS_CODE_OK" {
		t.Fatalf("status = %#v", span.Status)
	}
	if got := span.StartTimeUnixNano; got != "1780315200000000000" {
		t.Fatalf("startTimeUnixNano = %q", got)
	}
	if len(span.Attributes) != 1 || span.Attributes[0].Key != "http.method" {
		t.Fatalf("attributes = %#v", span.Attributes)
	}
	resourceAttrs := map[string]string{}
	for _, attribute := range trace.Batches[0].Resource.Attributes {
		resourceAttrs[attribute.Key] = attribute.Value.StringValue
	}
	if resourceAttrs["service.name"] != "api" || resourceAttrs["k8s.pod.name"] != "api-1" {
		t.Fatalf("resource attributes = %#v", resourceAttrs)
	}
}

func TestSummariesFromRecords(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	summaries := SummariesFromRecords([]segment.TraceRecord{
		{TraceID: "trace-1", SpanID: "span-1", Name: "GET /a", Service: "api", StartTime: base, EndTime: base.Add(time.Second)},
		{TraceID: "trace-1", SpanID: "span-2", ParentSpanID: "span-1", Name: "SELECT", Service: "db", StartTime: base.Add(100 * time.Millisecond), EndTime: base.Add(2 * time.Second)},
		{TraceID: "trace-2", SpanID: "span-3", Name: "GET /b", Service: "api", StartTime: base.Add(-time.Minute), EndTime: base.Add(-time.Minute + 500*time.Millisecond)},
	}, nil)
	if len(summaries) != 2 {
		t.Fatalf("summaries = %#v", summaries)
	}
	// Newest first.
	if summaries[0].TraceID != "trace-1" {
		t.Fatalf("first summary = %#v, want trace-1", summaries[0])
	}
	if summaries[0].RootServiceName != "api" || summaries[0].RootTraceName != "GET /a" {
		t.Fatalf("root = %#v", summaries[0])
	}
	if summaries[0].DurationMs != 2000 {
		t.Fatalf("duration = %d, want 2000", summaries[0].DurationMs)
	}
	if summaries[0].SpanCount != 2 {
		t.Fatalf("span count = %d, want 2", summaries[0].SpanCount)
	}
}

func TestTagKeysAndValuesFromRecords(t *testing.T) {
	records := []segment.TraceRecord{{
		Service: "api", Name: "GET /a", Kind: "SPAN_KIND_SERVER", StatusCode: "STATUS_CODE_OK",
		Attributes:         map[string]string{"http.method": "GET"},
		ResourceAttributes: map[string]string{"k8s.pod.name": "api-1"},
	}}
	keys := TagKeysFromRecords(records)
	if !contains(keys, "service.name") || !contains(keys, "http.method") || !contains(keys, "k8s.pod.name") {
		t.Fatalf("keys = %#v", keys)
	}
	values := TagValuesFromRecords(records, "http.method")
	if len(values) != 1 || values[0] != "GET" {
		t.Fatalf("values = %#v", values)
	}
	services := TagValuesFromRecords(records, "service.name")
	if len(services) != 1 || services[0] != "api" {
		t.Fatalf("services = %#v", services)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
