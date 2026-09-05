package wal_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/wal"
)

func TestTraceBatchLogAppendReadReplace(t *testing.T) {
	log, err := wal.NewTraceBatchLog(filepath.Join(t.TempDir(), "trace-wal"))
	if err != nil {
		t.Fatalf("NewTraceBatchLog() error = %v", err)
	}

	firstID, err := log.Append(wal.TraceBatch{
		Records: []segment.TraceRecord{{
			Tenant:    "tenant-a",
			TraceID:   "trace-1",
			SpanID:    "span-1",
			Service:   "api",
			StartTime: time.Unix(0, 1000).UTC(),
			EndTime:   time.Unix(0, 1500).UTC(),
		}},
	})
	if err != nil {
		t.Fatalf("Append(first) error = %v", err)
	}
	secondID, err := log.Append(wal.TraceBatch{
		Records: []segment.TraceRecord{{
			Tenant:    "tenant-a",
			TraceID:   "trace-2",
			SpanID:    "span-2",
			Service:   "api",
			StartTime: time.Unix(0, 2000).UTC(),
			EndTime:   time.Unix(0, 2500).UTC(),
		}},
	})
	if err != nil {
		t.Fatalf("Append(second) error = %v", err)
	}

	batches, err := log.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(batches))
	}
	if batches[0].ID != firstID || batches[1].ID != secondID {
		t.Fatalf("unexpected batch IDs: %+v", batches)
	}

	if err := log.Replace(batches[1:]); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	batches, err = log.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll() after replace error = %v", err)
	}
	if len(batches) != 1 || batches[0].ID != secondID {
		t.Fatalf("unexpected batches after replace: %+v", batches)
	}
}
