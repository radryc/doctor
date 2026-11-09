package ingest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/doctor/internal/wal"
)

// LogBufferConfig controls log buffer behavior.
type LogBufferConfig struct {
	FlushInterval   time.Duration
	MaxBytes        int
	MaxPendingBytes int
}

func (c LogBufferConfig) withDefaults() LogBufferConfig {
	if c.FlushInterval <= 0 {
		c.FlushInterval = 5 * time.Minute
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 128 << 20 // 128 MiB
	}
	if c.MaxPendingBytes <= 0 {
		c.MaxPendingBytes = 3 * c.MaxBytes
	}
	return c
}

// LogBufferStats holds buffer telemetry.
type LogBufferStats struct {
	PendingBatches int64 `json:"pending_batches"`
	PendingBytes   int64 `json:"pending_bytes"`
	Flushes        int64 `json:"flushes"`
}

// LogBuffer accumulates log records in memory and WAL, then flushes them as
// a single large columnar object to S3 once the size threshold is reached or
// the flush interval elapses. This produces far fewer, much larger S3 objects
// compared to writing each ingest batch immediately.
type LogBuffer struct {
	writer Flusher
	log    *wal.LogBatchLog
	cfg    LogBufferConfig

	mu        sync.Mutex
	batches   []wal.LogBatch
	byteCount int
	flushing  bool
	triggerCh chan struct{}
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	flushes atomic.Int64
}

// NewLogBuffer creates a LogBuffer.
func NewLogBuffer(writer Flusher, log *wal.LogBatchLog, cfg LogBufferConfig) *LogBuffer {
	return &LogBuffer{
		writer:    writer,
		log:       log,
		cfg:       cfg.withDefaults(),
		triggerCh: make(chan struct{}, 1),
	}
}

// Start launches the background flush goroutine.
func (b *LogBuffer) Start(ctx context.Context) {
	ctx, b.cancel = context.WithCancel(ctx)
	b.wg.Add(1)
	go b.run(ctx)
}

