package query

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

func TestTempoEcho(t *testing.T) {
	svc := &Service{defaultTenant: "default"}
	req := httptest.NewRequest(http.MethodGet, "/api/echo", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoEcho(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestTempoTraceByIDReturnsOTLPJSON(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{{
		TraceID:    "trace-123",
		SpanID:     "span-1",
		Name:       "GET /health",
		Kind:       "SPAN_KIND_SERVER",
		Service:    "api",
		StartTime:  base,
		EndTime:    base.Add(150 * time.Millisecond),
		Attributes: map[string]string{"http.method": "GET"},
	}}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/traces/trace-123", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoTraceByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Batches []struct {
			Resource struct {
				Attributes []struct {
					Key   string `json:"key"`
					Value struct {
						StringValue string `json:"stringValue"`
					} `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []struct {
					TraceID    string `json:"traceId"`
					SpanID     string `json:"spanId"`
					Name       string `json:"name"`
					Kind       string `json:"kind"`
					Attributes []struct {
						Key string `json:"key"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"batches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if len(resp.Batches) != 1 || len(resp.Batches[0].ScopeSpans) != 1 || len(resp.Batches[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("unexpected trace shape: %s", rec.Body.String())
	}
	span := resp.Batches[0].ScopeSpans[0].Spans[0]
	if span.TraceID != "trace-123" || span.SpanID != "span-1" || span.Name != "GET /health" {
		t.Fatalf("span = %#v", span)
	}
	if span.Kind != "SPAN_KIND_SERVER" {
		t.Fatalf("kind = %q", span.Kind)
	}
	foundService := false
	for _, attribute := range resp.Batches[0].Resource.Attributes {
		if attribute.Key == "service.name" && attribute.Value.StringValue == "api" {
			foundService = true
		}
	}
	if !foundService {
		t.Fatalf("service.name missing from resource attributes: %s", rec.Body.String())
	}
}

func TestTempoSearchReturnsSummaries(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{
		{TraceID: "trace-a", SpanID: "span-a", Name: "GET /a", Service: "api", StartTime: base.Add(-time.Minute), EndTime: base.Add(-time.Minute + 200*time.Millisecond)},
		{TraceID: "trace-b", SpanID: "span-b", Name: "GET /b", Service: "worker", StartTime: base.Add(-30 * time.Second), EndTime: base.Add(-30*time.Second + 100*time.Millisecond)},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/search?tags=service.name%3Dapi&limit=10", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoSearch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Traces []struct {
			TraceID         string `json:"traceID"`
			RootServiceName string `json:"rootServiceName"`
			DurationMs      int64  `json:"durationMs"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Traces) != 1 || resp.Traces[0].TraceID != "trace-a" {
		t.Fatalf("traces = %#v", resp.Traces)
	}
	if resp.Traces[0].DurationMs != 200 {
		t.Fatalf("durationMs = %d, want 200", resp.Traces[0].DurationMs)
	}
}

func TestTempoTagsAndValues(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{{
		TraceID: "trace-a", SpanID: "span-a", Name: "GET /a", Service: "api",
		StartTime: base, EndTime: base.Add(time.Second),
		Attributes: map[string]string{"http.method": "GET"},
	}}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	tagsReq := httptest.NewRequest(http.MethodGet, "/api/search/tags", nil)
	tagsRec := httptest.NewRecorder()
	svc.handleTempoTags(tagsRec, tagsReq)
	if tagsRec.Code != http.StatusOK {
		t.Fatalf("tags status = %d", tagsRec.Code)
	}
	var tags struct {
		TagNames []string `json:"tagNames"`
	}
	if err := json.Unmarshal(tagsRec.Body.Bytes(), &tags); err != nil {
		t.Fatalf("decode tags: %v", err)
	}
	if !containsString(tags.TagNames, "service.name") || !containsString(tags.TagNames, "http.method") {
		t.Fatalf("tagNames = %#v", tags.TagNames)
	}

	valuesReq := httptest.NewRequest(http.MethodGet, "/api/search/tag/service.name/values", nil)
	valuesRec := httptest.NewRecorder()
	svc.handleTempoTagValues(valuesRec, valuesReq)
	if valuesRec.Code != http.StatusOK {
		t.Fatalf("values status = %d", valuesRec.Code)
	}
	var values struct {
		TagValues []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"tagValues"`
	}
	if err := json.Unmarshal(valuesRec.Body.Bytes(), &values); err != nil {
		t.Fatalf("decode values: %v", err)
	}
	if len(values.TagValues) != 1 || values.TagValues[0].Value != "api" {
		t.Fatalf("tagValues = %#v", values.TagValues)
	}
}

func TestTempoTraceByIDFallsBackToAWS(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	aws := &fakeAWSBackend{spans: []awsstore.Span{{
		TraceID:   "1-abc",
		SpanID:    "seg-1",
		Name:      "api",
		Service:   "xray-api",
		StartTime: base,
		EndTime:   base.Add(250 * time.Millisecond),
	}}}
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}, aws: aws}

	req := httptest.NewRequest(http.MethodGet, "/api/traces/1-abc", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoTraceByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"traceId":"1-abc"`) {
		t.Fatalf("body missing AWS span: %s", rec.Body.String())
	}
}

func TestTempoSearchTraceQLUnsupportedConstructFails(t *testing.T) {
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}}
	svc.SetProxyRouter(newProxyTestRouter(t, svc))

	// Structural operators are outside the shared subset matcher and no Tempo
	// backend is configured, so the request must fail with a clear error.
	req := httptest.NewRequest(http.MethodGet, "/api/search?q=%7B%20.name%20%3D%20%22x%22%20%7D%20%3E%3E%20%7B%20.name%20%3D%20%22y%22%20%7D", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoSearch(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestTempoSearchTraceQLSubsetFiltersLocalTraces(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{
		{TraceID: "trace-a", SpanID: "span-a", Name: "GET /a", Service: "api", StartTime: base, EndTime: base.Add(time.Second)},
		{TraceID: "trace-b", SpanID: "span-b", Name: "GET /b", Service: "worker", StartTime: base, EndTime: base.Add(time.Second)},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}
	svc.SetProxyRouter(newProxyTestRouter(t, svc))

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=%7B%20.service.name%20%3D%20%22api%22%20%7D", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoSearch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Traces []struct {
			TraceID string `json:"traceID"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Traces) != 1 || resp.Traces[0].TraceID != "trace-a" {
		t.Fatalf("traces = %#v", resp.Traces)
	}
}

func TestTempoSearchTraceQLSubsetFiltersLocalTracesWithoutRouter(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{
		{TraceID: "trace-a", SpanID: "span-a", Name: "GET /a", Service: "api", StartTime: base, EndTime: base.Add(time.Second), Attributes: map[string]string{"source": "aws"}},
		{TraceID: "trace-b", SpanID: "span-b", Name: "GET /b", Service: "worker", StartTime: base, EndTime: base.Add(time.Second)},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=%7B%20source%20%3D%20%22aws%22%20%7D", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoSearch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Traces []struct {
			TraceID string `json:"traceID"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Traces) != 1 || resp.Traces[0].TraceID != "trace-a" {
		t.Fatalf("traces = %#v", resp.Traces)
	}
}

func TestTempoSearchRejectsInvalidDuration(t *testing.T) {
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}}
	req := httptest.NewRequest(http.MethodGet, "/api/search?minDuration=notaduration", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoSearch(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestTempoTraceByIDNotFound(t *testing.T) {
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}}
	req := httptest.NewRequest(http.MethodGet, "/api/traces/missing", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoTraceByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestTempoTraceByIDProtobuf(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{{
		TraceID:   traceID,
		SpanID:    "00f067aa0ba902b7",
		Name:      "GET /health",
		Kind:      "SPAN_KIND_SERVER",
		Service:   "api",
		StartTime: base,
		EndTime:   base.Add(150 * time.Millisecond),
	}}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/traces/"+traceID, nil)
	req.Header.Set("Accept", "application/protobuf")
	rec := httptest.NewRecorder()
	svc.handleTempoTraceByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/protobuf" {
		t.Fatalf("content-type = %q", got)
	}
	var tr coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	spans := tr.GetResourceSpans()
	if len(spans) != 1 || len(spans[0].GetScopeSpans()) != 1 || len(spans[0].GetScopeSpans()[0].GetSpans()) != 1 {
		t.Fatalf("unexpected proto shape: %#v", spans)
	}
	span := spans[0].GetScopeSpans()[0].GetSpans()[0]
	if span.GetName() != "GET /health" || span.GetKind() != tracepb.Span_SPAN_KIND_SERVER {
		t.Fatalf("span = %#v", span)
	}
}

func TestTempoTraceByIDLatestResolvesNewestTrace(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{
		{
			TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736",
			SpanID:    "00f067aa0ba902b7",
			Name:      "GET /old",
			Service:   "api",
			StartTime: base.Add(-2 * time.Minute),
			EndTime:   base.Add(-2*time.Minute + 100*time.Millisecond),
		},
		{
			TraceID:   "5bf92f3577b34da6a3ce929d0e0e4736",
			SpanID:    "11f067aa0ba902b7",
			Name:      "GET /new",
			Service:   "api",
			StartTime: base.Add(-1 * time.Minute),
			EndTime:   base.Add(-1*time.Minute + 100*time.Millisecond),
		},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/v2/traces/latest", nil)
	req.Header.Set("Accept", "application/protobuf")
	rec := httptest.NewRecorder()
	svc.handleTempoTraceByIDV2(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	number, wireType, tagLen := protowire.ConsumeTag(rec.Body.Bytes())
	if number != 1 || wireType != protowire.BytesType {
		t.Fatalf("tag = (%d, %d)", number, wireType)
	}
	traceBody, _ := protowire.ConsumeBytes(rec.Body.Bytes()[tagLen:])
	var tr coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(traceBody, &tr); err != nil {
		t.Fatalf("nested proto.Unmarshal: %v", err)
	}
	spans := tr.GetResourceSpans()
	if len(spans) != 1 || len(spans[0].GetScopeSpans()) != 1 || len(spans[0].GetScopeSpans()[0].GetSpans()) != 1 {
		t.Fatalf("unexpected proto shape: %#v", spans)
	}
	span := spans[0].GetScopeSpans()[0].GetSpans()[0]
	if got := hex.EncodeToString(span.GetTraceId()); got != "5bf92f3577b34da6a3ce929d0e0e4736" || span.GetName() != "GET /new" {
		t.Fatalf("span = traceId=%s name=%q", got, span.GetName())
	}
}

func TestTraceByIDLatestResolvesNewestTrace(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{
		{TraceID: "old", SpanID: "span-old", Name: "GET /old", Service: "api", StartTime: base.Add(-2 * time.Minute), EndTime: base.Add(-2*time.Minute + 100*time.Millisecond)},
		{TraceID: "new", SpanID: "span-new", Name: "GET /new", Service: "api", StartTime: base.Add(-1 * time.Minute), EndTime: base.Add(-1*time.Minute + 100*time.Millisecond)},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/v1/traces/latest", nil)
	rec := httptest.NewRecorder()
	svc.handleTraceByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"trace_id":"new"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestResolveTraceIDLatestScopedSources(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	local := &fakeLogEngineBackend{traces: []segment.TraceRecord{
		{TraceID: "local-new", SpanID: "span-local", Name: "GET /local", Service: "monofs-server", StartTime: base.Add(-1 * time.Minute), EndTime: base},
	}}
	aws := &fakeAWSBackend{traces: []awsstore.TraceSummary{
		{TraceID: "1-aws-trace", Service: "aws-demo-tracegen", StartTime: base.Add(-2 * time.Minute), EndTime: base.Add(-2 * time.Minute)},
	}}
	svc := &Service{defaultTenant: "default", logEngine: local, aws: aws}

	from, to := base.Add(-time.Hour), base
	cases := []struct{ magic, want string }{
		{latestTraceID, "local-new"},
		{latestAWSTraceID, "1-aws-trace"},
		{latestDoctorTraceID, "local-new"},
		{magicLatestTraceID, "local-new"},
		{magicLatestAWSTraceID, "1-aws-trace"},
		{magicLatestDoctorTraceID, "local-new"},
		{"latest-service:monofs", "local-new"},
		{"latest-service:aws-demo", "1-aws-trace"},
		{"ad006d6f6e6f66732d00000000000000", "local-new"},
	}
	for _, tc := range cases {
		got, err := svc.resolveTraceID(context.Background(), "default", tc.magic, from, to)
		if err != nil {
			t.Fatalf("resolveTraceID(%s) error = %v", tc.magic, err)
		}
		if got != tc.want {
			t.Fatalf("resolveTraceID(%s) = %q, want %q", tc.magic, got, tc.want)
		}
	}
}

func TestTempoTraceByIDV2Protobuf(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{{
		TraceID:   traceID,
		SpanID:    "00f067aa0ba902b7",
		Name:      "GET /health",
		Service:   "api",
		StartTime: base,
		EndTime:   base.Add(150 * time.Millisecond),
	}}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/api/v2/traces/"+traceID, nil)
	req.Header.Set("Accept", "application/protobuf")
	rec := httptest.NewRecorder()
	svc.handleTempoTraceByIDV2(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// TraceByIDResponse.trace is field 1; decode it and check the nested OTLP data.
	number, wireType, tagLen := protowire.ConsumeTag(rec.Body.Bytes())
	if number != 1 || wireType != protowire.BytesType {
		t.Fatalf("tag = (%d, %d)", number, wireType)
	}
	traceBody, _ := protowire.ConsumeBytes(rec.Body.Bytes()[tagLen:])
	var tr coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(traceBody, &tr); err != nil {
		t.Fatalf("nested proto.Unmarshal: %v", err)
	}
	if len(tr.GetResourceSpans()) != 1 {
		t.Fatalf("resourceSpans = %d, want 1", len(tr.GetResourceSpans()))
	}
}

func TestUICatchAllReturns404ForUnknownAPIPaths(t *testing.T) {
	svc := &Service{defaultTenant: "default"}
	req := httptest.NewRequest(http.MethodGet, "/api/v3/traces/missing", nil)
	rec := httptest.NewRecorder()
	svc.handleUI(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestTempoBuildInfo(t *testing.T) {
	svc := &Service{defaultTenant: "default"}
	req := httptest.NewRequest(http.MethodGet, "/api/status/buildinfo", nil)
	rec := httptest.NewRecorder()
	svc.handleTempoBuildInfo(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version"`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestTempoTraceByIDMergesLocalAndAWS(t *testing.T) {
	base := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	const (
		canonical = "4bf92f3577b34da6a3ce929d0e0e4736"
		xrayID    = "1-4bf92f35-77b34da6a3ce929d0e0e4736"
		rootSpan  = "aaaaaaaaaaaaaaaa"
		childSpan = "bbbbbbbbbbbbbbbb"
	)
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{{
		TraceID:   canonical,
		SpanID:    rootSpan,
		Name:      "checkout",
		Service:   "checkout-api",
		StartTime: base,
		EndTime:   base.Add(500 * time.Millisecond),
	}}}
	aws := &fakeAWSBackend{spans: []awsstore.Span{{
		TraceID:      xrayID,
		SpanID:       childSpan,
		ParentSpanID: rootSpan,
		Name:         "process-order",
		Service:      "order-processor",
		StartTime:    base.Add(100 * time.Millisecond),
		EndTime:      base.Add(400 * time.Millisecond),
	}}}
	svc := &Service{defaultTenant: "default", logEngine: backend, aws: aws}

	req := httptest.NewRequest(http.MethodGet, "/api/traces/"+canonical, nil)
	rec := httptest.NewRecorder()
	svc.handleTempoTraceByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{rootSpan, childSpan, canonical} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	if !strings.Contains(body, `"key":"source","value":{"stringValue":"aws"}`) {
		t.Fatalf("body missing AWS source label: %s", body)
	}
	if len(backend.traceQueries) != 1 {
		t.Fatalf("local trace queries = %d, want 1", len(backend.traceQueries))
	}
	query := backend.traceQueries[0]
	if query.from.IsZero() || query.to.IsZero() {
		t.Fatalf("local trace query was not bounded: %#v", query)
	}
	if query.from.After(base) || query.to.Before(base.Add(time.Second)) {
		t.Fatalf("local trace window = [%v, %v], does not cover trace", query.from, query.to)
	}
}
