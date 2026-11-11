package quota

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
)

// IngestBatch is the quota view of one OTLP export after flattening.
type IngestBatch struct {
	Signal  string // traces | logs | metrics
	Bytes   int64
	Records map[string]int64 // partition -> record count
	Metrics []segment.MetricPointRecord
}

// QueryRequest is one read-side admission request.
type QueryRequest struct {
	Identity   authz.Identity
	Partitions []string
	Cost       float64
}

// Engine enforces the effective quotas from a Provider.
type Engine struct {
	provider *Provider
	replicas float64
	series   *SeriesTracker
	storage  *StorageTracker
	logger   *log.Logger

	mu       sync.Mutex
	buckets  map[string]*bucket
	inflight map[string]*inflight
	stats    map[string]*stat

	lastSweep time.Time
	now       func() time.Time
}

// NewEngine builds an enforcement engine. replicas divides every rate so the
// aggregate across replicas stays below the configured limit.
func NewEngine(provider *Provider, replicas int, logger *log.Logger) *Engine {
	if replicas <= 0 {
		replicas = 1
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Engine{
		provider: provider,
		replicas: float64(replicas),
		series:   NewSeriesTracker(time.Hour),
		storage:  NewStorageTracker(),
		logger:   logger,
		buckets:  map[string]*bucket{},
		inflight: map[string]*inflight{},
		stats:    map[string]*stat{},
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Provider returns the underlying provider (used by the dashboard and rules).
func (e *Engine) Provider() *Provider {
	if e == nil {
		return nil
	}
	return e.provider
}

// CheckIngest admits one flattened OTLP batch.
func (e *Engine) CheckIngest(ctx context.Context, batch IngestBatch) error {
	if e == nil || e.provider == nil {
		return nil
	}
	_ = ctx
	signal := strings.ToLower(strings.TrimSpace(batch.Signal))
	if signal == "" || len(batch.Records) == 0 {
		return nil
	}
	total := int64(0)
	for _, count := range batch.Records {
		total += count
	}
	if total <= 0 {
		return nil
	}
	requestBytes := batch.Bytes
	if requestBytes <= 0 {
		requestBytes = total * 256
	}
	metricsByPartition := map[string][]segment.MetricPointRecord{}
	for _, rec := range batch.Metrics {
		key := partitionOf(rec.Partition)
		metricsByPartition[key] = append(metricsByPartition[key], rec)
	}

	partitions := make([]string, 0, len(batch.Records))
	for partition := range batch.Records {
		partitions = append(partitions, partition)
	}
	sort.Strings(partitions)

	for _, rawPartition := range partitions {
		partition := strings.TrimSpace(rawPartition)
		if partition == "" {
			partition = UnknownPartition
		}
		if e.provider.Blocked(partition) {
			return &LimitError{Key: "partition:" + partition, Resource: "blocked", Observed: false}
		}
		if e.provider.Policy() != nil && !e.provider.IsKnownPartition(partition) {
			if e.provider.Enforce(partition) {
				return fmt.Errorf("%w: %q", ErrUnknownPartition, partition)
			}
			partition = UnknownPartition
		}
		count := batch.Records[rawPartition]
		bytes := requestBytes * count / total
		if bytes < count*64 {
			bytes = count * 64
		}
		enforce := e.provider.Enforce(partition)
		ingest := e.provider.IngestQuota(partition)

		var limit policySignalQuota
		switch signal {
		case "traces":
			limit = policySignalQuotaFromSignal(ingest.Traces)
		case "logs":
			limit = policySignalQuotaFromSignal(ingest.Logs)
		case "metrics":
			limit = policySignalQuotaFromMetrics(ingest.Metrics)
		default:
			continue
		}
		if limit.maxBatchBytes > 0 && requestBytes > limit.maxBatchBytes {
			if err := e.reject(enforce, "partition:"+partition, signal+".batch", float64(limit.maxBatchBytes)); err != nil {
				return err
			}
		}
		if signal == "metrics" && e.series != nil && ingest.Metrics != nil {
			records := metricsByPartition[rawPartition]
			if partition != rawPartition {
				records = append(records, metricsByPartition[partition]...)
			}
			for _, rec := range records {
				if err := e.series.Check(partition, rec, *ingest.Metrics); err != nil {
					e.record("partition:"+partition, "partition", "metrics.cardinality", false)
					if enforce {
						return err
					}
					e.logger.Printf("quota: observe: %v", err)
					break
				}
			}
		}

		specs := make([]bucketSpec, 0, 3)
		if limit.bytesPerSecond > 0 {
			specs = append(specs, bucketSpec{
				key:      "partition:" + partition + ":ingest:" + signal + ":bytes",
				resource: signal + ".bytes",
				limit:    float64(limit.bytesPerSecond) / e.replicas,
				cost:     float64(bytes),
			})
		}
		if limit.recordsPerSecond > 0 {
			specs = append(specs, bucketSpec{
				key:      "partition:" + partition + ":ingest:" + signal + ":records",
				resource: signal + ".records",
				limit:    float64(limit.recordsPerSecond) / e.replicas,
				cost:     float64(count),
			})
		}
		if err := e.admit(enforce, specs); err != nil {
			return err
		}
		if e.storage != nil {
			if storageQuota := e.provider.StorageQuota(partition); storageQuota.MaxBytes > 0 {
				if err := e.storage.Check(partition, bytes, storageQuota); err != nil {
					if err := e.reject(enforce, "partition:"+partition, "storage.bytes", float64(storageQuota.MaxBytes)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// AcquireQuery admits one read request and returns a release function that
// must be called when the handler finishes.
func (e *Engine) AcquireQuery(ctx context.Context, req QueryRequest) (func(), error) {
	if e == nil || e.provider == nil {
		return func() {}, nil
	}
	_ = ctx
	cost := req.Cost
	if cost < 1 {
		cost = 1
	}
	partitions := cleanPartitions(req.Partitions)
	enforce := e.provider.Enforce("")
	if len(partitions) == 0 && e.provider.Blocked("") {
		return nil, &LimitError{Key: "global", Resource: "blocked"}
	}
	if len(partitions) > 0 {
		enforce = false
		for _, partition := range partitions {
			if e.provider.Blocked(partition) {
				return nil, &LimitError{Key: "partition:" + partition, Resource: "blocked"}
			}
			if e.provider.Enforce(partition) {
				enforce = true
			}
		}
	}

	subjectQuotas := e.provider.SubjectQuotas(req.Identity)
	specs := make([]bucketSpec, 0, len(subjectQuotas)*2+len(partitions)*2)
	concurrency := make([]concurrencySpec, 0, len(subjectQuotas)+len(partitions))

	for _, scoped := range subjectQuotas {
		if scoped.Quota.Blocked {
			return nil, &LimitError{Key: scoped.Key, Resource: "blocked"}
		}
		if scoped.Quota.Query == nil {
			continue
		}
		q := *scoped.Quota.Query
		enforce = enforce || strings.EqualFold(scoped.Quota.Enforcement, "enforce")
		specs = appendQuerySpecs(specs, scoped.Key, q, cost, e.replicas)
		if q.MaxConcurrent > 0 {
			concurrency = append(concurrency, concurrencySpec{key: scoped.Key + ":query:concurrent", resource: "query.concurrent", limit: q.MaxConcurrent})
		}
	}

	if len(partitions) == 0 {
		q := e.provider.QueryQuota("")
		specs = appendQuerySpecs(specs, "global", q, cost, e.replicas)
		if q.MaxConcurrent > 0 {
			concurrency = append(concurrency, concurrencySpec{key: "global:query:concurrent", resource: "query.concurrent", limit: q.MaxConcurrent})
		}
	} else {
		for _, partition := range partitions {
			q := e.provider.QueryQuota(partition)
			key := "partition:" + partition
			specs = appendQuerySpecs(specs, key, q, cost, e.replicas)
			if q.MaxConcurrent > 0 {
				concurrency = append(concurrency, concurrencySpec{key: key + ":query:concurrent", resource: "query.concurrent", limit: q.MaxConcurrent})
			}
		}
	}

	release, err := e.acquireConcurrency(enforce, concurrency)
	if err != nil {
		return nil, err
	}
	if err := e.admit(enforce, specs); err != nil {
		release()
		return nil, err
	}
	if release == nil {
		release = func() {}
	}
	return release, nil
}

func appendQuerySpecs(specs []bucketSpec, key string, q policy.QueryQuota, cost, replicas float64) []bucketSpec {
	if q.RequestsPerSecond > 0 {
		specs = append(specs, bucketSpec{
			key:      key + ":query:requests",
			resource: "query.requests",
			limit:    q.RequestsPerSecond / replicas,
			cost:     1,
		})
	}
	if q.BudgetPerSecond > 0 {
		specs = append(specs, bucketSpec{
			key:      key + ":query:budget",
			resource: "query.budget",
			limit:    float64(q.BudgetPerSecond) / replicas,
			cost:     cost,
		})
	}
	return specs
}

func (e *Engine) acquireConcurrency(enforce bool, specs []concurrencySpec) (func(), error) {
	acquired := make([]*inflight, 0, len(specs))
	for _, spec := range specs {
		if spec.limit <= 0 {
			continue
		}
		e.mu.Lock()
		limiter := e.inflight[spec.key]
		if limiter == nil {
			limiter = &inflight{}
			e.inflight[spec.key] = limiter
		}
		e.mu.Unlock()
		if limiter.acquire(spec.limit) {
			acquired = append(acquired, limiter)
			e.record(spec.key, "query", "concurrent", true)
			continue
		}
		e.record(spec.key, "query", "concurrent", false)
		if enforce {
			for _, limiter := range acquired {
				limiter.release()
			}
			return nil, &LimitError{Key: spec.key, Resource: spec.resource, Limit: float64(spec.limit)}
		}
	}
	return func() {
		for _, limiter := range acquired {
			limiter.release()
		}
	}, nil
}

type bucketSpec struct {
	key      string
	resource string
	limit    float64
	cost     float64
}

type concurrencySpec struct {
	key      string
	resource string
	limit    int
}

func (e *Engine) admit(enforce bool, specs []bucketSpec) error {
	now := e.now()
	type taken struct {
		bucket *bucket
		cost   float64
	}
	acquired := make([]taken, 0, len(specs))
	for _, spec := range specs {
		if spec.limit <= 0 || spec.cost <= 0 {
			continue
		}
		b := e.bucket(spec.key, spec.limit, now)
		ok, _, retry := b.take(now, spec.cost)
		if !ok {
			for _, item := range acquired {
				item.bucket.refund(now, item.cost)
			}
			e.record(spec.key, "partition", spec.resource, false)
			err := &LimitError{Key: spec.key, Resource: spec.resource, Limit: spec.limit, RetryAfter: retry, Observed: !enforce}
			if enforce {
				return err
			}
			e.logger.Printf("quota: observe: %s", err.Error())
			return nil
		}
		acquired = append(acquired, taken{bucket: b, cost: spec.cost})
		e.record(spec.key, scopeOf(spec.key), spec.resource, true)
	}
	return nil
}

func (e *Engine) reject(enforce bool, key, resource string, limit float64) error {
	e.record(key, scopeOf(key), resource, false)
	err := &LimitError{Key: key, Resource: resource, Limit: limit, Observed: !enforce}
	if enforce {
		return err
	}
	e.logger.Printf("quota: observe: %s", err.Error())
	return nil
}

func (e *Engine) bucket(key string, limit float64, now time.Time) *bucket {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastSweep.IsZero() || now.Sub(e.lastSweep) > 5*time.Minute {
		e.sweepLocked(now)
		e.lastSweep = now
	}
	b := e.buckets[key]
	if b == nil {
		b = newBucket(limit, limit, now)
		e.buckets[key] = b
		return b
	}
	b.configure(limit, limit, now)
	return b
}

// sweepLocked drops idle admission state. The engine is long-lived and subject
// keys are user-controlled, so an unbounded map would be a slow leak.
func (e *Engine) sweepLocked(now time.Time) {
	cutoff := now.Add(-30 * time.Minute)
	for key, b := range e.buckets {
		if b.idle(now) {
			delete(e.buckets, key)
		}
	}
	for key, st := range e.stats {
		if !st.lastSeen.After(cutoff) {
			delete(e.stats, key)
		}
	}
	for key, limiter := range e.inflight {
		if limiter.idle(now) {
			delete(e.inflight, key)
		}
	}
}

func (e *Engine) record(key, scope, resource string, accepted bool) {
	observeDecision(scope, resource, accepted)
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stats[key]
	if st == nil {
		st = &stat{scope: scope, resource: resource}
		e.stats[key] = st
	}
	st.lastSeen = e.now()
	if accepted {
		st.accepted++
	} else {
		st.rejected++
	}
}

// Snapshot returns current usage for dashboard and debugging. Prefix filters by
// key prefix (for example "partition:checkout").
func (e *Engine) Snapshot(prefix string) []Usage {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	keys := make([]string, 0, len(e.stats)+len(e.buckets)+len(e.inflight))
	for key := range e.stats {
		keys = append(keys, key)
	}
	for key := range e.buckets {
		if _, ok := e.stats[key]; !ok {
			keys = append(keys, key)
		}
	}
	for key := range e.inflight {
		if _, ok := e.stats[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	stats := make(map[string]*stat, len(e.stats))
	for key, st := range e.stats {
		copied := *st
		stats[key] = &copied
	}
	buckets := make(map[string]*bucket, len(e.buckets))
	for key, b := range e.buckets {
		buckets[key] = b
	}
	inflight := make(map[string]*inflight, len(e.inflight))
	for key, i := range e.inflight {
		inflight[key] = i
	}
	e.mu.Unlock()

	now := e.now()
	out := make([]Usage, 0, len(keys))
	for _, key := range keys {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		var usage Usage
		if st := stats[key]; st != nil {
			usage = st.clone(key)
		} else {
			usage.Key = key
			usage.Scope = scopeOf(key)
		}
		if b := buckets[key]; b != nil {
			limit, remaining := b.state(now)
			usage.Limit = limit
			usage.Remaining = remaining
			if limit > 0 {
				usage.Used = limit - remaining
				if usage.Used < 0 {
					usage.Used = 0
				}
			}
		}
		if i := inflight[key]; i != nil {
			current, limit := i.state()
			usage.Inflight = current
			usage.Limit = float64(limit)
			usage.Used = float64(current)
			usage.Remaining = float64(limit - current)
		}
		out = append(out, usage)
	}
	if e.storage != nil {
		for _, partition := range e.storage.Partitions() {
			key := "partition:" + partition + ":storage:bytes"
			if prefix != "" && !strings.HasPrefix(key, prefix) {
				continue
			}
			q := e.provider.StorageQuota(partition)
			used := e.storage.Used(partition, q)
			out = append(out, Usage{
				Key:       key,
				Scope:     "partition",
				Resource:  "storage.bytes",
				Limit:     float64(q.MaxBytes),
				Used:      float64(used),
				Remaining: float64(q.MaxBytes - used),
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	}
	return out
}

func cleanPartitions(partitions []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(partitions))
	for _, partition := range partitions {
		partition = strings.TrimSpace(partition)
		if partition == "" {
			continue
		}
		if _, ok := seen[partition]; ok {
			continue
		}
		seen[partition] = struct{}{}
		out = append(out, partition)
	}
	sort.Strings(out)
	return out
}

func partitionOf(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return UnknownPartition
	}
	return value
}

func scopeOf(key string) string {
	switch {
	case strings.HasPrefix(key, "partition:"):
		return "partition"
	case strings.HasPrefix(key, "user:"):
		return "user"
	case strings.HasPrefix(key, "group:"):
		return "group"
	case strings.HasPrefix(key, "client:"):
		return "client"
	default:
		return "global"
	}
}

// policySignalQuota is the internal view of a signal ingest quota.
type policySignalQuota struct {
	bytesPerSecond   int64
	recordsPerSecond int64
	maxBatchBytes    int64
}

func policySignalQuotaFromSignal(q *policy.SignalIngestQuota) policySignalQuota {
	if q == nil {
		return policySignalQuota{}
	}
	return policySignalQuota{bytesPerSecond: q.BytesPerSecond, recordsPerSecond: q.RecordsPerSecond, maxBatchBytes: q.MaxBatchBytes}
}

func policySignalQuotaFromMetrics(q *policy.MetricsIngestQuota) policySignalQuota {
	if q == nil {
		return policySignalQuota{}
	}
	return policySignalQuota{bytesPerSecond: q.BytesPerSecond, recordsPerSecond: q.SamplesPerSecond, maxBatchBytes: q.MaxBatchBytes}
}
