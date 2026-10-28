package query

import (
	"context"

	promlabels "github.com/prometheus/prometheus/model/labels"
	"github.com/radryc/doctor/internal/segment"
)

// maxDiscoverySeries bounds how many distinct series a metadata discovery call
// may return. The series index is small compared with samples, but a tenant with
// millions of active series still needs an explicit budget. Exceeding it is
// surfaced as a 422 rather than silently truncating the result.
const maxDiscoverySeries = 200_000

// forEachSeriesLabelsFromIndex streams series metadata from the chunk series
// index and applies the PromQL matchers to the projected label set. It returns
// the number of distinct series yielded so the caller can fall back to the
// legacy sample scan when the store has no series index at all.
func (q *doctorPromQuerier) forEachSeriesLabelsFromIndex(ctx context.Context, matchers []*promlabels.Matcher, yield func(promlabels.Labels)) (int, error) {
	from := timeFromPromMillis(q.mint)
	to := timeFromPromMillis(q.maxt)
	base := segment.MetricQuery{Service: exactServiceMatcher(matchers)}

	seen := make(map[string]struct{})
	emit := func(meta segment.MetricSeriesMeta) error {
		if !from.IsZero() && meta.MaxTime.Before(from) {
			return nil
		}
		if !to.IsZero() && meta.MinTime.After(to) {
			return nil
		}
		labelSet := metricSeriesLabels(meta)
		if !q.seriesMatchesPartition(labelSet) || !matchersMatch(matchers, labelSet) {
			return nil
		}
		key := labelSet.String()
		if _, ok := seen[key]; ok {
			return nil
		}
		if len(seen) >= maxDiscoverySeries {
			return errMetricQueryTooBroad
		}
		seen[key] = struct{}{}
		yield(labelSet)
		return nil
	}

	stream := func(query segment.MetricQuery) error {
		return q.backend.StreamMetricSeries(ctx, q.tenant, query, from, to, emit)
	}

	requestedMetricName := exactPromMetricNameMatcher(matchers)
	if requestedMetricName == "" {
		if err := stream(base); err != nil {
			return 0, err
		}
		return len(seen), nil
	}

	rawNames, err := q.resolveRawMetricNames(ctx, requestedMetricName)
	if err != nil {
		return 0, err
	}
	if len(rawNames) == 0 {
		rawNames = []string{requestedMetricName}
	}
	for _, rawName := range rawNames {
		query := base
		query.MetricName = rawName
		if err := stream(query); err != nil {
			return 0, err
		}
	}
	return len(seen), nil
}
