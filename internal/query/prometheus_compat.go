package query

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	promlabels "github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	promparser "github.com/prometheus/prometheus/promql/parser"
	promstorage "github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
	"github.com/radryc/doctor/internal/buildinfo"
	"github.com/radryc/doctor/internal/segment"
)

const (
	doctorPrometheusLookback = 5 * time.Minute
	doctorPrometheusTimeout  = 30 * time.Second
	// maxPrometheusQueryTimeout caps the opt-in ?timeout= value. Normal queries
	// keep the router's short backend timeout; callers that know a query is
	// expensive can request up to this limit for one request.
	maxPrometheusQueryTimeout = 5 * time.Minute
	doctorDiscoveryWindow     = 24 * time.Hour
	// doctorDiscoveryScanWindow caps how far back label/series discovery scans.
	// The label vocabulary is stable, so a short recent window yields the same
	// names while keeping the scan (and its memory) small even when Grafana asks
	// for hours or days.
	doctorDiscoveryScanWindow        = 15 * time.Minute
	doctorMetricDiscoveryMatcherName = "__doctor_discovery__"
	doctorMetricDiscoveryModeNames   = "metric_names"
	doctorMetricDiscoveryModeLabels  = "label_names"
	doctorMetricDiscoveryModeValues  = "label_values:"

	// maxDiscoveryScannedRecords bounds how many samples a series-discovery scan
	// reads before stopping. It is a safety valve; the projected label sets are
	// what callers consume.
	maxDiscoveryScannedRecords = 10000

	// maxQueryMetricNames bounds how many stored metric names a single
	// `__name__` matcher (e.g. a regex) may expand to before the query is
	// rejected.
	maxQueryMetricNames = 5_000

	// maxMetricRangePoints bounds the legacy /v1/metrics/range endpoint.
	maxMetricRangePoints = 200_000
)

// maxQueryLoadedSamples and maxQueryLoadedSeries bound how much a PromQL query
// may pull into memory. Samples stream straight into the series index (never
// buffered as records) and the query fails with a 422 once either budget is
// exhausted, so a broad selector cannot OOM the query service.
//
// The defaults accommodate whole-fleet metadata aggregations such as the
// Metrics Drilldown landing query
// (count by (__name__) ({__name__=~".+"})) which scans every stored series in
// the window. Operators can tune them with DOCTOR_PROM_MAX_SAMPLES and
// DOCTOR_PROM_MAX_SERIES.
var (
	maxQueryLoadedSamples = envInt64("DOCTOR_PROM_MAX_SAMPLES", 4_000_000)
	maxQueryLoadedSeries  = int(envInt64("DOCTOR_PROM_MAX_SERIES", 100_000))
)

// maxGlobalLoadedSamples bounds the samples held across ALL concurrent PromQL
// queries. Each querier accounts for the samples it streams and releases them
// on Close, so a burst of broad queries cannot multiply the per-query budget
// into an OOM. 8M samples is roughly 130MB of transient point data.
var (
	maxGlobalLoadedSamples = envInt64("DOCTOR_PROM_GLOBAL_MAX_SAMPLES", 8_000_000)
	globalLoadedSamples    atomic.Int64
)

