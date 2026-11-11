package federation

import (
	"testing"
	"time"
)

func TestTargetBreakerExpiredProbeAllowsRetry(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	breaker := &targetBreaker{
		openUntil:  now.Add(-time.Second),
		halfOpen:   true,
		probeUntil: now.Add(-time.Second),
	}

	// A probe that never reported back must not wedge the target forever.
	if !breaker.allow(now, time.Second) {
		t.Fatal("allow() rejected a fresh probe after the previous probe deadline expired")
	}
	if !breaker.halfOpen || !breaker.probeUntil.After(now) {
		t.Fatalf("allow() did not start a new probe: halfOpen=%v until=%v", breaker.halfOpen, breaker.probeUntil)
	}
	if breaker.allow(now, time.Second) {
		t.Fatal("allow() admitted a concurrent probe while one is in flight")
	}
}

func TestManagerPrunesBreakersOutsideCatalog(t *testing.T) {
	catalog := managerTestCatalog(t)
	manager := NewManager(catalog, Options{})
	manager.breakerFor("removed-target")
	if manager.breakerFor("aws-a") == nil {
		t.Fatal("expected breaker for catalog target")
	}
	manager.pruneBreakers(func(id string) bool {
		_, ok := catalog.ByID(id)
		return ok
	})
	manager.breakerMu.Lock()
	_, stale := manager.breakers["removed-target"]
	_, kept := manager.breakers["aws-a"]
	manager.breakerMu.Unlock()
	if stale {
		t.Fatal("breaker for a removed target was not pruned")
	}
	if !kept {
		t.Fatal("breaker for a live target was pruned")
	}
}
