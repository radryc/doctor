package quota

import (
	"math"
	"sync"
	"time"
)

// bucket is a float token bucket. Zero limit means unlimited.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	limit  float64
	burst  float64
	last   time.Time
	seen   time.Time
}

func newBucket(limit, burst float64, now time.Time) *bucket {
	if burst < limit {
		burst = limit
	}
	if burst < 1 {
		burst = 1
	}
	return &bucket{tokens: burst, limit: limit, burst: burst, last: now, seen: now}
}

// configure updates the bucket's rate. It does not invent tokens when the rate
// rises; it only clamps the stored tokens to the new burst.
func (b *bucket) configure(limit, burst float64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now)
	if burst < limit {
		burst = limit
	}
	if burst < 1 {
		burst = 1
	}
	if limit != b.limit {
		b.limit = limit
		b.burst = burst
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.seen = now
}

func (b *bucket) refillLocked(now time.Time) {
	if b.limit <= 0 {
		b.last = now
		return
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens = math.Min(b.burst, b.tokens+elapsed*b.limit)
	b.last = now
}

// take consumes cost tokens. It returns false when the bucket is exhausted.
func (b *bucket) take(now time.Time, cost float64) (bool, float64, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen = now
	if b.limit <= 0 {
		return true, 0, 0
	}
	b.refillLocked(now)
	if cost <= 0 {
		return true, b.tokens, 0
	}
	if cost > b.burst {
		wait := time.Duration((cost - b.tokens) / b.limit * float64(time.Second))
		if wait <= 0 {
			wait = time.Second
		}
		return false, b.tokens, wait
	}
	if b.tokens < cost {
		wait := time.Duration((cost - b.tokens) / b.limit * float64(time.Second))
		if wait <= 0 {
			wait = time.Second
		}
		return false, b.tokens, wait
	}
	b.tokens -= cost
	return true, b.tokens, 0
}

// refund returns tokens after a later bucket in the same admission failed.
func (b *bucket) refund(now time.Time, cost float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 || cost <= 0 {
		return
	}
	b.refillLocked(now)
	b.tokens = math.Min(b.burst, b.tokens+cost)
}

// state returns the current limit and remaining tokens for the dashboard.
func (b *bucket) state(now time.Time) (limit, remaining float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(now)
	return b.limit, b.tokens
}

// idle reports whether the bucket has not been used for a while.
func (b *bucket) idle(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return now.Sub(b.seen) > 30*time.Minute
}

// inflight is a per-key concurrency limiter.
type inflight struct {
	mu       sync.Mutex
	current  int
	limit    int
	lastSeen time.Time
}

func (i *inflight) acquire(limit int) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.limit = limit
	i.lastSeen = time.Now().UTC()
	if limit <= 0 {
		i.current++
		return true
	}
	if i.current >= limit {
		return false
	}
	i.current++
	return true
}

func (i *inflight) release() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.lastSeen = time.Now().UTC()
	if i.current > 0 {
		i.current--
	}
}

func (i *inflight) state() (current, limit int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.current, i.limit
}

func (i *inflight) idle(now time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.current == 0 && now.Sub(i.lastSeen) > 30*time.Minute
}
