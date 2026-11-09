package query

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/radryc/doctor/internal/segment"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
	"golang.org/x/sync/singleflight"
)

// queryCacheMaxRows bounds what the raw-result cache will store. Discovery
// calls use limits in the thousands and are cached at the catalog layer; the
// raw cache exists for the small dashboard widgets (logs/traces) where the
// same query is repeated for every viewer.
const (
	queryCacheMaxRows          = 500
	queryCacheMaxDiscoveryRows = 10000
	queryDiscoveryCacheEntries = 32
)

// cachingBackend wraps the raw MonoFS backend, before the access-policy
// decorator. That ordering is intentional: the cache stores unfiltered
// records and the existing policyBackend applies per-user partition filtering
// after every read, so a cache hit can never leak another user's data.
type cachingBackend struct {
	inner      LogEngineBackend
	cache      *resultCache
	largeCache *resultCache
}

func newCachingBackend(inner LogEngineBackend, ttl time.Duration) *cachingBackend {
	return &cachingBackend{
		inner:      inner,
		cache:      newResultCache(ttl),
		largeCache: newResultCacheWithLimit(ttl, queryDiscoveryCacheEntries),
	}
}

func queryCacheTTLFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("DOCTOR_QUERY_CACHE_TTL"))
	if raw == "" {
		return 15 * time.Second
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl <= 0 {
		return 0
	}
	return ttl
}

func (b *cachingBackend) QueryLogs(ctx context.Context, tenant, query, service string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	if limit <= 0 || limit > queryCacheMaxDiscoveryRows {
		return b.inner.QueryLogs(ctx, tenant, query, service, from, to, limit)
	}
	cache := b.cache
	if limit > queryCacheMaxRows {
		cache = b.largeCache
	}
	key := fmt.Sprintf("logs\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s", tenant, query, service, limit, cacheBucket(from), cacheBucket(to))
	value, err := cache.getOrLoad(ctx, key, func(ctx context.Context) (any, error) {
		return b.inner.QueryLogs(ctx, tenant, query, service, from, to, limit)
	})
	if err != nil {
		return nil, err
	}
	records, _ := value.([]segment.LogRecord)
	return cloneLogRecords(records), nil
}

func (b *cachingBackend) QueryLogsRaw(ctx context.Context, tenant, query string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	raw, ok := b.inner.(rawLogQueryBackend)
	if !ok {
		return b.inner.QueryLogs(ctx, tenant, query, "", from, to, limit)
	}
	if limit <= 0 || limit > queryCacheMaxRows {
		return raw.QueryLogsRaw(ctx, tenant, query, from, to, limit)
	}
	key := fmt.Sprintf("logs-raw\x00%s\x00%s\x00%d\x00%s\x00%s", tenant, query, limit, cacheBucket(from), cacheBucket(to))
	value, err := b.cache.getOrLoad(ctx, key, func(ctx context.Context) (any, error) {
		return raw.QueryLogsRaw(ctx, tenant, query, from, to, limit)
	})
	if err != nil {
		return nil, err
	}
	records, _ := value.([]segment.LogRecord)
	return cloneLogRecords(records), nil
}

func (b *cachingBackend) QueryTraces(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int) ([]segment.TraceRecord, error) {
	if limit <= 0 || limit > queryCacheMaxDiscoveryRows {
		return b.inner.QueryTraces(ctx, tenant, traceID, service, from, to, limit)
	}
	cache := b.cache
	if limit > queryCacheMaxRows {
		cache = b.largeCache
	}
	key := fmt.Sprintf("traces\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s", tenant, traceID, service, limit, cacheBucket(from), cacheBucket(to))
	value, err := cache.getOrLoad(ctx, key, func(ctx context.Context) (any, error) {
		return b.inner.QueryTraces(ctx, tenant, traceID, service, from, to, limit)
	})
	if err != nil {
		return nil, err
	}
	records, _ := value.([]segment.TraceRecord)
	return cloneTraceRecords(records), nil
}

// The remaining methods are pass-through. Metrics are discovered through the
// streaming APIs and are already covered by the PromQL/label paths; buffering
// an unbounded metric scan would multiply memory, so it is deliberately not
// cached here.
func (b *cachingBackend) QueryMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time) ([]segment.MetricPointRecord, error) {
	return b.inner.QueryMetrics(ctx, tenant, query, from, to)
}

func (b *cachingBackend) StreamMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricPointRecord) error) error {
	return b.inner.StreamMetrics(ctx, tenant, query, from, to, yield)
}