// Stop flushes any pending records and stops the background goroutine.
func (b *LogBuffer) Stop(timeout time.Duration) {
	if b.cancel != nil {
		b.cancel()
	}
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Append stages log records in the WAL and memory buffer.
// Returns ErrBufferFull when pending bytes exceed MaxPendingBytes.
func (b *LogBuffer) Append(ctx context.Context, records []segment.LogRecord) error {
	if len(records) == 0 {
		return nil
	}
	totalBytes := 0
	for _, r := range records {
		totalBytes += estimateLogRecordSize(r)
	}

	b.mu.Lock()
	if b.byteCount > 0 && b.byteCount+totalBytes > b.cfg.MaxPendingBytes {
		b.mu.Unlock()
		return ErrBufferFull
	}
	b.byteCount += totalBytes
	b.mu.Unlock()

	batch := wal.LogBatch{Records: records}
	batchID, err := b.log.Append(batch)
	if err != nil {
		b.mu.Lock()
		b.byteCount -= totalBytes
		b.mu.Unlock()
		return err
	}
	batch.ID = batchID

	b.mu.Lock()
	b.batches = append(b.batches, batch)
	shouldFlush := b.byteCount >= b.cfg.MaxBytes
	b.mu.Unlock()
	if shouldFlush {
		b.signalFlush()
	}
	// The batch is already durably staged in the WAL. Reporting a late
	// cancellation here would make the caller retry data that was accepted and
	// produce duplicate records.
	return nil
}

// Replay flushes any batches recovered from the WAL after a crash.
func (b *LogBuffer) Replay(ctx context.Context) error {
	if b.log == nil {
		return nil
	}
	replayBatches := make([]wal.LogBatch, 0)
	replayBytes := 0
	flushReplay := func() error {
		if len(replayBatches) == 0 {
			return nil
		}
		if err := b.flushBatches(ctx, replayBatches); err != nil {
			return err
		}
		replayBatches = replayBatches[:0]
		replayBytes = 0
		return nil
	}

	err := b.log.Iterate(func(batch wal.LogBatch) error {
		batchBytes := estimateLogBatchSize(batch)
		if len(replayBatches) > 0 && replayBytes+batchBytes > b.cfg.MaxBytes {
			if err := flushReplay(); err != nil {
				return err
			}
		}

		replayBatches = append(replayBatches, batch)
		replayBytes += batchBytes
		if replayBytes >= b.cfg.MaxBytes {
			return flushReplay()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := flushReplay(); err != nil {
		return err
	}
	return b.log.Retire(nil)
}

// Stats returns current buffer statistics.
func (b *LogBuffer) Stats() LogBufferStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return LogBufferStats{
		PendingBatches: int64(len(b.batches)),
		PendingBytes:   int64(b.byteCount),
		Flushes:        b.flushes.Load(),
	}
}

func (b *LogBuffer) run(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = b.flushPending(context.Background())
			return
		case <-ticker.C:
			_ = b.flushPending(ctx)
		case <-b.triggerCh:
			_ = b.flushPending(ctx)
		}
	}
}

func (b *LogBuffer) signalFlush() {
	select {
	case b.triggerCh <- struct{}{}:
	default:
	}
}

func (b *LogBuffer) flushPending(ctx context.Context) error {
	b.mu.Lock()
	if b.flushing || len(b.batches) == 0 {
		b.mu.Unlock()
		return nil
	}
	b.flushing = true
	batches, flushedBytes := b.takeFlushBatchesLocked()
	b.mu.Unlock()

	err := b.flushBatches(ctx, batches)

	b.mu.Lock()
	b.flushing = false
	if err != nil {
		// Re-queue on failure; WAL still has the data. Only restore the bytes
		// owned by these batches: reservations made by concurrent Appends that
		// are still in WAL I/O must stay counted.
		b.batches = append(batches, b.batches...)
		b.byteCount += flushedBytes
		b.mu.Unlock()
		return err
	}

	remaining := append([]wal.LogBatch(nil), b.batches...)
	hasMore := len(remaining) > 0
	if err := b.log.Retire(remaining); err != nil {
		b.mu.Unlock()
		return err
	}
	b.mu.Unlock()
	if hasMore {
		b.signalFlush()
	}
	return nil
}

// takeFlushBatchesLocked moves every buffered batch out of the buffer and
// subtracts exactly the bytes reserved for those batches. byteCount may also
// include reservations for Appends that have not inserted their batch yet;
// those are intentionally left in place so a concurrent flush cannot erase
// their accounting.
func (b *LogBuffer) takeFlushBatchesLocked() ([]wal.LogBatch, int) {
	batches := append([]wal.LogBatch(nil), b.batches...)
	b.batches = nil
	bytes := 0
	for _, batch := range batches {
		bytes += estimateLogBatchSize(batch)
	}
	b.byteCount -= bytes
	if b.byteCount < 0 {
		b.byteCount = 0
	}
	return batches, bytes
}

func (b *LogBuffer) flushBatches(ctx context.Context, batches []wal.LogBatch) error {
	total := 0
	for _, batch := range batches {
		total += len(batch.Records)
	}
	records := make([]segment.LogRecord, 0, total)
	for _, batch := range batches {
		records = append(records, batch.Records...)
	}
	if err := b.writer.WriteLogs(ctx, records); err != nil {
		return fmt.Errorf("flush log buffer: %w", err)
	}
	b.flushes.Add(1)
	return nil
}

func estimateLogRecordSize(r segment.LogRecord) int {
	size := len(r.Tenant) + len(r.Service) + len(r.Body) + len(r.SeverityText) +
		len(r.TraceID) + len(r.SpanID) + 64
	for k, v := range r.ResourceAttributes {
		size += len(k) + len(v)
	}
	for k, v := range r.Attributes {
		size += len(k) + len(v)
	}
	return size * 10
}

func estimateLogBatchSize(batch wal.LogBatch) int {
	total := 0
	for _, record := range batch.Records {
		total += estimateLogRecordSize(record)
	}
	return total
}
