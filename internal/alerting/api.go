package alerting

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// API serves the Sentinel HTTP surface.
type API struct {
	Store    *Store
	Service  *Service
	Logger   *log.Logger
	AllowAll bool
	Now      func() time.Time
}

// Handler builds the Sentinel HTTP handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/v1/alerts", a.handleAlerts)
	mux.HandleFunc("/v1/alerts/", a.handleAlertRoute)
	return mux
}

func (a *API) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, partition := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.Can(CapAlertsRead, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	alerts, err := a.Store.ListAlerts(r.Context(), tenant, partition)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	severity := strings.TrimSpace(r.URL.Query().Get("severity"))
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	filtered := make([]Alert, 0, len(alerts))
	for _, alert := range alerts {
		if status != "" && alert.Status != status {
			continue
		}
		if severity != "" && alert.Severity != severity {
			continue
		}
		if source != "" && alert.Source != source {
			continue
		}
		filtered = append(filtered, alert)
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": filtered, "returned": len(filtered)})
}

func (a *API) handleAlertRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/alerts/")
	switch {
	case path == "intake/guardian":
		a.handleGuardianIntake(w, r)
	case path == "silences":
		a.handleSilences(w, r)
	case strings.HasPrefix(path, "silences/"):
		a.handleSilence(w, r, strings.TrimPrefix(path, "silences/"))
	case path == "capabilities":
		a.handleCapabilities(w, r)
	case path == "sources":
		a.handleSources(w, r)
	case path == "sources/aws/policy":
		a.handleAWSPolicy(w, r)
	case path == "sources/aws/excluded":
		a.handleExcluded(w, r)
	case path == "receivers":
		a.handleReceivers(w, r)
	case path == "delivery":
		a.handleDelivery(w, r)
	case path == "rules":
		a.handleRules(w, r)
	case strings.HasPrefix(path, "rules/"):
		a.handleRule(w, r, strings.TrimPrefix(path, "rules/"))
	default:
		a.handleAlertDetail(w, r, path)
	}
}

func (a *API) handleAlertDetail(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	tenant, partition := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	fingerprint := parts[0]
	if len(parts) == 2 && parts[1] == "ack" {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !identity.Can(CapAlertsAck, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		alert, err := a.Service.Ack(r.Context(), tenant, partition, fingerprint, identity.User, r.Method == http.MethodPost)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, alert)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !identity.Can(CapAlertsRead, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	alert, err := a.Store.GetAlert(r.Context(), tenant, partition, fingerprint)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, alert)
}

func (a *API) handleGuardianIntake(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.Admin {
		// Service-to-service callers use the configured HMAC, but a user
		// identity without admin must not spoof Guardian alerts.
		if identity.User != "local-dev" {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload GuardianAlert
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	tenant := strings.TrimSpace(r.Header.Get("X-Doctor-Tenant"))
	if tenant == "" {
		tenant = a.Service.DefaultTenant
	}
	alert, err := a.Service.IngestGuardian(r.Context(), tenant, payload, r.Header.Get("X-Guardian-Signature"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "alert": alert})
}

func (a *API) handleSilences(w http.ResponseWriter, r *http.Request) {
	tenant, partition := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	switch r.Method {
	case http.MethodGet:
		if !identity.Can(CapAlertsRead, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		silences, err := a.Store.ListSilences(r.Context(), tenant, partition)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"silences": silences})
	case http.MethodPost:
		if !identity.Can(CapAlertsSilence, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		var silence Silence
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&silence); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if silence.Tenant == "" {
			silence.Tenant = tenant
		}
		if silence.Partition == "" {
			silence.Partition = partition
		}
		if silence.Scope == "partition" && !identity.Can(CapAlertsSilencePartition, tenant, partition) {
			writeError(w, http.StatusForbidden, "partition-wide silence requires elevated permission")
			return
		}
		created, err := a.Service.CreateSilence(r.Context(), silence, identity.User)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, created)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleSilence(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 3 {
		writeError(w, http.StatusBadRequest, "path must be /v1/alerts/silences/{tenant}/{partition}/{id}")
		return
	}
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.Can(CapAlertsSilence, parts[0], parts[1]) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err := a.Store.DeleteSilence(r.Context(), parts[0], parts[1], parts[2]); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "success"})
}

func (a *API) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	tenant, partition := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant":       tenant,
		"partition":    partition,
		"user":         identity.User,
		"capabilities": identity.Capabilities(tenant, partition),
	})
}

func (a *API) handleSources(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sources": []map[string]any{
			{"name": "guardian", "mode": "push", "status": "configured"},
			{"name": "aws", "mode": "pull", "status": "configured"},
			{"name": "alertmanager", "mode": "push", "status": "optional"},
			{"name": "sentinel", "mode": "internal", "status": "configured"},
		},
	})
}

func (a *API) handleAWSPolicy(w http.ResponseWriter, r *http.Request) {
	identity := IdentityFromRequest(r, a.AllowAll)
	tenant, partition := tenantPartition(r)
	switch r.Method {
	case http.MethodGet:
		if !identity.Can(CapSourcesRead, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		policy, err := a.Store.GetAWSPolicy(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, policy)
	case http.MethodPut:
		if !identity.Can(CapSourcesWrite, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		var policy AWSPolicy
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&policy); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.Store.PutAWSPolicy(r.Context(), policy); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "policy": policy})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleExcluded(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"excluded": []any{}, "count": 0})
}

func (a *API) handleReceivers(w http.ResponseWriter, r *http.Request) {
	tenant, partition := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	switch r.Method {
	case http.MethodGet:
		if !identity.Can(CapReceiversRead, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		cfg, err := a.Store.GetReceiverConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPut:
		if !identity.Can(CapReceiversWrite, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		var cfg ReceiverConfig
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&cfg); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.Store.PutReceiverConfig(r.Context(), cfg); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success"})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleDelivery(w http.ResponseWriter, r *http.Request) {
	tenant, partition := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.Can(CapDeliveryRead, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	records, err := a.Store.ListOutboxAll(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	filtered := make([]OutboxRecord, 0, len(records))
	for _, record := range records {
		if tenant != "" && record.Tenant != tenant {
			continue
		}
		if partition != "" && record.Partition != partition {
			continue
		}
		filtered = append(filtered, record)
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivery": filtered, "returned": len(filtered)})
}

func (a *API) handleRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"groups": []any{}})
}

func (a *API) handleRule(w http.ResponseWriter, _ *http.Request, _ string) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "not implemented"})
}

func tenantPartition(r *http.Request) (string, string) {
	return strings.TrimSpace(r.URL.Query().Get("tenant")), strings.TrimSpace(r.URL.Query().Get("partition"))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"status": "error", "error": message})
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
