package query

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

func TestCanonicalLabelsUnifySignals(t *testing.T) {
	attrs := map[string]string{
		"guardian.partition": "doctor",
		"guardian.intent":    "query",
		"k8s.pod.name":       "doctor-query-abc",
		"k8s.namespace.name": "doctor",
	}

	metricLabels := metricPointLabels(segment.MetricPointRecord{
		Service:    "doctor",
		Tenant:     "default",
		MetricName: "doctor.component.up",
		Attributes: attrs,
	})
	logLabels := lokiLabelsFromRecord(segment.LogRecord{
		Service:    "doctor",
		Tenant:     "default",
		Attributes: attrs,
	})
	traceTags := zipkinSpanFromRecord(segment.TraceRecord{
		Service:    "doctor",
		Tenant:     "default",
		Attributes: attrs,
	}).Tags

	want := map[string]string{
		"service":            "doctor",
		"guardian_partition": "doctor",
		"guardian_intent":    "query",
		"k8s_pod_name":       "doctor-query-abc",
		"k8s_namespace_name": "doctor",
		"source":             "doctor",
	}

	for key, expected := range want {
		if got := metricLabels.Get(key); got != expected {
			t.Fatalf("metric label %s = %q, want %q", key, got, expected)
		}
		if got := logLabels[key]; got != expected {
			t.Fatalf("log label %s = %q, want %q", key, got, expected)
		}
		if got := traceTags[key]; got != expected {
			t.Fatalf("trace tag %s = %q, want %q", key, got, expected)
		}
	}
}

func TestLokiLabelsIncludeCorrelationAndLevel(t *testing.T) {
	labels := lokiLabelsFromRecord(segment.LogRecord{
		Service:      "guardian",
		SeverityText: "ERROR",
		TraceID:      "trace-1",
		SpanID:       "span-1",
	})

	if got := labels["level"]; got != "error" {
		t.Fatalf("level = %q, want error", got)
	}
	if got := labels["trace_id"]; got != "trace-1" {
		t.Fatalf("trace_id = %q, want trace-1", got)
	}
	if got := labels["span_id"]; got != "span-1" {
		t.Fatalf("span_id = %q, want span-1", got)
	}
	if got := labels["source"]; got != "doctor" {
		t.Fatalf("source = %q, want doctor", got)
	}
}

func TestIdentityFromAttributesFallback(t *testing.T) {
	partition, intent, asset := identityFromAttributes(map[string]string{
		"k8s.namespace.name":          "monofs",
		"app.kubernetes.io/component": "router",
		"k8s.pod.name":                "monofs-router-0",
	})

	if partition != "monofs" {
		t.Fatalf("partition = %q, want monofs", partition)
	}
	if intent != "router" {
		t.Fatalf("intent = %q, want router", intent)
	}
	if asset != "monofs-router-0" {
		t.Fatalf("asset = %q, want monofs-router-0", asset)
	}
}

func TestTenantFromRequestPrefersHeader(t *testing.T) {
	svc := &Service{defaultTenant: "default", tenantHeader: "X-Doctor-Tenant"}

	headerReq := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	headerReq.Header.Set("X-Doctor-Tenant", "team-a")
	if got := svc.tenantFromRequest(headerReq); got != "team-a" {
		t.Fatalf("header tenant = %q, want team-a", got)
	}

	paramReq := httptest.NewRequest(http.MethodGet, "/api/v1/query?tenant=team-b", nil)
	paramReq.Header.Set("X-Doctor-Tenant", "team-a")
	if got := svc.tenantFromRequest(paramReq); got != "team-b" {
		t.Fatalf("param precedence tenant = %q, want team-b", got)
	}

	fallbackReq := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	if got := svc.tenantFromRequest(fallbackReq); got != "default" {
		t.Fatalf("fallback tenant = %q, want default", got)
	}
}

// Per-record correlation identifiers must never become metric series labels:
// each sample would form its own series and explode cardinality. This is the
// regression guard for the dashboard OOM.
func TestHandlePrometheusQueryExcludesCorrelationLabelsFromSeries(t *testing.T) {
	base := time.Date(2026, 5, 12, 14, 0, 0, 0, time.UTC)
	backend := &fakeLogEngineBackend{
		metrics: []segment.MetricPointRecord{{
			Service:    "api",
			MetricName: "http_requests_total",
			Timestamp:  base,
			Attributes: map[string]string{
				"trace_id":         "trace-123",
				"span_id":          "span-1",
				"guardian_task_id": "task-1",
				"route":            "/healthz",
			},
			HasValue: true,
			Value:    1,
		}},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/query?query=http_requests_total%7Bservice%3D%22api%22%7D&time="+strconv.FormatInt(base.Unix(), 10),
		nil,
	)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Data.Result) != 1 {
		t.Fatalf("result length = %d, want 1", len(resp.Data.Result))
	}
	metric := resp.Data.Result[0].Metric
	for _, label := range []string{"trace_id", "span_id", "guardian_task_id"} {
		if _, ok := metric[label]; ok {
			t.Fatalf("series unexpectedly exposes unbounded label %q: %v", label, metric)
		}
	}
	if got := metric["route"]; got != "/healthz" {
		t.Fatalf("bounded label route = %q, want /healthz", got)
	}
}
