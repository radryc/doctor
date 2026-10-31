package ruler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/radryc/doctor/internal/store"
	"github.com/radryc/monofs/pkg/authz"
	"sigs.k8s.io/yaml"
)

// Authorizer gates rule mutations. A nil authorizer allows everything
// (observe-first rollout).
type Authorizer interface {
	CanModify(ctx context.Context, tenant, group string) bool
}

// AuthenticatedAuthorizer allows mutations from any authenticated caller.
// The rules binary installs a static-token authentication middleware in front
// of the API, so reaching this authorizer implies a valid service credential.
type AuthenticatedAuthorizer struct{}

// CanModify implements Authorizer.
func (AuthenticatedAuthorizer) CanModify(ctx context.Context, _, _ string) bool {
	id, ok := authz.IdentityFromContext(ctx)
	return ok && !id.IsAnonymous()
}

// API serves the rule CRUD and manual evaluation endpoints.
type API struct {
	Store      *Store
	Evaluator  *Evaluator
	Authorizer Authorizer
	Scheduler  *Scheduler
	Lease      *LeaseManager
	Logger     *log.Logger
}

// Handler builds the rules HTTP handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/v1/rules", a.handleCollection)
	mux.HandleFunc("/v1/rules/status", a.handleStatus)
	mux.HandleFunc("/v1/rules/evaluate", a.handleEvaluate)
	mux.HandleFunc("/v1/rules/", a.handleGroup)
	return mux
}

func (a *API) handleStatus(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"enabled": true,
		"rulerId": "",
		"leader":  false,
	}
	if a.Lease != nil {
		lease := a.Lease.Status()
		payload["rulerId"] = lease.RulerID
		payload["leader"] = lease.Leader
		payload["lease"] = lease
	}
	if a.Scheduler != nil {
		payload["scheduler"] = a.Scheduler.Stats()
	}
	writeJSON(w, http.StatusOK, payload)
}

func (a *API) handleCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	refs, err := a.Store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type groupSummary struct {
		Tenant   string `json:"tenant"`
		Group    string `json:"group"`
		Interval string `json:"interval"`
		Rules    int    `json:"rules"`
	}
	out := make([]groupSummary, 0, len(refs))
	for _, ref := range refs {
		interval := ""
		rules := 0
		if ref.File != nil && len(ref.File.Groups) > 0 {
			interval = ref.File.Groups[0].IntervalOrDefault().String()
			rules = len(ref.File.Groups[0].Rules)
		}
		out = append(out, groupSummary{Tenant: ref.Tenant, Group: ref.Group, Interval: interval, Rules: rules})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

func (a *API) handleGroup(w http.ResponseWriter, r *http.Request) {
	tenant, group, ok := parseGroupPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "path must be /v1/rules/{tenant}/{group}")
		return
	}
	switch r.Method {
	case http.MethodGet:
		file, err := a.Store.Get(r.Context(), tenant, group)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		data, err := yaml.Marshal(file)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	case http.MethodPut:
		if !a.canModify(r.Context(), tenant, group) {
			writeError(w, http.StatusForbidden, "not authorized to modify this rule group")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		file, err := ParseFile(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		for i := range file.Groups {
			file.Groups[i].Tenant = tenant
		}
		if err := a.Store.Put(r.Context(), tenant, group, file, false); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "tenant": tenant, "group": group})
	case http.MethodDelete:
		if !a.canModify(r.Context(), tenant, group) {
			writeError(w, http.StatusForbidden, "not authorized to modify this rule group")
			return
		}
		if err := a.Store.Delete(r.Context(), tenant, group); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "tenant": tenant, "group": group})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	if tenant == "" || group == "" {
		writeError(w, http.StatusBadRequest, "tenant and group are required")
		return
	}
	file, err := a.Store.Get(r.Context(), tenant, group)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	results := make([]EvalResult, 0)
	for _, groupSpec := range file.Groups {
		results = append(results, a.Evaluator.EvaluateGroup(r.Context(), groupSpec)...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (a *API) canModify(ctx context.Context, tenant, group string) bool {
	if a.Authorizer == nil {
		return true
	}
	return a.Authorizer.CanModify(ctx, tenant, group)
}

func parseGroupPath(path string) (tenant, group string, ok bool) {
	rest := strings.TrimPrefix(path, "/v1/rules/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]any{"status": "error", "error": message})
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
