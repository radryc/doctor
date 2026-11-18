package incident

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/radryc/doctor/internal/alerting"
)

// API serves the incident, alert-set, policy, and postmortem surface.
type API struct {
	Store    *Store
	Service  *Service
	AllowAll bool
	Logger   *log.Logger
}

// Handler builds the incident HTTP handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/incidents", a.handleIncidents)
	mux.HandleFunc("/v1/incidents/", a.handleIncidentRoute)
	mux.HandleFunc("/v1/alert-sets", a.handleAlertSets)
	mux.HandleFunc("/v1/alert-sets/", a.handleAlertSetRoute)
	mux.HandleFunc("/v1/incident-policy", a.handlePolicy)
	mux.HandleFunc("/v1/postmortem-engines", a.handleEngines)
	return mux
}

func (a *API) identity(r *http.Request) alerting.Identity {
	return alerting.IdentityFromRequest(r, a.AllowAll)
}

func tenantPartition(r *http.Request) (string, string) {
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	if tenant == "" {
		tenant = strings.TrimSpace(r.Header.Get("X-Doctor-Tenant"))
	}
	if tenant == "" {
		tenant = "default"
	}
	return tenant, strings.TrimSpace(r.URL.Query().Get("partition"))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (a *API) canRead(identity alerting.Identity, tenant, partition string) bool {
	return identity.Can(alerting.CapAlertsRead, tenant, partition)
}

func (a *API) canAck(identity alerting.Identity, tenant, partition string) bool {
	return identity.Can(alerting.CapAlertsAck, tenant, partition)
}

func (a *API) canManage(identity alerting.Identity, tenant, partition string) bool {
	return identity.Can(alerting.CapRulesWrite, tenant, partition)
}

func (a *API) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, partition := tenantPartition(r)
	identity := a.identity(r)
	if partition != "" && !a.canRead(identity, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var incidents []Incident
	var err error
	if partition != "" {
		incidents, err = a.Store.ListIncidents(r.Context(), tenant, partition)
	} else {
		incidents, err = a.Store.ListTenantIncidents(r.Context(), tenant)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	set := strings.TrimSpace(r.URL.Query().Get("set"))
	severity := strings.TrimSpace(r.URL.Query().Get("severity"))
	filtered := make([]Incident, 0, len(incidents))
	for _, incident := range incidents {
		if partition == "" && !a.canRead(identity, tenant, incident.Partition) {
			continue
		}
		if state != "" && incident.State != state {
			continue
		}
		if set != "" && incident.Set != set {
			continue
		}
		if severity != "" && incident.Severity != severity {
			continue
		}
		filtered = append(filtered, incident)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].OpenedAt.After(filtered[j].OpenedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"incidents": filtered, "returned": len(filtered)})
}

func (a *API) handleIncidentRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/incidents/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	tenant, partition := tenantPartition(r)
	identity := a.identity(r)
	id := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !a.canRead(identity, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		incident, _, err := a.Store.GetIncident(r.Context(), tenant, partition, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if incident == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, incident)
		return
	}
	action := parts[1]
	if action == "postmortem" {
		a.handlePostmortem(w, r, tenant, partition, id, identity, parts[2:])
		return
	}
	if !a.canAck(identity, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch {
	case action == "ack" && r.Method == http.MethodPost:
		incident, err := a.Service.Ack(r.Context(), tenant, partition, id, identity.User)
		a.writeIncidentResult(w, incident, err)
	case action == "unack" && r.Method == http.MethodPost:
		incident, err := a.Service.Unack(r.Context(), tenant, partition, id, identity.User)
		a.writeIncidentResult(w, incident, err)
	case action == "mitigate" && r.Method == http.MethodPost:
		var body struct {
			Note     string `json:"note"`
			Override bool   `json:"override"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		incident, err := a.Service.Mitigate(r.Context(), tenant, partition, id, identity.User, body.Note, body.Override)
		if errors.Is(err, ErrAlertsFiring) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		a.writeIncidentResult(w, incident, err)
	case action == "resolve" && r.Method == http.MethodPost:
		var body struct {
			Resolution string `json:"resolution"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		incident, err := a.Service.Resolve(r.Context(), tenant, partition, id, identity.User, body.Resolution)
		if errors.Is(err, ErrResolveFromOpen) || errors.Is(err, ErrInvalidTransition) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		a.writeIncidentResult(w, incident, err)
	case action == "reopen" && r.Method == http.MethodPost:
		incident, err := a.Service.Reopen(r.Context(), tenant, partition, id, identity.User)
		a.writeIncidentResult(w, incident, err)
	case action == "notes" && r.Method == http.MethodPost:
		var body struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
		incident, err := a.Service.Note(r.Context(), tenant, partition, id, identity.User, body.Message)
		a.writeIncidentResult(w, incident, err)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (a *API) writeIncidentResult(w http.ResponseWriter, incident *Incident, err error) {
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrResolveFromOpen):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, incident)
}

func (a *API) handlePostmortem(w http.ResponseWriter, r *http.Request, tenant, partition, id string, identity alerting.Identity, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			if !a.canRead(identity, tenant, partition) {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
			doc, err := a.Service.Postmortem(r.Context(), tenant, partition, id)
			if errors.Is(err, ErrNotFound) {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, doc)
		case http.MethodPut:
			if !a.canAck(identity, tenant, partition) {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
			var body struct {
				Markdown string `json:"markdown"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			doc, err := a.Service.UpdatePostmortemDraft(r.Context(), tenant, partition, id, body.Markdown)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, doc)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if !a.canAck(identity, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch rest[0] {
	case "analyze":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if err := a.Service.AnalyzeNow(r.Context(), tenant, partition, id); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		doc, err := a.Service.Postmortem(r.Context(), tenant, partition, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, doc)
	case "publish":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		doc, err := a.Service.Publish(r.Context(), tenant, partition, id, identity.User)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, doc)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (a *API) handleAlertSets(w http.ResponseWriter, r *http.Request) {
	tenant, partition := tenantPartition(r)
	identity := a.identity(r)
	switch r.Method {
	case http.MethodGet:
		sets, err := a.Store.ListAlertSets(r.Context(), tenant)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]AlertSet, 0, len(sets))
		for _, set := range sets {
			if !a.canRead(identity, tenant, set.Partition) {
				continue
			}
			if partition != "" && set.Partition != partition {
				continue
			}
			out = append(out, set)
		}
		writeJSON(w, http.StatusOK, map[string]any{"sets": out, "returned": len(out)})
	case http.MethodPost:
		if !a.canManage(identity, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		var set AlertSet
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&set); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		set.Tenant = tenant
		set.Partition = partition
		if err := set.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, version, _ := a.Store.GetAlertSet(r.Context(), tenant, partition, set.Name); version != "absent" {
			writeError(w, http.StatusConflict, "alert set already exists")
			return
		}
		if err := a.Store.PutAlertSet(r.Context(), set, "absent"); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, set)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleAlertSetRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/alert-sets/")
	if rest == "preview" {
		a.handlePreview(w, r)
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	tenant, partition := tenantPartition(r)
	identity := a.identity(r)
	name := parts[0]
	switch r.Method {
	case http.MethodGet:
		if !a.canRead(identity, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		set, _, err := a.Store.GetAlertSet(r.Context(), tenant, partition, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if set == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, set)
	case http.MethodPut:
		if !a.canManage(identity, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		existing, version, err := a.Store.GetAlertSet(r.Context(), tenant, partition, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		var set AlertSet
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&set); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		set.Tenant = tenant
		set.Partition = partition
		set.Name = name
		set.CreatedAt = existing.CreatedAt
		if err := set.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.Store.PutAlertSet(r.Context(), set, version); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, set)
	case http.MethodDelete:
		if !a.canManage(identity, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if err := a.Store.DeleteAlertSet(r.Context(), tenant, partition, name); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, partition := tenantPartition(r)
	identity := a.identity(r)
	if !a.canRead(identity, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var set AlertSet
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&set); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	set.Tenant = tenant
	set.Partition = partition
	if err := set.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	preview, err := a.Service.Preview(r.Context(), set)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (a *API) handlePolicy(w http.ResponseWriter, r *http.Request) {
	tenant, partition := tenantPartition(r)
	identity := a.identity(r)
	if partition == "" {
		writeError(w, http.StatusBadRequest, "partition is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !a.canRead(identity, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		policy, err := a.Store.GetPolicy(r.Context(), tenant, partition)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, policy)
	case http.MethodPut:
		if !a.canManage(identity, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		var policy Policy
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&policy); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		policy.Normalize()
		if err := a.Store.PutPolicy(r.Context(), tenant, partition, policy); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, policy)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleEngines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"engines": a.Service.Engines.List()})
}
