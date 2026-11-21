package proxy

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/prometheus/promql/parser"
)

// ErrUnsupportedFederatedAggregation reports that a query is a federation
// aggregate we cannot evaluate correctly from per-shard results (for example a
// quantile, topk, or a native-histogram aggregate).
var ErrUnsupportedFederatedAggregation = errors.New("unsupported federated aggregation")

// AggregationPlan rewrites an associative outer aggregate so each backend
// evaluates the inner expression and Doctor aggregates the merged series.
//
// Example: sum(rate(http_requests_total[5m])) by (service)
//
//	inner  = rate(http_requests_total[5m])
//	op     = sum
//	by     = [service]
type AggregationPlan struct {
	Inner    string
	Op       string
	Grouping []string
	Without  bool
}

// PlanFederatedAggregation recognizes an outer aggregate that is safe to merge
// across independent shards. Supported operators are sum, min, max, count, and
// group. Everything else (avg, quantile, topk, bottomk, stddev, stdvar,
// count_values, limitk, limit_ratio) is returned unsupported because per-shard
// evaluation cannot produce a correct global value.
func PlanFederatedAggregation(query string) (*AggregationPlan, bool, error) {
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(strings.TrimSpace(query))
	if err != nil {
		return nil, false, err
	}
	aggregate, ok := expr.(*parser.AggregateExpr)
	if !ok {
		return nil, false, nil
	}
	op := strings.ToLower(aggregate.Op.String())
	switch op {
	case "sum", "min", "max", "count", "group":
	default:
		return nil, false, nil
	}
	return &AggregationPlan{
		Inner:    aggregate.Expr.String(),
		Op:       op,
		Grouping: append([]string(nil), aggregate.Grouping...),
		Without:  aggregate.Without,
	}, true, nil
}

// Apply aggregates merged per-shard vector or matrix data. The input must
// contain only successful backends; callers must not use this on a partial
// fleet because a missing shard would understate the result.
func (p *AggregationPlan) Apply(data map[string]any) (map[string]any, error) {
	if p == nil || data == nil {
		return data, nil
	}
	resultType, _ := data["resultType"].(string)
	switch resultType {
	case "vector":
		out, err := p.applyVector(PrometheusResultSlice(data))
		if err != nil {
			return nil, err
		}
		return map[string]any{"resultType": "vector", "result": out}, nil
	case "matrix":
		out, err := p.applyMatrix(PrometheusResultSlice(data))
		if err != nil {
			return nil, err
		}
		return map[string]any{"resultType": "matrix", "result": out}, nil
	default:
		return data, nil
	}
}

type aggregateState struct {
	labels map[string]string
	values map[int64]aggregatePoint
}

type aggregatePoint struct {
	timestamp float64
	value     float64
	count     int
	seen      bool
}

func (p *AggregationPlan) applyVector(items []any) ([]any, error) {
	groups := map[string]*aggregateState{}
	order := make([]string, 0)
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := entry["histogram"]; exists {
			return nil, ErrUnsupportedFederatedAggregation
		}
		labels := p.outputLabels(aggregateItemLabels(entry))
		key := seriesLabelKey(labels)
		group := groups[key]
		if group == nil {
			group = &aggregateState{labels: labels, values: map[int64]aggregatePoint{}}
			groups[key] = group
			order = append(order, key)
		}
		timestamp, value, err := aggregateSample(entry["value"])
		if err != nil {
			return nil, err
		}
		group.add(timestamp, value, p.Op)
	}

	sort.Strings(order)
	out := make([]any, 0, len(order))
	for _, key := range order {
		group := groups[key]
		for _, point := range group.sorted() {
			out = append(out, map[string]any{
				"metric": stringLabels(group.labels),
				"value":  []any{point.timestamp, formatAggregateValue(point.value)},
			})
		}
	}
	return out, nil
}

