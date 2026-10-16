package query

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/proxy/translate"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/tempo"
)

// traceMetricsSpanLimit bounds the spans scanned per TraceQL metrics query.
// Metrics queries aggregate over every matching span in the window, so the cap
// is higher than the search cap; the output caps below keep the response and
// aggregation memory bounded. 20k span records is roughly 40MB worst case.
const (
	traceMetricsSpanLimit    = 20000
	traceMetricsSampleBudget = 20000
)

// Output caps for TraceQL metrics responses. The Traces Drilldown uses a few
// dozen series and a few thousand buckets; anything beyond these caps is
// rejected with a clear error instead of exhausting memory.
const (
	traceMetricsMaxSeries  = 500
	traceMetricsMaxBuckets = 5000
	traceMetricsMaxSamples = 500_000
)

// traceQLMetricsQuery is a parsed TraceQL metrics query:
//
//	{filters} | rate()
//	{filters} | count_over_time()
//	{filters} | quantile_over_time(duration, 0.9, 0.95)
//
// optionally followed by `by (attr, ...)` and `with (sample=true)`.
type traceQLMetricsQuery struct {
	Search    proxy.TraceSearch
	Function  string // rate | count_over_time | quantile_over_time
	Quantiles []float64
	GroupBy   []traceQLGroupBy
}

// traceQLGroupBy is one group-by attribute. Label keeps the spelling from the
// query (clients look series labels up by it); Key is the normalized form used
// to resolve values.
type traceQLGroupBy struct {
	Label string
	Key   string
}

var traceQLMetricsFunctions = map[string]bool{
	"rate":               true,
	"count_over_time":    true,
	"quantile_over_time": true,
}

// parseTraceQLMetricsQuery parses the TraceQL metrics subset emitted by
// Grafana's Traces Drilldown.
func parseTraceQLMetricsQuery(raw string) (traceQLMetricsQuery, error) {
	raw = strings.TrimSpace(raw)
	filter, rest, err := splitTraceQLFilter(raw)
	if err != nil {
		return traceQLMetricsQuery{}, err
	}
	search := proxy.TraceSearch{TraceQL: filter}
	if err := search.ApplyTraceQL(); err != nil {
		return traceQLMetricsQuery{}, fmt.Errorf("translate metrics filters: %w", err)
	}

	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "|") {
		return traceQLMetricsQuery{}, fmt.Errorf("%w: metrics query must contain a pipeline stage: %q", translate.ErrUnsupported, raw)
	}
	rest = strings.TrimSpace(rest[1:])

	name, args, tail, err := parseTraceQLCall(rest)
	if err != nil {
		return traceQLMetricsQuery{}, err
	}
	name = strings.ToLower(name)
	if !traceQLMetricsFunctions[name] {
		return traceQLMetricsQuery{}, fmt.Errorf("%w: unsupported metrics function %q", translate.ErrUnsupported, name)
	}

	query := traceQLMetricsQuery{Search: search, Function: name}
	switch name {
	case "quantile_over_time":
		if len(args) < 2 {
			return traceQLMetricsQuery{}, fmt.Errorf("%w: quantile_over_time requires a duration and at least one quantile", translate.ErrUnsupported)
		}
		if strings.TrimSpace(strings.ToLower(args[0])) != "duration" {
			return traceQLMetricsQuery{}, fmt.Errorf("%w: quantile_over_time only supports the duration intrinsic", translate.ErrUnsupported)
		}
		for _, rawQuantile := range args[1:] {
			quantile, parseErr := strconv.ParseFloat(strings.TrimSpace(rawQuantile), 64)
			if parseErr != nil || quantile < 0 || quantile > 1 {
				return traceQLMetricsQuery{}, fmt.Errorf("%w: invalid quantile %q", translate.ErrUnsupported, rawQuantile)
			}
			query.Quantiles = append(query.Quantiles, quantile)
		}
	default:
		if len(args) > 0 {
			return traceQLMetricsQuery{}, fmt.Errorf("%w: %s takes no arguments", translate.ErrUnsupported, name)
		}
	}

	tail = strings.TrimSpace(tail)
	for tail != "" {
		switch {
		case strings.HasPrefix(strings.ToLower(tail), "by"):
			groupRaw, remaining, ok := parseTraceQLParenGroup(tail[2:])
			if !ok {
				return traceQLMetricsQuery{}, fmt.Errorf("%w: malformed by() clause", translate.ErrUnsupported)
			}
			for _, attr := range strings.Split(groupRaw, ",") {
				attr = strings.TrimSpace(attr)
				if attr == "" {
					continue
				}
				query.GroupBy = append(query.GroupBy, traceQLGroupBy{Label: attr, Key: translate.NormalizeTraceKey(attr)})
			}
			tail = strings.TrimSpace(remaining)
		case strings.HasPrefix(strings.ToLower(tail), "with"):
			_, remaining, ok := parseTraceQLParenGroup(tail[4:])
			if !ok {
				return traceQLMetricsQuery{}, fmt.Errorf("%w: malformed with() clause", translate.ErrUnsupported)
			}
			// Sampling is accepted and ignored: Doctor always evaluates the
			// exact span set.
			tail = strings.TrimSpace(remaining)
		default:
			return traceQLMetricsQuery{}, fmt.Errorf("%w: unexpected metrics query tail %q", translate.ErrUnsupported, tail)
		}
	}
	if len(query.GroupBy) > 8 {
		return traceQLMetricsQuery{}, fmt.Errorf("%w: too many group-by attributes", translate.ErrUnsupported)
	}
	return query, nil
}

