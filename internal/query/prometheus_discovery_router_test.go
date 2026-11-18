package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/proxy"
)

type discoveryTestUpstream struct {
	name    string
	series  []map[string]string
	labels  []string
	values  map[string][]string
	instant map[string]any
}

func (u *discoveryTestUpstream) Name() string {
	if u.name == "" {
		return "edge-prom"
	}
	return u.name
}

func (u *discoveryTestUpstream) PromInstant(context.Context, string, string, time.Time) (map[string]any, error) {
	if u.instant != nil {
		return u.instant, nil
	}
	return map[string]any{"resultType": "vector", "result": []any{}}, nil
}

func (u *discoveryTestUpstream) PromRange(context.Context, string, string, time.Time, time.Time, time.Duration) (map[string]any, error) {
	return map[string]any{"resultType": "matrix", "result": []any{}}, nil
}

func (u *discoveryTestUpstream) PromSeries(context.Context, string, []string, time.Time, time.Time) ([]map[string]string, error) {
	return u.series, nil
}

func (u *discoveryTestUpstream) PromLabels(context.Context, string, []string, time.Time, time.Time) ([]string, error) {
	return u.labels, nil
}

func (u *discoveryTestUpstream) PromLabelValues(_ context.Context, _, label string, _ []string, _, _ time.Time) ([]string, error) {
	return u.values[label], nil
}

func newDiscoveryService(t *testing.T, upstream *discoveryTestUpstream) *Service {
	t.Helper()
	router := proxy.NewRouter(proxy.RouterConfig{MaxInflight: 4, BackendTimeout: time.Second}, proxy.NewCompositeRegistry())
	if err := router.Register(proxy.Backend{
		Name:    "edge-prom",
		Kind:    proxy.KindPrometheus,
		BaseURL: "http://edge-prom",
		Signals: []proxy.Signal{proxy.SignalMetrics},
	}, proxy.Upstreams{Metrics: upstream}); err != nil {
		t.Fatalf("register remote: %v", err)
	}
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}}
	svc.SetProxyRouter(router)
	return svc
}

func TestPrometheusLabelsUseRouterDiscovery(t *testing.T) {
	svc := newDiscoveryService(t, &discoveryTestUpstream{labels: []string{"__name__", "job"}})
	rec := httptest.NewRecorder()
	svc.handlePrometheusLabels(rec, httptest.NewRequest(http.MethodGet, "/api/v1/labels", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"__name__": true, "job": true, "source": true}
	for _, value := range resp.Data {
		delete(want, value)
	}
	if len(want) != 0 {
		t.Fatalf("missing labels %v in %v", want, resp.Data)
	}
}

func TestPrometheusSeriesUseRouterDiscovery(t *testing.T) {
	svc := newDiscoveryService(t, &discoveryTestUpstream{
		series: []map[string]string{{"__name__": "up", "job": "api"}},
	})
	rec := httptest.NewRecorder()
	svc.handlePrometheusSeries(rec, httptest.NewRequest(http.MethodGet, "/api/v1/series?match[]=up", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0]["source"] != proxy.KindPrometheus {
		t.Fatalf("series = %#v", resp.Data)
	}
}

func TestPrometheusLabelValuesUseRouterDiscovery(t *testing.T) {
	svc := newDiscoveryService(t, &discoveryTestUpstream{
		values: map[string][]string{"__name__": {"up", "http_requests_total"}},
	})
	rec := httptest.NewRecorder()
	svc.handlePrometheusLabelValues(rec, httptest.NewRequest(http.MethodGet, "/api/v1/label/__name__/values?limit=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("values = %v", resp.Data)
	}
	if rec.Header().Get("X-Doctor-Metric-Truncated") != "true" {
		t.Fatalf("missing truncation header: %v", rec.Header())
	}
}

func TestPrometheusMetadataUsesRouterDiscovery(t *testing.T) {
	svc := newDiscoveryService(t, &discoveryTestUpstream{
		values: map[string][]string{"__name__": {"up"}},
	})
	rec := httptest.NewRecorder()
	svc.handlePrometheusMetadata(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metadata", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp.Data["up"]; !ok {
		t.Fatalf("metadata = %#v", resp.Data)
	}
}

func TestPrometheusLabelValuesSourceFilter(t *testing.T) {
	svc := newDiscoveryService(t, &discoveryTestUpstream{
		values: map[string][]string{"__name__": {"up"}},
	})
	path := "/api/v1/label/__name__/values?source=aws"
	rec := httptest.NewRecorder()
	svc.handlePrometheusLabelValues(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Doctor-Metric-Total") != "0" {
		t.Fatalf("metric total = %q", rec.Header().Get("X-Doctor-Metric-Total"))
	}
}

func TestPrometheusQueryUsesGlobalFederationAggregation(t *testing.T) {
	first := &discoveryTestUpstream{
		name: "prom-a",
		instant: map[string]any{
			"resultType": "vector",
			"result": []any{map[string]any{
				"metric": map[string]any{"service": "api", "source": "prometheus"},
				"value":  []any{float64(time.Now().Unix()), "2"},
			}},
		},
	}
	second := &discoveryTestUpstream{
		name: "prom-b",
		instant: map[string]any{
			"resultType": "vector",
			"result": []any{map[string]any{
				"metric": map[string]any{"service": "api", "source": "prometheus"},
				"value":  []any{float64(time.Now().Unix()), "3"},
			}},
		},
	}
	router := proxy.NewRouter(proxy.RouterConfig{MaxInflight: 4, BackendTimeout: time.Second}, proxy.NewCompositeRegistry())
	for _, upstream := range []*discoveryTestUpstream{first, second} {
		if err := router.Register(proxy.Backend{
			Name:    upstream.Name(),
			Kind:    proxy.KindPrometheus,
			BaseURL: "http://" + upstream.Name(),
			Signals: []proxy.Signal{proxy.SignalMetrics},
			Policy:  proxy.PolicyAlways,
		}, proxy.Upstreams{Metrics: upstream}); err != nil {
			t.Fatalf("register %s: %v", upstream.Name(), err)
		}
	}
	svc := &Service{defaultTenant: "default"}
	svc.SetProxyRouter(router)

	rec := httptest.NewRecorder()
	svc.handlePrometheusQuery(rec, httptest.NewRequest(http.MethodGet, "/api/v1/query?query=sum(up)%20by%20(service)", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Doctor-Federation-Aggregation"); got != "global" {
		t.Fatalf("aggregation header = %q", got)
	}
	var resp struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data.Result) != 1 || resp.Data.Result[0].Value[1] != "5" {
		t.Fatalf("result = %#v", resp.Data.Result)
	}
}
