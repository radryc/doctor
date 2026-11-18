package query

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/radryc/doctor/internal/alerting"
	"github.com/radryc/monofs/pkg/authz"
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

func TestSentinelProxyRoutesAlertmanagerAPI(t *testing.T) {
	var gotPath string
	sentinel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer sentinel.Close()

	svc := &Service{}
	svc.SetSentinelURL(sentinel.URL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v2/alerts", nil)
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/api/v2/alerts" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestSentinelProxySignsForwardedIdentity(t *testing.T) {
	var got http.Header
	sentinel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"alerts":[]}`))
	}))
	defer sentinel.Close()

	svc := &Service{}
	svc.SetSentinelURL(sentinel.URL)
	svc.SetSentinelToken("sentinel-service-token")
	svc.SetSentinelIdentitySecret("sentinel-identity-secret")

	req := httptest.NewRequest(http.MethodGet, "/v1/alerts?tenant=default&partition=payments", nil)
	identity := authz.Identity{Subject: "alice", Email: "alice@example.com", Groups: []string{"oncall"}}
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	svc.handleSentinelProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotAuth := got.Get("Authorization"); gotAuth != "Bearer sentinel-service-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if got.Get("X-Doctor-Identity-Sig") == "" {
		t.Fatal("identity signature was not forwarded")
	}
	verify := httptest.NewRequest(http.MethodGet, "/v1/alerts", nil)
	for _, key := range []string{"X-Doctor-User", "X-Doctor-Admin", "X-Doctor-Groups", "X-Doctor-Partition-Access", "X-Doctor-Identity-Sig"} {
		if value := got.Get(key); value != "" {
			verify.Header.Set(key, value)
		}
	}
	parsed := alerting.IdentityFromRequestWithAuth(verify, false, "sentinel-identity-secret", "sentinel-service-token")
	if parsed.User != "alice@example.com" || len(parsed.Groups) != 1 || parsed.Groups[0] != "oncall" {
		t.Fatalf("signed identity rejected: %+v", parsed)
	}
}

func TestSentinelProxyStripsUnsignedIdentityWithoutSecret(t *testing.T) {
	var got http.Header
	sentinel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"alerts":[]}`))
	}))
	defer sentinel.Close()

	svc := &Service{}
	svc.SetSentinelURL(sentinel.URL)
	svc.SetSentinelToken("sentinel-service-token")

	req := httptest.NewRequest(http.MethodGet, "/v1/alerts?tenant=default&partition=payments", nil)
	identity := authz.Identity{Subject: "alice", Email: "alice@example.com", Groups: []string{"oncall"}}
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	svc.handleSentinelProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	for _, key := range []string{"X-Doctor-User", "X-Doctor-Admin", "X-Doctor-Groups", "X-Doctor-Partition-Access", "X-Doctor-Identity-Sig"} {
		if value := got.Get(key); value != "" {
			t.Fatalf("%s leaked to sentinel without a signing secret: %q", key, value)
		}
	}
}
