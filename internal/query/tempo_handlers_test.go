package query

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	// Regex matchers are outside the shared subset matcher and no Tempo backend
	// is configured, so the request must fail with a clear error.
	req := httptest.NewRequest(http.MethodGet, "/api/search?q=%7B%20.name%20%3D~%20%22x%22%20%7D", nil)
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
