package query

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	promlabels "github.com/prometheus/prometheus/model/labels"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/radryc/doctor/internal/segment"
)

// This file implements the Prometheus endpoints Grafana calls beyond the query
// surface: query_exemplars (exemplar -> trace links) and rules (resource
// probes). Exemplars are read from the stored per-point trace/span correlation
// fields, which never take part in series identity.

const (
	// maxExemplarsPerSeries caps exemplars returned for one series.
	maxExemplarsPerSeries = 100
	// maxExemplarSeries caps how many series may contribute exemplars.
	maxExemplarSeries = 500
	// maxExemplarsTotal caps the exemplars held across all series.
	maxExemplarsTotal = 5000
	// maxExemplarScanRecords bounds the storage scan per selector.
	maxExemplarScanRecords = 500_000
)

// handlePrometheusQueryExemplars serves GET /api/v1/query_exemplars.
func (s *Service) handlePrometheusQueryExemplars(w http.ResponseWriter, r *http.Request) {
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
	start, err := parsePrometheusTime(requestParam(r, "start"), end.Add(-time.Hour))
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if end.Before(start) {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", "end must be after start")
		return
	}

	expr, err := promparser.NewParser(promparser.Options{}).ParseExpr(queryString)
	if err != nil {
		writePrometheusError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}

	selectorSets := promparser.ExtractSelectors(expr)
	if len(selectorSets) == 0 {
		writePrometheusSuccess(w, []any{})
		return
	}

	tenant := s.tenantFromRequest(r)
	querier, err := s.localQuerier(tenant, start, end)
	if err != nil {
		writePrometheusError(w, http.StatusInternalServerError, "execution", err.Error())
		return
	}
	defer func() { _ = querier.Close() }()
	doctorQuerier, ok := querier.(*doctorPromQuerier)
	if !ok {
		writePrometheusError(w, http.StatusInternalServerError, "execution", "unexpected querier type")
		return
	}

	series := make(map[string]*prometheusExemplarSeries)
	order := make([]string, 0)
	for _, matchers := range selectorSets {
		if err := s.collectSelectorExemplars(r.Context(), doctorQuerier, tenant, matchers, start, end, series, &order); err != nil {
			writePrometheusError(w, http.StatusInternalServerError, "execution", err.Error())
			return
		}
	}

	result := make([]any, 0, len(order))
	for _, key := range order {
		entry := series[key]
		sort.SliceStable(entry.exemplars, func(i, j int) bool {
			left, _ := entry.exemplars[i]["timestamp"].(float64)
			right, _ := entry.exemplars[j]["timestamp"].(float64)
			return left < right
		})
		result = append(result, map[string]any{
			"seriesLabels": entry.labels,
			"exemplars":    entry.exemplars,
		})
	}
	writePrometheusSuccess(w, result)
}

type prometheusExemplarSeries struct {
	labels    map[string]string
	exemplars []map[string]any
}

// collectSelectorExemplars streams the stored points behind one vector selector
// and records points that carry trace/span correlation IDs.
func (s *Service) collectSelectorExemplars(ctx context.Context, querier *doctorPromQuerier, tenant string, matchers []*promlabels.Matcher, start, end time.Time, series map[string]*prometheusExemplarSeries, order *[]string) error {
	names, err := querier.resolveStorageMetricNames(ctx, matchers, start, end)
	if err != nil {
		return err
	}
	if names != nil && len(names) == 0 {
		return nil
	}

	base := segment.MetricQuery{Service: exactServiceMatcher(matchers)}
	scanned := 0
	totalExemplars := 0
	emit := func(point segment.MetricPointRecord) error {
		if point.TraceID == "" && point.SpanID == "" {
			return nil
		}
		scanned++
		if scanned > maxExemplarScanRecords {
			return errMetricQueryTooBroad
		}
		labelSet := metricPointLabels(point)
		if !matchersMatch(matchers, labelSet) {
			return nil
		}
		key := labelSet.String()
		entry := series[key]
		if entry == nil {
			if len(series) >= maxExemplarSeries {
				return nil
			}
			entry = &prometheusExemplarSeries{labels: promLabelsToMap(labelSet)}
			series[key] = entry
			*order = append(*order, key)
		}
		if len(entry.exemplars) >= maxExemplarsPerSeries || totalExemplars >= maxExemplarsTotal {
			return nil
		}
		labels := make(map[string]string, 2)
		if point.TraceID != "" {
			labels["trace_id"] = point.TraceID
		}
		if point.SpanID != "" {
			labels["span_id"] = point.SpanID
		}
		entry.exemplars = append(entry.exemplars, map[string]any{
			"labels":    labels,
			"value":     strconv.FormatFloat(point.Value, 'f', -1, 64),
			"timestamp": float64(point.Timestamp.UTC().UnixMilli()) / 1000,
		})
		totalExemplars++
		return nil
	}

	if names == nil {
		if err := querier.backend.StreamMetrics(ctx, tenant, base, start, end, emit); err != nil {
			return translateMetricLoadError(err)
		}
	} else {
		for _, name := range names {
			query := base
			query.MetricName = name
			if err := querier.backend.StreamMetrics(ctx, tenant, query, start, end, emit); err != nil {
				return translateMetricLoadError(err)
			}
		}
	}
	return nil
}

// matchersMatch applies Prometheus label matchers to a projected label set.
func matchersMatch(matchers []*promlabels.Matcher, labelSet promlabels.Labels) bool {
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

// handlePrometheusRules serves GET /api/v1/rules. Doctor does not expose rule
// management through the Prometheus API; returning an empty group list keeps
// Grafana's resource probes green.
func (s *Service) handlePrometheusRules(w http.ResponseWriter, _ *http.Request) {
	writePrometheusSuccess(w, map[string]any{"groups": []any{}})
}
