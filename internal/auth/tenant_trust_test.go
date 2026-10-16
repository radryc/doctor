package auth

import (
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
)

func TestTenantTrustAttachesMachineIdentity(t *testing.T) {
	var captured authz.Identity
	var found bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured, found = authz.IdentityFromContext(r.Context())
	})

	handler := NewTenantTrust("X-Doctor-Tenant", log.Default()).Wrap(next)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	req.Header.Set("X-Doctor-Tenant", "default")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !found || captured.IsAnonymous() {
		t.Fatalf("identity = %#v, found = %v", captured, found)
	}
	if captured.ClientID != "tenant:default" {
		t.Fatalf("clientID = %q", captured.ClientID)
	}
	if !captured.HasGroup("doctor:tenant") {
		t.Fatalf("groups = %#v", captured.Groups)
	}
}

func TestTenantTrustFallsBackToQueryParam(t *testing.T) {
	var captured authz.Identity
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured, _ = authz.IdentityFromContext(r.Context())
	})

	handler := NewTenantTrust("X-Doctor-Tenant", nil).Wrap(next)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query?tenant=edge", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if captured.ClientID != "tenant:edge" {
		t.Fatalf("clientID = %q", captured.ClientID)
	}
}

func TestTenantTrustKeepsVerifiedIdentity(t *testing.T) {
	verified := authz.Identity{Subject: "user-1", Email: "user@example.com"}
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		identity, _ := authz.IdentityFromContext(r.Context())
		if identity.Subject != verified.Subject {
			t.Fatalf("verified identity was overwritten: %#v", identity)
		}
	})

	handler := NewTenantTrust("X-Doctor-Tenant", nil).Wrap(next)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	req.Header.Set("X-Doctor-Tenant", "default")
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), verified))
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestTenantTrustLeavesAnonymousRequestsWithoutHeader(t *testing.T) {
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		identity, _ := authz.IdentityFromContext(r.Context())
		if !identity.IsAnonymous() {
			t.Fatalf("identity = %#v, want anonymous", identity)
		}
	})

	handler := NewTenantTrust("X-Doctor-Tenant", nil).Wrap(next)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
}
