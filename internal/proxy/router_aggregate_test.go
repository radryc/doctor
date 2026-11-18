package proxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func aggregateTestRouter(t *testing.T, local, remote *fakeMetricsUpstream) *Router {
	t.Helper()
	router := NewRouter(RouterConfig{MaxInflight: 4, BackendTimeout: time.Second}, nil)
	if err := router.Register(Backend{
		Name:    "monofs",
		Kind:    KindMonoFS,
		Origin:  "local",
		Signals: []Signal{SignalMetrics},
	}, Upstreams{Metrics: local}); err != nil {
		t.Fatalf("register local: %v", err)
	}
	if err := router.Register(Backend{
		Name:    "edge-prom",
		Kind:    KindPrometheus,
		BaseURL: "http://edge-prom",
		Signals: []Signal{SignalMetrics},
		Policy:  PolicyAlways,
	}, Upstreams{Metrics: remote}); err != nil {
		t.Fatalf("register remote: %v", err)
	}
	return router
}

func aggregateVector(service, source string, value string) map[string]any {
	return map[string]any{
		"resultType": "vector",
		"result": []any{map[string]any{
			"metric": map[string]any{"__name__": "up", "service": service, "source": source},
			"value":  []any{float64(time.Now().Unix()), value},
		}},
	}
}

func TestRouterAppliesGlobalAggregationAcrossBackends(t *testing.T) {
	local := &fakeMetricsUpstream{name: "monofs", instant: aggregateVector("api", "doctor", "2")}
	remote := &fakeMetricsUpstream{name: "edge-prom", instant: aggregateVector("api", "prometheus", "3")}
	router := aggregateTestRouter(t, local, remote)

	data, warnings, meta, err := router.MetricsInstantWithMeta(context.Background(), "default", `sum(up) by (service)`, time.Now())
	if err != nil {
		t.Fatalf("MetricsInstantWithMeta: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if meta.Aggregation != "global" || meta.Backends != 2 {
		t.Fatalf("meta = %#v", meta)
	}
	result := PrometheusResultSlice(data)
	if len(result) != 1 {
		t.Fatalf("result = %#v", result)
	}
	entry := result[0].(map[string]any)
	labels := aggregateItemLabels(entry)
	if labels["service"] != "api" {
		t.Fatalf("labels = %#v", labels)
	}
	if _, ok := labels["source"]; ok {
		t.Fatalf("source leaked into global aggregate: %#v", labels)
	}
	if value := entry["value"].([]any)[1]; value != "5" {
		t.Fatalf("value = %v, want 5", value)
	}
}

func TestRouterDoesNotAggregatePartialFleet(t *testing.T) {
	local := &fakeMetricsUpstream{name: "monofs", instant: aggregateVector("api", "doctor", "2")}
	remote := &fakeMetricsUpstream{name: "edge-prom", err: errors.New("boom")}
	router := aggregateTestRouter(t, local, remote)

	data, warnings, meta, err := router.MetricsInstantWithMeta(context.Background(), "default", `sum(up) by (service)`, time.Now())
	if err != nil {
		t.Fatalf("MetricsInstantWithMeta: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected a partial-fleet warning")
	}
	if meta.Aggregation != "per-shard" {
		t.Fatalf("meta = %#v", meta)
	}
	result := PrometheusResultSlice(data)
	if len(result) != 1 {
		t.Fatalf("result = %#v", result)
	}
	entry := result[0].(map[string]any)
	if value := entry["value"].([]any)[1]; value != "2" {
		t.Fatalf("value = %v, want the per-shard local value 2", value)
	}
}

func TestRouterMarksNonAggregatableQueriesPerShard(t *testing.T) {
	local := &fakeMetricsUpstream{name: "monofs", instant: aggregateVector("api", "doctor", "2")}
	remote := &fakeMetricsUpstream{name: "edge-prom", instant: aggregateVector("api", "prometheus", "3")}
	router := aggregateTestRouter(t, local, remote)

	data, warnings, meta, err := router.MetricsInstantWithMeta(context.Background(), "default", `up`, time.Now())
	if err != nil {
		t.Fatalf("MetricsInstantWithMeta: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if meta.Aggregation != "per-shard" {
		t.Fatalf("meta = %#v", meta)
	}
	if got := len(PrometheusResultSlice(data)); got != 2 {
		t.Fatalf("result length = %d, want 2", got)
	}
}

func TestRouterAggregatesSingleBackendInnerExpression(t *testing.T) {
	local := &fakeMetricsUpstream{name: "monofs", instant: aggregateVector("api", "doctor", "2")}
	router := NewRouter(RouterConfig{MaxInflight: 4, BackendTimeout: time.Second}, nil)
	if err := router.Register(Backend{
		Name:    "monofs",
		Kind:    KindMonoFS,
		Origin:  "local",
		Signals: []Signal{SignalMetrics},
	}, Upstreams{Metrics: local}); err != nil {
		t.Fatalf("register local: %v", err)
	}

	data, warnings, meta, err := router.MetricsInstantWithMeta(context.Background(), "default", `sum(up) by (service)`, time.Now())
	if err != nil {
		t.Fatalf("MetricsInstantWithMeta: %v", err)
	}
	if len(warnings) != 0 || meta.Aggregation != "" {
		t.Fatalf("warnings=%v meta=%#v", warnings, meta)
	}
	result := PrometheusResultSlice(data)
	if len(result) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if value := result[0].(map[string]any)["value"].([]any)[1]; value != "2" {
		t.Fatalf("value = %v", value)
	}
}
