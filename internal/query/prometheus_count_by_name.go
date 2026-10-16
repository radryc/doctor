package query

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	promlabels "github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/rydzu/ainfra/doctor/internal/segment"
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

// countByNamePlan is a parsed `count by (__name__) (<selector>)` query.
type countByNamePlan struct {
	matchers []*promlabels.Matcher
}

// countByNamePlanFor matches the supported aggregation shape: an exact
// `count by (__name__)` over a plain vector selector.
func countByNamePlanFor(expr promparser.Expr) (countByNamePlan, bool) {
	aggregation, ok := expr.(*promparser.AggregateExpr)
	if !ok || aggregation.Op != promparser.COUNT {
		return countByNamePlan{}, false
	}
	if aggregation.Without || len(aggregation.Grouping) != 1 || aggregation.Grouping[0] != promlabels.MetricName {
		return countByNamePlan{}, false
	}
	selector, ok := aggregation.Expr.(*promparser.VectorSelector)
	if !ok {
		return countByNamePlan{}, false
	}
	return countByNamePlan{matchers: selector.LabelMatchers}, true
}

// seriesPresence tracks the first/last sample time of one series.
type seriesPresence struct {
	min time.Time
	max time.Time
}

// execCountByNameRange answers a range query from the chunk series index.
func (s *Service) execCountByNameRange(ctx context.Context, tenant string, plan countByNamePlan, start, end time.Time, step time.Duration) (map[string]any, error) {
	if step <= 0 {
		step = time.Minute
	}
	steps := int(end.Sub(start)/step) + 1
	if steps > maxCountByNameSteps {
		return nil, fmt.Errorf("count by (__name__): %d steps exceeds the supported %d; increase step", steps, maxCountByNameSteps)
	}

	// name -> series identity -> presence range
	groups, err := s.loadSeriesPresence(ctx, tenant, plan, start, end)
	if err != nil {
		return nil, err
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
				if !t.Before(presence.min) && !t.After(presence.max) {
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
func (s *Service) execCountByNameInstant(ctx context.Context, tenant string, plan countByNamePlan, at time.Time) (map[string]any, error) {
	window := at.Add(-doctorPrometheusLookback)
	groups, err := s.loadSeriesPresence(ctx, tenant, plan, window, at)
	if err != nil {
		return nil, err
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
			if !at.Before(presence.min) && !at.After(presence.max) {
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
func (s *Service) loadSeriesPresence(ctx context.Context, tenant string, plan countByNamePlan, start, end time.Time) (map[string]map[string]*seriesPresence, error) {
	query := segment.MetricQuery{
		Service:       exactServiceMatcher(plan.matchers),
		LabelMatchers: segmentMetricMatchers(plan.matchers),
	}
	groups := make(map[string]map[string]*seriesPresence)
	seriesCount := 0
	err := s.logEngine.StreamMetricSeries(ctx, tenant, query, start, end, func(meta segment.MetricSeriesMeta) error {
		labelSet := metricSeriesLabels(meta)
		if !matchersMatch(plan.matchers, labelSet) {
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
	})
	if err != nil {
		return nil, translateMetricLoadError(err)
	}
	return groups, nil
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
