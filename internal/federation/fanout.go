package federation

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
)

type fanoutResult[T any] struct {
	target Target
	value  T
	err    error
}

// fanout runs bounded, retrying calls against a set of targets. It preserves
// result order, skips open circuits, and records partial failures on the
// request context for the HTTP warning middleware.
func fanoutTargets[T any](
	m *Manager,
	ctx context.Context,
	targets []Target,
	signal Signal,
	call func(context.Context, Target) (T, error),
) []fanoutResult[T] {
	results := make([]fanoutResult[T], len(targets))
	if len(targets) == 0 {
		return results
	}
	querySem := make(chan struct{}, m.maxConcurrentTargets)
	var wg sync.WaitGroup
	for index, target := range targets {
		if !m.breakerAllow(target.ID, time.Now().UTC()) {
			results[index] = fanoutResult[T]{target: target, err: errTargetCircuitOpen}
			proxy.AddWarning(ctx, fanoutWarning(target, signal, errTargetCircuitOpen))
			m.metrics.RecordFanout(string(signal), "circuit_open", 0)
			m.metrics.RecordPartial()
			continue
		}
		wg.Add(1)
		go func(index int, target Target) {
			defer wg.Done()
			select {
			case querySem <- struct{}{}:
			case <-ctx.Done():
				results[index] = fanoutResult[T]{target: target, err: ctx.Err()}
				m.noteTargetAbort(target.ID)
				return
			}
			defer func() { <-querySem }()

			select {
			case m.inflight <- struct{}{}:
			case <-ctx.Done():
				results[index] = fanoutResult[T]{target: target, err: ctx.Err()}
				m.noteTargetAbort(target.ID)
				return
			}
			defer func() { <-m.inflight }()

			started := time.Now()
			value, err := callTargetWithRetry(m, ctx, target, call)
			elapsed := time.Since(started)
			results[index] = fanoutResult[T]{target: target, value: value, err: err}
			switch {
			case err == nil:
				m.noteTargetSuccess(target.ID)
				m.metrics.RecordFanout(string(signal), "success", elapsed)
				m.recordLatency(target, string(signal), elapsed)
			case ctx.Err() != nil:
				m.noteTargetAbort(target.ID)
				m.metrics.RecordFanout(string(signal), "canceled", elapsed)
			default:
				m.noteTargetFailure(target.ID, err)
				m.metrics.RecordFanout(string(signal), "error", elapsed)
				m.metrics.RecordPartial()
				proxy.AddWarning(ctx, fanoutWarning(target, signal, err))
			}
		}(index, target)
	}
	wg.Wait()
	return results
}

func callTargetWithRetry[T any](
	m *Manager,
	ctx context.Context,
	target Target,
	call func(context.Context, Target) (T, error),
) (T, error) {
	var zero T
	attempts := m.maxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * 100 * time.Millisecond
			if jitter := time.Duration(rand.Int63n(int64(25 * time.Millisecond))); jitter > 0 {
				backoff += jitter
			}
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(backoff):
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, m.timeout)
		value, err := call(callCtx, target)
		cancel()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !isRetryableFanoutError(err) || attempt == attempts-1 {
			return zero, err
		}
	}
	return zero, lastErr
}
