package gcpstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/gcp/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("namespace") != "run.googleapis.com" {
			t.Errorf("namespace = %q", r.URL.Query().Get("namespace"))
		}
		_, _ = w.Write([]byte(`{"metrics":[{"namespace":"run.googleapis.com","metric_name":"request_count","dimensions":["response_code"],"project":"demo"}]}`))
	})
	mux.HandleFunc("/v1/gcp/metrics/query", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dim.response_code") != "200" {
			t.Errorf("dimensions = %v", r.URL.Query())
		}
		_, _ = w.Write([]byte(`{"series":[{"namespace":"run.googleapis.com","metric_name":"request_count","points":[{"timestamp":"2026-11-06T10:00:00Z","value":7}]}]}`))
	})
	mux.HandleFunc("/v1/gcp/log-names", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"logNames":["app","worker"]}`))
	})
	mux.HandleFunc("/v1/gcp/logs/query", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("log") != "app" {
			t.Errorf("log = %q", r.URL.Query().Get("log"))
		}
		_, _ = w.Write([]byte(`{"records":[{"timestamp":"2026-11-06T10:00:00Z","log_group":"app","message":"hello","region":"europe-west1","project":"demo"}]}`))
	})
	mux.HandleFunc("/v1/gcp/services", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"services":[{"name":"demo-api","region":"europe-west1","project":"demo"}]}`))
	})
	mux.HandleFunc("/v1/gcp/traces", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"traces":[{"trace_id":"abc","service":"demo-api","start_time":"2026-11-06T10:00:00Z","end_time":"2026-11-06T10:00:01Z","duration":1000000000,"span_count":2,"region":"europe-west1","project":"demo"}]}`))
	})
	mux.HandleFunc("/v1/gcp/traces/abc", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"spans":[{"trace_id":"abc","span_id":"root","name":"demo-api","service":"demo-api","start_time":"2026-11-06T10:00:00Z","end_time":"2026-11-06T10:00:01Z"}]}`))
	})
	mux.HandleFunc("/v1/gcp/promql/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "up" {
			t.Errorf("query = %q", r.URL.Query().Get("query"))
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewClient(server.URL, "token")
}

func TestHealth(t *testing.T) {
	client := testClient(t)
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestMetrics(t *testing.T) {
	client := testClient(t)
	descriptors, err := client.ListMetrics(context.Background(), "run.googleapis.com", "")
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}
	if len(descriptors) != 1 || descriptors[0].MetricName != "request_count" {
		t.Fatalf("descriptors = %+v", descriptors)
	}
	series, err := client.QueryMetric(context.Background(), MetricQuery{
		Namespace:  "run.googleapis.com",
		MetricName: "request_count",
		Dimensions: map[string]string{"response_code": "200"},
		From:       time.Now().Add(-time.Hour),
		To:         time.Now(),
	})
	if err != nil {
		t.Fatalf("QueryMetric: %v", err)
	}
	if len(series) != 1 || len(series[0].Points) != 1 || series[0].Points[0].Value != 7 {
		t.Fatalf("series = %+v", series)
	}
}

func TestLogsAndTraces(t *testing.T) {
	client := testClient(t)
	names, err := client.ListLogNames(context.Background())
	if err != nil || len(names) != 2 {
		t.Fatalf("ListLogNames = %v %v", names, err)
	}
	records, err := client.QueryLogs(context.Background(), LogQuery{LogName: "app", From: time.Now().Add(-time.Hour), To: time.Now(), Limit: 10})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(records) != 1 || records[0].Message != "hello" {
		t.Fatalf("records = %+v", records)
	}
	services, err := client.ListServices(context.Background())
	if err != nil || len(services) != 1 || services[0].Name != "demo-api" {
		t.Fatalf("ListServices = %v %v", services, err)
	}
	summaries, err := client.QueryTraces(context.Background(), TraceQuery{From: time.Now().Add(-time.Hour), To: time.Now()})
	if err != nil || len(summaries) != 1 || summaries[0].TraceID != "abc" {
		t.Fatalf("QueryTraces = %v %v", summaries, err)
	}
	spans, err := client.GetTrace(context.Background(), "abc")
	if err != nil || len(spans) != 1 || spans[0].SpanID != "root" {
		t.Fatalf("GetTrace = %v %v", spans, err)
	}
}

func TestPromQL(t *testing.T) {
	client := testClient(t)
	response, err := client.PrometheusQuery(context.Background(), "up", time.Now())
	if err != nil {
		t.Fatalf("PrometheusQuery: %v", err)
	}
	if response["status"] != "success" {
		t.Fatalf("response = %v", response)
	}
}

func TestHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()
	client := NewClient(server.URL, "")
	_, err := client.ListLogNames(context.Background())
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected an HTTP error, got %v", err)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("error type = %T", err)
	}
}
