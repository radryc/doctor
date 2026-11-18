package query

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/access"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
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

func newAuditTestPolicyGate(t *testing.T, enforce bool) *access.PolicyGate {
	t.Helper()
	const yamlPolicy = `
version: 1
defaults:
  partitions: deny
  logs: deny
  traces: deny
  monofs: deny
users:
  viewer@example.com:
    partitions: [aws-x]
    role: viewer
  admin@example.com:
    partitions: ["*"]
    role: admin
`
	file, err := policy.ParseFile([]byte(yamlPolicy))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	bundle, _, err := policy.Compile(file, nil, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("compile policy: %v", err)
	}
	data, err := policy.MarshalBundle(*bundle)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	gate := access.NewPolicyGate(access.PolicyOptions{
		File:    path,
		Enforce: enforce,
		Logger:  log.New(io.Discard, "", 0),
	})
	if err := gate.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh policy: %v", err)
	}
	return gate
}

func writeTestAuditJSONL(t *testing.T, path string, events []map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHandleAuditEventsFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv("DOCTOR_AUDIT_LOG", path)
	t.Setenv("DOCTOR_AUDIT_ENABLED", "true")

	writeTestAuditJSONL(t, path, []map[string]any{
		{
			"time":      "2026-11-18T01:00:00Z",
			"requestId": "r1",
			"subject":   "alice",
			"email":     "alice@example.com",
			"action":    "read",
			"partition": "aws-x",
			"decision":  "allow",
		},
		{
			"time":      "2026-11-18T02:00:00Z",
			"requestId": "r2",
			"subject":   "bob",
			"email":     "bob@example.com",
			"action":    "write",
			"partition": "k8s-z",
			"decision":  "deny",
		},
		{
			"time":      "2026-11-18T03:00:00Z",
			"requestId": "r3",
			"subject":   "alice",
			"email":     "alice@example.com",
			"action":    "read",
			"partition": "aws-x",
			"decision":  "allow",
		},
		{
			"time":      "2026-11-18T04:00:00Z",
			"requestId": "r4",
			"subject":   "alice",
			"email":     "alice@example.com",
			"action":    "read",
			"partition": "k8s-z",
			"decision":  "allow",
		},
	})

	svc := &Service{}
	handler := svc.Handler()

	// Filter by actor, action, partition, and limit=1 (newest first -> r3).
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events?actor=alice@example.com&action=read&partition=aws-x&limit=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp auditEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Count != 1 || len(resp.Events) != 1 || !resp.Truncated {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Events[0]["requestId"] != "r3" {
		t.Fatalf("expected newest matching event r3, got %v", resp.Events[0]["requestId"])
	}

	// Filter by decision=deny and time range.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/audit/events?decision=deny&from=2026-11-18T01:30:00Z&to=2026-11-18T02:30:00Z", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	var resp2 auditEventsResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp2.Count != 1 || resp2.Events[0]["requestId"] != "r2" {
		t.Fatalf("expected r2, got %+v", resp2)
	}
}

func TestHandleAuditEventsAuthorizationAndSuperAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-authz.jsonl")
	t.Setenv("DOCTOR_AUDIT_LOG", path)
	t.Setenv("DOCTOR_AUDIT_ENABLED", "true")

	writeTestAuditJSONL(t, path, []map[string]any{
		{
			"time":      "2026-11-18T01:00:00Z",
			"requestId": "seed-1",
			"subject":   "alice",
			"action":    "read",
			"partition": "aws-x",
		},
	})

	svc := &Service{
		policyGate: newAuditTestPolicyGate(t, true),
	}
	handler := svc.Handler()

	// 1. Anonymous caller -> 403.
	anonReq := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil)
	anonReq.Header.Set("X-Request-ID", "req-anon")
	anonRec := httptest.NewRecorder()
	handler.ServeHTTP(anonRec, anonReq)
	if anonRec.Code != http.StatusForbidden {
		t.Fatalf("anonymous status = %d, want 403", anonRec.Code)
	}

	// 2. Regular viewer without audit/admin -> 403.
	viewer := authz.Identity{Subject: "viewer", Email: "viewer@example.com", Groups: []string{"devs"}}
	viewerReq := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil).
		WithContext(authz.ContextWithIdentity(context.Background(), viewer))
	viewerReq.Header.Set("X-Request-ID", "req-viewer")
	viewerRec := httptest.NewRecorder()
	handler.ServeHTTP(viewerRec, viewerReq)
	if viewerRec.Code != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want 403", viewerRec.Code)
	}

	// 3. Caller with audit group -> 200.
	auditor := authz.Identity{Subject: "auditor-1", Email: "auditor@example.com", Groups: []string{"audit"}}
	auditReq := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil).
		WithContext(authz.ContextWithIdentity(context.Background(), auditor))
	auditReq.Header.Set("X-Request-ID", "req-auditor")
	auditRec := httptest.NewRecorder()
	handler.ServeHTTP(auditRec, auditReq)
	if auditRec.Code != http.StatusOK {
		t.Fatalf("auditor status = %d, want 200: %s", auditRec.Code, auditRec.Body.String())
	}

	// 4. Policy admin caller -> 200.
	adminID := authz.Identity{Subject: "admin", Email: "admin@example.com"}
	adminReq := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil).
		WithContext(authz.ContextWithIdentity(context.Background(), adminID))
	adminReq.Header.Set("X-Request-ID", "req-admin")
	adminRec := httptest.NewRecorder()
	handler.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200: %s", adminRec.Code, adminRec.Body.String())
	}

	// 5. Verify super-auditing: all GET /v1/audit/events requests were recorded.
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	lines := string(content)
	for _, reqID := range []string{"req-anon", "req-viewer", "req-auditor", "req-admin"} {
		if !strings.Contains(lines, `"requestId":"`+reqID+`"`) {
			t.Fatalf("missing super-audit entry for %s in:\n%s", reqID, lines)
		}
	}
	if !strings.Contains(lines, `"action":"audit_read"`) {
		t.Fatalf("missing action=audit_read in:\n%s", lines)
	}
	if !strings.Contains(lines, `"path":"/v1/audit/events"`) {
		t.Fatalf("missing path=/v1/audit/events in:\n%s", lines)
	}
}
