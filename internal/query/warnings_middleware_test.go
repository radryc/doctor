package query

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/radryc/doctor/internal/federation"
	"github.com/radryc/doctor/internal/proxy"
)

func TestWarningHeadersFromContext(t *testing.T) {
	handler := withWarningHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.AddWarning(r.Context(), proxy.Warning{Backend: "aws-a", Signal: proxy.SignalMetrics, Message: "timeout"})
		proxy.AddWarning(r.Context(), proxy.Warning{Backend: "aws-a", Signal: proxy.SignalMetrics, Message: "timeout"})
		w.WriteHeader(http.StatusOK)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := recorder.Header().Get("X-Doctor-Partial"); got != "aws-a: timeout" {
		t.Fatalf("X-Doctor-Partial = %q", got)
	}
	if got := recorder.Header().Get("X-Doctor-Partial-Count"); got != "2" {
		t.Fatalf("X-Doctor-Partial-Count = %q", got)
	}
}

func TestFederationHeadersFromRequestMeta(t *testing.T) {
	handler := withWarningHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		federation.RequestMetaFromContext(r.Context()).SetPlan("catalog-v7", 5, 2)
		w.WriteHeader(http.StatusOK)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := recorder.Header().Get("X-Doctor-Federation-Version"); got != "catalog-v7" {
		t.Fatalf("version header = %q", got)
	}
	if got := recorder.Header().Get("X-Doctor-Fanout-Matched"); got != "5" {
		t.Fatalf("matched header = %q", got)
	}
	if got := recorder.Header().Get("X-Doctor-Fanout-Selected"); got != "2" {
		t.Fatalf("selected header = %q", got)
	}
}

func TestWarningHeadersPreserveExistingPartialHeader(t *testing.T) {
	handler := withWarningHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Doctor-Partial", "router: capped")
		proxy.AddWarning(r.Context(), proxy.Warning{Backend: "aws-a", Signal: proxy.SignalLogs, Message: "boom"})
		w.WriteHeader(http.StatusOK)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := recorder.Header().Get("X-Doctor-Partial"); got != "router: capped; aws-a: boom" {
		t.Fatalf("X-Doctor-Partial = %q", got)
	}
}
