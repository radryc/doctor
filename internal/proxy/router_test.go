package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

type fakeMetricsUpstream struct {
	name      string
	instant   map[string]any
	rangeData map[string]any
	err       error

	mu        sync.Mutex
	calls     int
	lastQuery string
}

func (f *fakeMetricsUpstream) Name() string { return f.name }

func (f *fakeMetricsUpstream) PromInstant(_ context.Context, _, query string, _ time.Time) (map[string]any, error) {
	f.mu.Lock()
	f.calls++
	f.lastQuery = query
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.instant, nil
}

func (f *fakeMetricsUpstream) PromRange(_ context.Context, _, query string, _, _ time.Time, _ time.Duration) (map[string]any, error) {
	f.mu.Lock()
	f.calls++
	f.lastQuery = query
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.rangeData, nil
}

func (f *fakeMetricsUpstream) PromSeries(context.Context, string, []string, time.Time, time.Time) ([]map[string]string, error) {
	return nil, f.err
}

func (f *fakeMetricsUpstream) PromLabels(context.Context, string, []string, time.Time, time.Time) ([]string, error) {
	return nil, f.err
}

func (f *fakeMetricsUpstream) PromLabelValues(context.Context, string, string, []string, time.Time, time.Time) ([]string, error) {
	return nil, f.err
}

