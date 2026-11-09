package proxy

import (
	"testing"
	"time"
)

func TestRouterSkipsDiscoveredBackendsForFederatedKind(t *testing.T) {
	registry := NewSliceRegistry([]Backend{
		{Name: "aws-123456789012", Kind: KindAWS, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
		{Name: "gcp-demo", Kind: KindGCP, Signals: []Signal{SignalMetrics}, Policy: PolicyFallback},
		{Name: "k8s-main", Kind: "kubernetes", Signals: []Signal{SignalMetrics}, Policy: PolicyAlways},
	})
	router := NewRouter(RouterConfig{MaxInflight: 4, BackendTimeout: time.Second}, registry)
	if err := router.Register(Backend{
		Name: "aws", Kind: KindAWS, Signals: []Signal{SignalMetrics},
		Policy: PolicyFallback, Federated: true,
	}, Upstreams{Metrics: &fakeMetricsUpstream{name: "aws"}}); err != nil {
		t.Fatalf("register aws: %v", err)
	}

	snapshot := router.snapshot()
	names := make(map[string]bool, len(snapshot))
	for _, backend := range snapshot {
		names[backend.Name] = true
	}
	if !names["aws"] {
		t.Fatal("explicit federated aws backend missing")
	}
	if names["aws-123456789012"] {
		t.Fatal("discovered AWS backend must be skipped when an AWS federated backend exists")
	}
	// GCP is not federated in this test, so its discovered backend remains.
	if !names["gcp-demo"] {
		t.Fatal("discovered GCP backend should remain when GCP is not federated")
	}
	if !names["k8s-main"] {
		t.Fatal("discovered kubernetes backend should remain")
	}
}
