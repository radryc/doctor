package tempo

import (
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

func sampleTraceRecord() segment.TraceRecord {
	start := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return segment.TraceRecord{
		TraceID:            "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:             "00f067aa0ba902b7",
		ParentSpanID:       "a1b2c3d4e5f60718",
		Name:               "GET /health",
		Kind:               "SPAN_KIND_SERVER",
		Service:            "api",
		Partition:          "monofs",
		Intent:             "api",
		Asset:              "web",
		StartTime:          start,
		EndTime:            start.Add(150 * time.Millisecond),
		StatusCode:         "STATUS_CODE_ERROR",
		StatusMessage:      "boom",
		ResourceAttributes: map[string]string{"service.namespace": "prod"},
		Attributes:         map[string]string{"http.method": "GET"},
		Events: []segment.TraceEvent{{
			Name:       "exception",
			Timestamp:  start.Add(20 * time.Millisecond),
			Attributes: map[string]string{"exception.type": "ValueError"},
		}},
	}
}

func TestOTLPTraceFromRecordsRoundTrip(t *testing.T) {
	body, err := ProtoFromRecords([]segment.TraceRecord{sampleTraceRecord()})
	if err != nil {
		t.Fatalf("ProtoFromRecords: %v", err)
	}
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	resourceSpans := req.GetResourceSpans()
	if len(resourceSpans) != 1 {
		t.Fatalf("resourceSpans = %d, want 1", len(resourceSpans))
	}
	scopeSpans := resourceSpans[0].GetScopeSpans()
	if len(scopeSpans) != 1 || len(scopeSpans[0].GetSpans()) != 1 {
		t.Fatalf("scopeSpans shape = %#v", scopeSpans)
	}
	span := scopeSpans[0].GetSpans()[0]
	if span.GetName() != "GET /health" {
		t.Fatalf("span name = %q", span.GetName())
	}
	if span.GetKind() != tracepb.Span_SPAN_KIND_SERVER {
		t.Fatalf("span kind = %v", span.GetKind())
	}
	if span.GetStatus().GetCode() != tracepb.Status_STATUS_CODE_ERROR || span.GetStatus().GetMessage() != "boom" {
		t.Fatalf("status = %#v", span.GetStatus())
	}
	if len(span.GetTraceId()) != 16 || len(span.GetSpanId()) != 8 || len(span.GetParentSpanId()) != 8 {
		t.Fatalf("id lengths = %d/%d/%d", len(span.GetTraceId()), len(span.GetSpanId()), len(span.GetParentSpanId()))
	}
	if len(span.GetEvents()) != 1 || span.GetEvents()[0].GetName() != "exception" {
		t.Fatalf("events = %#v", span.GetEvents())
	}
	serviceName := ""
	for _, attribute := range resourceSpans[0].GetResource().GetAttributes() {
		if attribute.GetKey() == "service.name" {
			serviceName = attribute.GetValue().GetStringValue()
		}
	}
	if serviceName != "api" {
		t.Fatalf("service.name = %q", serviceName)
	}
}

func TestProtoTraceByIDResponseWrapsTrace(t *testing.T) {
	body, err := ProtoTraceByIDResponse([]segment.TraceRecord{sampleTraceRecord()})
	if err != nil {
		t.Fatalf("ProtoTraceByIDResponse: %v", err)
	}
	// TraceByIDResponse.trace is field 1 (length-delimited Trace message).
	number, wireType, tagLen := protowire.ConsumeTag(body)
	if number != 1 || wireType != protowire.BytesType {
		t.Fatalf("tag = (%d, %d), want (1, BytesType)", number, wireType)
	}
	rest := body[tagLen:]
	value, valueLen := protowire.ConsumeBytes(rest)
	if len(rest[valueLen:]) != 0 {
		t.Fatalf("unexpected trailing bytes: %d", len(rest[valueLen:]))
	}
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(value, &req); err != nil {
		t.Fatalf("nested proto.Unmarshal: %v", err)
	}
	if len(req.GetResourceSpans()) != 1 {
		t.Fatalf("nested resourceSpans = %d, want 1", len(req.GetResourceSpans()))
	}
}
