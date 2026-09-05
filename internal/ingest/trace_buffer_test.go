package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/wal"
)

type traceBufferTestWriter struct {
	traceCallSizes []int
}

func (w *traceBufferTestWriter) WriteTraces(_ context.Context, records []segment.TraceRecord) error {
	w.traceCallSizes = append(w.traceCallSizes, len(records))
	return nil
}

func (w *traceBufferTestWriter) WriteLogs(context.Context, []segment.LogRecord) error {
	return nil
}

func (w *traceBufferTestWriter) WriteMetrics(context.Context, []segment.MetricPointRecord) error {
	return nil
}

func (w *traceBufferTestWriter) WriteGuardianEvents(context.Context, []segment.GuardianEvent) error {
	return nil
}

func testTraceRecord(traceID string) segment.TraceRecord {
	base := time.Unix(0, 1).UTC()
	return segment.TraceRecord{
		Tenant:    "default",
		TraceID:   traceID,
		SpanID:    traceID + "-span",
		Name:      "GET /health",
		Service:   "doctor-query",
		StartTime: base,
		EndTime:   base.Add(10 * time.Millisecond),
	}
}

func TestTraceBufferReplayFlushesIncrementally(t *testing.T) {
	writer := &traceBufferTestWriter{}
	log, err := wal.NewTraceBatchLog(filepath.Join(t.TempDir(), "trace-wal"))
	if err != nil {
		t.Fatalf("NewTraceBatchLog() error = %v", err)
	}

	records := []segment.TraceRecord{
		testTraceRecord("trace-a"),
		testTraceRecord("trace-b"),
		testTraceRecord("trace-c"),
	}
	for _, record := range records {
		if _, err := log.Append(wal.TraceBatch{Records: []segment.TraceRecord{record}}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	buffer := NewTraceBuffer(writer, log, TraceBufferConfig{
		FlushInterval: time.Second,
		MaxRecords:    1,
		MaxBytes:      1 << 20,
	})

	if err := buffer.Replay(context.Background()); err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := writer.traceCallSizes; len(got) != 3 || got[0] != 1 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("Replay() flushed %+v, want [1 1 1]", got)
	}
	if got := log.Count(); got != 0 {
		t.Fatalf("trace wal count after replay = %d, want 0", got)
	}
}

