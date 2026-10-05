package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestElasticsearchUpstreamQueryLogs(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_query" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &payload)
		gotQuery = payload.Query
		_ = json.NewEncoder(w).Encode(map[string]any{
			"columns": []map[string]any{
				{"name": "@timestamp", "type": "date"},
				{"name": "body.text", "type": "text"},
				{"name": "resource.attributes.service.name", "type": "keyword"},
				{"name": "severity_text", "type": "keyword"},
				{"name": "attributes.http.method", "type": "keyword"},
			},
			"values": []any{
				[]any{"2026-06-01T12:00:00.000Z", "boom", "api", "error", "GET"},
			},
		})
	}))
	defer server.Close()

	upstream := NewElasticsearchUpstream(Backend{
		Name:    "es-a",
		Kind:    KindElasticsearch,
		BaseURL: server.URL,
		Index:   "logs-*.otel-*",
	}, server.Client())

	records, err := upstream.QueryLogs(context.Background(), "default", LogPushdown{
		Selector:    `{service="api"}`,
		LineFilters: []LogLineFilter{{Op: "|=", Value: "boom"}},
	}, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC), 100)
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if !strings.Contains(gotQuery, "FROM logs-*.otel-*") || !strings.Contains(gotQuery, "body.text LIKE") {
		t.Fatalf("esql query = %s", gotQuery)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	record := records[0]
	if record.Service != "api" || record.Body != "boom" || record.SeverityText != "error" {
		t.Fatalf("record = %#v", record)
	}
	if record.Attributes["attributes.http.method"] != "GET" {
		t.Fatalf("attributes = %#v", record.Attributes)
	}
	if !record.Timestamp.Equal(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("timestamp = %s", record.Timestamp)
	}
}

func TestElasticsearchUpstreamUnsupportedSelector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("upstream should not be called for unsupported translation")
	}))
	defer server.Close()
	upstream := NewElasticsearchUpstream(Backend{Name: "es-a", Kind: KindElasticsearch, BaseURL: server.URL, Index: "logs-*"}, server.Client())
	if _, err := upstream.QueryLogs(context.Background(), "default", LogPushdown{Selector: `service="api"`}, time.Time{}, time.Time{}, 10); err == nil {
		t.Fatal("expected translation error")
	}
}
