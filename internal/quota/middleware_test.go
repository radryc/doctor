package quota

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz/policy"
)

func TestPartitionsFromRequestReadsLabelMatchers(t *testing.T) {
	r := httptest.NewRequest("GET", `/api/v1/query?query=sum(rate(http_requests_total{guardian_partition="checkout"}[5m]))&partition=payments`, nil)
	got := PartitionsFromRequest(r)
	want := map[string]bool{"checkout": true, "payments": true}
	if len(got) != len(want) {
		t.Fatalf("partitions = %#v, want %#v", got, want)
	}
	for _, partition := range got {
		if !want[partition] {
			t.Fatalf("unexpected partition %q in %#v", partition, got)
		}
	}
}

func TestPartitionsFromRequestCanonicalService(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/logs/search?service=checkout.web.api", nil)
	got := PartitionsFromRequest(r)
	if len(got) != 1 || got[0] != "checkout" {
		t.Fatalf("partitions = %#v, want [checkout]", got)
	}
}

func TestQueryCostScalesWithRange(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	r := httptest.NewRequest("GET", "/api/v1/query_range?query=up&start="+start.Format(time.RFC3339)+"&end="+end.Format(time.RFC3339)+"&step=1s", nil)
	if got := QueryCost(r); got < 7 {
		t.Fatalf("cost = %v, want >= 7", got)
	}
	if got := QueryCost(httptest.NewRequest("GET", "/api/v1/query?query=up", nil)); got != 1 {
		t.Fatalf("instant query cost = %v, want 1", got)
	}
}

func TestShouldLimitSkipsUIAndHealth(t *testing.T) {
	for _, path := range []string{"/healthz", "/metrics", "/static/app.js", "/v1/ui/config", "/v1/quota"} {
		if ShouldLimit(path) {
			t.Fatalf("ShouldLimit(%q) = true, want false", path)
		}
	}
	for _, path := range []string{"/api/v1/query", "/loki/api/v1/query", "/v1/logs/search", "/v1/rules"} {
		if !ShouldLimit(path) {
			t.Fatalf("ShouldLimit(%q) = false, want true", path)
		}
	}
}

func TestMiddlewareRejectsRangeAndSamples(t *testing.T) {
	provider := testProvider(t, policy.Quota{
		Query: &policy.QueryQuota{MaxRange: "1h", MaxSamples: 1000},
	}, nil, nil, []string{"checkout"}, true)
	engine := fixedEngine(t, provider, 1)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	handler := engine.Middleware(next)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	req := httptest.NewRequest("GET", "/api/v1/query_range?query=up&partition=checkout&start="+start.Format(time.RFC3339)+"&end="+end.Format(time.RFC3339)+"&step=1s", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if called {
		t.Fatal("next handler should not be called")
	}
}

func TestMiddlewareObserveAllowsLongRange(t *testing.T) {
	provider := testProvider(t, policy.Quota{
		Query: &policy.QueryQuota{MaxRange: "1h"},
	}, nil, nil, []string{"checkout"}, false)
	engine := fixedEngine(t, provider, 1)
	called := false
	handler := engine.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	req := httptest.NewRequest("GET", "/api/v1/query_range?query=up&partition=checkout&start="+start.Format(time.RFC3339)+"&end="+end.Format(time.RFC3339)+"&step=1s", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("observe mode should allow the request")
	}
}

func TestQueryWindowParsesLokiNanoseconds(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	r := httptest.NewRequest("GET", "/loki/api/v1/query_range?start="+strconv.FormatInt(start.UnixNano(), 10)+"&end="+strconv.FormatInt(end.UnixNano(), 10)+"&step=1s", nil)
	window, samples, ok := QueryWindow(r)
	if !ok || window != time.Hour {
		t.Fatalf("window = %v ok=%v, want 1h", window, ok)
	}
	if samples != 3600 {
		t.Fatalf("samples = %v, want 3600", samples)
	}
}
