package proxy

import (
	"testing"
)

func TestPlanFederatedAggregation(t *testing.T) {
	plan, ok, err := PlanFederatedAggregation(`sum(rate(http_requests_total[5m])) by (service)`)
	if err != nil || !ok {
		t.Fatalf("plan = %#v, ok=%v, err=%v", plan, ok, err)
	}
	if plan.Op != "sum" || plan.Inner != `rate(http_requests_total[5m])` || len(plan.Grouping) != 1 || plan.Grouping[0] != "service" {
		t.Fatalf("plan = %#v", plan)
	}

	for _, query := range []string{`topk(5, up)`, `quantile(0.9, up)`, `stddev(up)`, `sum(up) / count(up)`} {
		if plan, ok, err := PlanFederatedAggregation(query); err != nil || ok {
			t.Fatalf("%q: plan=%#v ok=%v err=%v", query, plan, ok, err)
		}
	}
}

func TestAggregationPlanApplyVector(t *testing.T) {
	plan, ok, _ := PlanFederatedAggregation(`sum(up) by (service)`)
	if !ok {
		t.Fatal("expected plan")
	}
	data := map[string]any{
		"resultType": "vector",
		"result": []any{
			aggregateVectorItem(map[string]any{"service": "api", "source": "doctor"}, 1, "2"),
			aggregateVectorItem(map[string]any{"service": "api", "source": "prometheus", "shard": "edge"}, 1, "3"),
			aggregateVectorItem(map[string]any{"service": "worker", "source": "doctor"}, 1, "4"),
		},
	}
	out, err := plan.Apply(data)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	result := PrometheusResultSlice(out)
	if len(result) != 2 {
		t.Fatalf("result = %#v", result)
	}
	got := map[string]string{}
	for _, item := range result {
		entry := item.(map[string]any)
		labels := aggregateItemLabels(entry)
		value := entry["value"].([]any)[1].(string)
		got[labels["service"]] = value
		if _, ok := labels["source"]; ok {
			t.Fatalf("source leaked into aggregate: %#v", labels)
		}
	}
	if got["api"] != "5" || got["worker"] != "4" {
		t.Fatalf("got = %#v", got)
	}
}

func TestAggregationPlanGroupAlwaysReturnsOne(t *testing.T) {
	plan, ok, _ := PlanFederatedAggregation(`group(up)`)
	if !ok {
		t.Fatal("expected group plan")
	}
	data := map[string]any{
		"resultType": "vector",
		"result": []any{
			aggregateVectorItem(map[string]any{"job": "api"}, 1, "1"),
			aggregateVectorItem(map[string]any{"job": "api"}, 1, "1"),
			aggregateVectorItem(map[string]any{"job": "api"}, 1, "1"),
		},
	}
	out, err := plan.Apply(data)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	result := PrometheusResultSlice(out)
	if len(result) != 1 || result[0].(map[string]any)["value"].([]any)[1] != "1" {
		t.Fatalf("result = %#v", result)
	}
}

func TestAggregationPlanApplyCountAndRange(t *testing.T) {
	countPlan, ok, _ := PlanFederatedAggregation(`count(up)`)
	if !ok {
		t.Fatal("expected count plan")
	}
	countData := map[string]any{
		"resultType": "vector",
		"result": []any{
			aggregateVectorItem(map[string]any{"instance": "a"}, 1, "7"),
			aggregateVectorItem(map[string]any{"instance": "b"}, 1, "9"),
		},
	}
	out, err := countPlan.Apply(countData)
	if err != nil {
		t.Fatalf("count Apply: %v", err)
	}
	result := PrometheusResultSlice(out)
	if len(result) != 1 {
		t.Fatalf("count result = %#v", result)
	}
	if value := result[0].(map[string]any)["value"].([]any)[1]; value != "2" {
		t.Fatalf("count = %v, want 2", value)
	}

	sumPlan, ok, _ := PlanFederatedAggregation(`sum(up)`)
	if !ok {
		t.Fatal("expected sum plan")
	}
	matrix := map[string]any{
		"resultType": "matrix",
		"result": []any{
			map[string]any{
				"metric": map[string]any{"instance": "a", "source": "doctor"},
				"values": []any{[]any{float64(100), "2"}, []any{float64(200), "3"}},
			},
			map[string]any{
				"metric": map[string]any{"instance": "b", "source": "prometheus"},
				"values": []any{[]any{float64(100), "5"}, []any{float64(200), "7"}},
			},
		},
	}
	out, err = sumPlan.Apply(matrix)
	if err != nil {
		t.Fatalf("matrix Apply: %v", err)
	}
	series := PrometheusResultSlice(out)
	if len(series) != 1 {
		t.Fatalf("matrix result = %#v", series)
	}
	values := series[0].(map[string]any)["values"].([]any)
	if len(values) != 2 {
		t.Fatalf("values = %#v", values)
	}
	if values[0].([]any)[1] != "7" || values[1].([]any)[1] != "10" {
		t.Fatalf("values = %#v", values)
	}
}

func aggregateVectorItem(metric map[string]any, timestamp float64, value string) map[string]any {
	return map[string]any{"metric": metric, "value": []any{timestamp, value}}
}

func TestAggregationPlanPreservesExplicitGrouping(t *testing.T) {
	plan, ok, _ := PlanFederatedAggregation(`sum(up) by (source)`)
	if !ok {
		t.Fatal("expected plan")
	}
	data := map[string]any{
		"resultType": "vector",
		"result": []any{
			aggregateVectorItem(map[string]any{"source": "doctor"}, 1, "2"),
			aggregateVectorItem(map[string]any{"source": "prometheus"}, 1, "3"),
		},
	}
	out, err := plan.Apply(data)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	result := PrometheusResultSlice(out)
	if len(result) != 2 {
		t.Fatalf("result = %#v", result)
	}
	got := map[string]string{}
	for _, item := range result {
		entry := item.(map[string]any)
		labels := aggregateItemLabels(entry)
		got[labels["source"]] = entry["value"].([]any)[1].(string)
	}
	if got["doctor"] != "2" || got["prometheus"] != "3" {
		t.Fatalf("got = %#v", got)
	}
}
