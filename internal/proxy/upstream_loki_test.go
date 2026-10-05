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

func TestLokiHTTPUpstreamQueryLogs(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		gotQuery = r.Form.Get("query")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "streams",
				"result": []any{map[string]any{
					"stream": map[string]string{"service": "api", "level": "error"},
					"values": [][]string{
						{strconv.FormatInt(base.UnixNano(), 10), "boom"},
					},
				}},
			},
		})
	}))
	defer server.Close()

	upstream := NewLokiHTTPUpstream(Backend{Name: "loki-a", Kind: KindLoki, BaseURL: server.URL}, server.Client())
	records, err := upstream.QueryLogs(context.Background(), "default", LogPushdown{
		Selector:    `{service="api"}`,
		LineFilters: []LogLineFilter{{Op: "|=", Value: "boom"}},
	}, base.Add(-time.Hour), base, 100)
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if gotQuery != `{service="api"} |= "boom"` {
		t.Fatalf("query = %q", gotQuery)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	record := records[0]
	if record.Service != "api" || record.Body != "boom" {
		t.Fatalf("record = %#v", record)
	}
	if !record.Timestamp.Equal(base) {
		t.Fatalf("timestamp = %s, want %s", record.Timestamp, base)
	}
	if record.Attributes["level"] != "error" {
		t.Fatalf("attributes = %#v", record.Attributes)
	}
}

func TestLogPushdownQuery(t *testing.T) {
	pushdown := LogPushdown{
		Selector: `{service="api"}`,
		LineFilters: []LogLineFilter{
			{Op: "|=", Value: "a"},
			{Op: "!~", Value: "b.*"},
		},
	}
	want := `{service="api"} |= "a" !~ "b.*"`
	if got := pushdown.Query(); got != want {
		t.Fatalf("Query() = %q, want %q", got, want)
	}
}
