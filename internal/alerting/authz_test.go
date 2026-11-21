package alerting

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIdentityFromRequestRejectsUnsignedHeadersWhenAllowAllFalse(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/alerts", nil)
	req.Header.Set("X-Doctor-User", "attacker")
	req.Header.Set("X-Doctor-Admin", "true")
	req.Header.Set("X-Doctor-Groups", "sentinel-admins")
	req.Header.Set("X-Doctor-Partition-Access", "*")

	id := IdentityFromRequest(req, false)
	if id.Admin || id.User != "" || len(id.Groups) != 0 || len(id.Partitions) != 0 {
		t.Fatalf("unsigned headers were trusted: %+v", id)
	}
	if id.Can(CapAlertsRead, "default", "payments") {
		t.Fatal("anonymous identity should not have CapAlertsRead")
	}

	// Even with a secret configured, missing or bad signature must be rejected.
	id = IdentityFromRequestWithAuth(req, false, "sentinel-secret", "svc-token")
	if id.Admin || id.User != "" {
		t.Fatalf("unsigned headers were trusted with secret configured: %+v", id)
	}

	req.Header.Set("X-Doctor-Identity-Sig", "deadbeef")
	id = IdentityFromRequestWithAuth(req, false, "sentinel-secret", "svc-token")
	if id.Admin || id.User != "" {
		t.Fatalf("tampered signature was trusted: %+v", id)
	}
}

func TestIdentityFromRequestAcceptsSignedHeaders(t *testing.T) {
	secret := "sentinel-secret"
	req := httptest.NewRequest(http.MethodGet, "/v1/alerts", nil)
	req.Header.Set("X-Doctor-User", "alice@example.com")
	req.Header.Set("X-Doctor-Admin", "true")
	req.Header.Set("X-Doctor-Groups", "sentinel-admins,oncall")
	req.Header.Set("X-Doctor-Identity-Sig", SignIdentity(secret, "alice@example.com", "true", "sentinel-admins,oncall"))

	id := IdentityFromRequestWithAuth(req, false, secret, "")
	if !id.Admin || id.User != "alice@example.com" || len(id.Groups) != 2 {
		t.Fatalf("signed identity = %+v", id)
	}
}

func TestIdentityFromRequestAcceptsServiceToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/alerts", nil)
	req.Header.Set("Authorization", "Bearer svc-secret-token")

	id := IdentityFromRequestWithAuth(req, false, "", "svc-secret-token")
	if !id.Admin || id.User != "service" {
		t.Fatalf("service token identity = %+v", id)
	}

	// Wrong service token must be rejected.
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("X-Doctor-Admin", "true")
	id = IdentityFromRequestWithAuth(req, false, "", "svc-secret-token")
	if id.Admin || id.User != "" {
		t.Fatalf("wrong service token was trusted: %+v", id)
	}
}

func TestAPIRejectsUnsignedAdminWhenAllowAllFalse(t *testing.T) {
	api, _ := testAPI(t)
	api.AllowAll = false
	api.IdentityHMACSecret = "sentinel-secret"
	api.ServiceToken = "svc-secret"

	req := httptest.NewRequest(http.MethodGet, "/v1/alerts?tenant=default&partition=payments", nil)
	req.Header.Set("X-Doctor-User", "attacker")
	req.Header.Set("X-Doctor-Admin", "true")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unsigned admin status = %d, want 404", rec.Code)
	}

	req.Header.Set("X-Doctor-Identity-Sig", SignIdentity("sentinel-secret", "attacker", "true", ""))
	rec = httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signed admin status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
}

func TestIdentitySignatureBindsPartitionAccess(t *testing.T) {
	secret := "sentinel-secret"
	req := httptest.NewRequest(http.MethodGet, "/v1/alerts", nil)
	req.Header.Set("X-Doctor-User", "alice@example.com")
	req.Header.Set("X-Doctor-Groups", "oncall")
	req.Header.Set("X-Doctor-Partition-Access", "payments")
	req.Header.Set("X-Doctor-Identity-Sig", SignIdentityWithPartitions(secret, "alice@example.com", "", "oncall", "payments"))

	id := IdentityFromRequestWithAuth(req, false, secret, "")
	if id.User != "alice@example.com" || !id.Partitions["payments"] {
		t.Fatalf("signed identity = %+v", id)
	}
	if id.Can(CapAlertsRead, "default", "billing") {
		t.Fatal("signed scope was widened")
	}

	// Replaying the signature with a wider scope must fail.
	req.Header.Set("X-Doctor-Partition-Access", "*")
	id = IdentityFromRequestWithAuth(req, false, secret, "")
	if id.User != "" || id.Admin {
		t.Fatalf("widened partition scope was trusted: %+v", id)
	}

	// Legacy signatures do not cover partition access and must be rejected
	// when a partition header is present.
	req.Header.Set("X-Doctor-Partition-Access", "payments")
	req.Header.Set("X-Doctor-Identity-Sig", SignIdentity(secret, "alice@example.com", "", "oncall"))
	id = IdentityFromRequestWithAuth(req, false, secret, "")
	if id.User != "" {
		t.Fatalf("legacy signature accepted alongside partition scope: %+v", id)
	}
}

func TestServiceTokenCannotForgeUnsignedIdentity(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/alerts", nil)
	req.Header.Set("Authorization", "Bearer svc-secret-token")
	req.Header.Set("X-Doctor-User", "attacker")
	req.Header.Set("X-Doctor-Admin", "true")
	req.Header.Set("X-Doctor-Partition-Access", "*")

	id := IdentityFromRequestWithAuth(req, false, "sentinel-secret", "svc-secret-token")
	if id.Admin || id.User != "" || len(id.Partitions) != 0 {
		t.Fatalf("unsigned identity was trusted with service token: %+v", id)
	}

	// A signed identity alongside the service token is accepted.
	req.Header.Set("X-Doctor-Identity-Sig", SignIdentityWithPartitions("sentinel-secret", "attacker", "true", "", "*"))
	id = IdentityFromRequestWithAuth(req, false, "sentinel-secret", "svc-secret-token")
	if !id.Admin || id.User != "attacker" {
		t.Fatalf("signed service identity = %+v", id)
	}
}
