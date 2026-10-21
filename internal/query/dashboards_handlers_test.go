package query

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rydzu/ainfra/doctor/internal/dashboards"
	"github.com/rydzu/ainfra/doctor/internal/ruler"
)

func newDashboardTestService() *Service {
	store := dashboards.NewStore(ruler.NewMemoryStore(), "", "/doctor/v1/dashboards")
	return &Service{dashboardStore: store}
}

func TestDashboardYAMLDownloadHeader(t *testing.T) {
	svc := newDashboardTestService()
	req := httptest.NewRequest(http.MethodGet, "/v1/dashboards/default/preset-service-red?format=yaml&download=1", nil)
	rec := httptest.NewRecorder()
	svc.handleDashboardsRouter(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="preset-service-red.yaml"`) {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "doctor.dashboard/v1") {
		t.Fatalf("body is not YAML dashboard: %s", rec.Body.String())
	}
}

func TestDashboardImportAcceptsYAML(t *testing.T) {
	svc := newDashboardTestService()
	body := `version: doctor.dashboard/v1
id: imported-yaml
title: Imported YAML
tenant: default
variables: []
layout:
  - id: w1
    title: Imports
    kind: stat
    gridPos: {x: 0, y: 0, w: 3, h: 3}
    queries:
      - refId: A
        datasource: prometheus
        expr: vector(1)
`
	req := httptest.NewRequest(http.MethodPost, "/v1/dashboards/import?tenant=default", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/yaml")
	rec := httptest.NewRecorder()
	svc.handleDashboardsRouter(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("import status = %d, body = %s", rec.Code, rec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/v1/dashboards/default/imported-yaml", nil)
	getRec := httptest.NewRecorder()
	svc.handleDashboardsRouter(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), `"Imported YAML"`) {
		t.Fatalf("imported dashboard body = %s", getRec.Body.String())
	}
}

func TestDashboardPutRejectsBuiltIn(t *testing.T) {
	svc := newDashboardTestService()
	body := `{"version":"doctor.dashboard/v1","id":"preset-service-red","title":"Nope","tenant":"default"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/dashboards/default/preset-service-red", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	svc.handleDashboardsRouter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