// envInt64 reads a positive integer environment variable with a fallback.
func envInt64(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// errMetricQueryTooBroad is returned internally while streaming once a query
// exceeds its sample or series budget. It is translated to
// promql.ErrTooManySamples (HTTP 422) at the query boundary.
var errMetricQueryTooBroad = errors.New("metric query exceeds the configured sample/series budget")

// errDiscoveryScanLimit is returned internally when a discovery scan hits its
// record budget and is treated as a successful partial result.
var errDiscoveryScanLimit = errors.New("metric discovery scan limit reached")

// doctorDiscoveryQuery builds a manifest-backed discovery query so that label
// and metric-name lookups read chunk metadata instead of scanning samples.
func doctorDiscoveryQuery(mode string, matchers []*promlabels.Matcher) segment.MetricQuery {
	query := metricQueryFromMatchers(matchers)
	if name := exactPromMetricNameMatcher(matchers); name != "" {
		query.MetricName = name
	}
	query.LabelMatchers = append(query.LabelMatchers, segment.MetricLabelMatcher{
		Name:  doctorMetricDiscoveryMatcherName,
		Value: mode,
		Type:  segment.MetricMatchEqual,
	})
	return query
}

type doctorPromQueryable struct {
	backend   LogEngineBackend
	tenant    string
	partition string
}

func (q doctorPromQueryable) Querier(mint, maxt int64) (promstorage.Querier, error) {
	return &doctorPromQuerier{
		backend:   q.backend,
		tenant:    q.tenant,
		partition: strings.TrimSpace(q.partition),
		mint:      mint,
		maxt:      maxt,
	}, nil
}

type doctorPromQuerier struct {
	backend   LogEngineBackend
	tenant    string
	partition string
	mint      int64
	maxt      int64

	// loadedSamples counts the samples this querier holds against the global
	// budget; released on Close.
	loadedSamples atomic.Int64

	mu      sync.Mutex
	indexes map[string]*doctorPromIndex
	errs    map[string]error
}

type doctorPromIndex struct {
	series []doctorPromSeries
}

type doctorPromSeries struct {
	labels promlabels.Labels
	series promstorage.Series
}

type doctorPromSeriesSet struct {
	series []promstorage.Series
	index  int
}

func (s *doctorPromSeriesSet) Next() bool {
	if s.index >= len(s.series) {
		return false
	}
	s.index++
	return true
}

func (s *doctorPromSeriesSet) At() promstorage.Series {
	if s.index == 0 || s.index > len(s.series) {
		return nil
	}
	return s.series[s.index-1]
}

func (s *doctorPromSeriesSet) Err() error {
	return nil
}

func (s *doctorPromSeriesSet) Warnings() annotations.Annotations {
	return nil
}

func (q *doctorPromQuerier) Select(ctx context.Context, sortSeries bool, _ *promstorage.SelectHints, matchers ...*promlabels.Matcher) promstorage.SeriesSet {
	index, err := q.load(ctx, matchers)
	if err != nil {
		return promstorage.ErrSeriesSet(err)
	}

	selected := make([]promstorage.Series, 0, len(index.series))
	for _, series := range index.series {
		if q.seriesMatchesPartition(series.labels) && metricLabelsMatch(series.labels, matchers) {
			selected = append(selected, series.series)
		}
	}

	if len(selected) == 0 {
		return promstorage.EmptySeriesSet()
	}

	if sortSeries {
		sort.Slice(selected, func(i, j int) bool {
			return promlabels.Compare(selected[i].Labels(), selected[j].Labels()) < 0
		})
	}

	return &doctorPromSeriesSet{series: selected}
}

func (q *doctorPromQuerier) LabelValues(ctx context.Context, name string, _ *promstorage.LabelHints, matchers ...*promlabels.Matcher) ([]string, annotations.Annotations, error) {
	if name == promlabels.MetricName && !hasPromMatchers(matchers) {
		return q.discoverMetricNames(ctx)
	}

	discovered, err := q.discoverLabelValues(ctx, name, matchers)
	if err == nil {
		// Manifest-backed discovery sees the stored label spelling. Resource
		// attributes keep their dotted OTel names (guardian.pusher) while the
		// PromQL/API label name is normalized (guardian_pusher); query both so
		// local and federated values are both returned.
		if dotted := dottedLabelName(name); dotted != name {
			if more, dottedErr := q.discoverLabelValues(ctx, dotted, matchers); dottedErr == nil {
				discovered = append(discovered, more...)
			}
		}
		if len(discovered) > 0 {
			set := make(map[string]struct{}, len(discovered))
			for _, value := range discovered {
				if value != "" {
					set[value] = struct{}{}
				}
			}
			return sortedStrings(set), nil, nil
		}
	}

	// Manifest-backed discovery only sees metric attributes. Resource-derived
	// labels such as service and guardian_partition can legitimately return an
	// empty set there, so fall back to scanning series labels.
	values := make(map[string]struct{})
	err = q.forEachSeriesLabels(ctx, matchers, func(labelSet promlabels.Labels) {
		if value := labelSet.Get(name); value != "" {
			values[value] = struct{}{}
		}
	})
	if err != nil {
		return nil, nil, err
	}

	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil, nil
}

// discoverLabelValues uses the backend's manifest-backed label discovery, which
// reads chunk metadata rather than samples, so it stays fast and bounded even
// for high-cardinality labels.
func (q *doctorPromQuerier) discoverLabelValues(ctx context.Context, name string, matchers []*promlabels.Matcher) ([]string, error) {
	if name == "" {
		return nil, fmt.Errorf("label name is required")
	}
	values := make(map[string]struct{})
	scanned := 0
	err := q.backend.StreamMetrics(ctx, q.tenant, doctorDiscoveryQuery(doctorMetricDiscoveryModeValues+name, matchers), timeFromPromMillis(q.mint), timeFromPromMillis(q.maxt), func(record segment.MetricPointRecord) error {
		if scanned >= maxDiscoveryScannedRecords {
			return errDiscoveryScanLimit
		}
		scanned++
		labelSet := metricPointLabels(record)
		if !q.seriesMatchesPartition(labelSet) {
			return nil
		}
		if value := labelSet.Get(name); value != "" {
			values[value] = struct{}{}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDiscoveryScanLimit) {
		return nil, err
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

// forEachSeriesLabels prefers the per-chunk series index, which carries the
// same projected label set as a sample but avoids decoding every point. It falls
// back to the legacy sample scan when the store has no series index (for
// example, chunks written before the index existed).
func (q *doctorPromQuerier) forEachSeriesLabels(ctx context.Context, matchers []*promlabels.Matcher, yield func(promlabels.Labels)) error {
	indexed, err := q.forEachSeriesLabelsFromIndex(ctx, matchers, yield)
	if err != nil {
		return err
	}
	if indexed > 0 {
		return nil
	}
	return q.forEachSeriesLabelsFromSamples(ctx, matchers, yield)
}

// forEachSeriesLabelsFromSamples is the legacy fallback: it streams metric
// samples and deduplicates their projected label sets. It is bounded by
// maxDiscoveryScannedRecords, so it is only appropriate for stores without a
// series index.
func (q *doctorPromQuerier) forEachSeriesLabelsFromSamples(ctx context.Context, matchers []*promlabels.Matcher, yield func(promlabels.Labels)) error {
	metricQuery := metricQueryFromMatchers(matchers)
	requestedMetricName := exactPromMetricNameMatcher(matchers)
	from := timeFromPromMillis(q.mint)
	to := timeFromPromMillis(q.maxt)

	seen := make(map[string]struct{})
	scanned := 0
	emit := func(point segment.MetricPointRecord) error {
		// Bound the scan: a pathological high-cardinality metric must not be
		// able to exhaust memory before the caller's limit is reached.
		if scanned >= maxDiscoveryScannedRecords {
			return errDiscoveryScanLimit
		}
		scanned++
		labelSet := metricPointLabels(point)
		if !q.seriesMatchesPartition(labelSet) || !metricLabelsMatch(labelSet, matchers) {
			return nil
		}
		key := string(labelSet.Bytes(nil))
		if _, ok := seen[key]; ok {
			return nil
		}
		seen[key] = struct{}{}
		yield(labelSet)
		return nil
	}

	stream := func(query segment.MetricQuery) error {
		err := q.backend.StreamMetrics(ctx, q.tenant, query, from, to, emit)
		if errors.Is(err, errDiscoveryScanLimit) {
			return nil
		}
		return err
	}

	if requestedMetricName == "" {
		return stream(metricQuery)
	}

	rawNames, err := q.resolveRawMetricNames(ctx, requestedMetricName)
	if err != nil {
		return err
	}
	if len(rawNames) == 0 {
		rawNames = []string{requestedMetricName}
	}
	for _, rawName := range rawNames {
		aliasQuery := metricQuery
		aliasQuery.MetricName = rawName
		if err := stream(aliasQuery); err != nil {
			return err
		}
	}
	return nil
}

// seriesLabelMaps returns the distinct label sets of all matching series
// without materializing samples.
func (q *doctorPromQuerier) seriesLabelMaps(ctx context.Context, matchers []*promlabels.Matcher) ([]map[string]string, error) {
	out := make([]map[string]string, 0)
	err := q.forEachSeriesLabels(ctx, matchers, func(labelSet promlabels.Labels) {
		out = append(out, promLabelsToMap(labelSet))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (q *doctorPromQuerier) discoverMetricNames(ctx context.Context) ([]string, annotations.Annotations, error) {
	values := make(map[string]struct{})
	if q.partition != "" {
		err := q.forEachSeriesLabels(ctx, nil, func(labelSet promlabels.Labels) {
			if name := labelSet.Get(promlabels.MetricName); name != "" {
				values[name] = struct{}{}
			}
		})
		if err != nil {
			return nil, nil, err
		}
		out := make([]string, 0, len(values))
		for value := range values {
			out = append(out, value)
		}
		sort.Strings(out)
		return out, nil, nil
	}

	points, err := q.backend.QueryMetrics(ctx, q.tenant, segment.MetricQuery{
		LabelMatchers: []segment.MetricLabelMatcher{{
			Name:  doctorMetricDiscoveryMatcherName,
			Value: doctorMetricDiscoveryModeNames,
			Type:  segment.MetricMatchEqual,
		}},
	}, timeFromPromMillis(q.mint), timeFromPromMillis(q.maxt))
	if err != nil {
		return nil, nil, err
	}

	for _, point := range points {
		name := normalizePrometheusMetricName(point.MetricName)
		if name == "" {
			continue
		}
		values[name] = struct{}{}
	}

	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil, nil
}

func (q *doctorPromQuerier) LabelNames(ctx context.Context, _ *promstorage.LabelHints, matchers ...*promlabels.Matcher) ([]string, annotations.Annotations, error) {
	if names, err := q.discoverLabelNames(ctx, matchers); err == nil {
		return names, nil, nil
	}

	names := make(map[string]struct{})
	err := q.forEachSeriesLabels(ctx, matchers, func(labelSet promlabels.Labels) {
		labelSet.Range(func(label promlabels.Label) {
			names[label.Name] = struct{}{}
		})
	})
	if err != nil {
		return nil, nil, err
	}

	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil, nil
}

// discoverLabelNames uses manifest-backed label discovery so /api/v1/labels
// reads chunk metadata instead of scanning every metric sample.
func (q *doctorPromQuerier) discoverLabelNames(ctx context.Context, matchers []*promlabels.Matcher) ([]string, error) {
	names := make(map[string]struct{})
	scanned := 0
	err := q.backend.StreamMetrics(ctx, q.tenant, doctorDiscoveryQuery(doctorMetricDiscoveryModeLabels, matchers), timeFromPromMillis(q.mint), timeFromPromMillis(q.maxt), func(record segment.MetricPointRecord) error {
		if scanned >= maxDiscoveryScannedRecords {
			return errDiscoveryScanLimit
		}
		scanned++
		labelSet := metricPointLabels(record)
		if !q.seriesMatchesPartition(labelSet) {
			return nil
		}
		labelSet.Range(func(label promlabels.Label) {
			if label.Name != "" {
				names[label.Name] = struct{}{}
			}
		})
		return nil
	})
	if err != nil && !errors.Is(err, errDiscoveryScanLimit) {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (q *doctorPromQuerier) Close() error {
	// Release the global sample budget this querier accounted for. The counter
	// is swapped so repeated Close calls are harmless.
	if released := q.loadedSamples.Swap(0); released > 0 {
		globalLoadedSamples.Add(-released)
	}
	return nil
}

func (q *doctorPromQuerier) load(ctx context.Context, matchers []*promlabels.Matcher) (*doctorPromIndex, error) {
	cacheKey := matcherCacheKey(matchers)

	q.mu.Lock()
	if index, ok := q.indexes[cacheKey]; ok {
		q.mu.Unlock()
		return index, nil
	}
	if err, ok := q.errs[cacheKey]; ok {
		q.mu.Unlock()
		return nil, err
	}
	q.mu.Unlock()

	index, err := q.buildIndex(ctx, matchers)
	if err != nil {
		q.mu.Lock()
		if q.errs == nil {
			q.errs = make(map[string]error)
		}
		q.errs[cacheKey] = err
		q.mu.Unlock()
		return nil, err
	}

	q.mu.Lock()
	if q.indexes == nil {
		q.indexes = make(map[string]*doctorPromIndex)
	}
	delete(q.errs, cacheKey)
	q.indexes[cacheKey] = index
	q.mu.Unlock()
	return index, nil
}

// buildIndex streams matching metric points straight into the PromQL series
// index. It never buffers raw records (unlike a QueryMetrics call), and it stops
// with errMetricQueryTooBroad once the sample or series budget is exhausted so a
// broad selector cannot exhaust memory.
//
// `__name__` matchers are expanded to exact stored names first. When the
// expansion is small the store is scanned once per name so narrow selectors
// only read matching chunks; when it is large (for example the Metrics
// Drilldown landing query `{__name__=~".+"}`) the store is scanned once and the
// name set is applied client-side, because per-name scans re-read every
// candidate chunk's parquet data once per metric name.
func (q *doctorPromQuerier) buildIndex(ctx context.Context, matchers []*promlabels.Matcher) (*doctorPromIndex, error) {
	from := timeFromPromMillis(q.mint)
	to := timeFromPromMillis(q.maxt)

	names, err := q.resolveStorageMetricNames(ctx, matchers, from, to)
	if err != nil {
		return nil, err
	}
	if names != nil && len(names) == 0 {
		// The selector named metrics that the store does not have; do not scan.
		return emptyDoctorPromIndex(), nil
	}

	base := segment.MetricQuery{Service: exactServiceMatcher(matchers)}

	builders := make(map[string]*promql.Series)
	labelsByKey := make(map[string]promlabels.Labels)
	samples := 0
	emit := func(point segment.MetricPointRecord) error {
		if !point.HasValue || point.MetricName == "" || point.Timestamp.IsZero() {
			return nil
		}
		samples++
		if int64(samples) > maxQueryLoadedSamples {
			return errMetricQueryTooBroad
		}
		if globalLoadedSamples.Add(1) > maxGlobalLoadedSamples {
			globalLoadedSamples.Add(-1)
			return errMetricQueryTooBroad
		}
		q.loadedSamples.Add(1)
		labelSet := metricPointLabels(point)
		key := string(labelSet.Bytes(nil))
		series := builders[key]
		if series == nil {
			if len(builders) >= maxQueryLoadedSeries {
				return errMetricQueryTooBroad
			}
			series = &promql.Series{Metric: labelSet.Copy()}
			builders[key] = series
			labelsByKey[key] = labelSet.Copy()
		}
		series.Floats = append(series.Floats, promql.FPoint{
			T: point.Timestamp.UTC().UnixMilli(),
			F: point.Value,
		})
		return nil
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if names == nil {
		if err := q.backend.StreamMetrics(streamCtx, q.tenant, base, from, to, emit); err != nil {
			return nil, translateMetricLoadError(err)
		}
		return finalizeDoctorPromIndex(builders, labelsByKey), nil
	}

	if len(names) > singleScanMetricNameThreshold {
		// One scan, filtered client-side: avoids re-reading every candidate
		// chunk once per metric name.
		nameSet := make(map[string]struct{}, len(names))
		for _, name := range names {
			nameSet[name] = struct{}{}
		}
		filtered := func(point segment.MetricPointRecord) error {
			if _, ok := nameSet[point.MetricName]; !ok {
				return nil
			}
			return emit(point)
		}
		if err := q.backend.StreamMetrics(streamCtx, q.tenant, base, from, to, filtered); err != nil {
			return nil, translateMetricLoadError(err)
		}
		return finalizeDoctorPromIndex(builders, labelsByKey), nil
	}

	for _, name := range names {
		query := base
		query.MetricName = name
		if err := q.backend.StreamMetrics(streamCtx, q.tenant, query, from, to, emit); err != nil {
			return nil, translateMetricLoadError(err)
		}
	}
	return finalizeDoctorPromIndex(builders, labelsByKey), nil
}

// singleScanMetricNameThreshold is the expansion size above which a selector is
// evaluated with one unfiltered store scan plus a client-side name filter.
const singleScanMetricNameThreshold = 16

// resolveStorageMetricNames expands the query's `__name__` matchers (exact or
// regex) into the exact raw names the store uses. A nil result means the query
// carries no name constraint and the store should be scanned unfiltered.
func (q *doctorPromQuerier) resolveStorageMetricNames(ctx context.Context, matchers []*promlabels.Matcher, from, to time.Time) ([]string, error) {
	nameMatchers := make([]*promlabels.Matcher, 0, 1)
	for _, matcher := range matchers {
		if matcher != nil && matcher.Name == promlabels.MetricName {
			nameMatchers = append(nameMatchers, matcher)
		}
	}
	if len(nameMatchers) == 0 {
		return nil, nil
	}

	rawNames, err := q.discoverRawMetricNames(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rawNames))
	for _, rawName := range rawNames {
		normalized := normalizePrometheusMetricName(rawName)
		matches := true
		for _, matcher := range nameMatchers {
			if !matcher.Matches(normalized) {
				matches = false
				break
			}
		}
		if matches {
			out = append(out, rawName)
		}
		if len(out) > maxQueryMetricNames {
			return nil, promql.ErrTooManySamples("metric name matcher selects too many metrics; narrow the selector")
		}
	}
	sort.Strings(out)
	return out, nil
}

// discoverRawMetricNames reads the distinct stored metric names without scanning
// samples.
func (q *doctorPromQuerier) discoverRawMetricNames(ctx context.Context, from, to time.Time) ([]string, error) {
	names := make([]string, 0)
	seen := make(map[string]struct{})
	scanned := 0
	err := q.backend.StreamMetrics(ctx, q.tenant, segment.MetricQuery{
		LabelMatchers: []segment.MetricLabelMatcher{{
			Name:  doctorMetricDiscoveryMatcherName,
			Value: doctorMetricDiscoveryModeNames,
			Type:  segment.MetricMatchEqual,
		}},
	}, from, to, func(record segment.MetricPointRecord) error {
		scanned++
		if scanned > maxDiscoveryScannedRecords {
			return errDiscoveryScanLimit
		}
		name := strings.TrimSpace(record.MetricName)
		if name == "" {
			return nil
		}
		if _, ok := seen[name]; ok {
			return nil
		}
		seen[name] = struct{}{}
		names = append(names, name)
		return nil
	})
	if err != nil && !errors.Is(err, errDiscoveryScanLimit) {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// translateMetricLoadError converts the internal budget error into the Prometheus
// sample-limit error so callers return HTTP 422 instead of a 500.
func translateMetricLoadError(err error) error {
	if errors.Is(err, errMetricQueryTooBroad) {
		return promql.ErrTooManySamples("metric query matches too many samples; narrow the time range or add a metric name")
	}
	return err
}

// matcherCacheKey identifies a query by its label matchers. Unlike the previous
// metric-query key it includes `__name__` matchers, which now select different
// exact metric names.
func matcherCacheKey(matchers []*promlabels.Matcher) string {
	parts := make([]string, 0, len(matchers))
	for _, matcher := range matchers {
		if matcher == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s\x01%d\x01%s", matcher.Name, matcher.Type, matcher.Value))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

func exactServiceMatcher(matchers []*promlabels.Matcher) string {
	for _, matcher := range matchers {
		if matcher != nil && matcher.Name == "service" && matcher.Type == promlabels.MatchEqual {
			return matcher.Value
		}
	}
	return ""
}

func (q *doctorPromQuerier) resolveRawMetricNames(ctx context.Context, requestedMetricName string) ([]string, error) {
	points, err := q.backend.QueryMetrics(ctx, q.tenant, segment.MetricQuery{
		LabelMatchers: []segment.MetricLabelMatcher{{
			Name:  doctorMetricDiscoveryMatcherName,
			Value: doctorMetricDiscoveryModeNames,
			Type:  segment.MetricMatchEqual,
		}},
	}, timeFromPromMillis(q.mint), timeFromPromMillis(q.maxt))
	if err != nil {
		return nil, err
	}

	wanted := normalizePrometheusMetricName(requestedMetricName)
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, point := range points {
		rawName := strings.TrimSpace(point.MetricName)
		if rawName == "" {
			continue
		}
		if rawName != requestedMetricName && normalizePrometheusMetricName(rawName) != wanted {
			continue
		}
		if _, ok := seen[rawName]; ok {
			continue
		}
		seen[rawName] = struct{}{}
		out = append(out, rawName)
	}
	sort.Strings(out)
	return out, nil
}

// finalizeDoctorPromIndex turns the streamed series builders into the sorted
// index the querier serves.
func finalizeDoctorPromIndex(builders map[string]*promql.Series, labelsByKey map[string]promlabels.Labels) *doctorPromIndex {
	seriesList := make([]doctorPromSeries, 0, len(builders))
	for key, built := range builders {
		if len(built.Floats) == 0 {
			continue
		}
		sort.Slice(built.Floats, func(i, j int) bool {
			return built.Floats[i].T < built.Floats[j].T
		})
		built.Floats = dedupePromFloatPoints(built.Floats)
		seriesList = append(seriesList, doctorPromSeries{
			labels: labelsByKey[key],
			series: promql.NewStorageSeries(*built),
		})
	}

	sort.Slice(seriesList, func(i, j int) bool {
		return promlabels.Compare(seriesList[i].labels, seriesList[j].labels) < 0
	})

	return &doctorPromIndex{series: seriesList}
}

func emptyDoctorPromIndex() *doctorPromIndex {
	return &doctorPromIndex{series: []doctorPromSeries{}}
}

// metricQueryFromMatchers builds the storage-level query for streaming.
//
// Only `service` is pushed down: the store keys its metric labels by their raw
// (dotted) names, while the Prometheus vocabulary this API exposes is
// normalized, so pushing a normalized label matcher (`guardian_partition`) would
// match nothing in the store. Labels are instead applied in Select against the
// normalized series labels. `__name__` matchers are handled separately by
// resolveStorageMetricNames so they can expand to exact stored names.
func metricQueryFromMatchers(matchers []*promlabels.Matcher) segment.MetricQuery {
	query := segment.MetricQuery{}
	for _, matcher := range matchers {
		if matcher == nil {
			continue
		}
		if matcher.Name == "service" && matcher.Type == promlabels.MatchEqual && query.Service == "" {
			query.Service = matcher.Value
		}
	}
	return query
}

func exactPromMetricNameMatcher(matchers []*promlabels.Matcher) string {
	for _, matcher := range matchers {
		if matcher == nil {
			continue
		}
		if matcher.Name == promlabels.MetricName && matcher.Type == promlabels.MatchEqual {
			return strings.TrimSpace(matcher.Value)
		}
	}
	return ""
}

func dedupePromFloatPoints(points []promql.FPoint) []promql.FPoint {
	if len(points) < 2 {
		return points
	}

	out := points[:0]
	for _, point := range points {
		if len(out) > 0 && out[len(out)-1].T == point.T {
			out[len(out)-1] = point
			continue
		}
		out = append(out, point)
	}
	return out
}

func metricPointLabels(point segment.MetricPointRecord) promlabels.Labels {
	attrs := mergeStringMaps(point.ResourceAttributes, point.Attributes)
	for key := range attrs {
		if segment.IsUnboundedMetricLabel(key) {
			delete(attrs, key)
		}
	}
	values := canonicalLabels(point.Service, point.Tenant, point.Partition, point.Intent, point.Asset, attrs)
	values[promlabels.MetricName] = normalizePrometheusMetricName(point.MetricName)
	return promlabels.FromMap(values)
}

func normalizePrometheusMetricName(name string) string {
	if name == "" {
		return ""
	}

	var builder strings.Builder
	lastUnderscore := false
	for _, r := range name {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == ':'
		if valid {
			builder.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			builder.WriteByte('_')
			lastUnderscore = true
		}
	}

	out := strings.Trim(builder.String(), "_")
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

func normalizeMetricLabelName(name string) string {
	if name == "" {
		return ""
	}
	if name == promlabels.MetricName {
		return name
	}

	var builder strings.Builder
	lastUnderscore := false
	for _, r := range name {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
		if valid {
			builder.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			builder.WriteByte('_')
			lastUnderscore = true
		}
	}

	out := strings.Trim(builder.String(), "_")
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

func (q *doctorPromQuerier) seriesMatchesPartition(labelSet promlabels.Labels) bool {
	if q == nil || q.partition == "" {
		return true
	}
	return labelSet.Get(labelPartition) == q.partition
}

func metricLabelsMatch(labelSet promlabels.Labels, matchers []*promlabels.Matcher) bool {
	for _, matcher := range matchers {
		if matcher == nil {
			continue
		}
		if !matcher.Matches(labelSet.Get(matcher.Name)) {
			return false
		}
	}
	return true
}

func hasPromMatchers(matchers []*promlabels.Matcher) bool {
	for _, matcher := range matchers {
		if matcher != nil {
			return true
		}
	}
	return false
}

// requestPrometheusContext applies the optional Prometheus-compatible
// ?timeout= duration. The default remains the router's short backend timeout;
// callers opt into a longer context explicitly for expensive queries.
func requestPrometheusContext(r *http.Request) (context.Context, context.CancelFunc, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("timeout"))
	if raw == "" {
		return r.Context(), func() {}, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return nil, nil, fmt.Errorf("invalid timeout %q: use a duration such as 30s or 2m", raw)
	}
	if timeout > maxPrometheusQueryTimeout {
		timeout = maxPrometheusQueryTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	return ctx, cancel, nil
}

func (s *Service) handlePrometheusQuery(w http.ResponseWriter, r *http.Request) {
	queryString := requestParam(r, "query")
	if queryString == "" {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "query is required")
		return
	}

	ctx, cancel, timeoutErr := requestPrometheusContext(r)
	if timeoutErr != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", timeoutErr.Error())
		return
	}
	defer cancel()
	r = r.WithContext(ctx)

	evaluationTime, err := parsePrometheusTime(requestParam(r, "time"), time.Now().UTC())
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}

	if s.proxyRouter != nil {
		s.handlePrometheusQueryViaRouter(w, r, queryString, evaluationTime)
		return
	}

	forwarded, mode := awsQueryMode(queryString)
	tenant := s.tenantFromRequest(r)
	partition := partitionFromRequest(r)

	var localData map[string]any
	if mode != awsQueryOnly {
		data, execErr, parseErr := s.execPrometheusInstant(r.Context(), tenant, partition, queryString, evaluationTime)
		if execErr != nil {
			if parseErr {
				writePrometheusError(w, http.StatusBadRequest, "bad_data", execErr.Error())
			} else {
				writePrometheusExecutionError(w, execErr)
			}
			return
		}
		localData = data
	}

	awsQuery := forwarded
	queryAWS := mode != awsQueryNone && s.awsReady()
	if !queryAWS && mode == awsQueryNone && s.awsReady() && prometheusResultEmpty(localData) {
		// Transparent fallback: a selector that resolved no local series may
		// name a CloudWatch metric surfaced by metric-name discovery. Forward it
		// verbatim so browsing AWS metrics in Grafana works without an explicit
		// `source="aws"` matcher.
		awsQuery = queryString
		queryAWS = true
	}

	var awsData map[string]any
	if queryAWS {
		if resp, awsErr := s.aws.PrometheusQuery(r.Context(), awsQuery, evaluationTime); awsErr == nil {
			awsData = extractPrometheusData(resp)
		}
	}

	writePrometheusSuccess(w, filterPrometheusDataByIdentity(r, mergePrometheusQueryData(localData, awsData, "vector")))
}

func (s *Service) handlePrometheusQueryRange(w http.ResponseWriter, r *http.Request) {
	queryString := requestParam(r, "query")
	if queryString == "" {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "query is required")
		return
	}

	ctx, cancel, timeoutErr := requestPrometheusContext(r)
	if timeoutErr != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", timeoutErr.Error())
		return
	}
	defer cancel()
	r = r.WithContext(ctx)

	end, err := parsePrometheusTime(requestParam(r, "end"), time.Now().UTC())
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	start, err := parsePrometheusTime(requestParam(r, "start"), end.Add(-1*time.Hour))
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	step, err := parsePrometheusStep(requestParam(r, "step"))
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if !end.After(start) {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "end must be after start")
		return
	}

	if s.proxyRouter != nil {
		s.handlePrometheusQueryRangeViaRouter(w, r, queryString, start, end, step)
		return
	}

	forwarded, mode := awsQueryMode(queryString)
	tenant := s.tenantFromRequest(r)
	partition := partitionFromRequest(r)

	var localData map[string]any
	if mode != awsQueryOnly {
		data, execErr, parseErr := s.execPrometheusRange(r.Context(), tenant, partition, queryString, start, end, step)
		if execErr != nil {
			if parseErr {
				writePrometheusError(w, http.StatusBadRequest, "bad_data", execErr.Error())
			} else {
				writePrometheusExecutionError(w, execErr)
			}
			return
		}
		localData = data
	}

	awsQuery := forwarded
	queryAWS := mode != awsQueryNone && s.awsReady()
	if !queryAWS && mode == awsQueryNone && s.awsReady() && prometheusResultEmpty(localData) {
		// See handlePrometheusQuery: fall back to CloudWatch when a selector
		// resolves no local series so discovered AWS metrics remain queryable.
		awsQuery = queryString
		queryAWS = true
	}

	var awsData map[string]any
	if queryAWS {
		if resp, awsErr := s.aws.PrometheusQueryRange(r.Context(), awsQuery, start, end, step); awsErr == nil {
			awsData = extractPrometheusData(resp)
		}
	}

	writePrometheusSuccess(w, filterPrometheusDataByIdentity(r, mergePrometheusQueryData(localData, awsData, "matrix")))
}

func (s *Service) handlePrometheusLabels(w http.ResponseWriter, r *http.Request) {
	partition := partitionFromRequest(r)
	matchValues := requestParams(r, "match[]")
	hasAWS, forwardedMatches := awsMatches(matchValues)
	awsEnabled := s.awsReady() && (partition == "" || s.awsPartitionVisible(r.Context(), partition))

	selectorSets, selErr := parsePrometheusSelectorSets(matchValues)
	if selErr != nil && !hasAWS && !awsEnabled {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", selErr.Error())
		return
	}

	names := make(map[string]struct{})
	if selErr == nil {
		querier, err := s.discoveryPrometheusQuerier(r)
		if err != nil && !hasAWS && !awsEnabled {
			writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
		if err == nil {
			defer func() { _ = querier.Close() }()
			if len(selectorSets) == 0 {
				values, _, labelErr := querier.LabelNames(r.Context(), nil)
				if labelErr != nil {
					writePrometheusDiscoveryError(w, labelErr)
					return
				}
				for _, value := range values {
					names[value] = struct{}{}
				}
			} else {
				for _, selectorSet := range selectorSets {
					values, _, labelErr := querier.LabelNames(r.Context(), nil, selectorSet...)
					if labelErr != nil {
						writePrometheusDiscoveryError(w, labelErr)
						return
					}
					for _, value := range values {
						names[value] = struct{}{}
					}
				}
			}
		}
	}

	if awsEnabled {
		start, end := prometheusDiscoveryRange(r)
		if resp, awsErr := s.aws.PrometheusLabels(r.Context(), forwardedMatches, start, end); awsErr == nil {
			for _, value := range extractPrometheusStringList(resp) {
				names[value] = struct{}{}
			}
		}
		// `source` is a Doctor-injected label, so CloudWatch cannot report it:
		// always surface the federated partition value when AWS is enabled.
		names[labelSource] = struct{}{}
	}

	writePrometheusSuccess(w, sortedStrings(names))
}

func (s *Service) handlePrometheusLabelValues(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/values") {
		http.NotFound(w, r)
		return
	}

	rawLabelName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/label/"), "/values")
	if rawLabelName == "" {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "label name is required")
		return
	}
	labelName, err := url.PathUnescape(rawLabelName)
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}

	partition := partitionFromRequest(r)
	matchValues := requestParams(r, "match[]")
	hasAWS, forwardedMatches := awsMatches(matchValues)
	awsEnabled := s.awsReady() && (partition == "" || s.awsPartitionVisible(r.Context(), partition))

	selectorSets, selErr := parsePrometheusSelectorSets(matchValues)
	if selErr != nil && !hasAWS && !awsEnabled {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", selErr.Error())
		return
	}

	values := make(map[string]struct{})
	if labelName == labelSource {
		values[sourceDoctor] = struct{}{}
		if awsEnabled {
			values[awsSourceLabel] = struct{}{}
		}
	}
	if selErr == nil {
		querier, err := s.discoveryPrometheusQuerier(r)
		if err != nil && !hasAWS && !awsEnabled {
			writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
		if err == nil {
			defer func() { _ = querier.Close() }()
			if len(selectorSets) == 0 {
				results, _, labelErr := querier.LabelValues(r.Context(), labelName, nil)
				if labelErr != nil {
					writePrometheusDiscoveryError(w, labelErr)
					return
				}
				for _, value := range results {
					values[value] = struct{}{}
				}
			} else {
				for _, selectorSet := range selectorSets {
					results, _, labelErr := querier.LabelValues(r.Context(), labelName, nil, selectorSet...)
					if labelErr != nil {
						writePrometheusDiscoveryError(w, labelErr)
						return
					}
					for _, value := range results {
						values[value] = struct{}{}
					}
				}
			}
		}
	}

	if awsEnabled {
		start, end := prometheusDiscoveryRange(r)
		if resp, awsErr := s.aws.PrometheusLabelValues(r.Context(), labelName, forwardedMatches, start, end); awsErr == nil {
			for _, value := range extractPrometheusStringList(resp) {
				values[value] = struct{}{}
			}
		}
		if labelName == promlabels.MetricName {
			// CloudWatch's PromQL API has no label-values route, so fall back to
			// ListMetrics to populate the Grafana metric-name browser.
			if partition := partitionFromRequest(r); partition == "" || s.awsPartitionVisible(r.Context(), partition) {
				for _, name := range s.awsMetricNames(r.Context()) {
					values[name] = struct{}{}
				}
			}
		} else if len(forwardedMatches) > 0 {
			// Derive label values from series when a metric selector is
			// available; CloudWatch's /series requires a __name__ matcher.
			if resp, seriesErr := s.aws.PrometheusSeries(r.Context(), forwardedMatches, start, end); seriesErr == nil {
				for _, labels := range extractPrometheusSeries(resp) {
					if value := labels[labelName]; value != "" {
						values[value] = struct{}{}
					}
				}
			}
		}
	}

	writePrometheusSuccess(w, sortedStrings(values))
}

func (s *Service) handlePrometheusSeries(w http.ResponseWriter, r *http.Request) {
	matchValues := requestParams(r, "match[]")
	if len(matchValues) == 0 {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "at least one match[] selector is required")
		return
	}
	partition := partitionFromRequest(r)
	hasAWS, forwardedMatches := awsMatches(matchValues)
	awsEnabled := s.awsReady() && (partition == "" || s.awsPartitionVisible(r.Context(), partition))
	selectorSets, err := parsePrometheusSelectorSets(matchValues)
	if err != nil && !hasAWS && !awsEnabled {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}

	seen := make(map[string]map[string]string)
	if err == nil {
		querier, queryErr := s.discoveryPrometheusQuerier(r)
		if queryErr != nil && !hasAWS && !awsEnabled {
			writePrometheusError(w, http.StatusBadRequest, "bad_data", queryErr.Error())
			return
		}
		if queryErr == nil {
			defer func() { _ = querier.Close() }()
			doctorQuerier, ok := querier.(*doctorPromQuerier)
			if !ok {
				writePrometheusError(w, http.StatusInternalServerError, "internal", "unexpected querier type")
				return
			}
			for _, selectorSet := range selectorSets {
				labelMaps, seriesErr := doctorQuerier.seriesLabelMaps(r.Context(), selectorSet)
				if seriesErr != nil && !hasAWS && !awsEnabled {
					if errors.Is(seriesErr, errMetricQueryTooBroad) {
						writePrometheusExecutionError(w, translateMetricLoadError(seriesErr))
					} else {
						writePrometheusError(w, http.StatusInternalServerError, "internal", seriesErr.Error())
					}
					return
				}
				for _, labels := range labelMaps {
					seen[metricLabelMapString(labels)] = labels
				}
			}
		}
	}

	if awsEnabled {
		start, end := prometheusDiscoveryRange(r)
		if resp, awsErr := s.aws.PrometheusSeries(r.Context(), forwardedMatches, start, end); awsErr == nil {
			for _, labels := range extractPrometheusSeries(resp) {
				seen[metricLabelMapString(labels)] = labels
			}
		}
	}

	out := make([]map[string]string, 0, len(seen))
	for _, labels := range seen {
		out = append(out, labels)
	}
	sort.Slice(out, func(i, j int) bool {
		return metricLabelMapString(out[i]) < metricLabelMapString(out[j])
	})
	writePrometheusSuccess(w, out)
}

func (s *Service) handlePrometheusMetadata(w http.ResponseWriter, r *http.Request) {
	metricNames := make(map[string]struct{})
	if querier, err := s.discoveryPrometheusQuerier(r); err == nil {
		names, _, labelErr := querier.LabelValues(r.Context(), promlabels.MetricName, nil)
		_ = querier.Close()
		if labelErr == nil {
			for _, name := range names {
				metricNames[name] = struct{}{}
			}
		}
	}

	if partition := partitionFromRequest(r); partition == "" || s.awsPartitionVisible(r.Context(), partition) {
		for _, name := range s.awsMetricNames(r.Context()) {
			metricNames[name] = struct{}{}
		}
	}

	metadata := make(map[string][]map[string]string, len(metricNames))
	for metricName := range metricNames {
		metadata[metricName] = []map[string]string{{
			"type": "untyped",
			"help": "",
			"unit": "",
		}}
	}

	writePrometheusSuccess(w, metadata)
}

func (s *Service) handlePrometheusBuildInfo(w http.ResponseWriter, _ *http.Request) {
	writePrometheusSuccess(w, buildinfo.Current().StatusFields())
}

func (s *Service) discoveryPrometheusQuerier(r *http.Request) (promstorage.Querier, error) {
	now := time.Now().UTC()
	end, err := parsePrometheusTime(requestParam(r, "end"), now)
	if err != nil {
		return nil, err
	}
	start, err := parsePrometheusTime(requestParam(r, "start"), end.Add(-doctorDiscoveryWindow))
	if err != nil {
		return nil, err
	}
	start, end = clampDiscoveryWindow(start, end)
	return doctorPromQueryable{
		backend:   s.logEngine,
		tenant:    s.tenantFromRequest(r),
		partition: partitionFromRequest(r),
	}.Querier(start.UnixMilli(), end.UnixMilli())
}

// clampDiscoveryWindow bounds a discovery time range to doctorDiscoveryScanWindow
// so label/series lookups never scan an unbounded number of samples.
func clampDiscoveryWindow(start, end time.Time) (time.Time, time.Time) {
	if end.IsZero() {
		end = time.Now().UTC()
	}
	if start.IsZero() || end.Sub(start) > doctorDiscoveryScanWindow {
		start = end.Add(-doctorDiscoveryScanWindow)
	}
	return start, end
}

// prometheusDiscoveryRange parses start/end for discovery endpoints, defaulting
// to the standard discovery window.
func prometheusDiscoveryRange(r *http.Request) (time.Time, time.Time) {
	now := time.Now().UTC()
	end, err := parsePrometheusTime(requestParam(r, "end"), now)
	if err != nil {
		end = now
	}
	start, err := parsePrometheusTime(requestParam(r, "start"), end.Add(-doctorDiscoveryWindow))
	if err != nil {
		start = end.Add(-doctorDiscoveryWindow)
	}
	return clampDiscoveryWindow(start, end)
}

func prometheusEngineTimeout(ctx context.Context) time.Duration {
	timeout := doctorPrometheusTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > timeout {
			timeout = remaining
		}
	}
	return timeout
}

func newDoctorPrometheusEngine(timeout time.Duration) *promql.Engine {
	if timeout <= 0 {
		timeout = doctorPrometheusTimeout
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return promql.NewEngine(promql.EngineOpts{
		Logger:        logger,
		Reg:           promclient.NewRegistry(),
		MaxSamples:    1_000_000,
		Timeout:       timeout,
		LookbackDelta: doctorPrometheusLookback,
		NoStepSubqueryIntervalFn: func(rangeMillis int64) int64 {
			if rangeMillis <= 0 {
				return int64(time.Minute / time.Millisecond)
			}
			step := rangeMillis / 5
			if step < 1 {
				return 1
			}
			return step
		},
		EnableAtModifier:     true,
		EnableNegativeOffset: true,
	})
}

func marshalPrometheusValue(value promparser.Value) (map[string]any, error) {
	// Keep every result as a []any: the AWS federation merge and emptiness
	// checks type-assert data["result"].([]any), and JSON-decoded CloudWatch
	// responses are []any as well. Returning a concrete []map[string]any here
	// made local series look empty to those helpers.
	switch typed := value.(type) {
	case promql.Vector:
		result := make([]any, 0, len(typed))
		for _, sample := range typed {
			entry := map[string]any{
				"metric": promLabelsToMap(sample.Metric),
				"value":  []any{prometheusJSONTime(sample.T), formatPrometheusSampleValue(sample.F)},
			}
			if exemplars := sampleExemplars(sample); len(exemplars) > 0 {
				entry["exemplars"] = exemplars
			}
			result = append(result, entry)
		}
		return map[string]any{"resultType": "vector", "result": result}, nil
	case promql.Matrix:
		result := make([]any, 0, len(typed))
		for _, series := range typed {
			values := make([][]any, 0, len(series.Floats))
			for _, point := range series.Floats {
				values = append(values, []any{prometheusJSONTime(point.T), formatPrometheusSampleValue(point.F)})
			}
			result = append(result, map[string]any{
				"metric": promLabelsToMap(series.Metric),
				"values": values,
			})
		}
		return map[string]any{"resultType": "matrix", "result": result}, nil
	case promql.Scalar:
		return map[string]any{
			"resultType": "scalar",
			"result":     []any{prometheusJSONTime(typed.T), formatPrometheusSampleValue(typed.V)},
		}, nil
	case promql.String:
		return map[string]any{
			"resultType": "string",
			"result":     []any{prometheusJSONTime(typed.T), typed.V},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported Prometheus result type %T", value)
	}
}

// sampleExemplars surfaces the trace/span correlation labels carried by a
// metric sample as Prometheus exemplars so Grafana can link metrics to traces.
func sampleExemplars(sample promql.Sample) []map[string]any {
	traceID := sample.Metric.Get("trace_id")
	spanID := sample.Metric.Get("span_id")
	if traceID == "" && spanID == "" {
		return nil
	}
	labels := make(map[string]string, 2)
	if traceID != "" {
		labels["trace_id"] = traceID
	}
	if spanID != "" {
		labels["span_id"] = spanID
	}
	return []map[string]any{{
		"labels":    labels,
		"value":     formatPrometheusSampleValue(sample.F),
		"timestamp": prometheusJSONTime(sample.T),
	}}
}

func promLabelsToMap(labelSet promlabels.Labels) map[string]string {
	out := make(map[string]string, labelSet.Len())
	labelSet.Range(func(label promlabels.Label) {
		out[label.Name] = label.Value
	})
	return out
}

func metricLabelMapString(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values[key])
	}
	return strings.Join(parts, ",")
}

func formatPrometheusSampleValue(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func prometheusJSONTime(milliseconds int64) float64 {
	return float64(milliseconds) / 1000
}

func parsePrometheusTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback.UTC(), nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid Prometheus timestamp %q", value)
	}
	whole, fractional := math.Modf(seconds)
	return time.Unix(int64(whole), int64(fractional*float64(time.Second))).UTC(), nil
}

func parsePrometheusStep(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 15 * time.Second, nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration <= 0 {
			return 0, fmt.Errorf("step must be positive")
		}
		return duration, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid Prometheus step %q", value)
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("step must be positive")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func parsePrometheusSelectorSets(values []string) ([][]*promlabels.Matcher, error) {
	if len(values) == 0 {
		return nil, nil
	}

	parser := promparser.NewParser(promparser.Options{})
	out := make([][]*promlabels.Matcher, 0, len(values))
	for _, value := range values {
		matchers, err := parser.ParseMetricSelector(value)
		if err == nil {
			out = append(out, matchers)
			continue
		}

		expr, exprErr := parser.ParseExpr(value)
		if exprErr != nil {
			return nil, err
		}
		selectors := promparser.ExtractSelectors(expr)
		if len(selectors) != 1 {
			return nil, fmt.Errorf("selector %q did not resolve to a single metric selector", value)
		}
		out = append(out, selectors[0])
	}
	return out, nil
}

func writePrometheusSuccess(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"data":   data,
	})
}

func writePrometheusError(w http.ResponseWriter, statusCode int, errorType, message string) {
	writeJSON(w, statusCode, map[string]any{
		"status":    "error",
		"errorType": errorType,
		"error":     message,
	})
}

func writePrometheusDiscoveryError(w http.ResponseWriter, err error) {
	if errors.Is(err, errMetricQueryTooBroad) {
		writePrometheusExecutionError(w, translateMetricLoadError(err))
		return
	}
	writePrometheusError(w, http.StatusInternalServerError, "internal", err.Error())
}

func writePrometheusExecutionError(w http.ResponseWriter, err error) {
	var tooManySamples promql.ErrTooManySamples
	if errors.As(err, &tooManySamples) {
		writePrometheusError(w, http.StatusUnprocessableEntity, "execution", "query matches too many samples; narrow the time range or add a metric name")
		return
	}
	switch err.(type) {
	case promql.ErrQueryTimeout:
		writePrometheusError(w, http.StatusServiceUnavailable, "timeout", err.Error())
	case promql.ErrQueryCanceled:
		writePrometheusError(w, http.StatusServiceUnavailable, "canceled", err.Error())
	default:
		writePrometheusError(w, http.StatusUnprocessableEntity, "execution", err.Error())
	}
}

func timeFromPromMillis(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}
