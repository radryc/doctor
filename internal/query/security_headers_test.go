package query

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersOnAPIAndUI(t *testing.T) {
	svc := &Service{}
	handler := svc.Handler()

	for _, path := range []string{"/healthz", "/v1/ui/config"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Content-Security-Policy"); got == "" {
			t.Fatalf("%s: missing CSP", path)
		} else if !strings.Contains(got, "frame-src") || !strings.Contains(got, "http://*:9191") {
			t.Fatalf("%s: CSP does not allow the Ask Strata embed: %q", path, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s: X-Content-Type-Options = %q", path, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("%s: X-Frame-Options = %q", path, got)
		}
		if got := rec.Header().Get("Referrer-Policy"); got == "" {
			t.Fatalf("%s: missing Referrer-Policy", path)
		}
	}
}

func TestSecurityHeadersHSTSOnlyWithTLS(t *testing.T) {
	svc := &Service{}
	handler := svc.Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("unexpected HSTS without TLS: %q", got)
	}
}
