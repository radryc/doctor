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
	TraceID           string    `json:"traceID"`
	RootServiceName   string    `json:"rootServiceName,omitempty"`
	RootTraceName     string    `json:"rootTraceName,omitempty"`
	StartTimeUnixNano string    `json:"startTimeUnixNano,omitempty"`
	DurationMs        int64     `json:"durationMs"`
	SpanSets          []SpanSet `json:"spanSets,omitempty"`
	SpanCount         int       `json:"-"`
}

// SpanSet groups the matched spans of one trace (Tempo SearchResponse spanSets).
type SpanSet struct {
	Spans      []SearchSpan `json:"spans,omitempty"`
	Matched    int          `json:"matched"`
	Attributes []KeyValue   `json:"attributes,omitempty"`
}

// SearchSpan is one span inside a search span set (tempopb Span).
type SearchSpan struct {
	SpanID            string     `json:"spanID"`
	Name              string     `json:"name,omitempty"`
	StartTimeUnixNano string     `json:"startTimeUnixNano,omitempty"`
	DurationNanos     string     `json:"durationNanos,omitempty"`
	Attributes        []KeyValue `json:"attributes,omitempty"`
}

// TagNamesResponse is the Tempo v1 tag-names response.
type TagNamesResponse struct {
	TagNames []string `json:"tagNames"`
}

// TagScopesResponse is the Tempo v2 tag-names response
// (GET /api/v2/search/tags).
type TagScopesResponse struct {
	Scopes []TagScope `json:"scopes"`
}

// TagScope is one tag scope in the Tempo v2 tag-names response.
type TagScope struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
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

// MetricLabel is one typed metric series label (tempopb common.v1.KeyValue).
type MetricLabel struct {
	Key   string           `json:"key"`
	Value MetricLabelValue `json:"value"`
}

// MetricLabelValue is a typed label value. Proto JSON emits int64 as a string;
// the other types keep their native JSON form.
type MetricLabelValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

// StringLabel builds a string-valued metric label.
func StringLabel(key, value string) MetricLabel {
	return MetricLabel{Key: key, Value: MetricLabelValue{StringValue: &value}}
}

// FloatLabel builds a double-valued metric label.
func FloatLabel(key string, value float64) MetricLabel {
	return MetricLabel{Key: key, Value: MetricLabelValue{DoubleValue: &value}}
}

// MetricSample is one point of a range metric series (tempopb Sample).
type MetricSample struct {
	TimestampMs int64   `json:"timestampMs"`
	Value       float64 `json:"value"`
}

// TimeSeries is one range metric series (tempopb TimeSeries).
type TimeSeries struct {
	Labels  []MetricLabel  `json:"labels,omitempty"`
	Samples []MetricSample `json:"samples"`
}

// InstantSeries is one instant metric series (tempopb InstantSeries).
type InstantSeries struct {
	Labels []MetricLabel `json:"labels,omitempty"`
	Value  float64       `json:"value"`
}

// QueryRangeResponse is the Tempo TraceQL metrics range response
// (GET /api/metrics/query_range).
type QueryRangeResponse struct {
	Series []TimeSeries `json:"series"`
}

// QueryInstantResponse is the Tempo TraceQL metrics instant response
// (GET /api/metrics/query).
type QueryInstantResponse struct {
	Series []InstantSeries `json:"series"`
}
