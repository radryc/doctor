package ruler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatusReportsLeaseAndScheduler(t *testing.T) {
	lease := NewLeaseManager(nil, "/doctor/v1/rules/.lease/leader.json", "ruler-1", 30*time.Second, 10*time.Second, nil)
	api := &API{Scheduler: &Scheduler{}, Lease: lease}

	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/rules/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Enabled bool   `json:"enabled"`
		RulerID string `json:"rulerId"`
		Leader  bool   `json:"leader"`
		Lease   struct {
			RulerID string `json:"rulerId"`
			TTL     string `json:"ttl"`
			Renew   string `json:"renew"`
		} `json:"lease"`
		Scheduler struct {
			Groups int `json:"groups"`
		} `json:"scheduler"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Enabled || payload.RulerID != "ruler-1" || payload.Lease.RulerID != "ruler-1" {
		t.Fatalf("unexpected payload: %s", rec.Body.String())
	}
	if payload.Lease.TTL != "30s" || payload.Lease.Renew != "10s" {
		t.Fatalf("unexpected lease timing: %s", rec.Body.String())
	}
}
