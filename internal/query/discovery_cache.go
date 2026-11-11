package query

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// discoveryCache is a small TTL cache for expensive catalog/discovery
// responses. Dashboards request the same service/partition catalog on every
// load; without this, each request scans the last hour of logs, traces, and
// metrics. The cache is per doctor-query process and is keyed by the caller's
// effective access scope, so policy-filtered responses are never shared across
// users with different visibility.
type discoveryCache struct {
	mu      sync.Mutex
	entries map[string]discoveryCacheEntry
	group   singleflight.Group
	ttl     time.Duration
}

type discoveryCacheEntry struct {
	body      []byte
	expiresAt time.Time
	storedAt  time.Time
}

func newDiscoveryCache(ttl time.Duration) *discoveryCache {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &discoveryCache{
		entries: make(map[string]discoveryCacheEntry),
		ttl:     ttl,
	}
}

// getOrLoad returns a fresh cached body, or executes load once for concurrent
// callers. A stale entry is served if the refresh fails, which keeps the
// dashboard usable while the query backend is unhealthy.
func (c *discoveryCache) getOrLoad(ctx context.Context, key string, timeout time.Duration, load func(context.Context) ([]byte, error)) ([]byte, string, error) {
	if c == nil {
		body, err := load(ctx)
		return body, "miss", err
	}

	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok && time.Now().Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.body, "hit", nil
	}
	hasStale := ok
	stale := entry
	c.mu.Unlock()

	value, err, _ := c.group.Do(key, func() (any, error) {
		c.mu.Lock()
		if entry, ok := c.entries[key]; ok && time.Now().Before(entry.expiresAt) {
			c.mu.Unlock()
			return entry.body, nil
		}
		c.mu.Unlock()

		// The load is shared by every waiter. Deriving it from the leader's
		// request context would make one client disconnect fail all of them.
		loadCtx := context.WithoutCancel(ctx)
		var cancel context.CancelFunc
		if timeout > 0 {
			loadCtx, cancel = context.WithTimeout(loadCtx, timeout)
			defer cancel()
		}
		body, loadErr := load(loadCtx)
		if loadErr != nil {
			return nil, loadErr
		}
		body = append([]byte(nil), body...)
		c.mu.Lock()
		now := time.Now()
		c.entries[key] = discoveryCacheEntry{
			body:      body,
			expiresAt: now.Add(c.ttl),
			storedAt:  now,
		}
		c.pruneLocked(now)
		c.mu.Unlock()
		return body, nil
	})
	if err != nil {
		if hasStale && time.Since(stale.storedAt) <= 10*time.Minute {
			return stale.body, "stale", nil
		}
		return nil, "", err
	}
	body, _ := value.([]byte)
	return body, "miss", nil
}

// bufferedResponse lets cacheable handlers reuse their existing
// http.ResponseWriter implementation and capture the marshaled body without
// moving the response-building code into a second function.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header {
	if b.header == nil {
		b.header = make(http.Header)
	}
	return b.header
}

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (c *discoveryCache) pruneLocked(now time.Time) {
	if len(c.entries) <= 256 {
		return
	}
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	// Hard cap: discovery keys are low-cardinality, so this only protects
	// against pathological tenant/variable churn.
	for key := range c.entries {
		if len(c.entries) <= 512 {
			break
		}
		delete(c.entries, key)
	}
}
