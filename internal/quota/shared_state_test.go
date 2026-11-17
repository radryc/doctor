package quota

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/store"
)

type memoryFile struct {
	content []byte
	version string
	seq     int
}

type memoryCASStore struct {
	mu        sync.Mutex
	files     map[string]memoryFile
	seq       int
	conflicts int
	listErr   error
}

func newMemoryCASStore() *memoryCASStore {
	return &memoryCASStore{files: map[string]memoryFile{}}
}

func (m *memoryCASStore) ReadVersion(_ context.Context, path string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[path]
	if !ok {
		return nil, "", os.ErrNotExist
	}
	return append([]byte(nil), file.content...), file.version, nil
}

func (m *memoryCASStore) WriteCAS(_ context.Context, path string, content []byte, expectedVersion string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conflicts > 0 {
		m.conflicts--
		return store.ErrConflict
	}
	current := m.files[path]
	if current.version != expectedVersion {
		return store.ErrConflict
	}
	m.seq++
	m.files[path] = memoryFile{content: append([]byte(nil), content...), version: "v" + string(rune('0'+m.seq%10)), seq: m.seq}
	return nil
}

func (m *memoryCASStore) ListDir(_ context.Context, dir string) ([]store.DirEntry, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]store.DirEntry{}
	prefix := strings.TrimRight(dir, "/") + "/"
	for file := range m.files {
		if !strings.HasPrefix(file, prefix) {
			continue
		}
		rest := strings.TrimPrefix(file, prefix)
		if rest == "" {
			continue
		}
		if index := strings.Index(rest, "/"); index >= 0 {
			name := rest[:index]
			seen[name] = store.DirEntry{Name: name, IsDir: true}
		} else {
			seen[rest] = store.DirEntry{Name: rest}
		}
	}
	out := make([]store.DirEntry, 0, len(seen))
	for _, entry := range seen {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func TestSharedStateReservesBlocksAndReturnsUnused(t *testing.T) {
	ctx := context.Background()
	store := newMemoryCASStore()
	state := newSharedState(store, "/state", "replica-a", 100, "closed")
	tracker, err := NewStorageTrackerWithState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	q := storageQuota(250, "720h")
	if err := tracker.Check(ctx, "tenant-a", "edge", 60, q); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 30, q); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 20, q); err != nil {
		t.Fatal(err)
	}
	if used := tracker.Used(StorageKey{Tenant: "tenant-a", Partition: "edge"}, q); used != 200 {
		t.Fatalf("used after first reservations = %d, want 200", used)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 60, q); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("over-limit error = %v", err)
	}
	if err := tracker.Check(ctx, "tenant-a", "edge", 50, q); err != nil {
		t.Fatalf("admit up to limit: %v", err)
	}
	if err := tracker.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	content, _, err := store.ReadVersion(ctx, "/state/tenant-a/edge/"+time.Now().UTC().Format("2006-01-02")+".json")
	if err != nil {
		t.Fatalf("read shared state: %v", err)
	}
	var doc sharedDoc
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Bytes != 160 {
		t.Fatalf("shared bytes after close = %d, want 160 consumed bytes", doc.Bytes)
	}
}

func TestSharedStateCASConflictRetries(t *testing.T) {
	ctx := context.Background()
	store := newMemoryCASStore()
	store.conflicts = 2
	state := newSharedState(store, "/state", "replica-a", 100, "closed")
	tracker, err := NewStorageTrackerWithState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tracker.Close(ctx) }()
	q := storageQuota(1000, "720h")
	if err := tracker.Check(ctx, "tenant-a", "edge", 60, q); err != nil {
		t.Fatalf("Check after CAS conflicts: %v", err)
	}
}

func TestSharedStateLoadsPersistedTotals(t *testing.T) {
	ctx := context.Background()
	store := newMemoryCASStore()
	day := time.Now().UTC().Format("2006-01-02")
	doc := sharedDoc{Tenant: "tenant-a", Partition: "edge", Day: day, Bytes: 120}
	content, _ := json.Marshal(doc)
	path := "/state/tenant-a/edge/" + day + ".json"
	store.files[path] = memoryFile{content: content, version: "v1"}

	state := newSharedState(store, "/state", "replica-b", 100, "closed")
	tracker, err := NewStorageTrackerWithState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tracker.Close(ctx) }()
	q := storageQuota(500, "720h")
	if used := tracker.Used(StorageKey{Tenant: "tenant-a", Partition: "edge"}, q); used != 120 {
		t.Fatalf("loaded used = %d, want 120", used)
	}
}

func TestSharedStateTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store := newMemoryCASStore()
	state := newSharedState(store, "/state", "replica-a", 100, "closed")
	tracker, err := NewStorageTrackerWithState(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tracker.Close(ctx) }()
	q := storageQuota(100, "720h")
	if err := tracker.Check(ctx, "tenant-a", "shared", 60, q); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Check(ctx, "tenant-b", "shared", 60, q); err != nil {
		t.Fatalf("tenant-b should not share tenant-a's counter: %v", err)
	}
}

func TestSharedStateFailMode(t *testing.T) {
	ctx := context.Background()
	store := newMemoryCASStore()
	store.listErr = errors.New("store unavailable")
	closed := newSharedState(store, "/state", "replica-a", 100, "closed")
	if err := closed.load(ctx); err == nil {
		t.Fatal("expected closed fail mode to return the load error")
	}
	open := newSharedState(store, "/state", "replica-a", 100, "open")
	if err := open.load(ctx); err != nil {
		t.Fatalf("open fail mode should tolerate a load error: %v", err)
	}
}
