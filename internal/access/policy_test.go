package access

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

const testPolicyYAML = `
version: 1
defaults:
  partitions: deny
  logs: deny
  traces: deny
  monofs: deny
users:
  foo@example.com:
    partitions: [aws-x, k8s-z]
    logs: [aws-x]
    traces: [k8s-z]
  admin@example.com:
    partitions: ["*"]
    role: admin
aws_bindings:
  - partition: aws-x
    account: "123456789012"
    region: us-east-1
`

func newTestGate(t *testing.T, enforce bool) *PolicyGate {
	t.Helper()
	file, err := policy.ParseFile([]byte(testPolicyYAML))
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
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	gate := NewPolicyGate(PolicyOptions{
		File:    path,
		Enforce: enforce,
		Logger:  log.New(io.Discard, "", 0),
	})
	if gate == nil {
		t.Fatal("expected a gate")
	}
	if err := gate.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return gate
}

func foo() authz.Identity   { return authz.Identity{Subject: "foo", Email: "foo@example.com"} }
func admin() authz.Identity { return authz.Identity{Subject: "admin", Email: "admin@example.com"} }

func logRecords() []segment.LogRecord {
	return []segment.LogRecord{
		{Partition: "aws-x", Body: "x"},
		{Partition: "k8s-z", Body: "z"},
		{Partition: "", Body: "unlabeled"},
	}
}

func spanRecords() []segment.TraceRecord {
	return []segment.TraceRecord{
		{Partition: "aws-x", TraceID: "t1", SpanID: "s1"},
		{Partition: "k8s-z", TraceID: "t2", SpanID: "s2"},
	}
}

func TestFilterLogsAndTraces(t *testing.T) {
	gate := newTestGate(t, true)

	logs := gate.FilterLogs(foo(), logRecords())
	if len(logs) != 1 || logs[0].Partition != "aws-x" {
		t.Fatalf("expected only aws-x logs, got %+v", logs)
	}

	spans := gate.FilterSpans(foo(), spanRecords())
	if len(spans) != 1 || spans[0].Partition != "k8s-z" {
		t.Fatalf("expected only k8s-z spans, got %+v", spans)
	}
}

func TestObserveModeDoesNotFilter(t *testing.T) {
	gate := newTestGate(t, false)
	logs := gate.FilterLogs(foo(), logRecords())
	if len(logs) != 3 {
		t.Fatalf("observe mode must return all records, got %d", len(logs))
	}
	spans := gate.FilterSpans(foo(), spanRecords())
	if len(spans) != 2 {
		t.Fatalf("observe mode must return all spans, got %d", len(spans))
	}
}

func TestAdminBypass(t *testing.T) {
	gate := newTestGate(t, true)
	if len(gate.FilterLogs(admin(), logRecords())) != 3 {
		t.Fatal("admin must keep every log record")
	}
	if len(gate.FilterSpans(admin(), spanRecords())) != 2 {
		t.Fatal("admin must keep every span")
	}
	bg := authz.Identity{Subject: "break-glass-admin"}
	if !gate.IsAdmin(bg) {
		t.Fatal("break-glass subject must be treated as admin")
	}
}

func TestFailClosedWhenNotReady(t *testing.T) {
	gate := NewPolicyGate(PolicyOptions{
		File:    filepath.Join(t.TempDir(), "missing.json"),
		Enforce: true,
		Logger:  log.New(io.Discard, "", 0),
	})
	if gate.Ready() {
		t.Fatal("gate must not be ready")
	}
	if got := gate.FilterLogs(foo(), logRecords()); len(got) != 0 {
		t.Fatalf("enforce without a policy must drop all records, got %d", len(got))
	}
	if gate.Allowed(foo(), policy.SignalLogs) {
		t.Fatal("enforce without a policy must deny authenticated callers")
	}
	if !gate.Allowed(foo(), policy.SignalMetrics) {
		t.Fatal("metrics are always allowed")
	}
	if !gate.Allowed(authz.Identity{Subject: "break-glass-admin"}, policy.SignalLogs) {
		t.Fatal("break-glass admin must pass even without a loaded policy")
	}
}

