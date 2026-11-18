package query

import (
	"bytes"
	"log"
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

func TestAuditLogsTelemetryReadQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-reads.log")
	t.Setenv("DOCTOR_AUDIT_LOG", path)
	t.Setenv("DOCTOR_AUDIT_ENABLED", "true")

	handler := withRequestID(withAuditLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/logs"},
		{http.MethodGet, "/v1/traces"},
		{http.MethodPost, "/v1/sql/query"},
		{http.MethodGet, "/loki/api/v1/query"},
		{http.MethodGet, "/loki/api/v1/query_range"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Request-ID", "read-"+tc.path)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	// Non-telemetry GET should not be audited.
	nonReadReq := httptest.NewRequest(http.MethodGet, "/v1/dashboards", nil)
	nonReadReq.Header.Set("X-Request-ID", "non-read-dashboards")
	handler.ServeHTTP(httptest.NewRecorder(), nonReadReq)

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	lines := string(content)
	for _, wantPath := range []string{"/v1/logs", "/v1/traces", "/v1/sql/query", "/loki/api/v1/query", "/loki/api/v1/query_range"} {
		if !strings.Contains(lines, `"path":"`+wantPath+`"`) {
			t.Fatalf("missing audit entry for %s in:\n%s", wantPath, lines)
		}
	}
	if !strings.Contains(lines, `"action":"read"`) {
		t.Fatalf("missing action=read in:\n%s", lines)
	}
	if strings.Contains(lines, "non-read-dashboards") {
		t.Fatalf("non-telemetry GET was unexpectedly audited:\n%s", lines)
	}
}

func TestAuditOpenFileFailureLogsFallbackWarning(t *testing.T) {
	// Point DOCTOR_AUDIT_LOG to a directory so os.OpenFile fails.
	badPath := t.TempDir()
	t.Setenv("DOCTOR_AUDIT_LOG", badPath)

	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(origOut)

	writeAudit(map[string]any{
		"requestId": "req-fallback",
		"path":      "/v1/logs",
		"action":    "read",
	})

	logged := buf.String()
	if !strings.Contains(logged, "audit_log_open_error") || !strings.Contains(logged, "req-fallback") {
		t.Fatalf("expected structured fallback warning with audit payload, got %q", logged)
	}
}
