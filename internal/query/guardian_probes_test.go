package query

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleGuardianProbesProxiesFiltersAndAuth(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	guardian := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"probes":[{"probeID":"p1","service":"api","status":"up","success":true}]}`)
	}))
	defer guardian.Close()

	svc := &Service{guardianURL: guardian.URL, guardianClient: guardian.Client()}
	handler := svc.guardianTokenMiddleware(http.HandlerFunc(svc.handleGuardianProbes))

	req := httptest.NewRequest(http.MethodGet, "/v1/guardian/probes?partition=prod&status=up", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/probes" {
		t.Fatalf("guardian path = %q, want /api/probes", gotPath)
	}
	if gotQuery != "partition=prod&status=up" {
		t.Fatalf("guardian query = %q", gotQuery)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("guardian auth = %q, want forwarded bearer token", gotAuth)
	}

	var body struct {
		Probes []struct {
			ProbeID string `json:"probeID"`
		} `json:"probes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Probes) != 1 || body.Probes[0].ProbeID != "p1" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestHandleGuardianProbesRejectsNonGET(t *testing.T) {
	svc := &Service{}
	rec := httptest.NewRecorder()
	svc.handleGuardianProbes(rec, httptest.NewRequest(http.MethodPost, "/v1/guardian/probes", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
