package query

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"github.com/rydzu/ainfra/doctor/internal/access"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

const catalogPolicyYAML = `
version: 1
defaults:
  partitions: deny
  logs: deny
  traces: deny
users:
  usera@example.com:
    partitions: [div-a-orders, div-a-billing]
    logs: [div-a-orders, div-a-billing]
    traces: [div-a-orders, div-a-billing]
  userb@example.com:
    partitions: [div-b-inventory]
    logs: [div-b-inventory]
    traces: [div-b-inventory]
  admin@example.com:
    partitions: ["*"]
    role: admin
`

func newCatalogGate(t *testing.T) *access.PolicyGate {
	t.Helper()
	file, err := policy.ParseFile([]byte(catalogPolicyYAML))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	bundle, _, err := policy.Compile(file, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}
	data, err := policy.MarshalBundle(*bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	gate := access.NewPolicyGate(access.PolicyOptions{
		File:    path,
		Enforce: true,
		Logger:  log.New(io.Discard, "", 0),
	})
	if gate == nil {
		t.Fatal("expected a policy gate")
	}
	if err := gate.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh policy: %v", err)
	}
	return gate
}

func catalogIdentity(email string) authz.Identity {
	return authz.Identity{Subject: email, Email: email}
}

func TestServiceDiscoveryFiltersPartitions(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute)
	backend := &fakeLogEngineBackend{
		traces: []segment.TraceRecord{
			{Service: "orders.app", TraceID: "trace-a", StartTime: base, EndTime: base, Partition: "div-a-orders", Intent: "app", Asset: "service"},
			{Service: "inventory.app", TraceID: "trace-b", StartTime: base, EndTime: base, Partition: "div-b-inventory", Intent: "app", Asset: "service"},
		},
		logs: []segment.LogRecord{
			{Service: "orders.app", Timestamp: base, Partition: "div-a-orders", Intent: "app", Asset: "service"},
			{Service: "inventory.app", Timestamp: base, Partition: "div-b-inventory", Intent: "app", Asset: "service"},
		},
		metrics: []segment.MetricPointRecord{
			{Service: "orders.app", MetricName: "http_requests_total", Timestamp: base, Partition: "div-a-orders", Attributes: map[string]string{"guardian_asset_type": "Compute"}},
			{Service: "inventory.app", MetricName: "http_requests_total", Timestamp: base, Partition: "div-b-inventory", Attributes: map[string]string{"guardian_asset_type": "Compute"}},
		},
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}
	svc.SetPolicyGate(newCatalogGate(t))

	discover := func(email string) map[string]bool {
		req := httptest.NewRequest(http.MethodGet, "/v1/services/discover?tenant=default&window=24h", nil)
		req = req.WithContext(authz.ContextWithIdentity(req.Context(), catalogIdentity(email)))
		rec := httptest.NewRecorder()
		svc.handleServiceDiscovery(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Services []struct {
				Name string `json:"name"`
			} `json:"services"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]bool{}
		for _, service := range resp.Services {
			out[service.Name] = true
		}
		return out
	}

	usera := discover("usera@example.com")
	if !usera["orders.app"] {
		t.Fatalf("usera missing div-a service: %#v", usera)
	}
	if usera["inventory.app"] {
		t.Fatalf("usera saw div-b service: %#v", usera)
	}

	admin := discover("admin@example.com")
	if !admin["orders.app"] || !admin["inventory.app"] {
		t.Fatalf("admin services = %#v, want both partitions", admin)
	}
}

func TestGuardianProbesFiltersPartitions(t *testing.T) {
	guardian := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"probes":[{"probeID":"a","partition":"div-a-orders","status":"up"},{"probeID":"b","partition":"div-b-inventory","status":"up"}]}`)
	}))
	defer guardian.Close()

	svc := &Service{guardianURL: guardian.URL, guardianClient: guardian.Client()}
	svc.SetPolicyGate(newCatalogGate(t))
	handler := svc.guardianTokenMiddleware(http.HandlerFunc(svc.handleGuardianProbes))

	probeIDs := func(email string) map[string]bool {
		req := httptest.NewRequest(http.MethodGet, "/v1/guardian/probes", nil)
		req = req.WithContext(authz.ContextWithIdentity(req.Context(), catalogIdentity(email)))
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Probes []struct {
				ProbeID string `json:"probeID"`
			} `json:"probes"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]bool{}
		for _, probe := range resp.Probes {
			out[probe.ProbeID] = true
		}
		return out
	}

	usera := probeIDs("usera@example.com")
	if !usera["a"] || usera["b"] {
		t.Fatalf("usera probes = %#v, want only div-a-orders", usera)
	}
	admin := probeIDs("admin@example.com")
	if !admin["a"] || !admin["b"] {
		t.Fatalf("admin probes = %#v, want both partitions", admin)
	}
}
