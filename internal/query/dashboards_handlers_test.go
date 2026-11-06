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
	"github.com/radryc/doctor/internal/dashboards"
	"github.com/radryc/doctor/internal/ruler"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
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

const importDashboardYAML = `version: doctor.dashboard/v1
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

func importDashboard(t *testing.T, svc *Service, query string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/dashboards/import?tenant=default"+query, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/yaml")
	rec := httptest.NewRecorder()
	svc.handleDashboardsRouter(rec, req)
	return rec
}

func TestDashboardImportConflictModes(t *testing.T) {
	svc := newDashboardTestService()
	if rec := importDashboard(t, svc, "", importDashboardYAML); rec.Code != http.StatusOK {
		t.Fatalf("first import status = %d, body = %s", rec.Code, rec.Body.String())
	}

	conflict := importDashboard(t, svc, "", importDashboardYAML)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("default conflict status = %d, body = %s", conflict.Code, conflict.Body.String())
	}
	if !strings.Contains(conflict.Body.String(), `"conflicts"`) {
		t.Fatalf("conflict body = %s", conflict.Body.String())
	}

	skipped := importDashboard(t, svc, "&mode=skip-existing", importDashboardYAML)
	if skipped.Code != http.StatusOK || !strings.Contains(skipped.Body.String(), `"skipped":1`) {
		t.Fatalf("skip status = %d, body = %s", skipped.Code, skipped.Body.String())
	}

	overwritten := importDashboard(t, svc, "&mode=overwrite", importDashboardYAML)
	if overwritten.Code != http.StatusOK || !strings.Contains(overwritten.Body.String(), `"imported":1`) {
		t.Fatalf("overwrite status = %d, body = %s", overwritten.Code, overwritten.Body.String())
	}
}

func TestDashboardImportRejectsUnknownMode(t *testing.T) {
	svc := newDashboardTestService()
	rec := importDashboard(t, svc, "&mode=merge", importDashboardYAML)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
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

const dashboardRBACPolicyYAML = `
version: 1
defaults:
  partitions: deny
  logs: deny
  traces: deny
users:
  maintainer@example.com:
    partitions: [agent, monofs]
    role: maintainer
  agent-admin@example.com:
    partitions: [agent]
    role: maintainer
  viewer@example.com:
    partitions: [agent]
    role: viewer
  admin@example.com:
    partitions: ["*"]
    role: admin
