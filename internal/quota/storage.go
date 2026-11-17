package quota

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/radryc/monofs/pkg/authz/policy"
)

// StorageTracker approximates per-partition stored bytes over the retention
// window. It is an ingest-side admission check, not a replacement for MonoFS
// compaction.
type StorageTracker struct {
	mu     sync.Mutex
	state  stateBackend
	now    func() time.Time
	logger *log.Logger
}

// NewStorageTracker builds the historical in-memory tracker.
func NewStorageTracker() *StorageTracker {
	return &StorageTracker{
		state: newMemoryState(),
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// NewStorageTrackerWithState builds a tracker over a durable or shared state
// backend and replays its persisted values.
func NewStorageTrackerWithState(ctx context.Context, state stateBackend) (*StorageTracker, error) {
	if state == nil {
		return nil, fmt.Errorf("quota: storage state is nil")
	}
	if err := state.load(ctx); err != nil {
		return nil, err
	}
	return &StorageTracker{
		state: state,
		now:   func() time.Time { return time.Now().UTC() },
	}, nil
}

// NewFileStorageTracker builds a journal/snapshot-backed tracker.
func NewFileStorageTracker(ctx context.Context, dir, replicaID string) (*StorageTracker, error) {
	state, err := newLocalState(dir, replicaID)
	if err != nil {
		return nil, err
	}
	return NewStorageTrackerWithState(ctx, state)
}

// SetLogger attaches a logger used by the background flusher.
func (t *StorageTracker) SetLogger(logger *log.Logger) {
	if t == nil {
		return
	}
	t.logger = logger
}

// Mode reports the backend for startup logging.
func (t *StorageTracker) Mode() string {
	if t == nil || t.state == nil {
		return "none"
	}
	return t.state.describe()
}

// Check admits bytes if the partition's rolling retention-window total stays
// under the configured maximum.
func (t *StorageTracker) Check(ctx context.Context, tenant, partition string, bytes int64, q policy.StorageQuota) error {
	if t == nil || q.MaxBytes <= 0 || bytes <= 0 {
		return nil
	}
	tenant = strings.TrimSpace(tenant)
	partition = normalizeStoragePart(partition)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	retention := storageRetention(q)
	total := totalFor(t.state.snapshot(), tenant, partition, now, retention)
	if total+bytes > q.MaxBytes {
		return fmt.Errorf("%w: tenant %s partition %s storage limit %d bytes",
			ErrQuotaExceeded, tenant, partition, q.MaxBytes)
	}
	return t.state.add(ctx, stateKey{Tenant: tenant, Partition: partition, Day: dayOf(now)}, bytes)
}

// Used returns the current rolling-window total for a partition.
func (t *StorageTracker) Used(key StorageKey, q policy.StorageQuota) int64 {
	if t == nil {
		return 0
	}
	return totalFor(t.state.snapshot(), strings.TrimSpace(key.Tenant), normalizeStoragePart(key.Partition),
		t.now(), storageRetention(q))
}

// Partitions lists partitions with tracked storage.
func (t *StorageTracker) Partitions() []StorageKey {
	if t == nil {
		return nil
	}
	return partitionKeys(t.state.snapshot())
}

// Flush persists buffered state. It is safe to call concurrently with Check.
func (t *StorageTracker) Flush(ctx context.Context) error {
	if t == nil || t.state == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.flush(ctx)
}

// Close flushes and releases the state backend.
func (t *StorageTracker) Close(ctx context.Context) error {
	if t == nil || t.state == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.state.flush(ctx); err != nil {
		_ = t.state.close(ctx)
		return err
	}
	return t.state.close(ctx)
}

// Run flushes durable state periodically until the context is canceled.
func (t *StorageTracker) Run(ctx context.Context, interval time.Duration) {
	if t == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := t.Close(closeCtx); err != nil && t.logger != nil {
				t.logger.Printf("quota: flush storage state on shutdown: %v", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := t.Flush(ctx); err != nil && t.logger != nil {
				t.logger.Printf("quota: flush storage state: %v", err)
			}
		}
	}
}