func (f *fakeMetricsUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeLogsUpstream struct {
	name    string
	records []segment.LogRecord
	err     error

	mu    sync.Mutex
	calls int
}

func (f *fakeLogsUpstream) Name() string { return f.name }

func (f *fakeLogsUpstream) QueryLogs(context.Context, string, LogPushdown, time.Time, time.Time, int) ([]segment.LogRecord, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.records, nil
}

func emptyVector() map[string]any {
	return map[string]any{"resultType": "vector", "result": []any{}}
}

func vectorWithSeries(source string) map[string]any {
	metric := map[string]any{"__name__": "up", "instance": "x"}
	if source != "" {
		metric[LabelSource] = source
	}
	return map[string]any{
		"resultType": "vector",
		"result": []any{map[string]any{
			"metric": metric,
			"value":  []any{float64(1780000000), "1"},
		}},
	}
}

func newTestRouter(t *testing.T, backends map[string]Backend, upstreams map[string]Upstreams) *Router {
	t.Helper()
	router := NewRouter(RouterConfig{MaxInflight: 8, BackendTimeout: time.Second}, NewCompositeRegistry())
	for name, upstream := range upstreams {
		if err := router.Register(backends[name], upstream); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	return router
}

func TestRouterMetricsFanoutMergesWithShard(t *testing.T) {
	local := &fakeMetricsUpstream{name: "doctor", instant: vectorWithSeries(SourceDoctor)}
	promA := &fakeMetricsUpstream{name: "prom-a", instant: vectorWithSeries("")}
	promB := &fakeMetricsUpstream{name: "prom-b", instant: vectorWithSeries("")}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
			"prom-a": {Name: "prom-a", Kind: KindPrometheus, BaseURL: "http://prom-a", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
			"prom-b": {Name: "prom-b", Kind: KindPrometheus, BaseURL: "http://prom-b", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
		},
		map[string]Upstreams{
			"doctor": {Metrics: local},
			"prom-a": {Metrics: promA},
			"prom-b": {Metrics: promB},
		},
	)

	data, warnings, err := router.MetricsInstant(context.Background(), "default", "up", time.Now())
	if err != nil {
		t.Fatalf("MetricsInstant: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	items := PrometheusResultSlice(data)
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	// Native doctor series keeps no shard; the two prometheus shards are
	// disambiguated with distinct shard labels.
	sourceShards := map[string][]string{}
	for _, item := range items {
		metric := item.(map[string]any)["metric"].(map[string]any)
		source, _ := metric[LabelSource].(string)
		shard, _ := metric[LabelShard].(string)
		sourceShards[source] = append(sourceShards[source], shard)
	}
	if got := sourceShards[SourceDoctor]; len(got) != 1 || got[0] != "" {
		t.Fatalf("doctor shards = %#v, want one empty shard", got)
	}
	prometheusShards := sourceShards["prometheus"]
	if len(prometheusShards) != 2 {
		t.Fatalf("prometheus shards = %#v, want two", prometheusShards)
	}
	seen := map[string]bool{}
	for _, shard := range prometheusShards {
		if shard != "prom-a" && shard != "prom-b" {
			t.Fatalf("unexpected shard %q", shard)
		}
		seen[shard] = true
	}
	if !seen["prom-a"] || !seen["prom-b"] {
		t.Fatalf("shards = %#v, want prom-a and prom-b", prometheusShards)
	}
}

func TestRouterMetricsFallbackPolicy(t *testing.T) {
	local := &fakeMetricsUpstream{name: "doctor", instant: emptyVector()}
	aws := &fakeMetricsUpstream{name: "aws", instant: vectorWithSeries("")}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
			"aws":    {Name: "aws", Kind: KindAWS, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
		},
		map[string]Upstreams{
			"doctor": {Metrics: local},
			"aws":    {Metrics: aws},
		},
	)

	if _, _, err := router.MetricsInstant(context.Background(), "default", "CPUUtilization", time.Now()); err != nil {
		t.Fatalf("MetricsInstant: %v", err)
	}
	if aws.callCount() != 1 {
		t.Fatalf("aws calls = %d, want 1 after empty local result", aws.callCount())
	}

	aws.calls = 0
	local.instant = vectorWithSeries(SourceDoctor)
	if _, _, err := router.MetricsInstant(context.Background(), "default", "up", time.Now()); err != nil {
		t.Fatalf("MetricsInstant: %v", err)
	}
	if aws.callCount() != 0 {
		t.Fatalf("aws calls = %d, want 0 when local has data", aws.callCount())
	}
}

func TestRouterMetricsAWSOnlyStripsSource(t *testing.T) {
	local := &fakeMetricsUpstream{name: "doctor", instant: vectorWithSeries(SourceDoctor)}
	aws := &fakeMetricsUpstream{name: "aws", instant: vectorWithSeries("")}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
			"aws":    {Name: "aws", Kind: KindAWS, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
		},
		map[string]Upstreams{
			"doctor": {Metrics: local},
			"aws":    {Metrics: aws},
		},
	)

	data, _, err := router.MetricsInstant(context.Background(), "default", `CPUUtilization{source="aws"}`, time.Now())
	if err != nil {
		t.Fatalf("MetricsInstant: %v", err)
	}
	if local.callCount() != 0 {
		t.Fatalf("local calls = %d, want 0 for aws-only query", local.callCount())
	}
	if aws.callCount() != 1 {
		t.Fatalf("aws calls = %d, want 1", aws.callCount())
	}
	if aws.lastQuery != "CPUUtilization" {
		t.Fatalf("aws query = %q, want stripped", aws.lastQuery)
	}
	items := PrometheusResultSlice(data)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
}

func TestRouterMetricsWarnsOnRemoteFailure(t *testing.T) {
	local := &fakeMetricsUpstream{name: "doctor", instant: vectorWithSeries(SourceDoctor)}
	remote := &fakeMetricsUpstream{name: "prom-a", err: errors.New("boom")}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
			"prom-a": {Name: "prom-a", Kind: KindPrometheus, BaseURL: "http://prom-a", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
		},
		map[string]Upstreams{
			"doctor": {Metrics: local},
			"prom-a": {Metrics: remote},
		},
	)

	data, warnings, err := router.MetricsInstant(context.Background(), "default", "up", time.Now())
	if err != nil {
		t.Fatalf("MetricsInstant: %v", err)
	}
	if len(warnings) != 1 || warnings[0].Backend != "prom-a" {
		t.Fatalf("warnings = %#v", warnings)
	}
	if len(PrometheusResultSlice(data)) != 1 {
		t.Fatalf("local result lost on remote failure")
	}
}

func TestRouterMetricsLocalParseErrorFatal(t *testing.T) {
	local := &fakeMetricsUpstream{name: "doctor", err: &QueryError{Err: errors.New("bad query"), Parse: true}}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
		},
		map[string]Upstreams{"doctor": {Metrics: local}},
	)
	_, _, err := router.MetricsInstant(context.Background(), "default", "up{", time.Now())
	if err == nil || !IsParseError(err) {
		t.Fatalf("err = %v, want parse error", err)
	}
}

