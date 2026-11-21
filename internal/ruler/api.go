package ruler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/radryc/doctor/internal/store"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"sigs.k8s.io/yaml"
)

// LimitsProvider exposes the partition rule quota. A nil provider disables
// rule-count enforcement.
type LimitsProvider interface {
	RulesQuota(partition string) policy.RulesQuota
}

// Authorizer gates rule mutations. A nil authorizer allows everything
// (observe-first rollout).
type Authorizer interface {
	CanModify(ctx context.Context, partition, group string) bool
}

// ReadAuthorizer optionally filters reads. Authorizers that do not implement
// it keep the pre-authorization behavior (any authenticated caller can list
// and read rules).
type ReadAuthorizer interface {
	CanRead(ctx context.Context, partition string) bool
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

// CanRead implements ReadAuthorizer.
func (AuthenticatedAuthorizer) CanRead(ctx context.Context, _ string) bool {
	id, ok := authz.IdentityFromContext(ctx)
	return ok && !id.IsAnonymous()
}

// API serves the rule CRUD and manual evaluation endpoints.
type API struct {
	Store      *Store
	Evaluator  *Evaluator
	Authorizer Authorizer
	Limits     LimitsProvider
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
	mux.HandleFunc("/v1/rules/backfill", a.handleBackfill)
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
		Partition string `json:"partition"`
		Group     string `json:"group"`
		Interval  string `json:"interval"`
		Rules     int    `json:"rules"`
	}
	out := make([]groupSummary, 0, len(refs))
	for _, ref := range refs {
		if !a.canRead(r.Context(), ref.Partition) {
			continue
		}
		interval := ""
		rules := 0
		if ref.File != nil && len(ref.File.Groups) > 0 {
			interval = ref.File.Groups[0].IntervalOrDefault().String()
			rules = len(ref.File.Groups[0].Rules)
		}
		out = append(out, groupSummary{Partition: ref.Partition, Group: ref.Group, Interval: interval, Rules: rules})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

func (a *API) handleGroup(w http.ResponseWriter, r *http.Request) {
	partition, group, ok := parseGroupPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "path must be /v1/rules/{partition}/{group}")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !a.canRead(r.Context(), partition) {
			writeError(w, http.StatusForbidden, "not authorized to read this rule group")
			return
		}
		file, err := a.Store.Get(r.Context(), partition, group)
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
		if !a.canModify(r.Context(), partition, group) {
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
			file.Groups[i].Partition = partition
		}
		if err := a.checkLimits(r.Context(), partition, group, file); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if err := a.Store.Put(r.Context(), partition, group, file, false); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "partition": partition, "group": group})
	case http.MethodDelete:
		if !a.canModify(r.Context(), partition, group) {
			writeError(w, http.StatusForbidden, "not authorized to modify this rule group")
			return
		}
		if err := a.Store.Delete(r.Context(), partition, group); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "partition": partition, "group": group})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	partition := strings.TrimSpace(r.URL.Query().Get("partition"))
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	if partition == "" || group == "" {
		writeError(w, http.StatusBadRequest, "partition and group are required")
		return
	}
	if !a.canModify(r.Context(), partition, group) {
		writeError(w, http.StatusForbidden, "not authorized to evaluate this rule group")
		return
	}
	file, err := a.Store.Get(r.Context(), partition, group)
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

// handleBackfill replays a rule group over an explicit time range. The
// evaluator uses deterministic chunk IDs, so replaying a tick already written
// does not duplicate samples.
func (a *API) handleBackfill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	partition := strings.TrimSpace(r.URL.Query().Get("partition"))
	groupName := strings.TrimSpace(r.URL.Query().Get("group"))
	if partition == "" || groupName == "" {
		writeError(w, http.StatusBadRequest, "partition and group are required")
		return
	}
	if !a.canModify(r.Context(), partition, groupName) {
		writeError(w, http.StatusForbidden, "not authorized to backfill this rule group")
		return
	}
	from, err := time.Parse(time.RFC3339, strings.TrimSpace(r.URL.Query().Get("from")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "from must be RFC3339")
		return
	}
	to, err := time.Parse(time.RFC3339, strings.TrimSpace(r.URL.Query().Get("to")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "to must be RFC3339")
		return
	}
	if !to.After(from) {
		writeError(w, http.StatusBadRequest, "to must be after from")
		return
	}

	file, err := a.Store.Get(r.Context(), partition, groupName)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var group *Group
	for i := range file.Groups {
		if file.Groups[i].Name == groupName {
			group = &file.Groups[i]
			break
		}
	}
	if group == nil {
		writeError(w, http.StatusNotFound, "rule group not found")
		return
	}

	step := group.IntervalOrDefault()
	if raw := strings.TrimSpace(r.URL.Query().Get("step")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 5*time.Second {
			writeError(w, http.StatusBadRequest, "step must be a duration of at least 5s")
			return
		}
		step = parsed
	}
	const maxBackfillTicks = 10000
	ticks := int(to.Sub(from)/step) + 1
	if ticks > maxBackfillTicks {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
			"backfill would evaluate %d ticks; maximum is %d (increase step or narrow the range)", ticks, maxBackfillTicks))
		return
	}

	results := make([]EvalResult, 0, ticks*len(group.Rules))
	for tick := from; !tick.After(to); tick = tick.Add(step) {
		results = append(results, a.Evaluator.EvaluateGroupAt(r.Context(), *group, tick)...)
		if r.Context().Err() != nil {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"partition": partition,
		"group":     groupName,
		"step":      step.String(),
		"ticks":     ticks,
		"results":   results,
	})
}

func (a *API) canModify(ctx context.Context, partition, group string) bool {
	if a.Authorizer == nil {
		return true
	}
	return a.Authorizer.CanModify(ctx, partition, group)
}

func (a *API) canRead(ctx context.Context, partition string) bool {
	if a.Authorizer == nil {
		return true
	}
	reader, ok := a.Authorizer.(ReadAuthorizer)
	if !ok {
		return true
	}
	return reader.CanRead(ctx, partition)
}

// checkLimits enforces the partition's group/rule envelope before a write.
func (a *API) checkLimits(ctx context.Context, partition, group string, file *File) error {
	if a.Limits == nil {
		return nil
	}
	limits := a.Limits.RulesQuota(partition)
	if limits.MaxGroups == 0 && limits.MaxRules == 0 && limits.MaxRulesPerGroup == 0 {
		return nil
	}
	newRules := countRules(file)
	if limits.MaxRulesPerGroup > 0 && newRules > limits.MaxRulesPerGroup {
		return fmt.Errorf("partition %s rule group %s has %d rules; limit %d", partition, group, newRules, limits.MaxRulesPerGroup)
	}
	groups, rules, err := a.Store.Count(ctx, partition)
	if err != nil {
		return err
	}
	_, getErr := a.Store.Get(ctx, partition, group)
	exists := getErr == nil
	if !exists {
		groups++
	} else {
		existingRules, err := a.Store.CountGroup(ctx, partition, group)
		if err != nil {
			return err
		}
		rules -= existingRules
	}
	if limits.MaxGroups > 0 && groups > limits.MaxGroups {
		return fmt.Errorf("partition %s has %d rule groups; limit %d", partition, groups, limits.MaxGroups)
	}
	if limits.MaxRules > 0 && rules+newRules > limits.MaxRules {
		return fmt.Errorf("partition %s would have %d rules; limit %d", partition, rules+newRules, limits.MaxRules)
	}
	return nil
}

func parseGroupPath(path string) (partition, group string, ok bool) {
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
