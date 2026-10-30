package alerting

import (
	"context"
	"testing"
	"time"
)

func TestShardForStable(t *testing.T) {
	first := ShardFor("default", "payments", "checkout", 8)
	second := ShardFor("default", "payments", "checkout", 8)
	if first != second {
		t.Fatalf("shard changed: %d != %d", first, second)
	}
	if first < 0 || first >= 8 {
		t.Fatalf("shard out of range: %d", first)
	}
	if got := ShardFor("default", "payments", "checkout", 1); got != 0 {
		t.Fatalf("single shard = %d", got)
	}
}

func TestLeaseTakeover(t *testing.T) {
	ctx := context.Background()
	files := NewMemoryStore()
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	first := NewLeaseManager(files, "token", "/doctor/v1/alerts/.leases/shard-0.json", "a", 0, time.Minute, time.Second, nil)
	first.Now = func() time.Time { return now }
	if !first.TryAcquire(ctx) {
		t.Fatal("first lease acquire failed")
	}
	second := NewLeaseManager(files, "token", "/doctor/v1/alerts/.leases/shard-0.json", "b", 0, time.Minute, time.Second, nil)
	second.Now = func() time.Time { return now.Add(30 * time.Second) }
	if second.TryAcquire(ctx) {
		t.Fatal("second manager stole an active lease")
	}
	second.Now = func() time.Time { return now.Add(2 * time.Minute) }
	if !second.TryAcquire(ctx) {
		t.Fatal("second manager did not take over expired lease")
	}
}
