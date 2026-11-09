package wal

import (
	"path/filepath"
	"testing"
)

func TestRetireCompactionPreservesLiveTail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "log-wal")
	log, err := NewLogBatchLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Force compaction as soon as any dead prefix exists.
	log.log.maxRetained = 1

	for _, id := range []string{"batch-0", "batch-1", "batch-2", "batch-3"} {
		if _, err := log.Append(LogBatch{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	batches, err := log.ReadAll()
	if err != nil || len(batches) != 4 {
		t.Fatalf("ReadAll() = %+v err=%v", batches, err)
	}

	// Retire the first two; the live tail starts at batch-2. batch-3 is not in
	// the retire list but must still be copied into the compacted file.
	if err := log.Retire(batches[2:3]); err != nil {
		t.Fatal(err)
	}
	got, err := log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "batch-2" || got[1].ID != "batch-3" {
		t.Fatalf("ReadAll() after compaction = %+v", got)
	}

	reopened, err := NewLogBatchLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err = reopened.ReadAll()
	if err != nil || len(got) != 2 || got[0].ID != "batch-2" {
		t.Fatalf("reopened ReadAll() = %+v err=%v", got, err)
	}
}
