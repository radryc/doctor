package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/wal"
)

type metricBufferTestWriter struct {
	metricCallSizes []int
}

func (w *metricBufferTestWriter) WriteTraces(context.Context, []segment.TraceRecord) error {
	return nil
}

func (w *metricBufferTestWriter) WriteLogs(context.Context, []segment.LogRecord) error {
	return nil
}

func (w *metricBufferTestWriter) WriteMetrics(_ context.Context, records []segment.MetricPointRecord) error {
	w.metricCallSizes = append(w.metricCallSizes, len(records))
	return nil
}

func (w *metricBufferTestWriter) WriteGuardianEvents(context.Context, []segment.GuardianEvent) error {
	return nil
}

func testMetricRecord(metricName string, value float64) segment.MetricPointRecord {
	return segment.MetricPointRecord{
		Tenant:     "default",
		Service:    "monofs-server",
		MetricName: metricName,
		Type:       "sum",
		Timestamp:  time.Unix(0, 1).UTC(),
		HasValue:   true,
		Value:      value,
	}
}

func TestMetricBufferFlushPendingChunksByMaxBytes(t *testing.T) {
	writer := &metricBufferTestWriter{}
	log, err := wal.NewMetricBatchLog(filepath.Join(t.TempDir(), "metric-wal"))
	if err != nil {
		t.Fatalf("NewMetricBatchLog() error = %v", err)
	}

	recordA := testMetricRecord("metric_a", 1)
	recordB := testMetricRecord("metric_b", 2)
	maxBytes := estimateMetricRecordSize(recordA) + 1
	buffer := NewMetricBuffer(writer, log, MetricBufferConfig{
		FlushInterval: time.Second,
		MaxBytes:      maxBytes,
	})

	if err := buffer.Append(context.Background(), []segment.MetricPointRecord{recordA}); err != nil {
		t.Fatalf("Append(recordA) error = %v", err)
	}
	if err := buffer.Append(context.Background(), []segment.MetricPointRecord{recordB}); err != nil {
		t.Fatalf("Append(recordB) error = %v", err)
	}

	if err := buffer.flushPending(context.Background()); err != nil {
		t.Fatalf("flushPending(first) error = %v", err)
	}
	if len(writer.metricCallSizes) != 1 || writer.metricCallSizes[0] != 1 {
		t.Fatalf("first flush wrote %+v, want [1]", writer.metricCallSizes)
	}
	if got := log.Count(); got != 1 {
		t.Fatalf("metric wal count after first flush = %d, want 1", got)
	}

	if err := buffer.flushPending(context.Background()); err != nil {
		t.Fatalf("flushPending(second) error = %v", err)
	}
	if len(writer.metricCallSizes) != 2 || writer.metricCallSizes[1] != 1 {
		t.Fatalf("second flush wrote %+v, want [1 1]", writer.metricCallSizes)
	}
	if got := log.Count(); got != 0 {
		t.Fatalf("metric wal count after second flush = %d, want 0", got)
	}
}

func TestMetricBufferReplayFlushesIncrementally(t *testing.T) {
	writer := &metricBufferTestWriter{}
	log, err := wal.NewMetricBatchLog(filepath.Join(t.TempDir(), "metric-wal"))
	if err != nil {
		t.Fatalf("NewMetricBatchLog() error = %v", err)
	}

	records := []segment.MetricPointRecord{
		testMetricRecord("metric_a", 1),
		testMetricRecord("metric_b", 2),
		testMetricRecord("metric_c", 3),
	}
	for _, record := range records {
		if _, err := log.Append(wal.MetricBatch{Records: []segment.MetricPointRecord{record}}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	buffer := NewMetricBuffer(writer, log, MetricBufferConfig{
		FlushInterval: time.Second,
		MaxBytes:      estimateMetricRecordSize(records[0]) + 1,
	})

	if err := buffer.Replay(context.Background()); err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := writer.metricCallSizes; len(got) != 3 || got[0] != 1 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("Replay() flushed %+v, want [1 1 1]", got)
	}
	if got := log.Count(); got != 0 {
		t.Fatalf("metric wal count after replay = %d, want 0", got)
	}
}

func TestMetricBufferReplaySplitsSingleOversizedBatch(t *testing.T) {
	writer := &metricBufferTestWriter{}
	log, err := wal.NewMetricBatchLog(filepath.Join(t.TempDir(), "metric-wal"))
	if err != nil {
		t.Fatalf("NewMetricBatchLog() error = %v", err)
	}

	records := []segment.MetricPointRecord{
		testMetricRecord("metric_a", 1),
		testMetricRecord("metric_b", 2),
		testMetricRecord("metric_c", 3),
	}
	if _, err := log.Append(wal.MetricBatch{Records: records}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	buffer := NewMetricBuffer(writer, log, MetricBufferConfig{
		FlushInterval: time.Second,
		MaxBytes:      (estimateMetricRecordSize(records[0]) + 1) * 2,
	})

	if err := buffer.Replay(context.Background()); err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if got := writer.metricCallSizes; len(got) != 3 || got[0] != 1 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("Replay() flushed %+v, want [1 1 1]", got)
	}
	if got := log.Count(); got != 0 {
		t.Fatalf("metric wal count after replay = %d, want 0", got)
	}
}

