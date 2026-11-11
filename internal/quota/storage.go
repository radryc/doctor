package quota

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/radryc/monofs/pkg/authz/policy"
)

// StorageTracker approximates per-partition stored bytes over the retention
// window. It is an ingest-side admission check, not a replacement for MonoFS
// compaction. A restart resets the window.
type StorageTracker struct {
	mu         sync.Mutex
	partitions map[string]*partitionStorage
	now        func() time.Time
}

type partitionStorage struct {
	bytes map[string]int64 // UTC day -> accepted bytes
}

// NewStorageTracker builds an empty tracker.
func NewStorageTracker() *StorageTracker {
	return &StorageTracker{
		partitions: map[string]*partitionStorage{},
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// Check admits bytes if the partition's rolling retention-window total stays
// under the configured maximum.
func (t *StorageTracker) Check(partition string, bytes int64, q policy.StorageQuota) error {
	if t == nil || q.MaxBytes <= 0 || bytes <= 0 {
		return nil
	}
	partition = strings.TrimSpace(partition)
	if partition == "" {
		partition = UnknownPartition
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ps := t.partitionLocked(partition)
	now := t.now()
	retention := storageRetention(q)
	t.expireLocked(ps, now, retention)
	total := int64(0)
	for _, value := range ps.bytes {
		total += value
	}
	if total+bytes > q.MaxBytes {
		return fmt.Errorf("%w: partition %s storage limit %d bytes", ErrQuotaExceeded, partition, q.MaxBytes)
	}
	ps.bytes[now.Format("2006-01-02")] += bytes
	return nil
}

// Used returns the current rolling-window total for a partition.
func (t *StorageTracker) Used(partition string, q policy.StorageQuota) int64 {
	if t == nil {
		return 0
	}
	partition = strings.TrimSpace(partition)
	t.mu.Lock()
	defer t.mu.Unlock()
	ps := t.partitions[partition]
	if ps == nil {
		return 0
	}
	t.expireLocked(ps, t.now(), storageRetention(q))
	total := int64(0)
	for _, value := range ps.bytes {
		total += value
	}
	return total
}

// Partitions lists partitions with tracked storage.
func (t *StorageTracker) Partitions() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.partitions))
	for partition, ps := range t.partitions {
		if len(ps.bytes) > 0 {
			out = append(out, partition)
		}
	}
	sort.Strings(out)
	return out
}

func (t *StorageTracker) partitionLocked(partition string) *partitionStorage {
	ps := t.partitions[partition]
	if ps == nil {
		ps = &partitionStorage{bytes: map[string]int64{}}
		t.partitions[partition] = ps
	}
	return ps
}

func (t *StorageTracker) expireLocked(ps *partitionStorage, now time.Time, retention time.Duration) {
	cutoff := now.Add(-retention)
	for day := range ps.bytes {
		parsed, err := time.Parse("2006-01-02", day)
		if err != nil || parsed.Before(cutoff) {
			delete(ps.bytes, day)
		}
	}
}

func storageRetention(q policy.StorageQuota) time.Duration {
	retention := 30 * 24 * time.Hour
	for _, raw := range []string{q.RetentionTraces, q.RetentionLogs, q.RetentionMetrics} {
		parsed, err := policy.ParseQuotaDuration(raw)
		if err == nil && parsed > retention {
			retention = parsed
		}
	}
	if retention > 400*24*time.Hour {
		retention = 400 * 24 * time.Hour
	}
	return retention
}
