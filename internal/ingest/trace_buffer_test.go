package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/doctor/internal/wal"
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

func TestTraceBufferFlushKeepsInflightReservations(t *testing.T) {
	log, err := wal.NewTraceBatchLog(filepath.Join(t.TempDir(), "trace-wal"))
	if err != nil {
		t.Fatalf("NewTraceBatchLog() error = %v", err)
	}
	buffer := NewTraceBuffer(&traceBufferTestWriter{}, log, TraceBufferConfig{})

	staged := testTraceRecord("staged")
	inFlight := testTraceRecord("in-flight-but-not-yet-inserted")
	stagedBytes := estimateTraceRecordSize(staged)
	inFlightBytes := estimateTraceRecordSize(inFlight)

	buffer.mu.Lock()
	buffer.batches = []wal.TraceBatch{{ID: "a", Records: []segment.TraceRecord{staged}}}
	buffer.recordCount = 1
	// byteCount contains the staged batch plus a reservation made by a
	// concurrent Append that has not inserted its batch yet.
	buffer.byteCount = stagedBytes + inFlightBytes
	batches, flushedBytes, flushedRecords := buffer.takeFlushBatchesLocked()
	buffer.mu.Unlock()

	if len(batches) != 1 || flushedBytes != stagedBytes || flushedRecords != 1 {
		t.Fatalf("takeFlushBatchesLocked() = %d batches, %d bytes, %d records; want 1, %d, 1",
			len(batches), flushedBytes, flushedRecords, stagedBytes)
	}
	if buffer.byteCount != inFlightBytes {
		t.Fatalf("byteCount after take = %d, want in-flight reservation %d", buffer.byteCount, inFlightBytes)
	}
	if buffer.recordCount != 0 {
		t.Fatalf("recordCount after take = %d, want 0", buffer.recordCount)
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
