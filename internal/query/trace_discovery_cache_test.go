package query

import (
	"context"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

func TestTraceTagValuesCacheIsIdentityScopedAndExpires(t *testing.T) {
	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{{
		TraceID:   "trace-1",
		SpanID:    "span-1",
		Service:   "api",
		StartTime: time.Now().UTC().Add(-time.Minute),
		EndTime:   time.Now().UTC(),
	}}}
	svc := &Service{
		defaultTenant:          "default",
		logEngine:              backend,
		traceDiscoveryCacheTTL: time.Hour,
	}
	ctxA := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "user-a"})
	values, _, err := svc.collectTraceTagValues(ctxA, "default", "service.name")
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	if _, ok := values["api"]; !ok {
		t.Fatalf("values = %#v, want api", values)
	}
	if len(backend.traceQueries) != 1 {
		t.Fatalf("trace queries = %d, want 1", len(backend.traceQueries))
	}

	if _, _, err := svc.collectTraceTagValues(ctxA, "default", "service.name"); err != nil {
		t.Fatalf("cached collect: %v", err)
	}
	if len(backend.traceQueries) != 1 {
		t.Fatalf("cache miss caused %d queries, want 1", len(backend.traceQueries))
	}

	ctxB := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "user-b"})
	if _, _, err := svc.collectTraceTagValues(ctxB, "default", "service.name"); err != nil {
		t.Fatalf("identity B collect: %v", err)
	}
	if len(backend.traceQueries) != 2 {
		t.Fatalf("identity-scoped cache queries = %d, want 2", len(backend.traceQueries))
	}

	svc.traceDiscoveryCacheTTL = time.Millisecond
	svc.traceDiscoveryMu.Lock()
	svc.traceTagValuesCache = nil
	svc.traceDiscoveryMu.Unlock()
	time.Sleep(2 * time.Millisecond)
	if _, _, err := svc.collectTraceTagValues(ctxA, "default", "service.name"); err != nil {
		t.Fatalf("expired collect: %v", err)
	}
	if len(backend.traceQueries) != 3 {
		t.Fatalf("expired cache queries = %d, want 3", len(backend.traceQueries))
	}
}

func TestTraceDiscoveryCacheCachesEmptyResults(t *testing.T) {
	backend := &fakeLogEngineBackend{}
	svc := &Service{defaultTenant: "default", logEngine: backend, traceDiscoveryCacheTTL: time.Hour}
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "user"})
	first, _, err := svc.collectTraceTagValues(ctx, "default", "service.name")
	if err != nil {
		t.Fatalf("first collect: %v", err)
	}
	second, _, err := svc.collectTraceTagValues(ctx, "default", "service.name")
	if err != nil {
		t.Fatalf("cached collect: %v", err)
	}
	if len(first) != 0 || len(second) != 0 || len(backend.traceQueries) != 1 {
		t.Fatalf("empty cache values=%v/%v queries=%d", first, second, len(backend.traceQueries))
	}
}
