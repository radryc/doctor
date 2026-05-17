package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	promlabels "github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	promparser "github.com/prometheus/prometheus/promql/parser"
	promstorage "github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
	"github.com/rydzu/ainfra/doctor/internal/buildinfo"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

const (
	doctorPrometheusLookback         = 5 * time.Minute
	doctorPrometheusTimeout          = 30 * time.Second
	doctorDiscoveryWindow            = 24 * time.Hour
	doctorMetricDiscoveryMatcherName = "__doctor_discovery__"
	doctorMetricDiscoveryModeNames   = "metric_names"
)

type doctorPromQueryable struct {
	backend LogEngineBackend
	tenant  string
}

func (q doctorPromQueryable) Querier(mint, maxt int64) (promstorage.Querier, error) {
	return &doctorPromQuerier{
		backend: q.backend,
		tenant:  q.tenant,
		mint:    mint,
		maxt:    maxt,
	}, nil
}

type doctorPromQuerier struct {
	backend LogEngineBackend
	tenant  string
	mint    int64
	maxt    int64

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
		if metricLabelsMatch(series.labels, matchers) {
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

	index, err := q.load(ctx, matchers)
	if err != nil {
		return nil, nil, err
	}

	values := make(map[string]struct{})
	for _, series := range index.series {
		if !metricLabelsMatch(series.labels, matchers) {
			continue
		}
		value := series.labels.Get(name)
		if value == "" {
			continue
		}
		values[value] = struct{}{}
	}

	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil, nil
}

func (q *doctorPromQuerier) discoverMetricNames(ctx context.Context) ([]string, annotations.Annotations, error) {
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

	values := make(map[string]struct{})
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
	index, err := q.load(ctx, matchers)
	if err != nil {
		return nil, nil, err
	}

	names := make(map[string]struct{})
	for _, series := range index.series {
		if !metricLabelsMatch(series.labels, matchers) {
			continue
		}
		series.labels.Range(func(label promlabels.Label) {
			names[label.Name] = struct{}{}
		})
	}

	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil, nil
}

func (q *doctorPromQuerier) Close() error {
	return nil
}

func (q *doctorPromQuerier) load(ctx context.Context, matchers []*promlabels.Matcher) (*doctorPromIndex, error) {
	metricQuery := metricQueryFromMatchers(matchers)
	requestedMetricName := exactPromMetricNameMatcher(matchers)
	cacheKey, err := metricQueryCacheKey(metricQuery, requestedMetricName)
	if err != nil {
		return nil, err
	}

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

	points, err := q.loadMetricPoints(ctx, metricQuery, requestedMetricName)
	if err != nil {
		q.mu.Lock()
		if q.errs == nil {
			q.errs = make(map[string]error)
		}
		q.errs[cacheKey] = err
		q.mu.Unlock()
		return nil, err
	}

	index := buildDoctorPromIndex(points)
	q.mu.Lock()
	if q.indexes == nil {
		q.indexes = make(map[string]*doctorPromIndex)
	}
	delete(q.errs, cacheKey)
	q.indexes[cacheKey] = index
	q.mu.Unlock()
	return index, nil
}

func (q *doctorPromQuerier) loadMetricPoints(ctx context.Context, metricQuery segment.MetricQuery, requestedMetricName string) ([]segment.MetricPointRecord, error) {
	from := timeFromPromMillis(q.mint)
	to := timeFromPromMillis(q.maxt)
	if requestedMetricName == "" {
		return q.backend.QueryMetrics(ctx, q.tenant, metricQuery, from, to)
	}

	rawNames, err := q.resolveRawMetricNames(ctx, requestedMetricName)
	if err != nil {
		return nil, err
	}
	if len(rawNames) == 0 {
		rawNames = []string{requestedMetricName}
	}

	all := make([]segment.MetricPointRecord, 0)
	for _, rawName := range rawNames {
		aliasQuery := metricQuery
		aliasQuery.MetricName = rawName
		points, err := q.backend.QueryMetrics(ctx, q.tenant, aliasQuery, from, to)
		if err != nil {
			return nil, err
		}
		all = append(all, points...)
	}
	return all, nil
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

func buildDoctorPromIndex(points []segment.MetricPointRecord) *doctorPromIndex {
	builders := make(map[string]*promql.Series)
	labelsByKey := make(map[string]promlabels.Labels)
	for _, point := range points {
		if !point.HasValue || point.MetricName == "" || point.Timestamp.IsZero() {
			continue
		}

		labelSet := metricPointLabels(point)
		key := string(labelSet.Bytes(nil))
		series := builders[key]
		if series == nil {
			series = &promql.Series{Metric: labelSet.Copy()}
			builders[key] = series
			labelsByKey[key] = labelSet.Copy()
		}
		series.Floats = append(series.Floats, promql.FPoint{
			T: point.Timestamp.UTC().UnixMilli(),
			F: point.Value,
		})
	}

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

func metricQueryFromMatchers(matchers []*promlabels.Matcher) segment.MetricQuery {
	query := segment.MetricQuery{}
	for _, matcher := range matchers {
		if matcher == nil {
			continue
		}
		switch {
		case matcher.Name == promlabels.MetricName:
			continue
		case matcher.Name == "service" && matcher.Type == promlabels.MatchEqual && query.Service == "":
			query.Service = matcher.Value
		default:
			query.LabelMatchers = append(query.LabelMatchers, segment.MetricLabelMatcher{
				Name:  matcher.Name,
				Value: matcher.Value,
				Type:  metricMatchTypeFromProm(matcher.Type),
			})
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

func metricMatchTypeFromProm(matchType promlabels.MatchType) segment.MetricMatchType {
	switch matchType {
	case promlabels.MatchNotEqual:
		return segment.MetricMatchNotEqual
	case promlabels.MatchRegexp:
		return segment.MetricMatchRegexp
	case promlabels.MatchNotRegexp:
		return segment.MetricMatchNotRegexp
	default:
		return segment.MetricMatchEqual
	}
}

func metricQueryCacheKey(query segment.MetricQuery, requestedMetricName string) (string, error) {
	normalized := struct {
		MetricQuery         segment.MetricQuery `json:"metricQuery"`
		RequestedMetricName string              `json:"requestedMetricName,omitempty"`
	}{
		MetricQuery:         query,
		RequestedMetricName: requestedMetricName,
	}
	if len(normalized.MetricQuery.LabelMatchers) > 0 {
		normalized.MetricQuery.LabelMatchers = append([]segment.MetricLabelMatcher(nil), normalized.MetricQuery.LabelMatchers...)
		sort.Slice(normalized.MetricQuery.LabelMatchers, func(i, j int) bool {
			if normalized.MetricQuery.LabelMatchers[i].Name != normalized.MetricQuery.LabelMatchers[j].Name {
				return normalized.MetricQuery.LabelMatchers[i].Name < normalized.MetricQuery.LabelMatchers[j].Name
			}
			if normalized.MetricQuery.LabelMatchers[i].Type != normalized.MetricQuery.LabelMatchers[j].Type {
				return normalized.MetricQuery.LabelMatchers[i].Type < normalized.MetricQuery.LabelMatchers[j].Type
			}
			return normalized.MetricQuery.LabelMatchers[i].Value < normalized.MetricQuery.LabelMatchers[j].Value
		})
	}
	b, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return string(b), nil
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
	values := map[string]string{
		promlabels.MetricName: normalizePrometheusMetricName(point.MetricName),
	}
	if point.Tenant != "" {
		values["tenant"] = point.Tenant
	}
	if point.Service != "" {
		values["service"] = point.Service
	}
	mergeNormalizedMetricLabels(values, point.ResourceAttributes, false)
	mergeNormalizedMetricLabels(values, point.Attributes, true)
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

func mergeNormalizedMetricLabels(dst map[string]string, src map[string]string, overwrite bool) {
	for key, value := range src {
		if value == "" {
			continue
		}
		normalized := normalizeMetricLabelName(key)
		if normalized == "" || normalized == promlabels.MetricName || normalized == "service" || normalized == "tenant" {
			continue
		}
		if _, exists := dst[normalized]; exists && !overwrite {
			continue
		}
		dst[normalized] = value
	}
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

func (s *Service) handlePrometheusQuery(w http.ResponseWriter, r *http.Request) {
	queryString := requestParam(r, "query")
	if queryString == "" {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "query is required")
		return
	}

	evaluationTime, err := parsePrometheusTime(requestParam(r, "time"), time.Now().UTC())
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}

	engine := newDoctorPrometheusEngine()
	defer func() { _ = engine.Close() }()

	query, err := engine.NewInstantQuery(
		r.Context(),
		doctorPromQueryable{backend: s.logEngine, tenant: s.tenantFromRequest(r)},
		promql.NewPrometheusQueryOpts(false, doctorPrometheusLookback),
		queryString,
		evaluationTime,
	)
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	defer query.Close()

	result := query.Exec(r.Context())
	if result.Err != nil {
		writePrometheusExecutionError(w, result.Err)
		return
	}

	data, err := marshalPrometheusValue(result.Value)
	if err != nil {
		writePrometheusError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writePrometheusSuccess(w, data)
}

func (s *Service) handlePrometheusQueryRange(w http.ResponseWriter, r *http.Request) {
	queryString := requestParam(r, "query")
	if queryString == "" {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "query is required")
		return
	}

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
	if isHighCardinalityCumulativeRangeQuery(queryString) {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "range queries over Guardian cumulative task/result totals are disabled; use an instant query to avoid materializing high-cardinality raw metric segments")
		return
	}

	engine := newDoctorPrometheusEngine()
	defer func() { _ = engine.Close() }()

	query, err := engine.NewRangeQuery(
		r.Context(),
		doctorPromQueryable{backend: s.logEngine, tenant: s.tenantFromRequest(r)},
		promql.NewPrometheusQueryOpts(false, doctorPrometheusLookback),
		queryString,
		start,
		end,
		step,
	)
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	defer query.Close()

	result := query.Exec(r.Context())
	if result.Err != nil {
		writePrometheusExecutionError(w, result.Err)
		return
	}

	data, err := marshalPrometheusValue(result.Value)
	if err != nil {
		writePrometheusError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writePrometheusSuccess(w, data)
}

func isHighCardinalityCumulativeRangeQuery(queryString string) bool {
	return strings.Contains(queryString, "guardian_task_executions") || strings.Contains(queryString, "guardian_result_executions")
}

func (s *Service) handlePrometheusLabels(w http.ResponseWriter, r *http.Request) {
	querier, err := s.discoveryPrometheusQuerier(r)
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	defer func() { _ = querier.Close() }()

	selectorSets, err := parsePrometheusSelectorSets(requestParams(r, "match[]"))
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}

	names := make(map[string]struct{})
	if len(selectorSets) == 0 {
		values, _, err := querier.LabelNames(r.Context(), nil)
		if err != nil {
			writePrometheusError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		for _, value := range values {
			names[value] = struct{}{}
		}
	} else {
		for _, selectorSet := range selectorSets {
			values, _, err := querier.LabelNames(r.Context(), nil, selectorSet...)
			if err != nil {
				writePrometheusError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			for _, value := range values {
				names[value] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	writePrometheusSuccess(w, out)
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

	querier, err := s.discoveryPrometheusQuerier(r)
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	defer func() { _ = querier.Close() }()

	selectorSets, err := parsePrometheusSelectorSets(requestParams(r, "match[]"))
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}

	values := make(map[string]struct{})
	if len(selectorSets) == 0 {
		results, _, err := querier.LabelValues(r.Context(), labelName, nil)
		if err != nil {
			writePrometheusError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		for _, value := range results {
			values[value] = struct{}{}
		}
	} else {
		for _, selectorSet := range selectorSets {
			results, _, err := querier.LabelValues(r.Context(), labelName, nil, selectorSet...)
			if err != nil {
				writePrometheusError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			for _, value := range results {
				values[value] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	writePrometheusSuccess(w, out)
}

func (s *Service) handlePrometheusSeries(w http.ResponseWriter, r *http.Request) {
	selectorSets, err := parsePrometheusSelectorSets(requestParams(r, "match[]"))
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if len(selectorSets) == 0 {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "at least one match[] selector is required")
		return
	}

	querier, err := s.discoveryPrometheusQuerier(r)
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	defer func() { _ = querier.Close() }()

	seen := make(map[string]map[string]string)
	for _, selectorSet := range selectorSets {
		seriesSet := querier.Select(r.Context(), true, nil, selectorSet...)
		for seriesSet.Next() {
			series := seriesSet.At()
			if series == nil {
				continue
			}
			labelSet := series.Labels()
			seen[string(labelSet.Bytes(nil))] = promLabelsToMap(labelSet)
		}
		if err := seriesSet.Err(); err != nil {
			writePrometheusError(w, http.StatusInternalServerError, "internal", err.Error())
			return
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
	querier, err := s.discoveryPrometheusQuerier(r)
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	defer func() { _ = querier.Close() }()

	metricNames, _, err := querier.LabelValues(r.Context(), promlabels.MetricName, nil)
	if err != nil {
		writePrometheusError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	metadata := make(map[string][]map[string]string, len(metricNames))
	for _, metricName := range metricNames {
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
	return doctorPromQueryable{backend: s.logEngine, tenant: s.tenantFromRequest(r)}.Querier(start.UnixMilli(), end.UnixMilli())
}

func newDoctorPrometheusEngine() *promql.Engine {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return promql.NewEngine(promql.EngineOpts{
		Logger:        logger,
		Reg:           promclient.NewRegistry(),
		MaxSamples:    1_000_000,
		Timeout:       doctorPrometheusTimeout,
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
	switch typed := value.(type) {
	case promql.Vector:
		result := make([]map[string]any, 0, len(typed))
		for _, sample := range typed {
			result = append(result, map[string]any{
				"metric": promLabelsToMap(sample.Metric),
				"value":  []any{prometheusJSONTime(sample.T), formatPrometheusSampleValue(sample.F)},
			})
		}
		return map[string]any{"resultType": "vector", "result": result}, nil
	case promql.Matrix:
		result := make([]map[string]any, 0, len(typed))
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

func writePrometheusExecutionError(w http.ResponseWriter, err error) {
	switch err.(type) {
	case promql.ErrQueryTimeout:
		writePrometheusError(w, http.StatusServiceUnavailable, "timeout", err.Error())
	case promql.ErrQueryCanceled:
		writePrometheusError(w, http.StatusServiceUnavailable, "canceled", err.Error())
	case promql.ErrTooManySamples:
		writePrometheusError(w, http.StatusUnprocessableEntity, "execution", err.Error())
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
