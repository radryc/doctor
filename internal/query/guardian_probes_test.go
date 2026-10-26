package query

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestHandleGuardianProbesReportsPusherSummary(t *testing.T) {
	now := time.Now().UTC()
	fresh := now.Format(time.RFC3339Nano)
	stale := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
	payload := fmt.Sprintf(`{"probes":[
		{"probeID":"a1","pusher":"pusher-a","partition":"div-a-orders","checkedAt":"%s"},
		{"probeID":"a2","pusher":"pusher-a","partition":"div-a-orders","checkedAt":"%s"},
		{"probeID":"b1","pusher":"pusher-b","partition":"div-b-inventory","checkedAt":"%s"},
		{"probeID":"c1","pusher":"pusher-c","partition":"div-a-billing","checkedAt":"%s"}
	]}`, fresh, stale, stale, fresh)

	guardian := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	defer guardian.Close()

	svc := &Service{guardianURL: guardian.URL, guardianClient: guardian.Client()}
	handler := svc.guardianTokenMiddleware(http.HandlerFunc(svc.handleGuardianProbes))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/guardian/probes", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		PusherSummary struct {
			Total     int `json:"total"`
			Available int `json:"available"`
			Down      int `json:"down"`
		} `json:"pusherSummary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.PusherSummary.Total != 3 || body.PusherSummary.Available != 2 || body.PusherSummary.Down != 1 {
		t.Fatalf("summary = %+v, want total=3 available=2 down=1", body.PusherSummary)
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