func (p *AggregationPlan) applyMatrix(items []any) ([]any, error) {
	groups := map[string]*aggregateState{}
	order := make([]string, 0)
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := entry["histograms"]; exists {
			return nil, ErrUnsupportedFederatedAggregation
		}
		labels := p.outputLabels(aggregateItemLabels(entry))
		key := seriesLabelKey(labels)
		group := groups[key]
		if group == nil {
			group = &aggregateState{labels: labels, values: map[int64]aggregatePoint{}}
			groups[key] = group
			order = append(order, key)
		}
		var points []any
		switch values := entry["values"].(type) {
		case []any:
			points = values
		case [][]any:
			points = make([]any, 0, len(values))
			for _, point := range values {
				points = append(points, point)
			}
		default:
			continue
		}
		for _, raw := range points {
			timestamp, value, err := aggregateSample(raw)
			if err != nil {
				return nil, err
			}
			group.add(timestamp, value, p.Op)
		}
	}

	sort.Strings(order)
	out := make([]any, 0, len(order))
	for _, key := range order {
		group := groups[key]
		values := make([]any, 0)
		for _, point := range group.sorted() {
			values = append(values, []any{point.timestamp, formatAggregateValue(point.value)})
		}
		out = append(out, map[string]any{
			"metric": stringLabels(group.labels),
			"values": values,
		})
	}
	return out, nil
}

func (s *aggregateState) add(timestamp, value float64, op string) {
	key := int64(math.Round(timestamp * 1000))
	point := s.values[key]
	if !point.seen {
		point = aggregatePoint{timestamp: timestamp, seen: true}
		switch op {
		case "min", "max":
			point.value = value
		case "count", "group":
			point.value = 1
		default:
			point.value = value
		}
		point.count = 1
		s.values[key] = point
		return
	}
	switch op {
	case "min":
		if math.IsNaN(point.value) || (!math.IsNaN(value) && value < point.value) {
			point.value = value
		}
	case "max":
		if math.IsNaN(point.value) || (!math.IsNaN(value) && value > point.value) {
			point.value = value
		}
	case "count":
		point.count++
		point.value = float64(point.count)
	case "group":
		point.value = 1
	default: // sum
		point.value += value
	}
	s.values[key] = point
}

func (s *aggregateState) sorted() []aggregatePoint {
	out := make([]aggregatePoint, 0, len(s.values))
	for _, point := range s.values {
		out = append(out, point)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].timestamp < out[j].timestamp })
	return out
}

// outputLabels applies PromQL aggregation semantics: `by` keeps only the
// grouping labels, `without` removes them, and the metric name plus injected
// provenance/shard labels are dropped unless explicitly grouped.
func (p *AggregationPlan) outputLabels(input map[string]string) map[string]string {
	grouped := make(map[string]struct{}, len(p.Grouping))
	for _, label := range p.Grouping {
		grouped[label] = struct{}{}
	}
	out := make(map[string]string, len(input))
	if p.Without {
		for key, value := range input {
			if key == "__name__" || key == LabelSource || key == LabelShard {
				if _, keep := grouped[key]; !keep {
					continue
				}
			}
			if _, remove := grouped[key]; remove {
				continue
			}
			out[key] = value
		}
		return out
	}
	for key, value := range input {
		if key == "__name__" && len(grouped) == 0 {
			continue
		}
		if key == LabelSource || key == LabelShard {
			if _, keep := grouped[key]; !keep {
				continue
			}
		}
		if _, keep := grouped[key]; keep {
			out[key] = value
		}
	}
	return out
}

func aggregateItemLabels(entry map[string]any) map[string]string {
	switch metric := entry["metric"].(type) {
	case map[string]string:
		return metric
	case map[string]any:
		out := make(map[string]string, len(metric))
		for key, value := range metric {
			if str, ok := value.(string); ok {
				out[key] = str
			}
		}
		return out
	default:
		return map[string]string{}
	}
}

func stringLabels(labels map[string]string) map[string]any {
	out := make(map[string]any, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}

func aggregateSample(raw any) (float64, float64, error) {
	value, ok := raw.([]any)
	if !ok || len(value) != 2 {
		return 0, 0, fmt.Errorf("%w: malformed sample %#v", ErrUnsupportedFederatedAggregation, raw)
	}
	timestamp, err := aggregateNumber(value[0])
	if err != nil {
		return 0, 0, err
	}
	sample, err := aggregateNumber(value[1])
	if err != nil {
		return 0, 0, err
	}
	return timestamp, sample, nil
}

func aggregateNumber(value any) (float64, error) {
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case float32:
		return float64(typed), nil
	case int:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: invalid number %q", ErrUnsupportedFederatedAggregation, typed)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%w: invalid number %v", ErrUnsupportedFederatedAggregation, value)
	}
}

func formatAggregateValue(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
