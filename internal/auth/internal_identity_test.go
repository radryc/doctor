package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

func TestSignAndVerifyInternalIdentity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	identity := authz.Identity{Subject: "alice", Groups: []string{"team-a"}}
	token, err := SignInternalIdentity("secret", identity, time.Minute, now)
	if err != nil {
		t.Fatalf("SignInternalIdentity: %v", err)
	}
	got, err := VerifyInternalIdentity("secret", token, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("VerifyInternalIdentity: %v", err)
	}
	if got.Subject != "alice" || len(got.Groups) != 1 || got.Groups[0] != "team-a" {
		t.Fatalf("identity = %#v", got)
	}
	if _, err := VerifyInternalIdentity("wrong", token, now); err == nil {
		t.Fatal("expected wrong secret to fail")
	}
	if _, err := VerifyInternalIdentity("secret", token, now.Add(2*time.Minute)); err == nil {
		t.Fatal("expected expired token to fail")
	}
}

func TestInternalIdentityMiddlewareReplacesIdentity(t *testing.T) {
	now := time.Now().UTC()
	token, err := SignInternalIdentity("secret", authz.Identity{Subject: "bob"}, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	var seen authz.Identity
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = authz.IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := InternalIdentityMiddleware("secret", nil)(next)
	req := httptest.NewRequest(http.MethodGet, "/v1/rules", nil)
	req.Header.Set(InternalIdentityHeader, token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || seen.Subject != "bob" {
		t.Fatalf("status = %d, identity = %#v", rec.Code, seen)
	}
	if got := req.Header.Get(InternalIdentityHeader); got != "" {
		t.Fatalf("header was not stripped: %q", got)
	}

	// Invalid header fails closed.
	req = httptest.NewRequest(http.MethodGet, "/v1/rules", nil)
	req.Header.Set(InternalIdentityHeader, "garbage")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
