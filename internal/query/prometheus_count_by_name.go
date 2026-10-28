package query

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	promlabels "github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/radryc/doctor/internal/segment"
)

// This file implements the series-index fast path for presence aggregations.
//
// Grafana's Metrics Drilldown landing query is
//
//	count by (__name__) ({__name__=~".+"})
//
// which only needs to know how many series of each metric have data at each
// step. Answering it from samples requires scanning every stored point, so the
// query is instead served from the per-chunk series index (name, labels,
// first/last sample time) written by the storage ingester.

const (
	// maxCountByNameSeries bounds the series considered by the fast path.
	maxCountByNameSeries = 200_000
	// maxCountByNameSteps bounds the step count of a fast-path response.
	maxCountByNameSteps = 10_000
)

// countByNamePlan is a parsed presence aggregation:
//
//	count by (__name__) (<selector>)   (byName=true)
//	count(<selector>)                  (byName=false)
type countByNamePlan struct {
	matchers []*promlabels.Matcher
	byName   bool
}

// countByNamePlanFor matches the supported presence aggregation shapes over a
// plain vector selector. Both `count by (__name__)` and a bare `count` are
// answered from the chunk series index, so a dashboard total never scans raw
// metric samples.
func countByNamePlanFor(expr promparser.Expr) (countByNamePlan, bool) {
	aggregation, ok := expr.(*promparser.AggregateExpr)
	if !ok || aggregation.Op != promparser.COUNT {
		return countByNamePlan{}, false
	}
	selector, ok := aggregation.Expr.(*promparser.VectorSelector)
	if !ok {
		return countByNamePlan{}, false
	}
	byName := false
	switch {
	case !aggregation.Without && len(aggregation.Grouping) == 1 && aggregation.Grouping[0] == promlabels.MetricName:
		byName = true
	case !aggregation.Without && len(aggregation.Grouping) == 0:
		byName = false
	default:
		return countByNamePlan{}, false
	}
	return countByNamePlan{matchers: selector.LabelMatchers, byName: byName}, true
}

// seriesPresence tracks the first/last sample time of one series.
type seriesPresence struct {
	min time.Time
	max time.Time
}

// countPresentSeries counts series active at t. Prometheus keeps a series
// visible for the lookback delta after its last sample, so compare against
// t-lookback rather than requiring t to be inside [min, max].
func countPresentSeries(groups map[string]map[string]*seriesPresence, t time.Time) int {
	count := 0
	for _, series := range groups {
		for _, presence := range series {
			if seriesActiveAt(presence, t) {
				count++
			}
		}
	}
	return count
}

// seriesActiveAt reports whether the series has a sample in the Prometheus
// lookback window ending at t.
func seriesActiveAt(presence *seriesPresence, t time.Time) bool {
	if presence == nil || t.Before(presence.min) {
		return false
	}
	return !presence.max.Before(t.Add(-doctorPrometheusLookback))
}

// execCountByNameRange answers a range query from the chunk series index.
func (s *Service) execCountByNameRange(ctx context.Context, tenant, partition string, plan countByNamePlan, start, end time.Time, step time.Duration) (map[string]any, error) {
	if step <= 0 {
		step = time.Minute
	}
	steps := int(end.Sub(start)/step) + 1
	if steps > maxCountByNameSteps {
		return nil, fmt.Errorf("count by (__name__): %d steps exceeds the supported %d; increase step", steps, maxCountByNameSteps)
	}

	// name -> series identity -> presence range
	groups, err := s.loadSeriesPresence(ctx, tenant, partition, plan, start, end)
	if err != nil {
		return nil, err
	}

	if !plan.byName {
		values := make([][]any, 0, steps)
		for t := start; !t.After(end); t = t.Add(step) {
			values = append(values, []any{prometheusJSONTime(t.UnixMilli()), strconv.Itoa(countPresentSeries(groups, t))})
		}
		return map[string]any{
			"resultType": "matrix",
			"result": []any{map[string]any{
				"metric": map[string]string{},
				"values": values,
			}},
		}, nil
	}

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]any, 0, len(names))
	for _, name := range names {
		series := groups[name]
		values := make([][]any, 0, steps)
		for t := start; !t.After(end); t = t.Add(step) {
			count := 0
			for _, presence := range series {
				if seriesActiveAt(presence, t) {
					count++
				}
			}
			values = append(values, []any{prometheusJSONTime(t.UnixMilli()), strconv.Itoa(count)})
		}
		result = append(result, map[string]any{
			"metric": map[string]string{promlabels.MetricName: name},
			"values": values,
		})
	}
	return map[string]any{"resultType": "matrix", "result": result}, nil
}