`

func newDashboardRBACService(t *testing.T) *Service {
	t.Helper()
	file, err := policy.ParseFile([]byte(dashboardRBACPolicyYAML))
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
	gate := access.NewPolicyGate(access.PolicyOptions{
		File:    path,
		Enforce: true,
		Logger:  log.New(io.Discard, "", 0),
	})
	if gate == nil {
		t.Fatal("expected a policy gate")
	}
	if err := gate.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh policy: %v", err)
	}
	svc := newDashboardTestService()
	svc.policyGate = gate
	return svc
}

func dashboardDocJSON(id, title string, partitions []string) string {
	doc := map[string]any{
		"version":    "doctor.dashboard/v1",
		"id":         id,
		"title":      title,
		"tenant":     "default",
		"partitions": partitions,
		"layout": []any{
			map[string]any{
				"id":      "panel",
				"title":   "Panel",
				"kind":    "stat",
				"gridPos": map[string]int{"x": 0, "y": 0, "w": 3, "h": 3},
				"queries": []any{
					map[string]any{"refId": "A", "datasource": "prometheus", "expr": "vector(1)"},
				},
			},
		},
	}
	data, _ := json.Marshal(doc)
	return string(data)
}

func dashboardRequestAs(svc *Service, method, path, body string, identity authz.Identity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	svc.handleDashboardsRouter(rec, req)
	return rec
}

func TestDashboardPutRequiresMaintainerForDeclaredPartitions(t *testing.T) {
	svc := newDashboardRBACService(t)
	body := dashboardDocJSON("agent-panel", "Agent panel", []string{"agent"})

	viewer := authz.Identity{Subject: "viewer@example.com", Email: "viewer@example.com"}
	if rec := dashboardRequestAs(svc, http.MethodPut, "/v1/dashboards/default/agent-panel", body, viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}

	agentAdmin := authz.Identity{Subject: "agent-admin@example.com", Email: "agent-admin@example.com"}
	if rec := dashboardRequestAs(svc, http.MethodPut, "/v1/dashboards/default/agent-panel", body, agentAdmin); rec.Code != http.StatusOK {
		t.Fatalf("maintainer PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The same maintainer must not gain access to another partition by editing
	// the document to include it.
	escalated := dashboardDocJSON("agent-panel", "Agent panel", []string{"agent", "monofs"})
	if rec := dashboardRequestAs(svc, http.MethodPut, "/v1/dashboards/default/agent-panel", escalated, agentAdmin); rec.Code != http.StatusForbidden {
		t.Fatalf("escalated PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestDashboardPutRequiresPartitionsUnderEnforcement(t *testing.T) {
	svc := newDashboardRBACService(t)
	body := dashboardDocJSON("unscoped", "Unscoped", nil)
	admin := authz.Identity{Subject: "admin@example.com", Email: "admin@example.com"}
	if rec := dashboardRequestAs(svc, http.MethodPut, "/v1/dashboards/default/unscoped", body, admin); rec.Code != http.StatusOK {
		t.Fatalf("admin PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}

	maintainer := authz.Identity{Subject: "maintainer@example.com", Email: "maintainer@example.com"}
	body = dashboardDocJSON("unscoped-user", "Unscoped user", nil)
	if rec := dashboardRequestAs(svc, http.MethodPut, "/v1/dashboards/default/unscoped-user", body, maintainer); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "partitions is required") {
		t.Fatalf("missing partitions status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestDashboardReadListAndExportArePartitionScoped(t *testing.T) {
	svc := newDashboardRBACService(t)
	admin := authz.Identity{Subject: "admin@example.com", Email: "admin@example.com"}
	for _, tc := range []struct {
		id         string
		partitions []string
	}{
		{"agent-dash", []string{"agent"}},
		{"monofs-dash", []string{"monofs"}},
	} {
		body := dashboardDocJSON(tc.id, tc.id, tc.partitions)
		if rec := dashboardRequestAs(svc, http.MethodPut, "/v1/dashboards/default/"+tc.id, body, admin); rec.Code != http.StatusOK {
			t.Fatalf("seed %s status = %d, body = %s", tc.id, rec.Code, rec.Body.String())
		}
	}

	viewer := authz.Identity{Subject: "viewer@example.com", Email: "viewer@example.com"}
	listRec := dashboardRequestAs(svc, http.MethodGet, "/v1/dashboards?tenant=default", "", viewer)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRec.Code, listRec.Body.String())
	}
	if strings.Contains(listRec.Body.String(), "monofs-dash") {
		t.Fatalf("viewer saw monofs dashboard: %s", listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), "agent-dash") {
		t.Fatalf("viewer did not see agent dashboard: %s", listRec.Body.String())
	}

	if rec := dashboardRequestAs(svc, http.MethodGet, "/v1/dashboards/default/monofs-dash", "", viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer GET monofs status = %d, body = %s", rec.Code, rec.Body.String())
	}

	exportRec := dashboardRequestAs(svc, http.MethodGet, "/v1/dashboards/export?tenant=default&format=json", "", viewer)
	if exportRec.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %s", exportRec.Code, exportRec.Body.String())
	}
	if strings.Contains(exportRec.Body.String(), "monofs-dash") || !strings.Contains(exportRec.Body.String(), "agent-dash") {
		t.Fatalf("export leaked dashboards: %s", exportRec.Body.String())
	}
}

func TestDashboardDeleteRequiresMaintainer(t *testing.T) {
	svc := newDashboardRBACService(t)
	admin := authz.Identity{Subject: "admin@example.com", Email: "admin@example.com"}
	body := dashboardDocJSON("agent-dash", "Agent", []string{"agent"})
	if rec := dashboardRequestAs(svc, http.MethodPut, "/v1/dashboards/default/agent-dash", body, admin); rec.Code != http.StatusOK {
		t.Fatalf("seed status = %d, body = %s", rec.Code, rec.Body.String())
	}

	viewer := authz.Identity{Subject: "viewer@example.com", Email: "viewer@example.com"}
	if rec := dashboardRequestAs(svc, http.MethodDelete, "/v1/dashboards/default/agent-dash", "", viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer DELETE status = %d, body = %s", rec.Code, rec.Body.String())
	}

	agentAdmin := authz.Identity{Subject: "agent-admin@example.com", Email: "agent-admin@example.com"}
	if rec := dashboardRequestAs(svc, http.MethodDelete, "/v1/dashboards/default/agent-dash", "", agentAdmin); rec.Code != http.StatusOK {
		t.Fatalf("maintainer DELETE status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestDashboardImportPreflightRejectsPartialAccess(t *testing.T) {
	svc := newDashboardRBACService(t)
	viewer := authz.Identity{Subject: "viewer@example.com", Email: "viewer@example.com"}
	bundle := `{"version":"doctor.dashboard.bundle/v1","exportedAt":"2026-11-05T00:00:00Z","dashboards":[` +
		dashboardDocJSON("agent-dash", "Agent", []string{"agent"}) + `,` +
		dashboardDocJSON("monofs-dash", "MonoFS", []string{"monofs"}) + `]}`
	rec := dashboardRequestAs(svc, http.MethodPost, "/v1/dashboards/import?tenant=default", bundle, viewer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("import status = %d, body = %s", rec.Code, rec.Body.String())
	}

	listRec := dashboardRequestAs(svc, http.MethodGet, "/v1/dashboards?tenant=default", "", viewer)
	if strings.Contains(listRec.Body.String(), "agent-dash") {
		t.Fatalf("partial import wrote agent dashboard: %s", listRec.Body.String())
	}
}
