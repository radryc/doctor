package query

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequestIDIsPropagated(t *testing.T) {
	svc := &Service{}
	handler := svc.Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "req-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != "req-123" {
		t.Fatalf("X-Request-ID = %q", got)
	}
}

func TestRequestIDIsGenerated(t *testing.T) {
	svc := &Service{}
	handler := svc.Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got == "" {
		t.Fatal("missing generated X-Request-ID")
	}
}

func TestAuditLogsStateChangingRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	t.Setenv("DOCTOR_AUDIT_LOG", path)
	t.Setenv("DOCTOR_AUDIT_ENABLED", "true")

	svc := &Service{}
	handler := svc.Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/dashboards/import", strings.NewReader(`{}`))
	req.Header.Set("X-Request-ID", "audit-req")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	line := string(content)
	if !strings.Contains(line, `"requestId":"audit-req"`) {
		t.Fatalf("audit line missing request ID: %s", line)
	}
	if !strings.Contains(line, `"path":"/v1/dashboards/import"`) {
		t.Fatalf("audit line missing path: %s", line)
	}
}
