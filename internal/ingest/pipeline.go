package ingest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

var ErrQueueFull = errors.New("ingest pipeline queue is full")

// work represents a unit of ingestion work.
type work struct {
	signal  segment.Signal
	traces  []segment.TraceRecord
	logs    []segment.LogRecord
	metrics []segment.MetricPointRecord
	events  []segment.GuardianEvent
	errCh   chan error
}

// Pipeline wraps a Flusher with a bounded work queue for backpressure.
type Pipeline struct {
	writer    Flusher
	queue     chan work
	workers   int
	wg        sync.WaitGroup
	cancel    context.CancelFunc
	pending   atomic.Int64
	dropped   atomic.Int64
	processed atomic.Int64
}

// PipelineConfig controls pipeline behavior.
type PipelineConfig struct {
	QueueSize int // Max queued work items before rejecting. Default: 1000.
	Workers   int // Concurrent writer goroutines. Default: 4.
}

func (c PipelineConfig) withDefaults() PipelineConfig {
	if c.QueueSize <= 0 {
		c.QueueSize = 1000
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	return c
}

// NewPipeline creates a backpressure-controlled ingestion pipeline.
func NewPipeline(writer Flusher, cfg PipelineConfig) *Pipeline {
	cfg = cfg.withDefaults()
	return &Pipeline{
		writer:  writer,
		queue:   make(chan work, cfg.QueueSize),
		workers: cfg.Workers,
	}
}

// Start launches worker goroutines. Call Stop to shut down.
func (p *Pipeline) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.runWorker(ctx)
	}
}

// Stop gracefully drains the queue and stops workers.
func (p *Pipeline) Stop(timeout time.Duration) {
	if p.cancel != nil {
		p.cancel()
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// SubmitTraces queues trace records for writing. Returns ErrQueueFull if the
// pipeline cannot accept more work.
func (p *Pipeline) SubmitTraces(ctx context.Context, records []segment.TraceRecord) error {
	return p.submit(ctx, work{signal: segment.SignalTraces, traces: records, errCh: make(chan error, 1)})
}

// SubmitLogs queues log records for writing.
func (p *Pipeline) SubmitLogs(ctx context.Context, records []segment.LogRecord) error {
	return p.submit(ctx, work{signal: segment.SignalLogs, logs: records, errCh: make(chan error, 1)})
}

// SubmitMetrics queues metric records for writing.
func (p *Pipeline) SubmitMetrics(ctx context.Context, records []segment.MetricPointRecord) error {
	return p.submit(ctx, work{signal: segment.SignalMetric, metrics: records, errCh: make(chan error, 1)})
}

// SubmitGuardianEvents queues guardian events for writing.
func (p *Pipeline) SubmitGuardianEvents(ctx context.Context, events []segment.GuardianEvent) error {
	return p.submit(ctx, work{signal: segment.SignalGuardian, events: events, errCh: make(chan error, 1)})
}

// Stats returns pipeline statistics.
func (p *Pipeline) Stats() PipelineStats {
	return PipelineStats{
		Pending:   p.pending.Load(),
		Dropped:   p.dropped.Load(),
		Processed: p.processed.Load(),
		QueueLen:  int64(len(p.queue)),
		QueueCap:  int64(cap(p.queue)),
	}
}

// PipelineStats holds pipeline metrics.
type PipelineStats struct {
	Pending   int64 `json:"pending"`
	Dropped   int64 `json:"dropped"`
	Processed int64 `json:"processed"`
	QueueLen  int64 `json:"queue_len"`
	QueueCap  int64 `json:"queue_cap"`
}

func (p *Pipeline) submit(ctx context.Context, w work) error {
	select {
	case p.queue <- w:
		p.pending.Add(1)
		// Wait for the worker to finish processing.
		select {
		case err := <-w.errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	default:
		p.dropped.Add(1)
		return ErrQueueFull
	}
}

func (p *Pipeline) runWorker(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Drain remaining items.
			for {
				select {
				case w := <-p.queue:
					p.processWork(ctx, w)
				default:
					return
				}
			}
		case w := <-p.queue:
			p.processWork(ctx, w)
		}
	}
}

func (p *Pipeline) processWork(ctx context.Context, w work) {
	defer p.pending.Add(-1)
	var err error
	switch w.signal {
	case segment.SignalTraces:
		err = p.writer.WriteTraces(ctx, w.traces)
	case segment.SignalLogs:
		err = p.writer.WriteLogs(ctx, w.logs)
	case segment.SignalMetric:
		err = p.writer.WriteMetrics(ctx, w.metrics)
	case segment.SignalGuardian:
		err = p.writer.WriteGuardianEvents(ctx, w.events)
	}
	if err != nil {
		w.errCh <- err
	} else {
		p.processed.Add(1)
		w.errCh <- nil
	}
}