func TestLocalBackendFailuresDoNotMarkEndpointUnhealthy(t *testing.T) {
	local := &fakeLogsUpstream{name: "doctor", err: errors.New("query failed")}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalLogs}, Policy: PolicyAlways},
		},
		map[string]Upstreams{"doctor": {Logs: local}},
	)

	for i := 0; i < breakerThreshold+2; i++ {
		_, _, _ = router.Logs(context.Background(), "default", LogPushdown{Selector: `{service="api"}`}, time.Now().Add(-time.Hour), time.Now(), 10)
	}
	if got := local.calls; got != breakerThreshold+2 {
		t.Fatalf("local calls = %d, want %d (local backend must not be circuit-broken)", got, breakerThreshold+2)
	}
	summary := router.SummarizeEndpoints()
	if summary.Healthy != 1 || summary.Unhealthy != 0 {
		t.Fatalf("summary = %#v, want the local backend healthy", summary)
	}
	endpoints := router.Endpoints(context.Background())
	if len(endpoints) != 1 || !endpoints[0].Healthy {
		t.Fatalf("endpoints = %#v, want local backend healthy", endpoints)
	}
	if endpoints[0].Health != "healthy" {
		t.Fatalf("health = %q, want healthy", endpoints[0].Health)
	}
}

func TestRemoteParseErrorsDoNotOpenBreaker(t *testing.T) {
	remote := &fakeMetricsUpstream{name: "prom-a", err: &QueryError{Err: errors.New("bad query"), Parse: true}}
	router := newTestRouter(t,
		map[string]Backend{
			"prom-a": {Name: "prom-a", Kind: KindPrometheus, BaseURL: "http://prom-a", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
		},
		map[string]Upstreams{"prom-a": {Metrics: remote}},
	)

	for i := 0; i < breakerThreshold+2; i++ {
		_, _, _ = router.MetricsInstant(context.Background(), "default", "up{", time.Now())
	}
	if got := remote.callCount(); got != breakerThreshold+2 {
		t.Fatalf("remote calls = %d, want %d (parse errors must not open the breaker)", got, breakerThreshold+2)
	}
}

func TestRemoteScopeErrorsDoNotOpenBreaker(t *testing.T) {
	remote := &fakeMetricsUpstream{name: "aws", err: ErrScopeRequired}
	router := newTestRouter(t,
		map[string]Backend{
			"aws": {Name: "aws", Kind: KindAWS, Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
		},
		map[string]Upstreams{"aws": {Metrics: remote}},
	)

	for i := 0; i < breakerThreshold+2; i++ {
		if _, _, err := router.MetricsInstant(context.Background(), "default", `sum(up{source="aws"})`, time.Now()); !errors.Is(err, ErrScopeRequired) {
			t.Fatalf("MetricsInstant error = %v, want ErrScopeRequired", err)
		}
	}
	if got := remote.callCount(); got != breakerThreshold+2 {
		t.Fatalf("remote calls = %d, want %d (scope errors must not open the breaker)", got, breakerThreshold+2)
	}
	summary := router.SummarizeEndpoints()
	if summary.Healthy != 1 || summary.Unhealthy != 0 {
		t.Fatalf("summary = %#v, want AWS backend healthy", summary)
	}
}

func TestRouterLogsMergesLocalFirst(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	local := &fakeLogsUpstream{name: "doctor", records: []segment.LogRecord{{
		Service: "api", Timestamp: base, Body: "local",
	}}}
	loki := &fakeLogsUpstream{name: "loki-a", records: []segment.LogRecord{{
		Service: "api", Timestamp: base.Add(time.Second), Body: "remote",
	}}}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalLogs}, Policy: PolicyAlways},
			"loki-a": {Name: "loki-a", Kind: KindLoki, BaseURL: "http://loki-a", Signals: []Signal{SignalLogs}, Policy: PolicyAlways},
		},
		map[string]Upstreams{
			"doctor": {Logs: local},
			"loki-a": {Logs: loki},
		},
	)

	records, warnings, err := router.Logs(context.Background(), "default", LogPushdown{Selector: `{service="api"}`}, base.Add(-time.Hour), base.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	if len(records) != 2 || records[0].Body != "local" || records[1].Body != "remote" {
		t.Fatalf("records = %#v", records)
	}
	if records[1].Attributes[LabelSource] != "loki" {
		t.Fatalf("remote source = %q, want loki", records[1].Attributes[LabelSource])
	}
}

