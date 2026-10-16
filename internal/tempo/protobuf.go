package tempo

import (
	"encoding/hex"
	"sort"
	"strings"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// OTLPTraceFromRecords converts internal span records into an OTLP
// ExportTraceServiceRequest. Spans are grouped into one ResourceSpans batch per
// service, preserving input order within a service.
func OTLPTraceFromRecords(records []segment.TraceRecord) *coltracepb.ExportTraceServiceRequest {
	req := &coltracepb.ExportTraceServiceRequest{}
	if len(records) == 0 {
		return req
	}
	order := make([]string, 0)
	batches := make(map[string]*tracepb.ResourceSpans)
	for _, record := range records {
		batch := batches[record.Service]
		if batch == nil {
			batch = &tracepb.ResourceSpans{
				Resource: &resourcepb.Resource{Attributes: resourceProtoAttributes(record)},
				// Grafana's OTLP→frame transform reads scopeSpans; Tempo returns a
				// single default scope for its own spans.
				ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{}}},
			}
			batches[record.Service] = batch
			order = append(order, record.Service)
		}
		batch.ScopeSpans[0].Spans = append(batch.ScopeSpans[0].Spans, spanProto(record))
	}
	for _, service := range order {
		req.ResourceSpans = append(req.ResourceSpans, batches[service])
	}
	return req
}

// ProtoFromRecords marshals records as the OTLP protobuf body served by
// GET /api/traces/{id} when the client accepts application/protobuf.
func ProtoFromRecords(records []segment.TraceRecord) ([]byte, error) {
	return proto.Marshal(OTLPTraceFromRecords(records))
}

// ProtoTraceByIDResponse marshals records as a Tempo v2 TraceByIDResponse
// protobuf: field 1 (trace) carries the OTLP resource spans. Grafana v12
// requests GET /api/v2/traces/{id} first and decodes this message.
func ProtoTraceByIDResponse(records []segment.TraceRecord) ([]byte, error) {
	trace, err := ProtoFromRecords(records)
	if err != nil {
		return nil, err
	}
	out := protowire.AppendTag(nil, 1, protowire.BytesType)
	out = protowire.AppendBytes(out, trace)
	return out, nil
}

func resourceProtoAttributes(record segment.TraceRecord) []*commonpb.KeyValue {
	attributes := make(map[string]string, len(record.ResourceAttributes)+1)
	if record.Service != "" {
		attributes["service.name"] = record.Service
	}
	for key, value := range record.ResourceAttributes {
		if value == "" {
			continue
		}
		attributes[key] = value
	}
	return protoKeyValues(attributes)
}

func spanProto(record segment.TraceRecord) *tracepb.Span {
	span := &tracepb.Span{
		TraceId:           decodeHexID(record.TraceID),
		SpanId:            decodeHexID(record.SpanID),
		ParentSpanId:      decodeHexID(record.ParentSpanID),
		Name:              record.Name,
		Kind:              spanKindProto(record.Kind),
		StartTimeUnixNano: uint64(record.StartTime.UTC().UnixNano()),
		EndTimeUnixNano:   uint64(record.EndTime.UTC().UnixNano()),
		Attributes:        protoKeyValues(record.Attributes),
		// Grafana's Tempo transform dereferences span.Status unconditionally,
		// so it must always be present even when the span has no status code.
		Status: &tracepb.Status{},
	}
	if record.StatusCode != "" || record.StatusMessage != "" {
		span.Status.Code = statusCodeProto(record.StatusCode)
		span.Status.Message = record.StatusMessage
	}
	for _, event := range record.Events {
		span.Events = append(span.Events, &tracepb.Span_Event{
			Name:         event.Name,
			TimeUnixNano: uint64(event.Timestamp.UTC().UnixNano()),
			Attributes:   protoKeyValues(event.Attributes),
		})
	}
	return span
}

func protoKeyValues(attributes map[string]string) []*commonpb.KeyValue {
	if len(attributes) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]*commonpb.KeyValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, &commonpb.KeyValue{
			Key:   key,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: attributes[key]}},
		})
	}
	return out
}

// decodeHexID returns the raw bytes for a hex trace/span ID. Invalid or empty
// values yield nil so malformed (for example AWS X-Ray) IDs do not fail the
// whole trace response.
func decodeHexID(value string) []byte {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil
	}
	return decoded
}

func spanKindProto(value string) tracepb.Span_SpanKind {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "SPAN_KIND_INTERNAL":
		return tracepb.Span_SPAN_KIND_INTERNAL
	case "SPAN_KIND_SERVER":
		return tracepb.Span_SPAN_KIND_SERVER
	case "SPAN_KIND_CLIENT":
		return tracepb.Span_SPAN_KIND_CLIENT
	case "SPAN_KIND_PRODUCER":
		return tracepb.Span_SPAN_KIND_PRODUCER
	case "SPAN_KIND_CONSUMER":
		return tracepb.Span_SPAN_KIND_CONSUMER
	default:
		return tracepb.Span_SPAN_KIND_UNSPECIFIED
	}
}

func statusCodeProto(value string) tracepb.Status_StatusCode {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "STATUS_CODE_OK", "OK":
		return tracepb.Status_STATUS_CODE_OK
	case "STATUS_CODE_ERROR", "ERROR":
		return tracepb.Status_STATUS_CODE_ERROR
	default:
		return tracepb.Status_STATUS_CODE_UNSET
	}
}
