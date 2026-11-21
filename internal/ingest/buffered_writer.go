package ingest

import (
	"context"

	"github.com/radryc/doctor/internal/segment"
)

// BufferedWriter adapts the WAL-backed trace/log/metric buffers to a Flusher.
// It lets the ingest pipeline stage accepted records durably before MonoFS sees
// them: Submit* returns once the batch is in the WAL, and the buffer's
// background loop retries the storage write.
type BufferedWriter struct {
	fallback Flusher
	traces   *TraceBuffer
	logs     *LogBuffer
	metrics  *MetricBuffer
}

// NewBufferedWriter builds a buffered writer. Signals without a buffer fall
// through to fallback.
func NewBufferedWriter(fallback Flusher, traces *TraceBuffer, logs *LogBuffer, metrics *MetricBuffer) *BufferedWriter {
	return &BufferedWriter{fallback: fallback, traces: traces, logs: logs, metrics: metrics}
}

// WriteTraces implements Flusher.
func (w *BufferedWriter) WriteTraces(ctx context.Context, records []segment.TraceRecord) error {
	if w.traces != nil {
		return w.traces.Append(ctx, records)
	}
	return w.fallback.WriteTraces(ctx, records)
}

// WriteLogs implements Flusher.
func (w *BufferedWriter) WriteLogs(ctx context.Context, records []segment.LogRecord) error {
	if w.logs != nil {
		return w.logs.Append(ctx, records)
	}
	return w.fallback.WriteLogs(ctx, records)
}

// WriteMetrics implements Flusher.
func (w *BufferedWriter) WriteMetrics(ctx context.Context, records []segment.MetricPointRecord) error {
	if w.metrics != nil {
		return w.metrics.Append(ctx, records)
	}
	return w.fallback.WriteMetrics(ctx, records)
}

// WriteGuardianEvents implements Flusher. Guardian events have no WAL buffer.
func (w *BufferedWriter) WriteGuardianEvents(ctx context.Context, events []segment.GuardianEvent) error {
	return w.fallback.WriteGuardianEvents(ctx, events)
}