// execCountByNameInstant answers an instant query from the chunk series index.
func (s *Service) execCountByNameInstant(ctx context.Context, tenant, partition string, plan countByNamePlan, at time.Time) (map[string]any, error) {
	window := at.Add(-doctorPrometheusLookback)
	groups, err := s.loadSeriesPresence(ctx, tenant, partition, plan, window, at)
	if err != nil {
		return nil, err
	}

	if !plan.byName {
		return map[string]any{
			"resultType": "vector",
			"result": []any{map[string]any{
				"metric": map[string]string{},
				"value":  []any{prometheusJSONTime(at.UnixMilli()), strconv.Itoa(countPresentSeries(groups, at))},
			}},
		}, nil
	}

	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]any, 0, len(names))
	for _, name := range names {
		count := 0
		for _, presence := range groups[name] {
			if seriesActiveAt(presence, at) {
				count++
			}
		}
		result = append(result, map[string]any{
			"metric": map[string]string{promlabels.MetricName: name},
			"value":  []any{prometheusJSONTime(at.UnixMilli()), strconv.Itoa(count)},
		})
	}
	return map[string]any{"resultType": "vector", "result": result}, nil
}

// loadSeriesPresence streams matching series metadata and merges it into
// name -> series -> presence ranges.
func (s *Service) loadSeriesPresence(ctx context.Context, tenant, partition string, plan countByNamePlan, start, end time.Time) (map[string]map[string]*seriesPresence, error) {
	query := segment.MetricQuery{
		Service:       exactServiceMatcher(plan.matchers),
		LabelMatchers: segmentMetricMatchers(plan.matchers),
	}
	matchers := plan.matchers
	if partition = strings.TrimSpace(partition); partition != "" {
		matchers = append(append([]*promlabels.Matcher(nil), matchers...),
			promlabels.MustNewMatcher(promlabels.MatchEqual, labelPartition, partition))
	}
	groups := make(map[string]map[string]*seriesPresence)
	seriesCount := 0
	emit := func(meta segment.MetricSeriesMeta) error {

		labelSet := metricSeriesLabels(meta)
		if !matchersMatch(matchers, labelSet) {
			return nil
		}
		name := labelSet.Get(promlabels.MetricName)
		if name == "" {
			return nil
		}
		series := groups[name]
		if series == nil {
			series = make(map[string]*seriesPresence)
			groups[name] = series
		}
		key := labelSet.String()
		presence := series[key]
		if presence == nil {
			seriesCount++
			if seriesCount > maxCountByNameSeries {
				return errMetricQueryTooBroad
			}
			series[key] = &seriesPresence{min: meta.MinTime.UTC(), max: meta.MaxTime.UTC()}
			return nil
		}
		if minTime := meta.MinTime.UTC(); minTime.Before(presence.min) {
			presence.min = minTime
		}
		if maxTime := meta.MaxTime.UTC(); maxTime.After(presence.max) {
			presence.max = maxTime
		}
		return nil
	}
	stream := func(q segment.MetricQuery) error {
		return s.logEngine.StreamMetricSeries(ctx, tenant, q, start, end, emit)
	}
	if err := stream(query); err != nil {
		return nil, translateMetricLoadError(err)
	}

	// Resource attributes such as guardian.pusher reach the stored series index
	// with their original dotted names (guardian.pusher), while PromQL matchers
	// use the normalized underscore form (guardian_pusher). Pushing the
	// underscore matcher into storage therefore finds no series even though the
	// samples carry the label. Retry with the dotted spelling so the matcher can
	// use the series index instead of scanning samples.
	if len(groups) == 0 && len(query.LabelMatchers) > 0 {
		dotted := query
		dotted.LabelMatchers = segmentMetricMatchers(dottedMetricMatchers(plan.matchers))
		if len(dotted.LabelMatchers) > 0 {
			if err := stream(dotted); err != nil {
				return nil, translateMetricLoadError(err)
			}
		}
	}

	// Last resort: drop label pushdown and apply the matchers to the normalized
	// labels client-side. The series index is metadata-only, so this stays
	// bounded even when the selector is broad.
	if len(groups) == 0 && len(query.LabelMatchers) > 0 {
		query.LabelMatchers = nil
		if err := stream(query); err != nil {
			return nil, translateMetricLoadError(err)
		}
	}
	return groups, nil
}