func TestAnonymousUnderEnforcement(t *testing.T) {
	gate := newTestGate(t, true)
	var anon authz.Identity
	if gate.Allowed(anon, policy.SignalLogs) || gate.Allowed(anon, policy.SignalTraces) {
		t.Fatal("anonymous callers must be denied under enforcement")
	}
	if !gate.Allowed(anon, policy.SignalMetrics) {
		t.Fatal("metrics stay open")
	}
}

func TestAWSCanonicalization(t *testing.T) {
	gate := newTestGate(t, true)
	records := []segment.LogRecord{{
		Partition: "us-east-1",
		ResourceAttributes: map[string]string{
			"aws_account": "123456789012",
			"aws_region":  "us-east-1",
		},
		Body: "cloudwatch line",
	}}
	kept := gate.FilterLogs(foo(), records)
	if len(kept) != 1 || kept[0].Partition != "aws-x" {
		t.Fatalf("expected record canonicalized to aws-x, got %+v", kept)
	}

	// Region-only fallback: the pre-binding behavior stored the region as
	// guardian_partition, so a bare region name must still resolve.
	regionOnly := gate.FilterLogs(foo(), []segment.LogRecord{{Partition: "us-east-1", Body: "line"}})
	if len(regionOnly) != 1 || regionOnly[0].Partition != "aws-x" {
		t.Fatalf("expected region-only record canonicalized to aws-x, got %+v", regionOnly)
	}
}

func TestCanSeePartition(t *testing.T) {
	gate := newTestGate(t, true)
	if !gate.CanSeePartition(foo(), "aws-x") || !gate.CanSeePartition(foo(), "k8s-z") {
		t.Fatal("foo should see granted partitions")
	}
	if gate.CanSeePartition(foo(), "other") {
		t.Fatal("foo should not see an ungranted partition")
	}
	if gate.CanSeePartition(foo(), "") {
		t.Fatal("empty partition should be denied under enforcement")
	}
	if !gate.CanSeePartition(admin(), "other") {
		t.Fatal("admin should see every partition")
	}
	observe := newTestGate(t, false)
	if !observe.CanSeePartition(foo(), "other") {
		t.Fatal("observe mode should not filter")
	}
}

func TestSignalForPath(t *testing.T) {
	cases := []struct {
		path   string
		signal policy.Signal
		ok     bool
	}{
		{"/v1/logs/search", policy.SignalLogs, true},
		{"/loki/api/v1/query_range", policy.SignalLogs, true},
		{"/v1/traces/recent", policy.SignalTraces, true},
		{"/api/v2/traces/abc", policy.SignalTraces, true},
		{"/api/search", policy.SignalTraces, true},
		{"/api/metrics/query", policy.SignalTraces, true},
		{"/api/v1/query", "", false},
		{"/v1/metrics/range", "", false},
		{"/v1/services/discover", "", false},
		{"/healthz", "", false},
	}
	for _, tc := range cases {
		signal, ok := SignalForPath(tc.path)
		if ok != tc.ok || signal != tc.signal {
			t.Fatalf("SignalForPath(%q) = (%q,%v), want (%q,%v)", tc.path, signal, ok, tc.signal, tc.ok)
		}
	}
}

func TestMiddleware(t *testing.T) {
	gate := newTestGate(t, true)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := gate.Middleware(next)

	// Anonymous request to a logs path is rejected.
	req := httptest.NewRequest(http.MethodGet, "/v1/logs/search", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous logs request: got %d, want 403", rec.Code)
	}

	// Metrics paths are never gated.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics request: got %d, want 200", rec.Code)
	}

	// Authenticated request passes the request-level gate (record filtering
	// happens in the backend decorator).
	req = httptest.NewRequest(http.MethodGet, "/v1/traces/recent", nil)
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), foo()))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated trace request: got %d, want 200", rec.Code)
	}
}
