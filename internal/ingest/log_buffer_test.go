package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/doctor/internal/wal"
)

type logBufferTestWriter struct {
	logCallSizes []int
}

func (w *logBufferTestWriter) WriteTraces(context.Context, []segment.TraceRecord) error {
	return nil
}

func (w *logBufferTestWriter) WriteLogs(_ context.Context, records []segment.LogRecord) error {
	w.logCallSizes = append(w.logCallSizes, len(records))
	return nil
}

func (w *logBufferTestWriter) WriteMetrics(context.Context, []segment.MetricPointRecord) error {
	return nil
}

func (w *logBufferTestWriter) WriteGuardianEvents(context.Context, []segment.GuardianEvent) error {
	return nil
}

func testLogRecord(body string) segment.LogRecord {
	return segment.LogRecord{
		Tenant:       "default",
		Service:      "doctor-query",
		SeverityText: "INFO",
		Body:         body,
		Timestamp:    time.Unix(0, 1).UTC(),
	}
}

func TestLogBufferFlushKeepsInflightReservations(t *testing.T) {
	log, err := wal.NewLogBatchLog(filepath.Join(t.TempDir(), "log-wal"))
	if err != nil {
		t.Fatalf("NewLogBatchLog() error = %v", err)
	}
	buffer := NewLogBuffer(&logBufferTestWriter{}, log, LogBufferConfig{})

	staged := testLogRecord("staged")
	inFlight := testLogRecord("in-flight-but-not-yet-inserted")
	stagedBytes := estimateLogRecordSize(staged)
	inFlightBytes := estimateLogRecordSize(inFlight)

	buffer.mu.Lock()
	buffer.batches = []wal.LogBatch{{ID: "a", Records: []segment.LogRecord{staged}}}
	// byteCount contains the staged batch plus a reservation made by a
	// concurrent Append that has not inserted its batch yet.
	buffer.byteCount = stagedBytes + inFlightBytes
	batches, flushedBytes := buffer.takeFlushBatchesLocked()
	buffer.mu.Unlock()

	if len(batches) != 1 || flushedBytes != stagedBytes {
		t.Fatalf("takeFlushBatchesLocked() = %d batches, %d bytes; want 1, %d", len(batches), flushedBytes, stagedBytes)
	}
	if buffer.byteCount != inFlightBytes {
		t.Fatalf("byteCount after take = %d, want in-flight reservation %d", buffer.byteCount, inFlightBytes)
	}
}

func TestLogBufferReplayFlushesIncrementally(t *testing.T) {
	writer := &logBufferTestWriter{}
	log, err := wal.NewLogBatchLog(filepath.Join(t.TempDir(), "log-wal"))
	if err != nil {
		t.Fatalf("NewLogBatchLog() error = %v", err)
	}

	records := []segment.LogRecord{
		testLogRecord("record-a"),
		testLogRecord("record-b"),
		testLogRecord("record-c"),
	}
	for _, record := range records {
		if _, err := log.Append(wal.LogBatch{Records: []segment.LogRecord{record}}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	buffer := NewLogBuffer(writer, log, LogBufferConfig{
		FlushInterval: time.Second,
		MaxBytes:      estimateLogRecordSize(records[0]) + 1,
	})

	if err := buffer.Replay(context.Background()); err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := writer.logCallSizes; len(got) != 3 || got[0] != 1 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("Replay() flushed %+v, want [1 1 1]", got)
	}
	if got := log.Count(); got != 0 {
		t.Fatalf("log wal count after replay = %d, want 0", got)
	}
}
