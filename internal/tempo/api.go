// Package tempo defines the Tempo-compatible HTTP API wire types and the
// conversion from Doctor's internal span model.
package tempo

// Trace is the OTLP-JSON shape returned by GET /api/traces/{traceID}.
type Trace struct {
	Batches []Batch `json:"batches"`
}

// Batch groups spans that share a resource.
type Batch struct {
	Resource   Resource     `json:"resource,omitempty"`
	ScopeSpans []ScopeSpans `json:"scopeSpans,omitempty"`
}

// Resource describes the emitting entity.
type Resource struct {
	Attributes []KeyValue `json:"attributes,omitempty"`
}

// ScopeSpans groups spans under one instrumentation scope.
type ScopeSpans struct {
	Scope Scope  `json:"scope"`
	Spans []Span `json:"spans,omitempty"`
}

// Scope is the instrumentation scope (empty for Doctor spans).
type Scope struct{}

// Span is an OTLP-JSON span.
type Span struct {
	TraceID           string     `json:"traceId,omitempty"`
	SpanID            string     `json:"spanId,omitempty"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name,omitempty"`
	Kind              string     `json:"kind,omitempty"`
	StartTimeUnixNano string     `json:"startTimeUnixNano,omitempty"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano,omitempty"`
	Attributes        []KeyValue `json:"attributes,omitempty"`
	Events            []Event    `json:"events,omitempty"`
	Status            *Status    `json:"status,omitempty"`
}

// Event is an OTLP-JSON span event.
type Event struct {
	Name         string     `json:"name,omitempty"`
	TimeUnixNano string     `json:"timeUnixNano,omitempty"`
	Attributes   []KeyValue `json:"attributes,omitempty"`
}

// KeyValue is an OTLP-JSON attribute.
type KeyValue struct {
	Key   string `json:"key"`
	Value Value  `json:"value"`
}

// Value is an OTLP-JSON attribute value. Only string values are produced by
// Doctor's conversion today.
type Value struct {
	StringValue string `json:"stringValue,omitempty"`
}

// Status is an OTLP-JSON span status.
type Status struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// SearchResponse is the Tempo search response.
type SearchResponse struct {
	Traces []SearchTrace `json:"traces"`
}

// SearchTrace is one search hit. SpanCount is internal bookkeeping and is not
// part of the Tempo wire format.
type SearchTrace struct {
	TraceID           string `json:"traceID"`
	RootServiceName   string `json:"rootServiceName,omitempty"`
	RootTraceName     string `json:"rootTraceName,omitempty"`
	StartTimeUnixNano string `json:"startTimeUnixNano,omitempty"`
	DurationMs        int64  `json:"durationMs"`
	SpanCount         int    `json:"-"`
}

// TagNamesResponse is the Tempo tag-names response.
type TagNamesResponse struct {
	TagNames []string `json:"tagNames"`
}

// TagValuesResponse is the Tempo tag-values response.
type TagValuesResponse struct {
	TagValues []TagValue `json:"tagValues"`
}

// TagValue is one tag value with its inferred type.
type TagValue struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}