// dottedMetricMatchers rewrites normalized PromQL label names to the dotted
// resource-attribute spelling stored in the series index. The canonical
// labels exposed to PromQL use underscores, but the ingest path preserves the
// original OTel names (guardian.pusher, k8s.cluster.name).
func dottedMetricMatchers(matchers []*promlabels.Matcher) []*promlabels.Matcher {
	out := make([]*promlabels.Matcher, 0, len(matchers))
	for _, matcher := range matchers {
		if matcher == nil {
			continue
		}
		out = append(out, promlabels.MustNewMatcher(
			matcher.Type,
			dottedLabelName(matcher.Name),
			matcher.Value,
		))
	}
	return out
}

// dottedLabelName maps a normalized PromQL/API label name to the dotted
// resource-attribute spelling used by the metric series index.
func dottedLabelName(name string) string {
	switch name {
	case promlabels.MetricName, "service", "tenant":
		// Stored as first-class fields, not label-map entries.
		return name
	case "partition":
		return "guardian.partition"
	case "intent":
		return "guardian.intent"
	case "asset":
		return "guardian.asset"
	}
	if strings.Contains(name, "_") {
		return strings.ReplaceAll(name, "_", ".")
	}
	return name
}

// metricSeriesLabels reconstructs the Prometheus label set of a series index
// entry, matching the labels metricPointLabels produces for samples.
func metricSeriesLabels(meta segment.MetricSeriesMeta) promlabels.Labels {
	attrs := make(map[string]string, len(meta.Labels))
	for key, value := range meta.Labels {
		if segment.IsUnboundedMetricLabel(key) {
			continue
		}
		attrs[key] = value
	}
	values := canonicalLabels(meta.Service, meta.Tenant, meta.Partition, meta.Intent, meta.Asset, attrs)
	values[promlabels.MetricName] = normalizePrometheusMetricName(meta.MetricName)
	return promlabels.FromMap(values)
}

// segmentMetricMatchers converts PromQL matchers into storage label matchers.
// `__name__` matchers are skipped: stored names are raw (dots preserved) while
// PromQL names are normalized, so name matching happens client-side against the
// normalized label set.
func segmentMetricMatchers(matchers []*promlabels.Matcher) []segment.MetricLabelMatcher {
	out := make([]segment.MetricLabelMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		if matcher == nil || matcher.Name == promlabels.MetricName {
			continue
		}
		var matchType segment.MetricMatchType
		switch matcher.Type {
		case promlabels.MatchEqual:
			matchType = segment.MetricMatchEqual
		case promlabels.MatchNotEqual:
			matchType = segment.MetricMatchNotEqual
		case promlabels.MatchRegexp:
			matchType = segment.MetricMatchRegexp
		case promlabels.MatchNotRegexp:
			matchType = segment.MetricMatchNotRegexp
		default:
			continue
		}
		out = append(out, segment.MetricLabelMatcher{
			Name:  matcher.Name,
			Value: matcher.Value,
			Type:  matchType,
		})
	}
	return out
}
