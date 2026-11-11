package query

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	monofsstore "github.com/radryc/doctor/internal/store/monofs"
)

type captureSQLBackend struct {
	fakeLogEngineBackend
	called     bool
	visible    []string
	restricted bool
	response   monofsstore.DuckDBSQLResponse
}

func (c *captureSQLBackend) QueryDuckDBSQL(_ context.Context, _ string, _, _ string, _, _ time.Time, _ int, visible []string, restricted bool) (monofsstore.DuckDBSQLResponse, error) {
	c.called = true
	c.visible = append([]string(nil), visible...)
	c.restricted = restricted
	return c.response, nil
}

func TestHandleDuckDBSQLReturnsPerNodeResults(t *testing.T) {
	backend := &captureSQLBackend{response: monofsstore.DuckDBSQLResponse{Nodes: []monofsstore.DuckDBSQLNodeResult{{
		NodeID: "node-a",
		Result: &monofsstore.DuckDBSQLResult{Dataset: "logs", Columns: []string{"count"}, Rows: [][]any{{float64(3)}}, RowCount: 1},
	}}}}
	svc := &Service{defaultTenant: "default", logEngine: backend}
	body := `{"dataset":"logs","query":"SELECT count(*) FROM logs","limit":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sql/query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	svc.handleDuckDBSQL(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !backend.called {
		t.Fatal("backend was not called")
	}
	if backend.restricted {
		t.Fatalf("expected unrestricted without a policy gate, got visible=%v", backend.visible)
	}
	var response duckDBSQLResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Dataset != "logs" || len(response.Nodes) != 1 || response.Nodes[0].NodeID != "node-a" {
		t.Fatalf("response = %#v", response)
	}
}

func TestHandleDuckDBSQLRejectsInvalidDatasetAndMethod(t *testing.T) {
	svc := &Service{defaultTenant: "default", logEngine: &captureSQLBackend{}}

	req := httptest.NewRequest(http.MethodPost, "/v1/sql/query", strings.NewReader(`{"dataset":"secrets","query":"SELECT 1"}`))
	rec := httptest.NewRecorder()
	svc.handleDuckDBSQL(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid dataset status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sql/query", nil)
	rec = httptest.NewRecorder()
	svc.handleDuckDBSQL(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", rec.Code)
	}
}

func TestHandleDuckDBSQLRejectsOversizedRange(t *testing.T) {
	svc := &Service{defaultTenant: "default", logEngine: &captureSQLBackend{}}
	body := `{"dataset":"logs","query":"SELECT * FROM logs","from":"2026-01-01T00:00:00Z","to":"2026-01-09T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sql/query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	svc.handleDuckDBSQL(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized range status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
