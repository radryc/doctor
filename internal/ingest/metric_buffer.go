package ingest

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/wal"
)

// MetricBufferConfig controls metric buffer behavior.
type MetricBufferConfig struct {
	FlushInterval   time.Duration
	MaxBytes        int
	MaxPendingBytes int
}

func (c MetricBufferConfig) withDefaults() MetricBufferConfig {
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

// MetricBufferStats holds buffer telemetry.
type MetricBufferStats struct {
	PendingBatches int64 `json:"pending_batches"`
	PendingBytes   int64 `json:"pending_bytes"`
	Flushes        int64 `json:"flushes"`
}

const defaultMetricFlushChunkBytes = 2 << 20

// MetricBuffer accumulates metric point records in memory and WAL, then flushes
// them as a single large columnar object to S3 once the size threshold is reached
// or the flush interval elapses.
type MetricBuffer struct {
	writer Flusher
	log    *wal.MetricBatchLog
	cfg    MetricBufferConfig

	mu        sync.Mutex
	batches   []wal.MetricBatch
	byteCount int
	flushing  bool
	triggerCh chan struct{}
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	flushes atomic.Int64
}

// NewMetricBuffer creates a MetricBuffer.
func NewMetricBuffer(writer Flusher, log *wal.MetricBatchLog, cfg MetricBufferConfig) *MetricBuffer {
	return &MetricBuffer{
		writer:    writer,
		log:       log,
		cfg:       cfg.withDefaults(),
		triggerCh: make(chan struct{}, 1),
	}
}

// Start launches the background flush goroutine.
func (b *MetricBuffer) Start(ctx context.Context) {
	ctx, b.cancel = context.WithCancel(ctx)
	b.wg.Add(1)
	go b.run(ctx)
}

// Stop flushes any pending records and stops the background goroutine.
func (b *MetricBuffer) Stop(timeout time.Duration) {
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

// Append stages metric records in the WAL and memory buffer.
// Returns ErrBufferFull when pending bytes exceed MaxPendingBytes.
func (b *MetricBuffer) Append(ctx context.Context, records []segment.MetricPointRecord) error {
	if len(records) == 0 {
		return nil
	}
	totalBytes := 0
	for _, r := range records {
		totalBytes += estimateMetricRecordSize(r)
	}

	// Throttle: reject if adding this batch would exceed the pending limit.
	// Allow the batch when the buffer is currently empty so that an oversized
	// single batch is not permanently rejected.
	// Reserve bytes under the lock before WAL I/O so concurrent appends see
	// accurate accounting.
	b.mu.Lock()
	if b.byteCount > 0 && b.byteCount+totalBytes > b.cfg.MaxPendingBytes {
		b.mu.Unlock()
		return ErrBufferFull
	}
	b.byteCount += totalBytes
	b.mu.Unlock()

	batch := wal.MetricBatch{Records: records}
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
func (b *MetricBuffer) Replay(ctx context.Context) error {
	if b.log == nil {
		return nil
	}
	replayBatches := make([]wal.MetricBatch, 0)
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

	err := b.log.Iterate(func(batch wal.MetricBatch) error {
		batchBytes := estimateMetricBatchSize(batch)
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
func (b *MetricBuffer) Stats() MetricBufferStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return MetricBufferStats{
		PendingBatches: int64(len(b.batches)),
		PendingBytes:   int64(b.byteCount),
		Flushes:        b.flushes.Load(),
	}
}

func (b *MetricBuffer) run(ctx context.Context) {
	defer b.wg.Done()
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if err := b.flushPending(context.Background()); err != nil {
				log.Printf("metric buffer flush on shutdown: %v", err)
			}
			return
		case <-ticker.C:
			if err := b.flushPending(ctx); err != nil {
				log.Printf("metric buffer flush: %v", err)
			}
		case <-b.triggerCh:
			if err := b.flushPending(ctx); err != nil {
				log.Printf("metric buffer flush: %v", err)
			}
		}
	}
}

func (b *MetricBuffer) signalFlush() {
	select {
	case b.triggerCh <- struct{}{}:
	default:
	}
}

func (b *MetricBuffer) flushPending(ctx context.Context) error {
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
		restored := make([]wal.MetricBatch, 0, len(batches)+len(b.batches))
		restored = append(restored, batches...)
		restored = append(restored, b.batches...)
		b.batches = restored
		b.byteCount += flushedBytes
		b.mu.Unlock()
		return err
	}

	remaining := append([]wal.MetricBatch(nil), b.batches...)
	hasMore := len(remaining) > 0
	if err := b.log.Replace(remaining); err != nil {
		b.mu.Unlock()
		return err
	}
	b.mu.Unlock()
	if hasMore {
		b.signalFlush()
	}
	return nil
}

func (b *MetricBuffer) takeFlushBatchesLocked() ([]wal.MetricBatch, int) {
	limit := b.cfg.MaxBytes
	if limit <= 0 {
		limit = b.byteCount
	}

	count := 0
	bytes := 0
	for count < len(b.batches) {
		batchBytes := estimateMetricBatchSize(b.batches[count])
		if count > 0 && bytes+batchBytes > limit {
			break
		}
		bytes += batchBytes
		count++
		if bytes >= limit {
			break
		}
	}
	if count == 0 {
		count = 1
		bytes = estimateMetricBatchSize(b.batches[0])
	}

	selected := append([]wal.MetricBatch(nil), b.batches[:count]...)
	b.batches = append([]wal.MetricBatch(nil), b.batches[count:]...)
	b.byteCount -= bytes
	if b.byteCount < 0 {
		b.byteCount = 0
	}
	return selected, bytes
}

func (b *MetricBuffer) flushBatches(ctx context.Context, batches []wal.MetricBatch) error {
	total := 0
	for _, batch := range batches {
		total += len(batch.Records)
	}
	records := make([]segment.MetricPointRecord, 0, total)
	for _, batch := range batches {
		records = append(records, batch.Records...)
	}
	for _, chunk := range splitMetricRecords(records, b.metricFlushChunkBytes()) {
		if err := b.writer.WriteMetrics(ctx, chunk); err != nil {
			return fmt.Errorf("flush metric buffer: %w", err)
		}
	}
	b.flushes.Add(1)
	return nil
}

func (b *MetricBuffer) metricFlushChunkBytes() int {
	if b.cfg.MaxBytes > 1 {
		return b.cfg.MaxBytes / 2
	}
	return defaultMetricFlushChunkBytes
}

func splitMetricRecords(records []segment.MetricPointRecord, maxBytes int) [][]segment.MetricPointRecord {
	if len(records) == 0 {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = defaultMetricFlushChunkBytes
	}

	chunks := make([][]segment.MetricPointRecord, 0, 1)
	start := 0
	bytes := 0
	for i, record := range records {
		recordBytes := estimateMetricRecordSize(record)
		if i > start && bytes+recordBytes > maxBytes {
			chunks = append(chunks, records[start:i])
			start = i
			bytes = 0
		}
		bytes += recordBytes
	}
	chunks = append(chunks, records[start:])
	return chunks
}

func estimateMetricRecordSize(r segment.MetricPointRecord) int {
	// Count raw string bytes then apply an overhead multiplier to account for
	// Go struct, map bucket, string header, and json.Unmarshal allocations.
	// Empirically the in-memory footprint is ~10x the raw string content.
	size := len(r.Tenant) + len(r.Service) + len(r.MetricName) + len(r.Unit) +
		len(r.Type) + len(r.Temporality) + 80
	for k, v := range r.ResourceAttributes {
		size += len(k) + len(v)
	}
	for k, v := range r.Attributes {
		size += len(k) + len(v)
	}
	return size * 10
}

func estimateMetricBatchSize(batch wal.MetricBatch) int {
	size := 0
	for _, record := range batch.Records {
		size += estimateMetricRecordSize(record)
	}
	return size
}
