package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuardianDiscoveryParsesPushersAndPartitions(t *testing.T) {
	var gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/api/pushers":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pushers": []map[string]any{
					{
						"name":           "aws-prod",
						"observedApiUrl": "http://pusher-aws:19090",
						"token":          "secret",
						"region":         "eu-west-1",
						"labels":         map[string]string{"kind": "aws"},
					},
					{
						"name":           "prom-edge",
						"observedApiUrl": "http://prom-edge:9090",
						"labels": map[string]string{
							"kind":    "prometheus",
							"signals": "metrics",
						},
					},
				},
			})
		case "/api/m2m/partitions":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"name": "loki-shard-1",
					"labels": map[string]string{
						"kind":     "loki",
						"endpoint": "http://loki-1:3100",
						"signals":  "logs",
					},
				},
				{"name": "plain", "labels": map[string]string{}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	discovery := NewGuardianDiscovery(GuardianDiscoveryConfig{
		BaseURL: server.URL,
		Token:   "guardian-token",
	})
	if err := discovery.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if gotToken != "Bearer guardian-token" {
		t.Fatalf("authorization = %q, want bearer token", gotToken)
	}

	byName := map[string]Backend{}
	for _, backend := range discovery.Snapshot() {
		byName[backend.Name] = backend
	}
	if len(byName) != 3 {
		t.Fatalf("backends = %#v, want 3", byName)
	}

	aws := byName["aws-prod"]
	if aws.Kind != KindAWS || aws.Policy != PolicyFallback {
		t.Fatalf("aws backend = %#v, want kind aws with fallback policy", aws)
	}
	if aws.Token != "secret" {
		t.Fatalf("aws token = %q, want secret", aws.Token)
	}
	if aws.OwnersPath != "/doctor/v1/endpoints/aws-prod" {
		t.Fatalf("aws ownersPath = %q", aws.OwnersPath)
	}

	prom := byName["prom-edge"]
	if prom.Kind != KindPrometheus || prom.Policy != PolicyAlways {
		t.Fatalf("prom backend = %#v, want prometheus/always", prom)
	}
	if len(prom.Signals) != 1 || prom.Signals[0] != SignalMetrics {
		t.Fatalf("prom signals = %#v", prom.Signals)
	}

	loki := byName["loki-shard-1"]
	if loki.Kind != KindLoki || loki.Origin != "guardian-partition" {
		t.Fatalf("loki backend = %#v", loki)
	}
}

func TestGuardianDiscoveryKeepsSnapshotOnFailure(t *testing.T) {
	healthy := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pushers" && !healthy {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/pushers" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pushers": []map[string]any{{"name": "prom-a", "observedApiUrl": "http://a", "labels": map[string]string{"kind": "prometheus"}}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer server.Close()

	discovery := NewGuardianDiscovery(GuardianDiscoveryConfig{BaseURL: server.URL})
	if err := discovery.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	refreshed := discovery.LastRefresh()

	healthy = false
	if err := discovery.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh failure")
	}
	if len(discovery.Snapshot()) != 1 {
		t.Fatalf("snapshot lost on failure: %#v", discovery.Snapshot())
	}
	if !discovery.LastRefresh().Equal(refreshed) {
		t.Fatalf("LastRefresh moved on failure")
	}
	if discovery.LastError() == nil {
		t.Fatal("LastError is nil after failure")
	}
}

func TestSignalsFromLabelsDefaults(t *testing.T) {
	if got := signalsFromLabels(nil, KindPrometheus); len(got) != 1 || got[0] != SignalMetrics {
		t.Fatalf("prometheus defaults = %#v", got)
	}
	if got := signalsFromLabels(map[string]string{"signals": "logs,traces"}, KindPrometheus); len(got) != 2 {
		t.Fatalf("explicit signals = %#v", got)
	}
	if got := signalsFromLabels(nil, "unknown"); got != nil {
		t.Fatalf("unknown kind signals = %#v, want nil", got)
	}
}
