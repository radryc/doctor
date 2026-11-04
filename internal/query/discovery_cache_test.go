package query

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

type countingLogBackend struct {
	fakeLogEngineBackend
	logQueries   atomic.Int64
	traceQueries atomic.Int64
}

func (c *countingLogBackend) QueryLogs(ctx context.Context, tenant, query, service string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	c.logQueries.Add(1)
	return c.fakeLogEngineBackend.QueryLogs(ctx, tenant, query, service, from, to, limit)
}

func (c *countingLogBackend) QueryLogsRaw(ctx context.Context, tenant, query string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	c.logQueries.Add(1)
	return c.fakeLogEngineBackend.QueryLogsRaw(ctx, tenant, query, from, to, limit)
}

func (c *countingLogBackend) QueryTraces(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int) ([]segment.TraceRecord, error) {
	c.traceQueries.Add(1)
	return c.fakeLogEngineBackend.QueryTraces(ctx, tenant, traceID, service, from, to, limit)
}

func TestHandleLogServicesCachesRepeatedRequests(t *testing.T) {
	backend := &countingLogBackend{}
	backend.logs = []segment.LogRecord{{
		Timestamp: time.Now().UTC().Add(-time.Minute),
		Service:   "guardian.pusher.k8s-main",
		Partition: "guardian",
		Intent:    "pusher",
		Asset:     "k8s-main",
		Body:      "started",
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/v1/logs/services?tenant=default", nil)
	first := httptest.NewRecorder()
	svc.handleLogServices(first, req)
	if got := first.Header().Get("X-Doctor-Cache"); got != "miss" {
		t.Fatalf("first cache status = %q, want miss", got)
	}
	second := httptest.NewRecorder()
	svc.handleLogServices(second, req)
	if got := second.Header().Get("X-Doctor-Cache"); got != "hit" {
		t.Fatalf("second cache status = %q, want hit", got)
	}
	if got := backend.logQueries.Load(); got != 1 {
		t.Fatalf("QueryLogs calls = %d, want 1", got)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("cached body differs from first response")
	}
}

func TestHandleServiceDiscoveryCachesRepeatedRequests(t *testing.T) {
	backend := &countingLogBackend{}
	now := time.Now().UTC().Add(-time.Minute)
	backend.logs = []segment.LogRecord{{
		Timestamp: now,
		Service:   "agent.agent.lagent-backend",
		Partition: "agent",
		Intent:    "agent",
		Asset:     "lagent-backend",
		Body:      "request ok",
	}}
	backend.traces = []segment.TraceRecord{{
		StartTime: now,
		EndTime:   now.Add(10 * time.Millisecond),
		Service:   "agent.agent.lagent-backend",
		Name:      "GET /health",
		TraceID:   "trace-1",
		SpanID:    "span-1",
		Partition: "agent",
	}}
	backend.metrics = []segment.MetricPointRecord{{
		Timestamp:  now,
		Service:    "agent.agent.lagent-backend",
		MetricName: "http_requests_total",
		Value:      1,
		Partition:  "agent",
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/v1/services/discover?tenant=default&window=1h", nil)
	first := httptest.NewRecorder()
	svc.handleServiceDiscovery(first, req)
	if got := first.Header().Get("X-Doctor-Cache"); got != "miss" {
		t.Fatalf("first cache status = %q, want miss", got)
	}
	second := httptest.NewRecorder()
	svc.handleServiceDiscovery(second, req)
	if got := second.Header().Get("X-Doctor-Cache"); got != "hit" {
		t.Fatalf("second cache status = %q, want hit", got)
	}
	if got := backend.logQueries.Load(); got != 1 {
		t.Fatalf("QueryLogs calls = %d, want 1", got)
	}
	if got := backend.traceQueries.Load(); got != 1 {
		t.Fatalf("QueryTraces calls = %d, want 1", got)
	}
}
