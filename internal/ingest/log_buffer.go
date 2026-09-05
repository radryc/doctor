package ingest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/wal"
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
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
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
	return b.log.Replace(nil)
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
	batches := append([]wal.LogBatch(nil), b.batches...)
	b.batches = nil
	b.byteCount = 0
	b.mu.Unlock()

	err := b.flushBatches(ctx, batches)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushing = false
	if err != nil {
		// Re-queue on failure; WAL still has the data.
		b.batches = append(batches, b.batches...)
		for _, batch := range batches {
			for _, r := range batch.Records {
				b.byteCount += estimateLogRecordSize(r)
			}
		}
		return err
	}
	remaining := append([]wal.LogBatch(nil), b.batches...)
	// Recalculate byteCount from remaining batches to correct for reservation
	// drift: byteCount was reset to 0 when flush started, so any bytes reserved
	// by concurrent Appends during the flush are no longer reflected.
	recalcBytes := 0
	for _, batch := range remaining {
		for _, r := range batch.Records {
			recalcBytes += estimateLogRecordSize(r)
		}
	}
	b.byteCount = recalcBytes
	if err := b.log.Replace(remaining); err != nil {
		return err
	}
	return nil
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
