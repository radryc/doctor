package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/dashboards"
	"sigs.k8s.io/yaml"
)

// DashboardStore is the interface used by the dashboards HTTP handlers.
type DashboardStore interface {
	Get(ctx context.Context, tenant, id string) (*dashboards.DashboardDocument, error)
	List(ctx context.Context, tenant, tag, search string) ([]dashboards.DashboardDocument, error)
	Put(ctx context.Context, tenant, id string, doc *dashboards.DashboardDocument, ifMatch string) error
	Delete(ctx context.Context, tenant, id string) error
	ListVersions(ctx context.Context, tenant, id string) ([]dashboards.DashboardVersion, error)
}

// dashboardReadable reports whether the caller may see a dashboard. Dashboards
// without declared partitions are legacy/built-in and stay readable; new
// documents declare partitions so reads can be scoped.
func (s *Service) dashboardReadable(r *http.Request, doc *dashboards.DashboardDocument) bool {
	if s.policyGate == nil || !s.policyGate.Enforced() || doc == nil {
		return true
	}
	partitions := doc.EffectivePartitions()
	if len(partitions) == 0 {
		return true
	}
	identity := identityFromContext(r.Context())
	for _, partition := range partitions {
		if !s.policyGate.CanSeePartition(identity, partition) {
			return false
		}
	}
	return true
}

// dashboardWritable reports whether the caller may create, update, or delete a
// dashboard. Every effective partition requires maintainer or admin role.
func (s *Service) dashboardWritable(r *http.Request, doc *dashboards.DashboardDocument) (bool, string) {
	if s.policyGate == nil || !s.policyGate.Enforced() || doc == nil {
		return true, ""
	}
	identity := identityFromContext(r.Context())
	if s.policyGate.IsAdmin(identity) {
		return true, ""
	}
	partitions := doc.EffectivePartitions()
	if len(partitions) == 0 {
		return false, "partitions is required when access enforcement is enabled"
	}
	for _, partition := range partitions {
		if !s.policyGate.CanModifyPartition(identity, partition) {
			return false, fmt.Sprintf("maintainer or admin role required on partition %q", partition)
		}
	}
	return true, ""
}

func filterReadableDashboards(r *http.Request, s *Service, docs []dashboards.DashboardDocument) []dashboards.DashboardDocument {
	if s.policyGate == nil || !s.policyGate.Enforced() {
		return docs
	}
	out := make([]dashboards.DashboardDocument, 0, len(docs))
	for i := range docs {
		if s.dashboardReadable(r, &docs[i]) {
			out = append(out, docs[i])
		}
	}
	return out
}

// handleListDashboards serves GET /v1/dashboards?tenant=<t>&tag=<tag>&q=<search>
func (s *Service) handleListDashboards(w http.ResponseWriter, r *http.Request) {
	if s.dashboardStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "dashboards store not configured"})
		return
	}
	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		tenant = s.tenantFromRequest(r)
	}
	tag := r.URL.Query().Get("tag")
	search := r.URL.Query().Get("q")

	docs, err := s.dashboardStore.List(r.Context(), tenant, tag, search)
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	docs = filterReadableDashboards(r, s, docs)
	writeJSON(w, http.StatusOK, map[string]any{"dashboards": docs})
}

// handleGetDashboard serves GET /v1/dashboards/{tenant}/{id}
// Supports ?format=json|yaml and ?download=1
func (s *Service) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	if s.dashboardStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "dashboards store not configured"})
		return
	}
	tenant, id, ok := parseDashboardPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path: expected /v1/dashboards/{tenant}/{id}"})
		return
	}

	doc, err := s.dashboardStore.Get(r.Context(), tenant, id)
	if err != nil {
		if errors.Is(err, dashboards.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "dashboard not found"})
		} else {
			writeInternalError(w, r, err)
		}
		return
	}
	if !s.dashboardReadable(r, doc) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: dashboard affects partitions you cannot read"})
		return
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	download := r.URL.Query().Get("download") == "1"

	switch format {
	case "yaml":
		data, err := doc.MarshalYAMLBytes()
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		if download {
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.yaml"`, id))
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(data)
	default:
		if download {
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, id))
		}
		writeJSON(w, http.StatusOK, doc)
	}
}

