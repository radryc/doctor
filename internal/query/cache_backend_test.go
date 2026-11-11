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

func TestCloneTraceRecordsDoesNotShareMutableMaps(t *testing.T) {
	records := []segment.TraceRecord{{
		TraceID:            "trace-1",
		Service:            "api",
		Partition:          "doctor",
		Attributes:         map[string]string{"http.method": "GET"},
		ResourceAttributes: map[string]string{"service.name": "api"},
		Events: []segment.TraceEvent{{
			Name:       "exception",
			Attributes: map[string]string{"exception.type": "boom"},
		}},
	}}
	cloned := cloneTraceRecords(records)
	if len(cloned) != 1 {
		t.Fatalf("cloneTraceRecords() returned %d records", len(cloned))
	}

	// Canonicalization writes into Attributes; the clone must be independent or
	// concurrent requests using the same cache entry race on the map.
	cloned[0].Attributes["canonical"] = "1"
	cloned[0].ResourceAttributes["service.name"] = "mutated"
	cloned[0].Events[0].Attributes["exception.type"] = "mutated"

	if _, ok := records[0].Attributes["canonical"]; ok {
		t.Fatal("clone shares Attributes map with the cached record")
	}
	if got := records[0].ResourceAttributes["service.name"]; got != "api" {
		t.Fatalf("clone shares ResourceAttributes map: got %q", got)
	}
	if got := records[0].Events[0].Attributes["exception.type"]; got != "boom" {
		t.Fatalf("clone shares event Attributes map: got %q", got)
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
