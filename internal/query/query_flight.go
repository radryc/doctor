package query

import (
	"context"
	"sync"
	"time"
)

// queryFlightGroup coalesces identical in-flight queries. Unlike
// singleflight.Group it is reference-counted: the shared backend call is only
// canceled when every waiter has canceled. A single Grafana tab closing must
// not fail another dashboard waiting on the same result.
type queryFlightGroup struct {
	mu    sync.Mutex
	calls map[string]*queryFlightCall
}

type queryFlightCall struct {
	ctx     context.Context
	cancel  context.CancelFunc
	waiters int
	done    chan struct{}
	result  proxyQueryResult
	err     error
}

// Do runs fn once for key and shares the result. The backend context is
// detached from any single request but is canceled when the last waiter goes
// away, and is bounded by maxQueryFlightDuration.
func (g *queryFlightGroup) Do(ctx context.Context, key string, fn func(context.Context) (proxyQueryResult, error)) (proxyQueryResult, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*queryFlightCall)
	}
	call, exists := g.calls[key]
	if !exists {
		callCtx, cancel := context.WithTimeout(context.Background(), maxQueryFlightDuration)
		call = &queryFlightCall{
			ctx:     callCtx,
			cancel:  cancel,
			waiters: 1,
			done:    make(chan struct{}),
		}
		g.calls[key] = call
		go func() {
			defer close(call.done)
			call.result, call.err = fn(callCtx)
			g.mu.Lock()
			if g.calls[key] == call {
				delete(g.calls, key)
			}
			g.mu.Unlock()
			cancel()
		}()
	} else {
		call.waiters++
	}
	g.mu.Unlock()

	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		g.release(key, call)
		return proxyQueryResult{}, ctx.Err()
	}
}

const maxQueryFlightDuration = 30 * time.Second

func (g *queryFlightGroup) release(key string, call *queryFlightCall) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if call.waiters > 0 {
		call.waiters--
	}
	if call.waiters == 0 {
		if g.calls[key] == call {
			delete(g.calls, key)
		}
		call.cancel()
	}
}