func TestRouterBreakerOpensAfterFailures(t *testing.T) {
	local := &fakeMetricsUpstream{name: "doctor", instant: vectorWithSeries(SourceDoctor)}
	remote := &fakeMetricsUpstream{name: "prom-a", err: errors.New("boom")}
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
			"prom-a": {Name: "prom-a", Kind: KindPrometheus, BaseURL: "http://prom-a", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
		},
		map[string]Upstreams{
			"doctor": {Metrics: local},
			"prom-a": {Metrics: remote},
		},
	)

	for i := 0; i < breakerThreshold; i++ {
		if _, _, err := router.MetricsInstant(context.Background(), "default", "up", time.Now()); err != nil {
			t.Fatalf("MetricsInstant: %v", err)
		}
	}
	callsAfterFailures := remote.callCount()
	if callsAfterFailures != breakerThreshold {
		t.Fatalf("remote calls = %d, want %d", callsAfterFailures, breakerThreshold)
	}

	// The breaker is open: the next query must not reach the failing backend.
	if _, _, err := router.MetricsInstant(context.Background(), "default", "up", time.Now()); err != nil {
		t.Fatalf("MetricsInstant: %v", err)
	}
	if remote.callCount() != callsAfterFailures {
		t.Fatalf("remote was called while breaker open")
	}
}

func TestRouterEndpointsIncludesExplicitBackends(t *testing.T) {
	router := newTestRouter(t,
		map[string]Backend{
			"doctor": {Name: "doctor", Kind: KindMonoFS, Signals: []Signal{SignalMetrics}},
		},
		map[string]Upstreams{"doctor": {Metrics: &fakeMetricsUpstream{name: "doctor"}}},
	)
	endpoints := router.Endpoints(context.Background())
	if len(endpoints) != 1 || endpoints[0].Name != "doctor" || !endpoints[0].Healthy {
		t.Fatalf("endpoints = %#v", endpoints)
	}
}

func TestRouterPartitionPruningAndMaxFanoutCap(t *testing.T) {
	u1 := &fakeMetricsUpstream{name: "aws-us", instant: vectorWithSeries("")}
	u2 := &fakeMetricsUpstream{name: "aws-eu", instant: vectorWithSeries("")}
	u3 := &fakeMetricsUpstream{name: "aws-ap", instant: vectorWithSeries("")}
	router := NewRouter(RouterConfig{MaxFanoutBackends: 2}, nil)
	backends := []struct {
		b Backend
		u *fakeMetricsUpstream
	}{
		{Backend{Name: "aws-us", Kind: KindAWS, Origin: "guardian-pusher", Region: "us-east-1", Account: "111", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways}, u1},
		{Backend{Name: "aws-eu", Kind: KindAWS, Origin: "guardian-pusher", Region: "eu-west-1", Account: "222", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways}, u2},
		{Backend{Name: "aws-ap", Kind: KindAWS, Origin: "guardian-pusher", Region: "ap-south-1", Account: "333", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways}, u3},
	}
	for _, item := range backends {
		if err := router.Register(item.b, Upstreams{Metrics: item.u}); err != nil {
			t.Fatalf("Register(%s): %v", item.b.Name, err)
		}
	}

	// 1. Scoped query by region="eu-west-1" should prune to ONLY aws-eu!
	_, warnings, err := router.MetricsInstant(context.Background(), "default", `CPUUtilization{source="aws", region="eu-west-1"}`, time.Now())
	if err != nil {
		t.Fatalf("MetricsInstant scoped: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings on scoped query: %#v", warnings)
	}
	if u1.callCount() != 0 || u2.callCount() != 1 || u3.callCount() != 0 {
		t.Fatalf("calls = (%d, %d, %d), want (0, 1, 0)", u1.callCount(), u2.callCount(), u3.callCount())
	}

	// 2. Unscoped query across 3 backends with MaxFanoutBackends=2 should cap at 2 and emit a router warning.
	_, warnings, err = router.MetricsInstant(context.Background(), "default", `CPUUtilization{source="aws"}`, time.Now())
	if err != nil {
		t.Fatalf("MetricsInstant unscoped: %v", err)
	}
	if len(warnings) != 1 || warnings[0].Backend != "router" {
		t.Fatalf("expected MaxFanoutBackends warning, got %#v", warnings)
	}
	page := router.EndpointsPage(context.Background(), EndpointPageFilter{Region: "eu-west-1", Limit: 10})
	if page.TotalMatch != 1 || len(page.Endpoints) != 1 || page.Endpoints[0].Name != "aws-eu" || page.Summary.Total != 3 {
		t.Fatalf("unexpected EndpointsPage: %+v", page)
	}
}
