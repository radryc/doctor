package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTenantGuardRejectsDisallowedTenant(t *testing.T) {
	guard := NewTenantGuard("X-Doctor-Tenant", "default", []string{"default", "edge"}, "")
	handler := guard.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/logs/search?tenant=other", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/logs/search?tenant=edge", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestTenantGuardSingleTenant(t *testing.T) {
	guard := NewTenantGuard("X-Doctor-Tenant", "default", nil, "default")
	handler := guard.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/logs/search", nil)
	req.Header.Set("X-Doctor-Tenant", "edge")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/logs/search", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("default tenant status = %d, want 200", rec.Code)
	}
}
