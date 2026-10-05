package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPromHTTPUpstreamInstant(t *testing.T) {
	var (
		gotPath  string
		gotQuery string
		gotAuth  string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = r.ParseForm()
		gotQuery = r.Form.Get("query")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []any{map[string]any{
					"metric": map[string]any{"__name__": "up"},
					"value":  []any{float64(1780000000), "1"},
				}},
			},
		})
	}))
	defer server.Close()

	upstream := NewPromHTTPUpstream(Backend{
		Name:    "prom-a",
		Kind:    KindPrometheus,
		BaseURL: server.URL,
		Token:   "secret",
	}, server.Client())

	data, err := upstream.PromInstant(context.Background(), "default", "up", time.Unix(1780000000, 0))
	if err != nil {
		t.Fatalf("PromInstant: %v", err)
	}
	if gotPath != "/api/v1/query" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotQuery != "up" {
		t.Fatalf("query = %q", gotQuery)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if len(PrometheusResultSlice(data)) != 1 {
		t.Fatalf("data = %#v", data)
	}
}

func TestPromHTTPUpstreamSeriesAndLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/series":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   []any{map[string]any{"__name__": "up", "job": "api"}},
			})
		case "/api/v1/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   []any{"__name__", "job"},
			})
		case "/api/v1/label/job/values":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   []any{"api"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	upstream := NewPromHTTPUpstream(Backend{Name: "prom-a", Kind: KindPrometheus, BaseURL: server.URL}, server.Client())
	series, err := upstream.PromSeries(context.Background(), "default", []string{`up{job="api"}`}, time.Time{}, time.Time{})
	if err != nil || len(series) != 1 || series[0]["job"] != "api" {
		t.Fatalf("series = %#v, err = %v", series, err)
	}
	labels, err := upstream.PromLabels(context.Background(), "default", nil, time.Time{}, time.Time{})
	if err != nil || len(labels) != 2 {
		t.Fatalf("labels = %#v, err = %v", labels, err)
	}
	values, err := upstream.PromLabelValues(context.Background(), "default", "job", nil, time.Time{}, time.Time{})
	if err != nil || len(values) != 1 || values[0] != "api" {
		t.Fatalf("values = %#v, err = %v", values, err)
	}
}

func TestPromHTTPUpstreamErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	upstream := NewPromHTTPUpstream(Backend{Name: "prom-a", Kind: KindPrometheus, BaseURL: server.URL}, server.Client())
	if _, err := upstream.PromInstant(context.Background(), "default", "up", time.Now()); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}
