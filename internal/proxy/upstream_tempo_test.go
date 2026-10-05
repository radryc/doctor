package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func tempoServer(t *testing.T) *httptest.Server {
	t.Helper()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/search":
			if r.URL.Query().Get("q") != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"traces": []map[string]any{{"traceID": "traceql-hit", "durationMs": 10}},
				})
				return
			}
			if r.URL.Query().Get("tags") == "service.name=api" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"traces": []map[string]any{{
						"traceID":           "trace-1",
						"rootServiceName":   "api",
						"rootTraceName":     "GET /a",
						"startTimeUnixNano": strconv.FormatInt(base.UnixNano(), 10),
						"durationMs":        200,
					}},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"traces": []any{}})
		case r.URL.Path == "/api/traces/trace-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"batches": []map[string]any{{
					"resource": map[string]any{
						"attributes": []map[string]any{{
							"key":   "service.name",
							"value": map[string]any{"stringValue": "api"},
						}},
					},
					"scopeSpans": []map[string]any{{
						"scope": map[string]any{},
						"spans": []map[string]any{{
							"traceId":           "trace-1",
							"spanId":            "span-1",
							"name":              "GET /a",
							"kind":              "SPAN_KIND_SERVER",
							"startTimeUnixNano": strconv.FormatInt(base.UnixNano(), 10),
							"endTimeUnixNano":   strconv.FormatInt(base.Add(200*time.Millisecond).UnixNano(), 10),
							"status":            map[string]any{"code": "STATUS_CODE_OK"},
						}},
					}},
				}},
			})
		case r.URL.Path == "/api/traces/missing":
			http.NotFound(w, r)
		case r.URL.Path == "/api/search/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"tagNames": []string{"service.name", "http.method"}})
		case r.URL.Path == "/api/search/tag/service.name/values":
			_ = json.NewEncoder(w).Encode(map[string]any{"tagValues": []map[string]any{{"type": "string", "value": "api"}}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestTempoHTTPUpstreamSearchAndTrace(t *testing.T) {
	server := tempoServer(t)
	defer server.Close()
	upstream := NewTempoHTTPUpstream(Backend{Name: "tempo-a", Kind: KindTempo, BaseURL: server.URL}, server.Client())

	summaries, err := upstream.SearchTraces(context.Background(), "default", TraceSearch{Service: "api"})
	if err != nil {
		t.Fatalf("SearchTraces: %v", err)
	}
	if len(summaries) != 1 || summaries[0].TraceID != "trace-1" || summaries[0].Duration != 200*time.Millisecond {
		t.Fatalf("summaries = %#v", summaries)
	}

	records, found, err := upstream.GetTrace(context.Background(), "default", "trace-1")
	if err != nil || !found {
		t.Fatalf("GetTrace: found=%v err=%v", found, err)
	}
	if len(records) != 1 || records[0].Service != "api" || records[0].Name != "GET /a" {
		t.Fatalf("records = %#v", records)
	}
	if records[0].StatusCode != "STATUS_CODE_OK" {
		t.Fatalf("status = %q", records[0].StatusCode)
	}

	if _, found, err := upstream.GetTrace(context.Background(), "default", "missing"); err != nil || found {
		t.Fatalf("missing trace: found=%v err=%v", found, err)
	}
}

func TestTempoHTTPUpstreamTraceQLAndTags(t *testing.T) {
	server := tempoServer(t)
	defer server.Close()
	upstream := NewTempoHTTPUpstream(Backend{Name: "tempo-a", Kind: KindTempo, BaseURL: server.URL}, server.Client())

	summaries, err := upstream.SearchTraceQL(context.Background(), "default", `{ .name = "x" }`, time.Time{}, time.Time{}, 10)
	if err != nil || len(summaries) != 1 || summaries[0].TraceID != "traceql-hit" {
		t.Fatalf("traceql summaries = %#v, err = %v", summaries, err)
	}

	tags, err := upstream.TraceTags(context.Background(), "default")
	if err != nil || len(tags) != 2 {
		t.Fatalf("tags = %#v, err = %v", tags, err)
	}
	values, err := upstream.TraceTagValues(context.Background(), "default", "service.name")
	if err != nil || len(values) != 1 || values[0] != "api" {
		t.Fatalf("values = %#v, err = %v", values, err)
	}
}

func TestTempoHTTPUpstreamErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	upstream := NewTempoHTTPUpstream(Backend{Name: "tempo-a", Kind: KindTempo, BaseURL: server.URL}, server.Client())
	if _, err := upstream.SearchTraces(context.Background(), "default", TraceSearch{}); err == nil {
		t.Fatal("expected error on 500")
	}
}
