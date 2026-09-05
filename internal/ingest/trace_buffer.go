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

type TraceBufferConfig struct {
	FlushInterval   time.Duration
	MaxRecords      int
	MaxBytes        int
	MaxPendingBytes int
}

func (c TraceBufferConfig) withDefaults() TraceBufferConfig {
	if c.FlushInterval <= 0 {
		c.FlushInterval = 5 * time.Minute
	}
	if c.MaxRecords <= 0 {
		c.MaxRecords = 500_000
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 128 << 20 // 128 MiB
	}
	if c.MaxPendingBytes <= 0 {
		c.MaxPendingBytes = 3 * c.MaxBytes
	}
	return c
}

type TraceBufferStats struct {
	PendingBatches int64 `json:"pending_batches"`
	PendingBytes   int64 `json:"pending_bytes"`
	PendingRecords int64 `json:"pending_records"`
	Flushes        int64 `json:"flushes"`
}

type TraceBuffer struct {
	writer Flusher
	log    *wal.TraceBatchLog
	cfg    TraceBufferConfig

	mu          sync.Mutex
	batches     []wal.TraceBatch
	recordCount int
	byteCount   int
	flushing    bool
	triggerCh   chan struct{}
	cancel      context.CancelFunc
	wg          sync.WaitGroup

	flushes atomic.Int64
}

func NewTraceBuffer(writer Flusher, log *wal.TraceBatchLog, cfg TraceBufferConfig) *TraceBuffer {
	return &TraceBuffer{
		writer:    writer,
		log:       log,
		cfg:       cfg.withDefaults(),
		triggerCh: make(chan struct{}, 1),
	}
}

func (b *TraceBuffer) Start(ctx context.Context) {
	ctx, b.cancel = context.WithCancel(ctx)
	b.wg.Add(1)
	go b.run(ctx)
}

func (b *TraceBuffer) Stop(timeout time.Duration) {
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

func (b *TraceBuffer) Append(ctx context.Context, records []segment.TraceRecord) error {
	if len(records) == 0 {
		return nil
	}
	normalized := make([]segment.TraceRecord, 0, len(records))
	totalBytes := 0
	for _, record := range records {
		record = normalizeTraceRecord(record)
		normalized = append(normalized, record)
		totalBytes += estimateTraceRecordSize(record)
	}

	b.mu.Lock()
	if b.byteCount > 0 && b.byteCount+totalBytes > b.cfg.MaxPendingBytes {
		b.mu.Unlock()
		return ErrBufferFull
	}
	b.byteCount += totalBytes
	b.mu.Unlock()

	batch := wal.TraceBatch{Records: normalized}
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
	b.recordCount += len(normalized)
	shouldFlush := b.recordCount >= b.cfg.MaxRecords || b.byteCount >= b.cfg.MaxBytes
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

func (b *TraceBuffer) Replay(ctx context.Context) error {
	if b.log == nil {
		return nil
	}
	replayBatches := make([]wal.TraceBatch, 0)
	replayBytes := 0
	replayRecords := 0
	flushReplay := func() error {
		if len(replayBatches) == 0 {
			return nil
		}
		if err := b.flushBatches(ctx, replayBatches); err != nil {
			return err
		}
		replayBatches = replayBatches[:0]
		replayBytes = 0
		replayRecords = 0
		return nil
	}

	err := b.log.Iterate(func(batch wal.TraceBatch) error {
		batchBytes := estimateTraceBatchSize(batch)
		batchRecords := len(batch.Records)
		if len(replayBatches) > 0 && (replayBytes+batchBytes > b.cfg.MaxBytes || replayRecords+batchRecords > b.cfg.MaxRecords) {
			if err := flushReplay(); err != nil {
				return err
			}
		}

		replayBatches = append(replayBatches, batch)
		replayBytes += batchBytes
		replayRecords += batchRecords
		if replayBytes >= b.cfg.MaxBytes || replayRecords >= b.cfg.MaxRecords {
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


func (b *TraceBuffer) Stats() TraceBufferStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return TraceBufferStats{
		PendingBatches: int64(len(b.batches)),
		PendingBytes:   int64(b.byteCount),
		PendingRecords: int64(b.recordCount),
		Flushes:        b.flushes.Load(),
	}
}

func (b *TraceBuffer) run(ctx context.Context) {
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

func (b *TraceBuffer) signalFlush() {
	select {
	case b.triggerCh <- struct{}{}:
	default:
	}
}

func (b *TraceBuffer) flushPending(ctx context.Context) error {
	b.mu.Lock()
	if b.flushing || len(b.batches) == 0 {
		b.mu.Unlock()
		return nil
	}
	b.flushing = true
	batches := append([]wal.TraceBatch(nil), b.batches...)
	b.batches = nil
	b.recordCount = 0
	b.byteCount = 0
	b.mu.Unlock()

	err := b.flushBatches(ctx, batches)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushing = false
	if err != nil {
		b.batches = append(batches, b.batches...)
		for _, batch := range batches {
			b.recordCount += len(batch.Records)
			for _, record := range batch.Records {
				b.byteCount += estimateTraceRecordSize(record)
			}
		}
		return err
	}
	remaining := append([]wal.TraceBatch(nil), b.batches...)
	recalcBytes := 0
	recalcRecords := 0
	for _, batch := range remaining {
		recalcRecords += len(batch.Records)
		for _, record := range batch.Records {
			recalcBytes += estimateTraceRecordSize(record)
		}
	}
	b.byteCount = recalcBytes
	b.recordCount = recalcRecords
	if err := b.log.Replace(remaining); err != nil {
		return err
	}
	return nil
}

func (b *TraceBuffer) flushBatches(ctx context.Context, batches []wal.TraceBatch) error {
	total := 0
	for _, batch := range batches {
		total += len(batch.Records)
	}
	records := make([]segment.TraceRecord, 0, total)
	for _, batch := range batches {
		records = append(records, batch.Records...)
	}
	if err := b.writer.WriteTraces(ctx, records); err != nil {
		return fmt.Errorf("flush trace buffer: %w", err)
	}
	b.flushes.Add(1)
	return nil
}

func estimateTraceRecordSize(record segment.TraceRecord) int {
	size := len(record.Tenant) + len(record.TraceID) + len(record.SpanID) + len(record.ParentSpanID) +
		len(record.Name) + len(record.Kind) + len(record.Service) + len(record.StatusCode) + len(record.StatusMessage) + 64
	for key, value := range record.ResourceAttributes {
		size += len(key) + len(value)
	}
	for key, value := range record.Attributes {
		size += len(key) + len(value)
	}
	for _, event := range record.Events {
		size += len(event.Name) + 32
		for key, value := range event.Attributes {
			size += len(key) + len(value)
		}
	}
	return size * 10
}

func estimateTraceBatchSize(batch wal.TraceBatch) int {
	total := 0
	for _, record := range batch.Records {
		total += estimateTraceRecordSize(record)
	}
	return total
}

func normalizeTraceRecord(record segment.TraceRecord) segment.TraceRecord {
	if record.Attributes == nil {
		record.Attributes = map[string]string{}
	}
	if record.ResourceAttributes == nil {
		record.ResourceAttributes = map[string]string{}
	}
	return record
}
