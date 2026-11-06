package query

import (
	"context"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

func TestCachingBackendCachesSmallLogQueries(t *testing.T) {
	now := time.Now().UTC()
	inner := &countingLogBackend{}
	inner.logs = []segment.LogRecord{{
		Timestamp: now.Add(-time.Minute),
		Service:   "doctor.query.query",
		Partition: "doctor",
		Body:      "lookup failed",
	}}
	backend := newCachingBackend(inner, time.Minute)

	from, to := now.Add(-time.Hour), now
	first, err := backend.QueryLogsRaw(context.Background(), "default", `{service=~".+"} |= "failed"`, from, to, 50)
	if err != nil {
		t.Fatalf("first QueryLogsRaw() error = %v", err)
	}
	second, err := backend.QueryLogsRaw(context.Background(), "default", `{service=~".+"} |= "failed"`, from, to, 50)
	if err != nil {
		t.Fatalf("second QueryLogsRaw() error = %v", err)
	}
	if got := inner.logQueries.Load(); got != 1 {
		t.Fatalf("backend QueryLogs calls = %d, want 1", got)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("result lengths = %d/%d, want 1/1", len(first), len(second))
	}

	// Cached slices must not be shared with callers: handlers filter records
	// in place, and one caller must not corrupt another caller's view.
	first[0].Body = "mutated"
	if second[0].Body != "lookup failed" {
		t.Fatalf("cached result was mutated: %q", second[0].Body)
	}
}

func TestCachingBackendCachesDiscoverySizedQueries(t *testing.T) {
	now := time.Now().UTC()
	inner := &countingLogBackend{}
	backend := newCachingBackend(inner, time.Minute)

	from, to := now.Add(-time.Hour), now
	for i := 0; i < 2; i++ {
		if _, err := backend.QueryLogs(context.Background(), "default", "", "", from, to, 2000); err != nil {
			t.Fatalf("QueryLogs() error = %v", err)
		}
	}
	if got := inner.logQueries.Load(); got != 1 {
		t.Fatalf("backend QueryLogs calls = %d, want 1 for a cached discovery query", got)
	}
}

func TestCachingBackendSkipsVeryLargeQueries(t *testing.T) {
	now := time.Now().UTC()
	inner := &countingLogBackend{}
	backend := newCachingBackend(inner, time.Minute)

	from, to := now.Add(-time.Hour), now
	for i := 0; i < 2; i++ {
		if _, err := backend.QueryLogsRaw(context.Background(), "default", `{service=~".+"}`, from, to, 20000); err != nil {
			t.Fatalf("QueryLogsRaw() error = %v", err)
		}
	}
	if got := inner.logQueries.Load(); got != 2 {
		t.Fatalf("backend QueryLogs calls = %d, want 2 for an uncached large query", got)
	}
}
