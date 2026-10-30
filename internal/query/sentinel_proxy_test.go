package query

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSentinelProxyForwardsAlertsRequest(t *testing.T) {
	var gotPath, gotQuery string
	sentinel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"alerts":[],"returned":0}`))
	}))
	defer sentinel.Close()

	svc := &Service{}
	svc.SetSentinelURL(sentinel.URL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/alerts?tenant=default&partition=payments", nil)
	svc.handleSentinelProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/alerts" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotQuery != "tenant=default&partition=payments" {
		t.Fatalf("query = %q", gotQuery)
	}
}

func TestSentinelProxyUnconfigured(t *testing.T) {
	svc := &Service{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/alerts", nil)
	svc.handleSentinelProxy(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
