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

func TestBigQueryUpstreamQueryLogs(t *testing.T) {
	var gotSQL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/projects/proj/queries":
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Query        string `json:"query"`
				UseLegacySQL bool   `json:"useLegacySql"`
			}
			_ = json.Unmarshal(body, &payload)
			gotSQL = payload.Query
			if payload.UseLegacySQL {
				t.Error("useLegacySql must be false")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobComplete": true,
				"jobReference": map[string]any{
					"jobId": "job-1",
				},
				"schema": map[string]any{
					"fields": []map[string]any{
						{"name": "timestamp", "type": "TIMESTAMP"},
						{"name": "body", "type": "STRING"},
						{"name": "service", "type": "STRING"},
						{"name": "severity", "type": "STRING"},
						{"name": "http_method", "type": "STRING"},
					},
				},
				"rows": []map[string]any{
					{"f": []map[string]any{{"v": "1780315200.000000"}, {"v": "boom"}, {"v": "api"}, {"v": "error"}, {"v": "GET"}}},
				},
				"pageToken": "next",
			})
		case "/projects/proj/queries/job-1":
			if r.URL.Query().Get("pageToken") != "next" {
				t.Errorf("pageToken = %q, want next", r.URL.Query().Get("pageToken"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobComplete": true,
				"rows": []map[string]any{
					{"f": []map[string]any{{"v": "1780315260.000000"}, {"v": "boom-2"}, {"v": "api"}, {"v": "error"}, {"v": "POST"}}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	upstream := NewBigQueryUpstream(Backend{
		Name:    "bq-a",
		Kind:    KindBigQuery,
		BaseURL: server.URL,
		Project: "proj",
		Dataset: "obs",
		Table:   "logs",
	}, server.Client())

	records, err := upstream.QueryLogs(context.Background(), "default", LogPushdown{
		Selector:    `{service="api"}`,
		LineFilters: []LogLineFilter{{Op: "|=", Value: "boom"}},
	}, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC), 100)
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if !strings.Contains(gotSQL, "FROM `proj.obs.logs`") || !strings.Contains(gotSQL, "STRPOS(body, 'boom') > 0") {
		t.Fatalf("sql = %s", gotSQL)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2 (%#v)", len(records), records)
	}
	if records[0].Service != "api" || records[0].Body != "boom" {
		t.Fatalf("first record = %#v", records[0])
	}
	if records[1].Body != "boom-2" {
		t.Fatalf("second record = %#v", records[1])
	}
	if !records[0].Timestamp.Equal(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("timestamp = %s", records[0].Timestamp)
	}
	if records[0].Attributes["http_method"] != "GET" {
		t.Fatalf("attributes = %#v", records[0].Attributes)
	}
}

func TestBigQueryUpstreamErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer server.Close()
	upstream := NewBigQueryUpstream(Backend{Name: "bq-a", Kind: KindBigQuery, BaseURL: server.URL, Project: "proj", Table: "logs"}, server.Client())
	if _, err := upstream.QueryLogs(context.Background(), "default", LogPushdown{Selector: `{service="api"}`}, time.Time{}, time.Time{}, 10); err == nil {
		t.Fatal("expected error on 403")
	}
}
