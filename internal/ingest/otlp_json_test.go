package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

type otlpJSONCaptureWriter struct {
	traces  []segment.TraceRecord
	logs    []segment.LogRecord
	metrics []segment.MetricPointRecord
}

func (w *otlpJSONCaptureWriter) WriteTraces(_ context.Context, records []segment.TraceRecord) error {
	w.traces = append(w.traces, records...)
	return nil
}
func (w *otlpJSONCaptureWriter) WriteLogs(_ context.Context, records []segment.LogRecord) error {
	w.logs = append(w.logs, records...)
	return nil
}
func (w *otlpJSONCaptureWriter) WriteMetrics(_ context.Context, records []segment.MetricPointRecord) error {
	w.metrics = append(w.metrics, records...)
	return nil
}
func (w *otlpJSONCaptureWriter) WriteGuardianEvents(context.Context, []segment.GuardianEvent) error {
	return nil
}

func TestHandleHTTPTracesAcceptsOTLPJSONWithHexIDs(t *testing.T) {
	writer := &otlpJSONCaptureWriter{}
	svc := NewService("X-Doctor-Tenant", "default", writer, nil)
	body := `{
	  "resourceSpans": [{
	    "resource": {"attributes": [
	      {"key": "service.name", "value": {"stringValue": "guardian-probe"}},
	      {"key": "guardian.partition", "value": {"stringValue": "prod"}}
	    ]},
	    "scopeSpans": [{"spans": [{
	      "traceId": "4bf92f3577b34da6a3ce929d0e0e4736",
	      "spanId": "00f067aa0ba902b7",
	      "name": "guardian.probe.run",
	      "kind": 1,
	      "startTimeUnixNano": "1700000000000000000",
	      "endTimeUnixNano": "1700000001000000000"
	    }]}]
	  }]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	svc.handleHTTPTraces(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("response content type = %q, want application/json", ct)
	}
	if len(writer.traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(writer.traces))
	}
	if got := writer.traces[0].Service; got != "guardian-probe" {
		t.Fatalf("service = %q, want guardian-probe", got)
	}
	if got := writer.traces[0].TraceID; got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace ID = %q", got)
	}
}

func TestHandleHTTPMetricsAcceptsOTLPJSON(t *testing.T) {
	writer := &otlpJSONCaptureWriter{}
	svc := NewService("X-Doctor-Tenant", "default", writer, nil)
	body := `{
	  "resourceMetrics": [{
	    "resource": {"attributes": [
	      {"key": "service.name", "value": {"stringValue": "guardian-probe"}}
	    ]},
	    "scopeMetrics": [{"metrics": [{
	      "name": "guardian_probe_up",
	      "unit": "1",
	      "gauge": {"dataPoints": [{
	        "asDouble": 1,
	        "timeUnixNano": "1700000000000000000"
	      }]}
	    }]}]
	  }]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	svc.handleHTTPMetrics(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(writer.metrics) != 1 {
		t.Fatalf("metrics = %d, want 1", len(writer.metrics))
	}
	if got := writer.metrics[0].MetricName; got != "guardian_probe_up" {
		t.Fatalf("metric name = %q", got)
	}
}

func TestHandleHTTPLogsAcceptsOTLPJSON(t *testing.T) {
	writer := &otlpJSONCaptureWriter{}
	svc := NewService("X-Doctor-Tenant", "default", writer, nil)
	body := `{
	  "resourceLogs": [{
	    "resource": {"attributes": [
	      {"key": "service.name", "value": {"stringValue": "guardian-probe"}}
	    ]},
	    "scopeLogs": [{"logRecords": [{
	      "timeUnixNano": "1700000000000000000",
	      "severityNumber": 17,
	      "severityText": "ERROR",
	      "body": {"stringValue": "probe failed"}
	    }]}]
	  }]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	svc.handleHTTPLogs(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(writer.logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(writer.logs))
	}
	if got := writer.logs[0].Body; got != "probe failed" {
		t.Fatalf("log body = %q", got)
	}
}