// splitTraceQLFilter returns the leading brace expression and the rest of the
// query, honouring quoted strings.
func splitTraceQLFilter(raw string) (filter, rest string, err error) {
	if !strings.HasPrefix(raw, "{") {
		return "", "", fmt.Errorf("%w: traceql must start with a brace expression: %q", translate.ErrUnsupported, raw)
	}
	var (
		inQuotes bool
		escaped  bool
	)
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inQuotes:
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
		case c == '}' && !inQuotes:
			return raw[:i+1], raw[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("%w: unterminated traceql filter: %q", translate.ErrUnsupported, raw)
}

// parseTraceQLCall splits `name(arg1, arg2) tail` into its parts.
func parseTraceQLCall(raw string) (name string, args []string, tail string, err error) {
	open := strings.IndexByte(raw, '(')
	if open < 0 {
		return "", nil, "", fmt.Errorf("%w: expected function call in %q", translate.ErrUnsupported, raw)
	}
	name = strings.TrimSpace(raw[:open])
	close, ok := matchingParen(raw, open)
	if !ok {
		return "", nil, "", fmt.Errorf("%w: unbalanced parentheses in %q", translate.ErrUnsupported, raw)
	}
	argRaw := strings.TrimSpace(raw[open+1 : close])
	if argRaw != "" {
		for _, arg := range strings.Split(argRaw, ",") {
			args = append(args, strings.TrimSpace(arg))
		}
	}
	return name, args, raw[close+1:], nil
}

// parseTraceQLParenGroup parses ` (a, b) rest` after a keyword like by/with.
func parseTraceQLParenGroup(raw string) (inner string, rest string, ok bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "(") {
		return "", "", false
	}
	close, matched := matchingParen(raw, 0)
	if !matched {
		return "", "", false
	}
	return raw[1:close], raw[close+1:], true
}

