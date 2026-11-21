package quota

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/radryc/monofs/pkg/authz/policy"
)

// StorageKey identifies one partition's storage quota. Tenant is included so
// identical partition names in different tenants never share a counter.
type StorageKey struct {
	Tenant    string `json:"tenant"`
	Partition string `json:"partition"`
}

type stateKey struct {
	Tenant    string `json:"tenant"`
	Partition string `json:"partition"`
	Day       string `json:"day"`
}

func (k stateKey) storageKey() StorageKey {
	return StorageKey{Tenant: k.Tenant, Partition: k.Partition}
}

// stateBackend persists and exposes accepted bytes. Implementations must make
// snapshot() cheap enough for the ingest hot path (an in-memory sum is fine).
type stateBackend interface {
	load(ctx context.Context) error
	snapshot() map[stateKey]int64
	add(ctx context.Context, key stateKey, delta int64) error
	flush(ctx context.Context) error
	close(ctx context.Context) error
	describe() string
}

// memoryState is the pre-durable behavior: a process-local map.
type memoryState struct {
	mu      sync.Mutex
	entries map[stateKey]int64
}

func newMemoryState() *memoryState {
	return &memoryState{entries: map[stateKey]int64{}}
}

func (m *memoryState) load(context.Context) error { return nil }

func (m *memoryState) snapshot() map[stateKey]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[stateKey]int64, len(m.entries))
	for key, value := range m.entries {
		out[key] = value
	}
	return out
}

func (m *memoryState) add(_ context.Context, key stateKey, delta int64) error {
	m.mu.Lock()
	m.entries[key] += delta
	m.mu.Unlock()
	return nil
}

func (m *memoryState) flush(context.Context) error { return nil }
func (m *memoryState) close(context.Context) error { return nil }
func (m *memoryState) describe() string            { return "memory" }

func normalizeStoragePart(part string) string {
	part = strings.TrimSpace(part)
	if part == "" {
		return UnknownPartition
	}
	return part
}

func dayOf(now time.Time) string {
	return now.UTC().Format("2006-01-02")
}

// totalFor sums the days inside the retention window for one partition.
func totalFor(entries map[stateKey]int64, tenant, partition string, now time.Time, retention time.Duration) int64 {
	cutoff := now.Add(-retention)
	total := int64(0)
	for key, value := range entries {
		if key.Tenant != tenant || key.Partition != partition {
			continue
		}
		day, err := time.Parse("2006-01-02", key.Day)
		if err != nil || day.Before(cutoff) {
			continue
		}
		total += value
	}
	return total
}

// partitionKeys returns the distinct tenant/partition pairs in the state.
func partitionKeys(entries map[stateKey]int64) []StorageKey {
	seen := make(map[StorageKey]struct{})
	out := make([]StorageKey, 0)
	for key := range entries {
		storageKey := key.storageKey()
		if _, ok := seen[storageKey]; ok {
			continue
		}
		seen[storageKey] = struct{}{}
		out = append(out, storageKey)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Partition < out[j].Partition
	})
	return out
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
