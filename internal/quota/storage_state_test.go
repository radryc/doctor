package quota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz/policy"
)

func storageQuota(max int64, retention string) policy.StorageQuota {
	return policy.StorageQuota{MaxBytes: max, RetentionTraces: retention, RetentionLogs: retention, RetentionMetrics: retention}
}

func TestFileStorageTrackerSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	q := storageQuota(100, "720h")

	tracker, err := NewFileStorageTracker(ctx, dir, "replica-a")
	if err != nil {
		t.Fatalf("NewFileStorageTracker: %v", err)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 60, q); err != nil {
		t.Fatalf("first Check: %v", err)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 50, q); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second Check error = %v, want ErrQuotaExceeded", err)
	}
	if err := tracker.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := NewFileStorageTracker(ctx, dir, "replica-a")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close(ctx) }()
	key := StorageKey{Tenant: "tenant-a", Partition: "edge"}
	if used := reopened.Used(key, q); used != 60 {
		t.Fatalf("used after restart = %d, want 60", used)
	}
	if err := reopened.Check(ctx, "tenant-a", "edge", 40, q); err != nil {
		t.Fatalf("admit remaining 40: %v", err)
	}
	if err := reopened.Check(ctx, "tenant-a", "edge", 1, q); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over-limit error = %v", err)
	}
}

func TestFileStorageTrackerReplaysJournalBeforeSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	q := storageQuota(100, "720h")

	first, err := NewFileStorageTracker(ctx, dir, "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Check(ctx, "tenant-a", "edge", 10, q); err != nil {
		t.Fatal(err)
	}
	// Do not flush: the journal entry alone must be replayed.
	second, err := NewFileStorageTracker(ctx, dir, "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close(ctx) }()
	if used := second.Used(StorageKey{Tenant: "tenant-a", Partition: "edge"}, q); used != 10 {
		t.Fatalf("replayed used = %d, want 10", used)
	}
	_ = first.Close(ctx)
}

func TestFileStorageTrackerSnapshotDoesNotDoubleCount(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	q := storageQuota(100, "720h")

	tracker, err := NewFileStorageTracker(ctx, dir, "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 10, q); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 20, q); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Close(ctx); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewFileStorageTracker(ctx, dir, "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close(ctx) }()
	if used := reopened.Used(StorageKey{Tenant: "tenant-a", Partition: "edge"}, q); used != 30 {
		t.Fatalf("used = %d, want 30 (snapshot + journal, no double count)", used)
	}
}

func TestStorageTrackerTenantIsolation(t *testing.T) {
	ctx := context.Background()
	q := storageQuota(100, "720h")
	tracker, err := NewFileStorageTracker(ctx, t.TempDir(), "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tracker.Close(ctx) }()

	if err := tracker.Check(ctx, "tenant-a", "shared", 60, q); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Check(ctx, "tenant-b", "shared", 60, q); err != nil {
		t.Fatalf("tenant-b should have its own counter: %v", err)
	}
	if used := tracker.Used(StorageKey{Tenant: "tenant-a", Partition: "shared"}, q); used != 60 {
		t.Fatalf("tenant-a used = %d", used)
	}
	if used := tracker.Used(StorageKey{Tenant: "tenant-b", Partition: "shared"}, q); used != 60 {
		t.Fatalf("tenant-b used = %d", used)
	}
}

func TestStorageTrackerRetentionExpiry(t *testing.T) {
	ctx := context.Background()
	q := storageQuota(100, "24h")
	tracker := NewStorageTracker()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return base }

	if err := tracker.Check(ctx, "tenant-a", "edge", 90, q); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 20, q); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected limit before expiry, got %v", err)
	}
	tracker.now = func() time.Time { return base.Add(31 * 24 * time.Hour) }
	if used := tracker.Used(StorageKey{Tenant: "tenant-a", Partition: "edge"}, q); used != 0 {
		t.Fatalf("used after retention expiry = %d", used)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 90, q); err != nil {
		t.Fatalf("expected expired bytes to be released: %v", err)
	}
}

func TestFileStorageTrackerCorruptJournalFailsClosed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "journal-replica-bad.log")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStorageTracker(ctx, dir, "replica-bad"); err == nil {
		t.Fatal("expected corrupt journal to fail startup")
	}
}

func TestStorageTrackerPartitionsIncludesTenant(t *testing.T) {
	ctx := context.Background()
	q := storageQuota(1000, "720h")
	tracker, err := NewFileStorageTracker(ctx, t.TempDir(), "replica-a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tracker.Close(ctx) }()
	if err := tracker.Check(ctx, "tenant-a", "edge", 10, q); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Check(ctx, "tenant-b", "edge", 10, q); err != nil {
		t.Fatal(err)
	}
	partitions := tracker.Partitions()
	if len(partitions) != 2 {
		t.Fatalf("partitions = %#v", partitions)
	}
}
