package ingest

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricapi "go.opentelemetry.io/otel/metric"
)

type BufferMetricsHandle struct {
	registration metricapi.Registration
}

func RegisterBufferMetrics(traceBuffer *TraceBuffer, logBuffer *LogBuffer, metricBuffer *MetricBuffer) (*BufferMetricsHandle, error) {
	if traceBuffer == nil && logBuffer == nil && metricBuffer == nil {
		return nil, nil
	}

	meter := otel.Meter("doctor/ingest")
	pendingBatches, err := meter.Int64ObservableGauge(
		"doctor.ingest.buffer.pending.batches",
		metricapi.WithDescription("Current number of buffered batches waiting to flush, by signal."),
	)
	if err != nil {
		return nil, err
	}
	pendingBytes, err := meter.Int64ObservableGauge(
		"doctor.ingest.buffer.pending.bytes",
		metricapi.WithDescription("Current estimated buffered bytes waiting to flush, by signal."),
	)
	if err != nil {
		return nil, err
	}
	pendingRecords, err := meter.Int64ObservableGauge(
		"doctor.ingest.buffer.pending.records",
		metricapi.WithDescription("Current buffered record count waiting to flush, for trace buffers."),
	)
	if err != nil {
		return nil, err
	}
	flushes, err := meter.Int64ObservableCounter(
		"doctor.ingest.buffer.flushes",
		metricapi.WithDescription("Total completed buffer flushes, by signal."),
	)
	if err != nil {
		return nil, err
	}

	registration, err := meter.RegisterCallback(func(_ context.Context, observer metricapi.Observer) error {
		if traceBuffer != nil {
			stats := traceBuffer.Stats()
			observeBufferStats(observer, pendingBatches, pendingBytes, flushes, "traces", stats.PendingBatches, stats.PendingBytes, stats.Flushes)
			observer.ObserveInt64(pendingRecords, stats.PendingRecords, metricapi.WithAttributes(attribute.String("signal", "traces")))
		}
		if logBuffer != nil {
			stats := logBuffer.Stats()
			observeBufferStats(observer, pendingBatches, pendingBytes, flushes, "logs", stats.PendingBatches, stats.PendingBytes, stats.Flushes)
		}
		if metricBuffer != nil {
			stats := metricBuffer.Stats()
			observeBufferStats(observer, pendingBatches, pendingBytes, flushes, "metrics", stats.PendingBatches, stats.PendingBytes, stats.Flushes)
		}
		return nil
	}, pendingBatches, pendingBytes, pendingRecords, flushes)
	if err != nil {
		return nil, err
	}

	return &BufferMetricsHandle{registration: registration}, nil
}

func (h *BufferMetricsHandle) Unregister() error {
	if h == nil || h.registration == nil {
		return nil
	}
	return h.registration.Unregister()
}

func observeBufferStats(
	observer metricapi.Observer,
	pendingBatches metricapi.Int64ObservableGauge,
	pendingBytes metricapi.Int64ObservableGauge,
	flushes metricapi.Int64ObservableCounter,
	signal string,
	batches int64,
	bytes int64,
	flushCount int64,
) {
	attrs := metricapi.WithAttributes(attribute.String("signal", signal))
	observer.ObserveInt64(pendingBatches, batches, attrs)
	observer.ObserveInt64(pendingBytes, bytes, attrs)
	observer.ObserveInt64(flushes, flushCount, attrs)
}
