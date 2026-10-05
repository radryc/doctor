package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

type fakeProxyMetricsUpstream struct {
	name    string
	instant map[string]any
}

func (f *fakeProxyMetricsUpstream) Name() string { return f.name }

func (f *fakeProxyMetricsUpstream) PromInstant(context.Context, string, string, time.Time) (map[string]any, error) {
	return f.instant, nil
}

func (f *fakeProxyMetricsUpstream) PromRange(context.Context, string, string, time.Time, time.Time, time.Duration) (map[string]any, error) {
	return f.instant, nil
}

func (f *fakeProxyMetricsUpstream) PromSeries(context.Context, string, []string, time.Time, time.Time) ([]map[string]string, error) {
	return nil, nil
}

func (f *fakeProxyMetricsUpstream) PromLabels(context.Context, string, []string, time.Time, time.Time) ([]string, error) {
	return nil, nil
}

func (f *fakeProxyMetricsUpstream) PromLabelValues(context.Context, string, string, []string, time.Time, time.Time) ([]string, error) {
	return nil, nil
}

type fakeProxyLogsUpstream struct {
	name    string
	records []segment.LogRecord
}

func (f *fakeProxyLogsUpstream) Name() string { return f.name }

func (f *fakeProxyLogsUpstream) QueryLogs(context.Context, string, proxy.LogPushdown, time.Time, time.Time, int) ([]segment.LogRecord, error) {
	return f.records, nil
}

func newProxyTestRouter(t *testing.T, svc *Service) *proxy.Router {
	t.Helper()
	router := proxy.NewRouter(proxy.RouterConfig{MaxInflight: 8, BackendTimeout: 5 * time.Second}, proxy.NewCompositeRegistry())
	if err := router.Register(proxy.Backend{
		Name:    "doctor",
		Kind:    proxy.KindMonoFS,
		Signals: []proxy.Signal{proxy.SignalMetrics, proxy.SignalLogs, proxy.SignalTraces},
		Policy:  proxy.PolicyAlways,
	}, proxy.Upstreams{
		Metrics: svc.LocalMetricsUpstream(),
		Logs:    svc.LocalLogsUpstream(),
		Traces:  svc.LocalTracesUpstream(),
	}); err != nil {
		t.Fatalf("register local: %v", err)
	}
	return router
}

func promVector(metric map[string]any, value string) map[string]any {
	return map[string]any{
		"resultType": "vector",
		"result": []any{map[string]any{
			"metric": metric,
			"value":  []any{float64(time.Now().Unix()), value},
		}},
	}
}

func TestProxyRouterMergesRemoteShardsThroughHandler(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	local := &fakeLogEngineBackend{metrics: []segment.MetricPointRecord{{
		MetricName: "up",
		Service:    "api",
		HasValue:   true,
		Value:      1,
		Timestamp:  now,
	}}}
	svc := &Service{defaultTenant: "default", logEngine: local}
	router := newProxyTestRouter(t, svc)

	for _, name := range []string{"prom-a", "prom-b"} {
		upstream := &fakeProxyMetricsUpstream{
			name:    name,
			instant: promVector(map[string]any{"__name__": "up", "instance": "x"}, "1"),
		}
		if err := router.Register(proxy.Backend{
			Name:    name,
			Kind:    proxy.KindPrometheus,
			BaseURL: "http://" + name,
			Signals: []proxy.Signal{proxy.SignalMetrics},
			Policy:  proxy.PolicyAlways,
		}, proxy.Upstreams{Metrics: upstream}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	svc.SetProxyRouter(router)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?query=up", nil)
	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if len(resp.Data.Result) != 3 {
		t.Fatalf("result length = %d, want 3 (body=%s)", len(resp.Data.Result), rec.Body.String())
	}
	shards := map[string]string{}
	for _, item := range resp.Data.Result {
		shards[item.Metric["source"]+"/"+item.Metric["shard"]] = item.Metric["instance"]
	}
	if _, ok := shards["doctor/"]; !ok {
		t.Fatalf("missing native series: %#v", shards)
	}
	if _, ok := shards["prometheus/prom-a"]; !ok {
		t.Fatalf("missing prom-a shard: %#v", shards)
	}
	if _, ok := shards["prometheus/prom-b"]; !ok {
		t.Fatalf("missing prom-b shard: %#v", shards)
	}
}

func TestProxyRouterMergesLogsThroughLokiHandler(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	local := &fakeLogEngineBackend{logs: []segment.LogRecord{{
		Service:   "api",
		Timestamp: base,
		Body:      "local",
	}}}
	svc := &Service{defaultTenant: "default", logEngine: local}
	router := newProxyTestRouter(t, svc)
	if err := router.Register(proxy.Backend{
		Name:    "loki-a",
		Kind:    proxy.KindLoki,
		BaseURL: "http://loki-a",
		Signals: []proxy.Signal{proxy.SignalLogs},
		Policy:  proxy.PolicyAlways,
	}, proxy.Upstreams{Logs: &fakeProxyLogsUpstream{
		name:    "loki-a",
		records: []segment.LogRecord{{Service: "api", Timestamp: base.Add(time.Second), Body: "remote"}},
	}}); err != nil {
		t.Fatalf("register loki: %v", err)
	}
	svc.SetProxyRouter(router)

	req := httptest.NewRequest(http.MethodGet, `/loki/api/v1/query_range?query={service="api"}`, nil)
	rec := httptest.NewRecorder()
	svc.handleLokiQueryRange(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Result []struct {
				Values [][]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	total := 0
	for _, stream := range resp.Data.Result {
		total += len(stream.Values)
	}
	if total != 2 {
		t.Fatalf("stream values = %d, want 2 (body=%s)", total, rec.Body.String())
	}
}

func TestProxyEndpointsHandler(t *testing.T) {
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}}
	svc.SetProxyRouter(newProxyTestRouter(t, svc))

	req := httptest.NewRequest(http.MethodGet, "/v1/proxy/endpoints", nil)
	rec := httptest.NewRecorder()
	svc.handleProxyEndpoints(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Endpoints []proxy.EndpointInfo `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Endpoints) != 1 || resp.Endpoints[0].Name != "doctor" {
		t.Fatalf("endpoints = %#v", resp.Endpoints)
	}
}
