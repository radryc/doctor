package ruler

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// Querier executes instant PromQL queries through doctor-query. partial is true
// when the proxy reported a degraded response.
type Querier interface {
	PromInstant(ctx context.Context, query string, at time.Time) (data map[string]any, partial bool, err error)
}

// MetricWriter persists derived samples.
type MetricWriter interface {
	WriteMetrics(ctx context.Context, records []segment.MetricPointRecord) error
}

// Evaluator evaluates rule groups and writes derived samples.
type Evaluator struct {
	Querier Querier
	Writer  MetricWriter
	Timeout time.Duration
	Logger  *log.Logger
	Now     func() time.Time
}

// EvalResult summarizes one rule evaluation.
type EvalResult struct {
	Tenant  string `json:"tenant"`
	Group   string `json:"group"`
	Rule    string `json:"rule"`
	Samples int    `json:"samples"`
	Skipped bool   `json:"skipped,omitempty"`
	Partial bool   `json:"partial,omitempty"`
	Error   string `json:"error,omitempty"`
}

// EvaluateGroup evaluates every rule in a group.
func (e *Evaluator) EvaluateGroup(ctx context.Context, group Group) []EvalResult {
	results := make([]EvalResult, 0, len(group.Rules))
	for _, rule := range group.Rules {
		results = append(results, e.EvaluateRule(ctx, group, rule))
	}
	return results
}

// EvaluateRule evaluates one recording rule.
func (e *Evaluator) EvaluateRule(ctx context.Context, group Group, rule Rule) EvalResult {
	result := EvalResult{Tenant: group.Tenant, Group: group.Name, Rule: rule.Record}
	now := e.now()
	at := now.Add(-group.EvaluationDelayOrDefault())

	callCtx := ctx
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}
	data, partial, err := e.Querier.PromInstant(callCtx, rule.Expr, at)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Partial = partial

	samples, err := e.merge(rule, data, at)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// Skip partial sums: adding a partial shard set produces incorrect totals.
	if partial && rule.MergeStrategy() == MergeSum {
		result.Skipped = true
		return result
	}

	records := make([]segment.MetricPointRecord, 0, len(samples))
	for _, sample := range samples {
		records = append(records, e.recordFor(group, rule, sample, at))
	}
	if len(records) > 0 {
		if err := e.Writer.WriteMetrics(ctx, records); err != nil {
			result.Error = err.Error()
			return result
		}
	}
	result.Samples = len(records)
	return result
}

type mergedSample struct {
	labels map[string]string
	value  float64
}

func (e *Evaluator) merge(rule Rule, data map[string]any, at time.Time) ([]mergedSample, error) {
	items := proxy.PrometheusResultSlice(data)
	if len(items) == 0 {
		return nil, nil
	}
	strategy := rule.MergeStrategy()

	order := make([]string, 0, len(items))
	byKey := make(map[string]*mergedSample)
	lastTimestamp := make(map[string]float64)
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		metric, _ := m["metric"].(map[string]any)
		labels := make(map[string]string, len(metric)+len(rule.Labels))
		for key, value := range rule.Labels {
			labels[key] = value
		}
		for key, raw := range metric {
			if key == "__name__" {
				continue
			}
			str, ok := raw.(string)
			if !ok {
				continue
			}
			if (key == proxy.LabelSource || key == proxy.LabelShard) && strategy != MergeKeep {
				continue
			}
			labels[key] = str
		}
		value, timestamp, err := promSampleValue(m)
		if err != nil {
			return nil, err
		}
		key := labelKey(labels)
		existing := byKey[key]
		if existing == nil {
			byKey[key] = &mergedSample{labels: labels, value: value}
			order = append(order, key)
			lastTimestamp[key] = timestamp
			continue
		}
		switch strategy {
		case MergeMax:
			if value > existing.value {
				existing.value = value
			}
		case MergeMin:
			if value < existing.value {
				existing.value = value
			}
		case MergeLast:
			if timestamp >= lastTimestamp[key] {
				existing.value = value
				lastTimestamp[key] = timestamp
			}
		default: // MergeSum, MergeKeep (keep keys differ so no merge)
			existing.value += value
		}
	}

	out := make([]mergedSample, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out, nil
}

func (e *Evaluator) recordFor(group Group, rule Rule, sample mergedSample, at time.Time) segment.MetricPointRecord {
	attributes := make(map[string]string, len(sample.labels)+4)
	service := "recording"
	for key, value := range sample.labels {
		if key == "service" {
			service = value
			continue
		}
		attributes[key] = value
	}
	attributes[proxy.LabelSource] = proxy.SourceRecording
	attributes["rule_group"] = group.Name
	attributes["rule"] = rule.Record
	return segment.MetricPointRecord{
		Tenant:     group.Tenant,
		Service:    service,
		MetricName: rule.Record,
		Type:       "gauge",
		Timestamp:  at,
		HasValue:   true,
		Value:      sample.value,
		Attributes: attributes,
	}
}

func (e *Evaluator) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func promSampleValue(item map[string]any) (float64, float64, error) {
	value, ok := item["value"].([]any)
	if !ok || len(value) != 2 {
		return 0, 0, fmt.Errorf("ruler: malformed sample %#v", item["value"])
	}
	timestamp, err := numericValue(value[0])
	if err != nil {
		return 0, 0, err
	}
	sample, err := numericValue(value[1])
	if err != nil {
		return 0, 0, err
	}
	return sample, timestamp, nil
}

func numericValue(value any) (float64, error) {
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return 0, fmt.Errorf("ruler: invalid numeric value %q", typed)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("ruler: invalid numeric value %v", value)
	}
}

func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(labels[key])
		builder.WriteByte(0)
	}
	return builder.String()
}
