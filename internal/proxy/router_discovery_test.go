package proxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type discoveryUpstream struct {
	name    string
	series  []map[string]string
	labels  []string
	values  map[string][]string
	err     error
	mu      sync.Mutex
	queries [][]string
}

func (u *discoveryUpstream) Name() string { return u.name }

func (u *discoveryUpstream) PromInstant(context.Context, string, string, time.Time) (map[string]any, error) {
	return emptyVector(), nil
}

func (u *discoveryUpstream) PromRange(context.Context, string, string, time.Time, time.Time, time.Duration) (map[string]any, error) {
	return emptyVector(), nil
}

func (u *discoveryUpstream) PromSeries(_ context.Context, _ string, match []string, _, _ time.Time) ([]map[string]string, error) {
	u.record(match)
	return u.series, u.err
}

func (u *discoveryUpstream) PromLabels(_ context.Context, _ string, match []string, _, _ time.Time) ([]string, error) {
	u.record(match)
	return u.labels, u.err
}

func (u *discoveryUpstream) PromLabelValues(_ context.Context, _, label string, match []string, _, _ time.Time) ([]string, error) {
	u.record(match)
	return u.values[label], u.err
}

func (u *discoveryUpstream) record(match []string) {
	u.mu.Lock()
	u.queries = append(u.queries, append([]string(nil), match...))
	u.mu.Unlock()
}

func (u *discoveryUpstream) lastQueries() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.queries) == 0 {
		return nil
	}
	return u.queries[len(u.queries)-1]
}

type discoveryBackend struct {
	backend  Backend
	upstream *discoveryUpstream
}

func newDiscoveryRouter(t *testing.T, backends ...discoveryBackend) *Router {
	t.Helper()
	router := NewRouter(RouterConfig{MaxInflight: 4, BackendTimeout: time.Second}, nil)
	for _, item := range backends {
		if err := router.Register(item.backend, Upstreams{Metrics: item.upstream}); err != nil {
			t.Fatalf("register %s: %v", item.backend.Name, err)
		}
	}
	return router
}

func localBackend(name string) Backend {
	return Backend{Name: name, Kind: KindMonoFS, Origin: "local", Signals: []Signal{SignalMetrics}}
}

func remoteBackend(name, kind string) Backend {
	backend := Backend{Name: name, Kind: kind, Signals: []Signal{SignalMetrics}}
	if kind == KindPrometheus {
		backend.BaseURL = "http://127.0.0.1:9090"
	}
	return backend
}

func TestRouterDiscoveryMergesSeriesAndStampsSource(t *testing.T) {
	local := &discoveryUpstream{
		name:   "monofs",
		series: []map[string]string{{"__name__": "up", "job": "local"}},
	}
	remote := &discoveryUpstream{
		name:   "edge-prom",
		series: []map[string]string{{"__name__": "up", "job": "remote"}},
	}
	router := newDiscoveryRouter(t,
		discoveryBackend{localBackend("monofs"), local},
		discoveryBackend{remoteBackend("edge-prom", KindPrometheus), remote},
	)

	series, warnings, err := router.MetricsSeries(context.Background(), "t", []string{`up`}, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatalf("MetricsSeries: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if len(series) != 2 {
		t.Fatalf("series = %#v", series)
	}
	sources := map[string]string{}
	for _, labels := range series {
		sources[labels["job"]] = labels[LabelSource]
	}
	if sources["local"] != SourceDoctor || sources["remote"] != KindPrometheus {
		t.Fatalf("sources = %#v", sources)
	}
}

func TestRouterDiscoveryHonorsSourceMatcher(t *testing.T) {
	local := &discoveryUpstream{name: "monofs", series: []map[string]string{{"job": "local"}}}
	aws := &discoveryUpstream{name: "aws", series: []map[string]string{{"job": "aws"}}}
	router := newDiscoveryRouter(t,
		discoveryBackend{localBackend("monofs"), local},
		discoveryBackend{remoteBackend("aws", KindAWS), aws},
	)

	series, _, err := router.MetricsSeries(context.Background(), "t", []string{`up{source="aws"}`}, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatalf("MetricsSeries: %v", err)
	}
	if len(series) != 1 || series[0]["job"] != "aws" {
		t.Fatalf("series = %#v", series)
	}
	if got := aws.lastQueries(); len(got) != 1 || strings.Contains(got[0], "source=") {
		t.Fatalf("aws selectors = %#v, want source stripped", got)
	}
	if len(local.queries) != 0 {
		t.Fatalf("local should not be queried: %#v", local.queries)
	}
}

func TestRouterDiscoveryWarnsOnRemoteFailure(t *testing.T) {
	local := &discoveryUpstream{name: "monofs", labels: []string{"__name__"}}
	remote := &discoveryUpstream{name: "edge-prom", err: errors.New("boom")}
	router := newDiscoveryRouter(t,
		discoveryBackend{localBackend("monofs"), local},
		discoveryBackend{remoteBackend("edge-prom", KindPrometheus), remote},
	)

	labels, warnings, err := router.MetricsLabels(context.Background(), "t", nil, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatalf("MetricsLabels: %v", err)
	}
	if len(warnings) != 1 || warnings[0].Backend != "edge-prom" {
		t.Fatalf("warnings = %#v", warnings)
	}
	if len(labels) != 2 { // __name__ plus source
		t.Fatalf("labels = %#v", labels)
	}
}

func TestRouterDiscoveryLocalFailureIsFatal(t *testing.T) {
	local := &discoveryUpstream{name: "monofs", err: errors.New("boom")}
	router := newDiscoveryRouter(t, discoveryBackend{localBackend("monofs"), local})

	if _, _, err := router.MetricsLabels(context.Background(), "t", nil, time.Time{}, time.Time{}, ""); err == nil {
		t.Fatal("expected local discovery failure")
	}
}

func TestRouterDiscoveryAppliesPartition(t *testing.T) {
	local := &discoveryUpstream{name: "monofs", values: map[string][]string{"__name__": []string{"up"}}}
	router := newDiscoveryRouter(t, discoveryBackend{localBackend("monofs"), local})

	values, _, err := router.MetricsLabelValues(context.Background(), "t", "__name__", []string{`up`}, time.Time{}, time.Time{}, "edge")
	if err != nil {
		t.Fatalf("MetricsLabelValues: %v", err)
	}
	if len(values) != 1 || values[0].Value != "up" || values[0].Source != SourceDoctor {
		t.Fatalf("values = %#v", values)
	}
	queries := local.lastQueries()
	if len(queries) != 1 || !strings.Contains(queries[0], `guardian_partition="edge"`) {
		t.Fatalf("selectors = %#v, want partition matcher", queries)
	}
}
