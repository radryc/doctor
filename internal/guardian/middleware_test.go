package guardian_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/guardian"
)

func TestAuthMiddlewareAcceptsValidToken(t *testing.T) {
	am := guardian.NewAuthMiddleware("my-secret")
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := am.Wrap(inner)

	req := httptest.NewRequest(http.MethodPost, "/events", nil)
	req.Header.Set("Authorization", "Bearer my-secret")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.Code)
	}
}

func TestAuthMiddlewareRejectsInvalidToken(t *testing.T) {
	am := guardian.NewAuthMiddleware("my-secret")
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := am.Wrap(inner)

	req := httptest.NewRequest(http.MethodPost, "/events", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Code)
	}
}

func TestAuthMiddlewareSkipsWhenEmpty(t *testing.T) {
	am := guardian.NewAuthMiddleware("")
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := am.Wrap(inner)

	req := httptest.NewRequest(http.MethodPost, "/events", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 when auth disabled, got %d", resp.Code)
	}
}

func TestRateLimiterRejectsExcess(t *testing.T) {
	rl := guardian.NewRateLimiter(2) // 2 per second
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := rl.Wrap(inner)

	// First 2 should pass.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/events", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, resp.Code)
		}
	}

	// Third should be rate limited.
	req := httptest.NewRequest(http.MethodPost, "/events", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.Code)
	}
}

func TestRateLimiterDifferentIPs(t *testing.T) {
	rl := guardian.NewRateLimiter(1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := rl.Wrap(inner)

	// First IP uses its token.
	req := httptest.NewRequest(http.MethodPost, "/events", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("first IP: expected 200, got %d", resp.Code)
	}

	// Second IP should still pass.
	req = httptest.NewRequest(http.MethodPost, "/events", nil)
	req.RemoteAddr = "10.0.0.2:12345"
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("second IP: expected 200, got %d", resp.Code)
	}
}

func TestDeduplicatorDetectsDuplicates(t *testing.T) {
	d := guardian.NewDeduplicator(time.Hour)

	if d.IsDuplicate("event-1") {
		t.Fatal("first call should not be duplicate")
	}
	if !d.IsDuplicate("event-1") {
		t.Fatal("second call should be duplicate")
	}
	if d.IsDuplicate("event-2") {
		t.Fatal("different key should not be duplicate")
	}
}
