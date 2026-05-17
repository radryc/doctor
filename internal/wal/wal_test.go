package wal_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/wal"
)

func TestStageAndCommit(t *testing.T) {
	w, err := wal.New(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	entry := wal.Entry{
		Signal:    "traces",
		Tenant:    "tenant-a",
		ObjectKey: "traces/tenant=tenant-a/date=2026-04-07/hour=10/segment-1.json.gz",
		Content:   []byte("test-content"),
		Manifest:  []byte(`{"id":"1"}`),
	}

	id, err := w.Stage(entry)
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}
	if id == "" {
		t.Fatal("Stage() returned empty ID")
	}

	pending, err := w.Pending()
	if err != nil {
		t.Fatalf("Pending() error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	if pending[0].ObjectKey != entry.ObjectKey {
		t.Fatalf("expected object key %s, got %s", entry.ObjectKey, pending[0].ObjectKey)
	}

	if err := w.Commit(id); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	pending, err = w.Pending()
	if err != nil {
		t.Fatalf("Pending() after commit error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after commit, got %d", len(pending))
	}
}

func TestPendingOrderedByStagedTime(t *testing.T) {
	w, err := wal.New(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for i := 0; i < 3; i++ {
		_, err := w.Stage(wal.Entry{
			Signal:  "traces",
			Tenant:  "tenant-a",
			Content: []byte("content"),
		})
		if err != nil {
			t.Fatalf("Stage() error = %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	pending, err := w.Pending()
	if err != nil {
		t.Fatalf("Pending() error = %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 pending, got %d", len(pending))
	}
	for i := 1; i < len(pending); i++ {
		if pending[i].StagedAt.Before(pending[i-1].StagedAt) {
			t.Fatalf("pending entries not sorted by staged time")
		}
	}
}

func TestCount(t *testing.T) {
	w, err := wal.New(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if w.Count() != 0 {
		t.Fatalf("expected 0, got %d", w.Count())
	}

	id, _ := w.Stage(wal.Entry{Signal: "traces", Content: []byte("c")})
	if w.Count() != 1 {
		t.Fatalf("expected 1, got %d", w.Count())
	}

	_ = w.Commit(id)
	if w.Count() != 0 {
		t.Fatalf("expected 0 after commit, got %d", w.Count())
	}
}
