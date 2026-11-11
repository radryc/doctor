package quota

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/monofs/pkg/authz/policy"
)

// unboundedLabels are correlation identifiers that must not become series
// identity. The storage layer applies a similar list.
var unboundedLabels = map[string]struct{}{
	"trace_id": {}, "traceId": {},
	"span_id": {}, "spanId": {},
	"parent_span_id": {}, "parentSpanId": {},
	"task_id": {}, "log_id": {}, "request_id": {},
	"guardian.task.id": {}, "guardian.task_id": {},
}

// SeriesTracker tracks approximate active metric series per partition. It is
// deliberately bounded and local: a restart may briefly re-admit series, but
// the cap cannot be exceeded for long.
type SeriesTracker struct {
	mu         sync.Mutex
	partitions map[string]*partitionSeries
	now        func() time.Time
	window     time.Duration
}

type partitionSeries struct {
	names     map[string]struct{}
	series    map[uint64]time.Time
	perMetric map[string]map[uint64]struct{}
	lastSweep time.Time
}

// NewSeriesTracker builds a tracker. window<=0 defaults to one hour.
func NewSeriesTracker(window time.Duration) *SeriesTracker {
	if window <= 0 {
		window = time.Hour
	}
	return &SeriesTracker{
		partitions: map[string]*partitionSeries{},
		now:        func() time.Time { return time.Now().UTC() },
		window:     window,
	}
}

// Check validates and records one metric point. It returns an error when the
// point would create a new series beyond the configured caps.
func (t *SeriesTracker) Check(partition string, rec segment.MetricPointRecord, q policy.MetricsIngestQuota) error {
	if t == nil {
		return nil
	}
	labels := seriesLabels(rec)
	if q.MaxLabelsPerSeries > 0 && len(labels) > q.MaxLabelsPerSeries {
		return fmt.Errorf("%w: metric %s has %d labels, limit %d", ErrQuotaExceeded, rec.MetricName, len(labels), q.MaxLabelsPerSeries)
	}
	hash := seriesHash(rec.MetricName, labels)

	t.mu.Lock()
	defer t.mu.Unlock()
	ps := t.partition(partition)
	now := t.now()
	t.sweepLocked(ps, now)

	_, seen := ps.series[hash]
	_, named := ps.names[rec.MetricName]
	if !seen {
		if q.MaxActiveSeries > 0 && len(ps.series) >= q.MaxActiveSeries {
			return fmt.Errorf("%w: partition %s active series limit %d", ErrQuotaExceeded, partition, q.MaxActiveSeries)
		}
		if q.MaxMetricNames > 0 && !named && len(ps.names) >= q.MaxMetricNames {
			return fmt.Errorf("%w: partition %s metric name limit %d", ErrQuotaExceeded, partition, q.MaxMetricNames)
		}
		if q.MaxSeriesPerMetric > 0 {
			metricSet := ps.perMetric[rec.MetricName]
			if len(metricSet) >= q.MaxSeriesPerMetric {
				return fmt.Errorf("%w: metric %s series limit %d", ErrQuotaExceeded, rec.MetricName, q.MaxSeriesPerMetric)
			}
			if metricSet == nil {
				metricSet = map[uint64]struct{}{}
				ps.perMetric[rec.MetricName] = metricSet
			}
			metricSet[hash] = struct{}{}
		}
	}
	ps.series[hash] = now
	ps.names[rec.MetricName] = struct{}{}
	return nil
}

func (t *SeriesTracker) partition(name string) *partitionSeries {
	name = strings.TrimSpace(name)
	if name == "" {
		name = UnknownPartition
	}
	ps := t.partitions[name]
	if ps == nil {
		ps = &partitionSeries{
			names:     map[string]struct{}{},
			series:    map[uint64]time.Time{},
			perMetric: map[string]map[uint64]struct{}{},
		}
		t.partitions[name] = ps
	}
	return ps
}

func (t *SeriesTracker) sweepLocked(ps *partitionSeries, now time.Time) {
	if now.Sub(ps.lastSweep) < 5*time.Minute {
		return
	}
	ps.lastSweep = now
	cutoff := now.Add(-t.window)
	for hash, seen := range ps.series {
		if seen.Before(cutoff) {
			delete(ps.series, hash)
		}
	}
	// Drop swept series from the per-metric sets as well.
	for name, set := range ps.perMetric {
		for hash := range set {
			if _, ok := ps.series[hash]; !ok {
				delete(set, hash)
			}
		}
		if len(set) == 0 {
			delete(ps.perMetric, name)
		}
	}
}

// Counts returns (metric names, active series) for a partition.
func (t *SeriesTracker) Counts(partition string) (int, int) {
	if t == nil {
		return 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	ps := t.partitions[strings.TrimSpace(partition)]
	if ps == nil {
		return 0, 0
	}
	return len(ps.names), len(ps.series)
}

func seriesLabels(rec segment.MetricPointRecord) map[string]string {
	labels := make(map[string]string, len(rec.Attributes)+len(rec.ResourceAttributes))
	for key, value := range rec.ResourceAttributes {
		if _, skip := unboundedLabels[key]; skip {
			continue
		}
		labels[key] = value
	}
	for key, value := range rec.Attributes {
		if _, skip := unboundedLabels[key]; skip {
			continue
		}
		labels[key] = value
	}
	return labels
}

func seriesHash(name string, labels map[string]string) uint64 {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	_, _ = h.Write([]byte{0})
	for _, key := range keys {
		_, _ = h.Write([]byte(key))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(labels[key]))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}
