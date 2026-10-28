package ruler

import (
	"context"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

const validGroupYAML = `
version: 1
groups:
  - name: shard-health
    tenant: default
    interval: 30s
    evaluation_delay: 10s
    rules:
      - record: doctor:shard_up:sum
        expr: sum(up) by (job)
        labels:
          tier: edge
        sources: [prom-a]
        merge: sum
`

func TestParseFileValid(t *testing.T) {
	file, err := ParseFile([]byte(validGroupYAML))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	group := file.Groups[0]
	if group.Name != "shard-health" || group.IntervalOrDefault() != 30*time.Second {
		t.Fatalf("group = %#v", group)
	}
	if group.EvaluationDelayOrDefault() != 10*time.Second {
		t.Fatalf("evaluation delay = %s", group.EvaluationDelayOrDefault())
	}
	rule := group.Rules[0]
	if rule.Record != "doctor:shard_up:sum" || rule.MergeStrategy() != MergeSum {
		t.Fatalf("rule = %#v", rule)
	}
}

func TestParseFileRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"no groups":      `version: 1`,
		"no rules":       "version: 1\ngroups:\n  - name: g\n",
		"bad name":       "version: 1\ngroups:\n  - name: g\n    rules:\n      - record: '1bad'\n        expr: up\n",
		"no expr":        "version: 1\ngroups:\n  - name: g\n    rules:\n      - record: good\n",
		"dup record":     "version: 1\ngroups:\n  - name: g\n    rules:\n      - record: good\n        expr: up\n      - record: good\n        expr: up\n",
		"bad merge":      "version: 1\ngroups:\n  - name: g\n    rules:\n      - record: good\n        expr: up\n        merge: nope\n",
		"short interval": "version: 1\ngroups:\n  - name: g\n    interval: 1s\n    rules:\n      - record: good\n        expr: up\n",
		"bad duration":   "version: 1\ngroups:\n  - name: g\n    interval: soon\n    rules:\n      - record: good\n        expr: up\n",
	}
	for name, data := range cases {
		if _, err := ParseFile([]byte(data)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

type fakeQuerier struct {
	data    map[string]any
	partial bool
	err     error
	calls   int
}

func (f *fakeQuerier) PromInstant(context.Context, string, time.Time) (map[string]any, bool, error) {
	f.calls++
	return f.data, f.partial, f.err
}

type fakeWriter struct {
	records []segment.MetricPointRecord
}

func (f *fakeWriter) WriteMetrics(_ context.Context, records []segment.MetricPointRecord) error {
	f.records = append(f.records, records...)
	return nil
}

func vector(samples ...map[string]any) map[string]any {
	result := make([]any, 0, len(samples))
	for _, sample := range samples {
		result = append(result, sample)
	}
	return map[string]any{"resultType": "vector", "result": result}
}

func sample(labels map[string]string, value string) map[string]any {
	metric := map[string]any{}
	for key, label := range labels {
		metric[key] = label
	}
	return map[string]any{"metric": metric, "value": []any{float64(1780000000), value}}
}

func testGroup(rule Rule) Group {
	return Group{Name: "g", Tenant: "default", Rules: []Rule{rule}}
}

func TestEvaluatorMergesPerShardSums(t *testing.T) {
	querier := &fakeQuerier{data: vector(
		sample(map[string]string{"job": "api", "source": "prometheus", "shard": "prom-a"}, "1"),
		sample(map[string]string{"job": "api", "source": "prometheus", "shard": "prom-b"}, "2"),
	)}
	writer := &fakeWriter{}
	evaluator := &Evaluator{Querier: querier, Writer: writer, Now: func() time.Time { return time.Unix(1780000000, 0).UTC() }}

	group := testGroup(Rule{Record: "doctor:up:sum", Expr: "sum(up) by (job)", Labels: map[string]string{"tier": "edge"}})
	results := evaluator.EvaluateGroup(context.Background(), group)
	if len(results) != 1 || results[0].Error != "" || results[0].Samples != 1 {
		t.Fatalf("results = %#v", results)
	}
	if len(writer.records) != 1 {
		t.Fatalf("records = %#v", writer.records)
	}
	record := writer.records[0]
	if record.MetricName != "doctor:up:sum" || record.Value != 3 {
		t.Fatalf("record = %#v", record)
	}
	if record.Attributes["tier"] != "edge" || record.Attributes["job"] != "api" {
		t.Fatalf("attributes = %#v", record.Attributes)
	}
	if record.Attributes["source"] != "recording" || record.Attributes["rule_group"] != "g" {
		t.Fatalf("recording attributes = %#v", record.Attributes)
	}
	if _, exists := record.Attributes["shard"]; exists {
		t.Fatalf("sum merge kept shard label: %#v", record.Attributes)
	}
}

func TestEvaluatorMergeStrategies(t *testing.T) {
	data := vector(
		sample(map[string]string{"job": "api", "shard": "a"}, "1"),
		sample(map[string]string{"job": "api", "shard": "b"}, "3"),
	)
	cases := map[MergeStrategy]struct {
		wantSamples int
		wantValue   float64
	}{
		MergeSum: {1, 4},
		MergeMax: {1, 3},
		MergeMin: {1, 1},
		// Samples share an instant-query timestamp, so "last" is the last
		// sample in deterministic input order.
		MergeLast: {1, 3},
		MergeKeep: {2, 0},
	}
	for strategy, want := range cases {
		writer := &fakeWriter{}
		evaluator := &Evaluator{Querier: &fakeQuerier{data: data}, Writer: writer}
		group := testGroup(Rule{Record: "doctor:up", Expr: "up", Merge: strategy})
		results := evaluator.EvaluateGroup(context.Background(), group)
		if results[0].Error != "" || results[0].Samples != want.wantSamples {
			t.Fatalf("%s: results = %#v", strategy, results)
		}
		if want.wantValue != 0 && writer.records[0].Value != want.wantValue {
			t.Fatalf("%s: value = %v, want %v", strategy, writer.records[0].Value, want.wantValue)
		}
	}
}

func TestEvaluatorSkipsPartialSum(t *testing.T) {
	querier := &fakeQuerier{
		data:    vector(sample(map[string]string{"job": "api"}, "1")),
		partial: true,
	}
	writer := &fakeWriter{}
	evaluator := &Evaluator{Querier: querier, Writer: writer}
	group := testGroup(Rule{Record: "doctor:up:sum", Expr: "sum(up)"})
	results := evaluator.EvaluateGroup(context.Background(), group)
	if len(results) != 1 || !results[0].Skipped || results[0].Samples != 0 {
		t.Fatalf("results = %#v", results)
	}
	if len(writer.records) != 0 {
		t.Fatalf("partial sum was written: %#v", writer.records)
	}
}

func TestEvaluatorQueryErrorRecorded(t *testing.T) {
	evaluator := &Evaluator{Querier: &fakeQuerier{err: context.DeadlineExceeded}, Writer: &fakeWriter{}}
	results := evaluator.EvaluateGroup(context.Background(), testGroup(Rule{Record: "r", Expr: "up"}))
	if len(results) != 1 || results[0].Error == "" {
		t.Fatalf("results = %#v", results)
	}
}