// handlePutDashboard serves PUT /v1/dashboards/{tenant}/{id}
func (s *Service) handlePutDashboard(w http.ResponseWriter, r *http.Request) {
	if s.dashboardStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "dashboards store not configured"})
		return
	}
	tenant, id, ok := parseDashboardPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path: expected /v1/dashboards/{tenant}/{id}"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	_ = r.Body.Close()

	doc, err := dashboards.ParseDocument(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Enforce path parameters take precedence.
	doc.Tenant = tenant
	doc.ID = id

	// A dashboard is partition-scoped control-plane state: changing it requires
	// maintainer/admin on both the old and the new declared partitions.
	existing, getErr := s.dashboardStore.Get(r.Context(), tenant, id)
	if getErr != nil && !errors.Is(getErr, dashboards.ErrNotFound) {
		writeInternalError(w, r, getErr)
		return
	}
	if existing != nil {
		if ok, reason := s.dashboardWritable(r, existing); !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: " + reason})
			return
		}
	}
	if ok, reason := s.dashboardWritable(r, doc); !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: " + reason})
		return
	}

	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if err := s.dashboardStore.Put(r.Context(), tenant, id, doc, ifMatch); err != nil {
		switch err {
		case dashboards.ErrBuiltIn:
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot modify a built-in preset; clone it first"})
		case dashboards.ErrConflict:
			writeJSON(w, http.StatusConflict, map[string]string{"error": "version conflict: dashboard was modified concurrently"})
		default:
			writeInternalError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleDeleteDashboard serves DELETE /v1/dashboards/{tenant}/{id}
func (s *Service) handleDeleteDashboard(w http.ResponseWriter, r *http.Request) {
	if s.dashboardStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "dashboards store not configured"})
		return
	}
	tenant, id, ok := parseDashboardPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path: expected /v1/dashboards/{tenant}/{id}"})
		return
	}

	existing, err := s.dashboardStore.Get(r.Context(), tenant, id)
	if err != nil {
		if errors.Is(err, dashboards.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "dashboard not found"})
		} else {
			writeInternalError(w, r, err)
		}
		return
	}
	if ok, reason := s.dashboardWritable(r, existing); !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: " + reason})
		return
	}

	if err := s.dashboardStore.Delete(r.Context(), tenant, id); err != nil {
		switch err {
		case dashboards.ErrBuiltIn:
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot delete a built-in preset"})
		case dashboards.ErrNotFound:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "dashboard not found"})
		default:
			writeInternalError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleDashboardVersions serves GET /v1/dashboards/{tenant}/{id}/versions
