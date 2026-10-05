package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
)

func staticVerifier(t *testing.T) *authz.StaticTokenVerifier {
	t.Helper()
	verifier := authz.NewStaticTokenVerifier()
	verifier.Add("secret", authz.Identity{Subject: "alice", Email: "alice@example.com"})
	return verifier
}

func TestMiddlewareAttachesIdentity(t *testing.T) {
	var seen authz.Identity
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = authz.IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := NewMiddleware(staticVerifier(t), false, nil, nil).Wrap(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/logs/search", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || seen.Subject != "alice" {
		t.Fatalf("status = %d, identity = %#v", rec.Code, seen)
	}
}

func TestMiddlewareRequiresTokenOutsideExemptPaths(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := NewMiddleware(staticVerifier(t), true, nil, nil).Wrap(next)

	// Logs are gated.
	req := httptest.NewRequest(http.MethodGet, "/loki/api/v1/query", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("logs status = %d, want 401", rec.Code)
	}

	// Prometheus metrics stay public.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", rec.Code)
	}

	// Health stays public.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}
}

func TestMiddlewareObserveModeNeverRejects(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := NewMiddleware(staticVerifier(t), false, nil, nil).Wrap(next)

	req := httptest.NewRequest(http.MethodGet, "/loki/api/v1/query", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 in observe mode", rec.Code)
	}
}

func TestNewVerifierBreakGlass(t *testing.T) {
	verifier, err := NewVerifier(Config{BreakGlassToken: "admin-token"})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if verifier == nil {
		t.Fatal("verifier is nil")
	}
	identity, err := verifier.Verify(nil, "admin-token")
	if err != nil || identity.ClientID == "" {
		t.Fatalf("identity = %#v, err = %v", identity, err)
	}
}

func TestNewVerifierDisabled(t *testing.T) {
	verifier, err := NewVerifier(Config{})
	if err != nil || verifier != nil {
		t.Fatalf("verifier = %#v, err = %v", verifier, err)
	}
}
