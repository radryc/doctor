package query

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInternalErrorDoesNotLeakDetails(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/traces/x", nil)
	req = req.WithContext(contextWithRequestID(req, "req-1"))
	rec := httptest.NewRecorder()
	writeInternalError(rec, req, errors.New("dial tcp 10.0.0.1: failure"))

	body := rec.Body.String()
	if strings.Contains(body, "10.0.0.1") || strings.Contains(body, "dial tcp") {
		t.Fatalf("internal error leaked to client: %s", body)
	}
	if !strings.Contains(body, "req-1") {
		t.Fatalf("request ID missing from error envelope: %s", body)
	}
	if !strings.Contains(body, "internal server error") {
		t.Fatalf("generic message missing: %s", body)
	}
}
