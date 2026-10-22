package federation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rydzu/ainfra/doctor/internal/ruler"
	"github.com/rydzu/ainfra/doctor/internal/store"
)

func testSnapshot(version string) Snapshot {
	return Snapshot{
		Version: version,
		Targets: []Target{
			{ID: "aws-a", Account: "111111111111", Region: "us-east-1", Partitions: []string{"div-a"}, Endpoint: "http://a", Token: "a"},
			{ID: "aws-b", Account: "222222222222", Region: "us-east-1", Partitions: []string{"div-b"}, Endpoint: "http://b", Token: "b"},
			{ID: "aws-c", Account: "333333333333", Region: "us-west-2", Partitions: []string{"div-c"}, Endpoint: "http://c", Token: "c"},
		},
	}
}

func TestMonoFSSnapshotStoreRoundTripAndCAS(t *testing.T) {
	files := ruler.NewMemoryStore()
	snapshotStore := NewMonoFSSnapshotStore(files, "", "/doctor/v1/federation", "default")
	snapshotStore.ShardTargets = 1

	version, err := snapshotStore.Save(context.Background(), testSnapshot("v1"), "")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if version != "v1" {
		t.Fatalf("version = %q, want v1", version)
	}

	loaded, err := snapshotStore.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Version != "v1" || len(loaded.Targets) != 3 {
		t.Fatalf("loaded = %#v", loaded)
	}

	// The manifest is the commit point and should reference three shards.
	manifest, err := files.ReadFile(context.Background(), "/doctor/v1/federation/default/manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var parsed Manifest
	if err := json.Unmarshal(manifest, &parsed); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(parsed.Shards) != 3 {
		t.Fatalf("shards = %d, want 3", len(parsed.Shards))
	}

	// Correct CAS succeeds.
	if _, err := snapshotStore.Save(context.Background(), testSnapshot("v2"), "v1"); err != nil {
		t.Fatalf("Save(v2) error = %v", err)
	}
	loaded, err = snapshotStore.Load(context.Background())
	if err != nil || loaded.Version != "v2" {
		t.Fatalf("Load(v2) = %#v err=%v", loaded, err)
	}

	// Wrong CAS fails.
	if _, err := snapshotStore.Save(context.Background(), testSnapshot("v3"), "v1"); err == nil {
		t.Fatal("expected version conflict")
	}
}

func TestMonoFSSnapshotStoreMissingAndCorrupt(t *testing.T) {
	files := ruler.NewMemoryStore()
	snapshotStore := NewMonoFSSnapshotStore(files, "", "/doctor/v1/federation", "default")
	if _, err := snapshotStore.Load(context.Background()); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("Load() error = %v, want ErrSnapshotNotFound", err)
	}

	if _, err := snapshotStore.Save(context.Background(), testSnapshot("v1"), ""); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	manifest, err := files.ReadFile(context.Background(), "/doctor/v1/federation/default/manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var parsed Manifest
	if err := json.Unmarshal(manifest, &parsed); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	// Corrupt the first shard.
	if len(parsed.Shards) == 0 {
		t.Fatal("manifest has no shards")
	}
	if _, err := files.UpsertPaths(context.Background(), "", []store.PathWrite{{
		LogicalPath:       parsed.Shards[0].Path,
		Content:           []byte(`{"targets":[]}`),
		ExpectedVersionID: "",
	}}, store.MutationContext{}); err != nil {
		t.Fatalf("corrupt shard: %v", err)
	}
	if _, err := snapshotStore.Load(context.Background()); err == nil {
		t.Fatal("expected checksum error")
	}
}

func TestFileSnapshotStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	snapshotStore := &FileSnapshotStore{Path: path}
	if _, err := snapshotStore.Load(context.Background()); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("Load() error = %v, want ErrSnapshotNotFound", err)
	}
	if _, err := snapshotStore.Save(context.Background(), testSnapshot("v1"), ""); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	loaded, err := snapshotStore.Load(context.Background())
	if err != nil || loaded.Version != "v1" || len(loaded.Targets) != 3 {
		t.Fatalf("Load() = %#v err=%v", loaded, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
}

func TestMemorySnapshotStoreCAS(t *testing.T) {
	snapshotStore := &MemorySnapshotStore{}
	if _, err := snapshotStore.Save(context.Background(), testSnapshot("v1"), ""); err != nil {
		t.Fatalf("Save(v1): %v", err)
	}
	if _, err := snapshotStore.Save(context.Background(), testSnapshot("v2"), "bad"); err == nil {
		t.Fatal("expected CAS conflict")
	}
	if _, err := snapshotStore.Save(context.Background(), testSnapshot("v2"), "v1"); err != nil {
		t.Fatalf("Save(v2): %v", err)
	}
	loaded, err := snapshotStore.Load(context.Background())
	if err != nil || loaded.Version != "v2" {
		t.Fatalf("Load() = %#v err=%v", loaded, err)
	}
}
