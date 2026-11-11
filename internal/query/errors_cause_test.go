package query

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInternalCauseClassification(t *testing.T) {
	cases := map[string]string{
		"rpc error: code = Unknown desc = no healthy nodes available": causeStorageUnavailable,
		"context deadline exceeded":                                   causeBackendTimeout,
		"dial tcp 10.0.0.1:9000: connect: connection refused":         causeBackendUnavailable,
		"permission denied":                                           causeAccessDenied,
		"unexpected number of samples":                                causeInternal,
	}
	for message, want := range cases {
		if got := internalCause(message); got != want {
			t.Errorf("internalCause(%q) = %q, want %q", message, got, want)
		}
	}
}

func TestPrometheusInternalErrorCarriesCause(t *testing.T) {
	rec := httptest.NewRecorder()
	writePrometheusError(rec, http.StatusInternalServerError, "internal",
		"rpc error: code = Unknown desc = no healthy nodes available")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["cause"] != causeStorageUnavailable {
		t.Fatalf("cause = %v, want %q", body["cause"], causeStorageUnavailable)
	}
	if body["error"] != internalErrorMessage {
		t.Fatalf("internal error detail leaked: %v", body["error"])
	}
}

func TestLokiInternalErrorCarriesCause(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLokiError(rec, http.StatusInternalServerError,
		"rpc error: code = Unknown desc = no healthy nodes available")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["cause"] != causeStorageUnavailable {
		t.Fatalf("cause = %v, want %q", body["cause"], causeStorageUnavailable)
	}
	if body["error"] != internalErrorMessage {
		t.Fatalf("internal error detail leaked: %v", body["error"])
	}
}