func (s *Service) handleDashboardVersions(w http.ResponseWriter, r *http.Request) {
	if s.dashboardStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "dashboards store not configured"})
		return
	}
	// Path: /v1/dashboards/{tenant}/{id}/versions
	trimmed := strings.TrimPrefix(r.URL.Path, "/v1/dashboards/")
	trimmed = strings.TrimSuffix(trimmed, "/versions")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path"})
		return
	}
	tenant, id := parts[0], parts[1]

	doc, err := s.dashboardStore.Get(r.Context(), tenant, id)
	if err != nil {
		if errors.Is(err, dashboards.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "dashboard not found"})
		} else {
			writeInternalError(w, r, err)
		}
		return
	}
	if !s.dashboardReadable(r, doc) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: dashboard affects partitions you cannot read"})
		return
	}

	versions, err := s.dashboardStore.ListVersions(r.Context(), tenant, id)
	if err != nil {
		writeInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

// handleImportDashboards serves POST /v1/dashboards/import
// Accepts a single DashboardDocument or a DashboardBundle.
func (s *Service) handleImportDashboards(w http.ResponseWriter, r *http.Request) {
	if s.dashboardStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "dashboards store not configured"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	_ = r.Body.Close()

	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		tenant = s.tenantFromRequest(r)
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("on_conflict")))
	}
	if mode == "" {
		mode = "fail-on-conflict"
	}
	switch mode {
	case "fail-on-conflict", "skip-existing", "overwrite":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be fail-on-conflict, skip-existing, or overwrite"})
		return
	}

	docs := make([]dashboards.DashboardDocument, 0, 1)
	if bundle, parseErr := dashboards.ParseBundle(body); parseErr == nil {
		docs = append(docs, bundle.Dashboards...)
	} else {
		doc, parseDocErr := dashboards.ParseDocument(body)
		if parseDocErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": parseDocErr.Error()})
			return
		}
		docs = append(docs, *doc)
	}

	// Preflight the whole bundle before writing anything. A partially applied
	// import is worse than an explicit 403, and each document must be writable
	// on every partition it declares.
	for idx := range docs {
		doc := &docs[idx]
		t := doc.Tenant
		if t == "" || t == "*" {
			t = tenant
		}
		doc.Tenant = t
		if ok, reason := s.dashboardWritable(r, doc); !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": fmt.Sprintf("forbidden: dashboard %q: %s", doc.ID, reason)})
			return
		}
		if mode == "overwrite" {
			existing, getErr := s.dashboardStore.Get(r.Context(), t, doc.ID)
			if getErr != nil && !errors.Is(getErr, dashboards.ErrNotFound) {
				writeInternalError(w, r, getErr)
				return
			}
			if existing != nil {
				if ok, reason := s.dashboardWritable(r, existing); !ok {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": fmt.Sprintf("forbidden: existing dashboard %q: %s", doc.ID, reason)})
					return
				}
			}
		}
	}

	imported := 0
	skipped := 0
	conflicts := make([]map[string]string, 0)
	for idx := range docs {
		doc := &docs[idx]
		t := doc.Tenant
		if t == "" || t == "*" {
			t = tenant
		}
		doc.Tenant = t
		existing, getErr := s.dashboardStore.Get(r.Context(), t, doc.ID)
		if getErr != nil && !errors.Is(getErr, dashboards.ErrNotFound) {
			writeInternalError(w, r, getErr)
			return
		}
		if getErr == nil {
			switch mode {
			case "skip-existing":
				skipped++
				continue
			case "fail-on-conflict":
				conflicts = append(conflicts, map[string]string{
					"tenant": t,
					"id":     doc.ID,
					"error":  "dashboard already exists",
				})
				continue
			}
		}
		ifMatch := "absent"
		if existing != nil && existing.Meta != nil && existing.Meta.VersionID != "" {
			ifMatch = existing.Meta.VersionID
		} else if existing != nil {
			// Overwrite an existing dashboard whose version could not be read.
			ifMatch = ""
		}
		if putErr := s.dashboardStore.Put(r.Context(), t, doc.ID, doc, ifMatch); putErr != nil {
			switch {
			case errors.Is(putErr, dashboards.ErrConflict):
				conflicts = append(conflicts, map[string]string{
					"tenant": t,
					"id":     doc.ID,
					"error":  "version conflict: dashboard was modified concurrently",
				})
				continue
			case errors.Is(putErr, dashboards.ErrBuiltIn):
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cannot modify a built-in preset; clone it first"})
				return
			default:
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("import %s: %s", doc.ID, putErr.Error())})
				return
			}
		}
		imported++
	}

	response := map[string]any{"imported": imported, "mode": mode}
	if skipped > 0 {
		response["skipped"] = skipped
	}
	if len(conflicts) > 0 {
		response["conflicts"] = conflicts
		writeJSON(w, http.StatusConflict, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleExportDashboards serves GET /v1/dashboards/export?tenant=<t>&ids=a,b,c&format=json|yaml
func (s *Service) handleExportDashboards(w http.ResponseWriter, r *http.Request) {
	if s.dashboardStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "dashboards store not configured"})
		return
	}
	tenant := r.URL.Query().Get("tenant")
	if tenant == "" {
		tenant = s.tenantFromRequest(r)
	}
	idsParam := r.URL.Query().Get("ids")
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))

	docs, err := s.dashboardStore.List(r.Context(), tenant, "", "")
	if err != nil {
		writeInternalError(w, r, err)
		return
	}

	if idsParam != "" {
		want := map[string]bool{}
		for _, id := range strings.Split(idsParam, ",") {
			want[strings.TrimSpace(id)] = true
		}
		filtered := docs[:0]
		for _, d := range docs {
			if want[d.ID] {
				filtered = append(filtered, d)
			}
		}
		docs = filtered
	}

	docs = filterReadableDashboards(r, s, docs)

	bundle := dashboards.DashboardBundle{
		Version:    dashboards.BundleVersion,
		ExportedAt: time.Now().UTC(),
		Dashboards: docs,
	}

	switch format {
	case "yaml":
		data, err := yaml.Marshal(bundle)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="dashboards-bundle.yaml"`)
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(data)
	default:
		data, err := json.Marshal(bundle)
		if err != nil {
			writeInternalError(w, r, err)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="dashboards-bundle.json"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

// handleDashboardsRouter routes all /v1/dashboards/* traffic.
func (s *Service) handleDashboardsRouter(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// /v1/dashboards/import
	if path == "/v1/dashboards/import" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.handleImportDashboards(w, r)
		return
	}

	// /v1/dashboards/export
	if path == "/v1/dashboards/export" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.handleExportDashboards(w, r)
		return
	}

	// /v1/dashboards  (list)
	if path == "/v1/dashboards" || path == "/v1/dashboards/" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.handleListDashboards(w, r)
		return
	}

	// /v1/dashboards/{tenant}/{id}/versions
	if strings.HasSuffix(path, "/versions") {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.handleDashboardVersions(w, r)
		return
	}

	// /v1/dashboards/{tenant}/{id}
	switch r.Method {
	case http.MethodGet:
		s.handleGetDashboard(w, r)
	case http.MethodPut:
		s.handlePutDashboard(w, r)
	case http.MethodDelete:
		s.handleDeleteDashboard(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// parseDashboardPath extracts tenant and id from /v1/dashboards/{tenant}/{id}
func parseDashboardPath(path string) (tenant, id string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/v1/dashboards/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
