package wal_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/doctor/internal/wal"
)

func TestLogBatchLogConcurrentAppendsAreAllDurable(t *testing.T) {
	log, err := wal.NewLogBatchLog(filepath.Join(t.TempDir(), "log-wal"))
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 8
	const perGoroutine = 25
	ids := make(chan string, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				id, appendErr := log.Append(wal.LogBatch{
					Records: []segment.LogRecord{{
						Tenant:    "default",
						Service:   "bench",
						Body:      fmt.Sprintf("g=%d i=%d", g, i),
						Timestamp: time.Unix(0, int64(i)).UTC(),
					}},
				})
				if appendErr != nil {
					t.Errorf("Append() error = %v", appendErr)
					return
				}
				ids <- id
			}
		}(g)
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, goroutines*perGoroutine)
	for id := range ids {
		if id == "" {
			t.Fatal("Append() returned an empty ID")
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate batch ID %q", id)
		}
		seen[id] = struct{}{}
	}

	batches, err := log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != goroutines*perGoroutine {
		t.Fatalf("ReadAll() returned %d batches, want %d", len(batches), goroutines*perGoroutine)
	}
}

func TestLogBatchLogRetirePersistsCheckpoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log-wal")
	log, err := wal.NewLogBatchLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := log.Append(wal.LogBatch{ID: fmt.Sprintf("batch-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	batches, err := log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Retire(batches[1:]); err != nil {
		t.Fatal(err)
	}
	got, err := log.ReadAll()
	if err != nil || len(got) != 2 || got[0].ID != "batch-1" {
		t.Fatalf("ReadAll() after Retire = %+v err=%v", got, err)
	}

	// The checkpoint must survive a restart.
	reopened, err := wal.NewLogBatchLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err = reopened.ReadAll()
	if err != nil || len(got) != 2 {
		t.Fatalf("reopened ReadAll() = %+v err=%v", got, err)
	}
	if _, err := reopened.Append(wal.LogBatch{ID: "batch-3"}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Retire(nil); err != nil {
		t.Fatal(err)
	}
	again, err := wal.NewLogBatchLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err = again.ReadAll()
	if err != nil || len(got) != 0 {
		t.Fatalf("ReadAll() after Retire(nil) = %+v err=%v", got, err)
	}
}

func TestLogBatchLogAppendAfterReplace(t *testing.T) {
	log, err := wal.NewLogBatchLog(filepath.Join(t.TempDir(), "log-wal"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := log.Append(wal.LogBatch{ID: fmt.Sprintf("batch-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	batches, err := log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Replace(batches[1:]); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(wal.LogBatch{ID: "after-replace"}); err != nil {
		t.Fatal(err)
	}

	batches, err = log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 {
		t.Fatalf("ReadAll() returned %d batches, want 3", len(batches))
	}
	if batches[2].ID != "after-replace" {
		t.Fatalf("last batch ID = %q, want after-replace", batches[2].ID)
	}
}
