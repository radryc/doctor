package query

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMutationOriginCheck(t *testing.T) {
	svc := &Service{}
	handler := svc.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/dashboards/import", nil)
	req.Host = "doctor.example.com"
	req.Header.Set("Origin", "https://evil.example.net")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/dashboards/import", nil)
	req.Host = "doctor.example.com"
	req.Header.Set("Origin", "https://doctor.example.com")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusBadRequest {
		t.Fatalf("same-origin status = %d, want handler response", rec.Code)
	}
}

func TestCookieMutationRequiresOrigin(t *testing.T) {
	svc := &Service{}
	handler := svc.Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/dashboards/import", nil)
	req.AddCookie(&http.Cookie{Name: "doctor_session", Value: "x"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