// matchingParen returns the index of the parenthesis closing the one at open.
func matchingParen(raw string, open int) (int, bool) {
	depth := 0
	var (
		inQuotes bool
		escaped  bool
	)
	for i := open; i < len(raw); i++ {
		c := raw[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inQuotes:
			escaped = true
		case c == '"':
			inQuotes = !inQuotes
		case inQuotes:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// metricBucket aggregates one series bucket.
type metricBucket struct {
	count     int
	durations []time.Duration
}

// evaluateTraceQLMetricsRange runs a TraceQL metrics range query over the local
// span store and returns Tempo-shaped series.
func (s *Service) evaluateTraceQLMetricsRange(ctx context.Context, tenant string, query traceQLMetricsQuery, from, to time.Time, step time.Duration) (*tempo.QueryRangeResponse, error) {
	records, err := s.loadMetricSpans(ctx, tenant, query.Search, from, to)
	if err != nil {
		return nil, err
	}

	stepSeconds := step.Seconds()
	if stepSeconds <= 0 {
		stepSeconds = 1
	}
	numBuckets := int(math.Ceil(to.Sub(from).Seconds()/stepSeconds)) + 1
	if numBuckets <= 0 {
		numBuckets = 1
	}
	if numBuckets > traceMetricsMaxBuckets {
		return nil, fmt.Errorf("%w: time range covers too many steps; increase step", translate.ErrUnsupported)
	}

	// series key -> bucket index -> bucket
	type seriesKey struct {
		group string
	}
	series := make(map[seriesKey]map[int]*metricBucket)
	groupValues := make(map[string][]string)

	for _, record := range records {
		bucket := int(record.StartTime.Sub(from) / step)
		if bucket < 0 || bucket >= numBuckets {
			continue
		}
		values := make([]string, 0, len(query.GroupBy))
		for _, attr := range query.GroupBy {
			value, _ := proxy.TraceRecordValue(record, attr.Key)
			values = append(values, value)
		}
		key := seriesKey{group: strings.Join(values, "\x00")}
		buckets := series[key]
		if buckets == nil {
			if len(series) >= traceMetricsMaxSeries {
				return nil, fmt.Errorf("%w: group-by produces more than %d series; narrow the grouping", translate.ErrUnsupported, traceMetricsMaxSeries)
			}
			buckets = make(map[int]*metricBucket)
			series[key] = buckets
			groupValues[key.group] = values
		}
		bucketState := buckets[bucket]
		if bucketState == nil {
			bucketState = &metricBucket{}
			buckets[bucket] = bucketState
		}
		bucketState.count++
		if query.Function == "quantile_over_time" {
			bucketState.durations = append(bucketState.durations, spanDurationOf(record))
		}
	}

	// Deterministic series order.
	keys := make([]string, 0, len(series))
	for key := range series {
		keys = append(keys, key.group)
	}
	sort.Strings(keys)

	response := &tempo.QueryRangeResponse{Series: make([]tempo.TimeSeries, 0, len(keys))}
	totalSamples := 0
	appendSamples := func(labels []tempo.MetricLabel, samples []tempo.MetricSample) error {
		totalSamples += len(samples)
		if totalSamples > traceMetricsMaxSamples {
			return fmt.Errorf("%w: metrics response exceeds %d samples; increase step or narrow the query", translate.ErrUnsupported, traceMetricsMaxSamples)
		}
		response.Series = append(response.Series, tempo.TimeSeries{Labels: labels, Samples: samples})
		return nil
	}
	for _, groupKey := range keys {
		buckets := series[seriesKey{group: groupKey}]
		values := groupValues[groupKey]
		labels := make([]tempo.MetricLabel, 0, len(query.GroupBy))
		for i, attr := range query.GroupBy {
			labels = append(labels, tempo.StringLabel(attr.Label, values[i]))
		}

		switch query.Function {
		case "quantile_over_time":
			for _, quantile := range query.Quantiles {
				samples := make([]tempo.MetricSample, 0, numBuckets)
				for bucket := 0; bucket < numBuckets; bucket++ {
					state := buckets[bucket]
					if state == nil || len(state.durations) == 0 {
						continue
					}
					samples = append(samples, tempo.MetricSample{
						TimestampMs: from.Add(time.Duration(bucket) * step).UnixMilli(),
						Value:       quantileDuration(state.durations, quantile),
					})
				}
				if len(samples) == 0 {
					continue
				}
				seriesLabels := append(append([]tempo.MetricLabel(nil), labels...), tempo.FloatLabel("p", quantile))
				if err := appendSamples(seriesLabels, samples); err != nil {
					return nil, err
				}
			}
		default:
			samples := make([]tempo.MetricSample, 0, numBuckets)
			for bucket := 0; bucket < numBuckets; bucket++ {
				state := buckets[bucket]
				if state == nil {
					continue
				}
				value := float64(state.count)
				if query.Function == "rate" {
					value = value / stepSeconds
				}
				samples = append(samples, tempo.MetricSample{
					TimestampMs: from.Add(time.Duration(bucket) * step).UnixMilli(),
					Value:       value,
				})
			}
			if len(samples) == 0 {
				continue
			}
			if err := appendSamples(labels, samples); err != nil {
				return nil, err
			}
		}
	}
	return response, nil
}

// evaluateTraceQLMetricsInstant runs the instant form: the aggregate over the
// whole window collapses into a single value per series.
func (s *Service) evaluateTraceQLMetricsInstant(ctx context.Context, tenant string, query traceQLMetricsQuery, from, to time.Time) (*tempo.QueryInstantResponse, error) {
	records, err := s.loadMetricSpans(ctx, tenant, query.Search, from, to)
	if err != nil {
		return nil, err
	}
	windowSeconds := to.Sub(from).Seconds()
	if windowSeconds <= 0 {
		windowSeconds = 1
	}

	type aggregate struct {
		count     int
		durations []time.Duration
	}
	series := make(map[string]*aggregate)
	groupValues := make(map[string][]string)
	order := make([]string, 0)
	for _, record := range records {
		values := make([]string, 0, len(query.GroupBy))
		for _, attr := range query.GroupBy {
			value, _ := proxy.TraceRecordValue(record, attr.Key)
			values = append(values, value)
		}
		groupKey := strings.Join(values, "\x00")
		state := series[groupKey]
		if state == nil {
			if len(series) >= traceMetricsMaxSeries {
				return nil, fmt.Errorf("%w: group-by produces more than %d series; narrow the grouping", translate.ErrUnsupported, traceMetricsMaxSeries)
			}
			state = &aggregate{}
			series[groupKey] = state
			groupValues[groupKey] = values
			order = append(order, groupKey)
		}
		state.count++
		if query.Function == "quantile_over_time" {
			state.durations = append(state.durations, spanDurationOf(record))
		}
	}
	sort.Strings(order)

	response := &tempo.QueryInstantResponse{Series: make([]tempo.InstantSeries, 0, len(order))}
	for _, groupKey := range order {
		state := series[groupKey]
		values := groupValues[groupKey]
		labels := make([]tempo.MetricLabel, 0, len(query.GroupBy))
		for i, attr := range query.GroupBy {
			labels = append(labels, tempo.StringLabel(attr.Label, values[i]))
		}
		switch query.Function {
		case "quantile_over_time":
			for _, quantile := range query.Quantiles {
				if len(state.durations) == 0 {
					continue
				}
				seriesLabels := append(append([]tempo.MetricLabel(nil), labels...), tempo.FloatLabel("p", quantile))
				response.Series = append(response.Series, tempo.InstantSeries{Labels: seriesLabels, Value: quantileDuration(state.durations, quantile)})
			}
		default:
			value := float64(state.count)
			if query.Function == "rate" {
				value = value / windowSeconds
			}
			response.Series = append(response.Series, tempo.InstantSeries{Labels: labels, Value: value})
		}
	}
	return response, nil
}

// loadMetricSpans loads the matching spans for a metrics query window.
func (s *Service) loadMetricSpans(ctx context.Context, tenant string, search proxy.TraceSearch, from, to time.Time) ([]segment.TraceRecord, error) {
	records, err := s.logEngine.QueryTraces(ctx, tenant, "", search.Service, from, to, traceMetricsSpanLimit)
	if err != nil {
		return nil, err
	}
	records = canonicalizeTraceRecords(records)
	matched := make([]segment.TraceRecord, 0, len(records))
	for _, record := range records {
		if !proxy.MatchTraceRecord(record, search) {
			continue
		}
		if len(matched) >= traceMetricsSampleBudget {
			break
		}
		matched = append(matched, record)
	}
	return matched, nil
}

func spanDurationOf(record segment.TraceRecord) time.Duration {
	if record.StartTime.IsZero() || record.EndTime.IsZero() {
		return 0
	}
	duration := record.EndTime.Sub(record.StartTime)
	if duration < 0 {
		return 0
	}
	return duration
}

// quantileDuration returns the q-quantile of the durations using linear
// interpolation between the closest ranks.
func quantileDuration(durations []time.Duration, q float64) float64 {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if q <= 0 {
		return float64(sorted[0])
	}
	if q >= 1 {
		return float64(sorted[len(sorted)-1])
	}
	rank := q * float64(len(sorted)-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))
	if lower == upper {
		return float64(sorted[lower])
	}
	fraction := rank - float64(lower)
	return float64(sorted[lower])*(1-fraction) + float64(sorted[upper])*fraction
}

// defaultTraceQLMetricsStep mirrors Tempo's DefaultQueryRangeStep so clients
// that omit `step` get the same bucketing as upstream Tempo.
func defaultTraceQLMetricsStep(from, to time.Time) time.Duration {
	delta := to.Sub(from)
	baseline := delta / 240
	if delta < time.Minute {
		granularity := 50 * time.Millisecond
		if baseline > granularity {
			return baseline / granularity * granularity
		}
		return granularity
	}
	granularities := []time.Duration{
		time.Hour,
		5 * time.Minute,
		time.Minute,
		15 * time.Second,
		5 * time.Second,
		time.Second,
	}
	for _, granularity := range granularities {
		if baseline > granularity {
			return baseline / granularity * granularity
		}
	}
	return time.Second
}

// parseTraceQLMetricsStep accepts Tempo's step forms: a float number of
// seconds or a duration string like "28s".
func parseTraceQLMetricsStep(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(seconds * float64(time.Second)), nil
	}
	return time.ParseDuration(raw)
}
