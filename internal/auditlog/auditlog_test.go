package auditlog

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMutationMiddlewareAuditsScopedMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	t.Setenv("DOCTOR_AUDIT_LOG", path)

	handler := MutationMiddleware("doctor-rules", "/v1/rules")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPut, "/v1/rules/payments/group-a", nil)
	req.Header.Set(RequestIDHeader, "req-mutation")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	// Read-only requests and unrelated mutations are not audited.
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/rules/payments/group-a", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/logs", nil))

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	lines := strings.TrimSpace(string(content))
	if strings.Count(lines, "\n") != 0 {
		t.Fatalf("expected one audit line, got:\n%s", lines)
	}
	for _, want := range []string{`"requestId":"req-mutation"`, `"component":"doctor-rules"`, `"method":"PUT"`, `"status":204`, `"action":"mutation"`} {
		if !strings.Contains(lines, want) {
			t.Fatalf("audit line missing %s: %s", want, lines)
		}
	}
}

func TestWriteFailureIsReturned(t *testing.T) {
	t.Setenv("DOCTOR_AUDIT_LOG", t.TempDir())

	if err := Write(map[string]any{"requestId": "req-fail"}); err == nil {
		t.Fatal("expected write to a directory path to fail")
	}
	IncrementWriteError("auditlog-test")
}
