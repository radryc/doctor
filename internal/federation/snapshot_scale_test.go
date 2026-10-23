package federation

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestMonoFSSnapshotStore300ShardsBudget is opt-in because it writes the
// equivalent of a 150k-target catalog (300 shards at 500 targets each).
func TestMonoFSSnapshotStore300ShardsBudget(t *testing.T) {
	if os.Getenv("DOCTOR_LOADTEST") != "1" {
		t.Skip("set DOCTOR_LOADTEST=1 to run the 300-shard snapshot budget test")
	}
	files := ruler.NewMemoryStore()
	store := NewMonoFSSnapshotStore(files, "", "/doctor/v1/federation", "default")
	store.ShardTargets = 500
	store.ShardWorkers = 16

	start := time.Now()
	if _, err := store.Save(context.Background(), Snapshot{Version: "v1", Targets: scaleTargets(150000)}, ""); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	saveElapsed := time.Since(start)
	start = time.Now()
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	loadElapsed := time.Since(start)
	t.Logf("300-shard save = %s, load = %s", saveElapsed, loadElapsed)
	if len(loaded.Targets) != 150000 {
		t.Fatalf("loaded %d targets, want 150000", len(loaded.Targets))
	}
	if loadElapsed > 10*time.Second {
		t.Fatalf("300-shard load took %s, budget is 10s", loadElapsed)
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
