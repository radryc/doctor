package federation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rydzu/ainfra/doctor/internal/ruler"
)

type countingFileStore struct {
	*ruler.MemoryStore
	mu    sync.Mutex
	reads []string
}

func (s *countingFileStore) ReadFile(ctx context.Context, logicalPath string) ([]byte, error) {
	s.mu.Lock()
	s.reads = append(s.reads, logicalPath)
	s.mu.Unlock()
	return s.MemoryStore.ReadFile(ctx, logicalPath)
}

func (s *countingFileStore) resetReads() {
	s.mu.Lock()
	s.reads = nil
	s.mu.Unlock()
}

func (s *countingFileStore) shardReads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, path := range s.reads {
		if strings.Contains(path, "/shards/") {
			out = append(out, path)
		}
	}
	return out
}

func TestMonoFSSnapshotStoreManyShardsRoundTrip(t *testing.T) {
	files := ruler.NewMemoryStore()
	store := NewMonoFSSnapshotStore(files, "", "/doctor/v1/federation", "default")
	store.ShardTargets = 10
	store.ShardWorkers = 8

	const count = 1000
	targets := make([]Target, 0, count)
	for i := 0; i < count; i++ {
		targets = append(targets, Target{
			ID:       fmt.Sprintf("aws-%04d", i),
			Account:  fmt.Sprintf("%012d", i),
			Region:   "us-east-1",
			Endpoint: fmt.Sprintf("http://pusher-%d:19090", i),
			Token:    "token",
		})
	}
	version, err := store.Save(context.Background(), Snapshot{Version: "v1", Targets: targets}, "")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if version != "v1" {
		t.Fatalf("version = %q, want v1", version)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Targets) != count {
		t.Fatalf("loaded %d targets, want %d", len(loaded.Targets), count)
	}
	for index, target := range loaded.Targets {
		if target.ID != targets[index].ID {
			t.Fatalf("target[%d] = %q, want %q", index, target.ID, targets[index].ID)
		}
	}
}

func TestMonoFSSnapshotStoreSaveExpectedVersionReadsOnlyManifest(t *testing.T) {
	files := &countingFileStore{MemoryStore: ruler.NewMemoryStore()}
	store := NewMonoFSSnapshotStore(files, "", "/doctor/v1/federation", "default")
	store.ShardTargets = 2
	store.ShardWorkers = 4

	snapshot := testSnapshot("v1")
	if _, err := store.Save(context.Background(), snapshot, ""); err != nil {
		t.Fatalf("Save(v1) error = %v", err)
	}
	files.resetReads()
	if _, err := store.Save(context.Background(), testSnapshot("v2"), "v1"); err != nil {
		t.Fatalf("Save(v2) error = %v", err)
	}
	if reads := files.shardReads(); len(reads) != 0 {
		t.Fatalf("Save(v2) read shards: %v", reads)
	}
}