func (b *cachingBackend) StreamMetricSeries(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricSeriesMeta) error) error {
	return b.inner.StreamMetricSeries(ctx, tenant, query, from, to, yield)
}

func (b *cachingBackend) QueryDuckDBSQL(ctx context.Context, tenant, dataset, query string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) (monofsstore.DuckDBSQLResponse, error) {
	return b.inner.QueryDuckDBSQL(ctx, tenant, dataset, query, from, to, limit, visiblePartitions, restricted)
}

// QueryTraceDiscovery is a pass-through so wrapping the backend does not hide
// the optional fast trace-discovery RPC from localTracesUpstream.
func (b *cachingBackend) QueryTraceDiscovery(ctx context.Context, tenant, discovery, tag string, from, to time.Time) ([]string, error) {
	source, ok := b.inner.(traceDiscoverySource)
	if !ok {
		return nil, fmt.Errorf("trace discovery is not supported by the configured backend")
	}
	return source.QueryTraceDiscovery(ctx, tenant, discovery, tag, from, to)
}

func cloneLogRecords(records []segment.LogRecord) []segment.LogRecord {
	if len(records) == 0 {
		return records
	}
	out := make([]segment.LogRecord, len(records))
	for i, record := range records {
		record.Attributes = cloneStringMap(record.Attributes)
		record.ResourceAttributes = cloneStringMap(record.ResourceAttributes)
		out[i] = record
	}
	return out
}

// cloneTraceRecords performs a deep copy. The cache stores the original
// records, and downstream canonicalization (internal/query/proxy_wiring.go)
// writes into Attributes. A shallow slice copy would let concurrent requests
// mutate the same map and crash with "concurrent map read and map write".
func cloneTraceRecords(records []segment.TraceRecord) []segment.TraceRecord {
	if len(records) == 0 {
		return records
	}
	out := make([]segment.TraceRecord, len(records))
	for i, record := range records {
		record.Attributes = cloneStringMap(record.Attributes)
		record.ResourceAttributes = cloneStringMap(record.ResourceAttributes)
		if len(record.Events) > 0 {
			events := make([]segment.TraceEvent, len(record.Events))
			for j, event := range record.Events {
				event.Attributes = cloneStringMap(event.Attributes)
				events[j] = event
			}
			record.Events = events
		}
		out[i] = record
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

type resultCacheEntry struct {
	value     any
	expiresAt time.Time
	storedAt  time.Time
}

type resultCache struct {
	mu         sync.Mutex
	entries    map[string]resultCacheEntry
	group      singleflight.Group
	ttl        time.Duration
	maxEntries int
}

func newResultCache(ttl time.Duration) *resultCache {
	return newResultCacheWithLimit(ttl, 256)
}

func newResultCacheWithLimit(ttl time.Duration, maxEntries int) *resultCache {
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = 256
	}
	return &resultCache{entries: make(map[string]resultCacheEntry), ttl: ttl, maxEntries: maxEntries}
}

func (c *resultCache) getOrLoad(ctx context.Context, key string, load func(context.Context) (any, error)) (any, error) {
	if c == nil {
		return load(ctx)
	}
	c.mu.Lock()
	entry, ok := c.entries[key]
	if ok && time.Now().Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.value, nil
	}
	hasStale := ok
	stale := entry
	c.mu.Unlock()

	value, err, _ := c.group.Do(key, func() (any, error) {
		c.mu.Lock()
		if entry, ok := c.entries[key]; ok && time.Now().Before(entry.expiresAt) {
			c.mu.Unlock()
			return entry.value, nil
		}
		c.mu.Unlock()

		// The load is shared by every waiter. Deriving it from the leader's
		// request context would make one client disconnect fail all of them.
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		value, loadErr := load(loadCtx)
		if loadErr != nil {
			return nil, loadErr
		}
		now := time.Now()
		c.mu.Lock()
		c.entries[key] = resultCacheEntry{
			value:     value,
			expiresAt: now.Add(c.ttl),
			storedAt:  now,
		}
		c.pruneLocked(now)
		c.mu.Unlock()
		return value, nil
	})
	if err != nil {
		if hasStale && time.Since(stale.storedAt) <= 2*time.Minute {
			return stale.value, nil
		}
		return nil, err
	}
	return value, nil
}

func (c *resultCache) pruneLocked(now time.Time) {
	if len(c.entries) <= c.maxEntries {
		return
	}
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	for key := range c.entries {
		if len(c.entries) <= c.maxEntries {
			break
		}
		delete(c.entries, key)
	}
}

func cacheBucket(t time.Time) string {
	return t.UTC().Truncate(15 * time.Second).Format(time.RFC3339)
}
