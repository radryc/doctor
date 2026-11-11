package alerting

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// API serves the Sentinel HTTP surface.
type API struct {
	Store        *Store
	Service      *Service
	Logger       *log.Logger
	AllowAll     bool
	ServiceToken string
	Now          func() time.Time
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
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("scope")), "subspace") {
		alerts, err := a.Service.ListSubspace(r.Context(), tenant, identity)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts, "returned": len(alerts)})
		return
	}
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
	case strings.HasPrefix(path, "intake/"):
		source := strings.TrimPrefix(path, "intake/")
		if strings.HasPrefix(source, "preview/") {
			a.handleGenericPreview(w, r, strings.TrimPrefix(source, "preview/"))
			return
		}
		a.handleGenericIntake(w, r, source)
	case path == "subspace":
		a.handleSubspace(w, r)
	case path == "subspace/preview":
		a.handleSubspacePreview(w, r)
	case strings.HasPrefix(path, "subspace/"):
		a.handleSubspaceResolve(w, r, strings.TrimPrefix(path, "subspace/"))
	case path == "silences":
		a.handleSilences(w, r)
	case strings.HasPrefix(path, "silences/"):
		a.handleSilence(w, r, strings.TrimPrefix(path, "silences/"))
	case path == "capabilities":
		a.handleCapabilities(w, r)
	case path == "summary":
		a.handleAlertSummary(w, r)
	case path == "sources":
		a.handleSources(w, r)
	case path == "sources/aws/policy":
		a.handleAWSPolicy(w, r)
	case path == "sources/gcp/policy":
		a.handleGCPPolicy(w, r)
	case path == "sources/guardian/policy":
		a.handleGuardianPolicy(w, r)
	case path == "sources/aws/excluded":
		a.handleExcluded(w, r)
	case path == "receivers":
		a.handleReceivers(w, r)
	case path == "routing/overview":
		a.handleRoutingOverview(w, r)
	case path == "delivery":
		a.handleDelivery(w, r)
	case path == "rules":
		a.handleRules(w, r)
	case path == "rules/preview":
		a.handleRulePreview(w, r)
	case path == "rules/compile":
		a.handleRuleCompile(w, r)
	case path == "rules/test":
		a.handleRuleTest(w, r)
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
	if !identity.Admin && len(a.Service.GuardianHMACKey) == 0 && !a.AllowAll {
		// Service-to-service callers authenticate with the HMAC when one is
		// configured. Without HMAC, only a local/admin caller may inject.
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	signature := r.Header.Get("X-Guardian-Signature")
	if !VerifyGuardianSignature(a.Service.GuardianHMACKey, body, signature) {
		writeError(w, http.StatusUnauthorized, "invalid Guardian signature")
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
	alert, err := a.Service.IngestGuardian(r.Context(), tenant, payload, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "alert": alert})
}

func (a *API) handleSubspace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity := IdentityFromRequest(r, a.AllowAll)
	tenant, _ := tenantPartition(r)
	if !identity.Can(CapSubspaceWrite, tenant, "") && !a.isServiceToken(r) {
		writeError(w, http.StatusForbidden, "subspace broadcast requires an admin or service token")
		return
	}
	var req SubspaceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	alert, err := a.Service.UpsertSubspace(r.Context(), req, tenant, identity.User)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "alert": alert})
}

func (a *API) handleSubspacePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req SubspaceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"audienceType": req.Audience.Type,
		"partitions":   req.Audience.Partitions,
		"groups":       req.Audience.Groups,
		"channels":     "configured receivers",
		"note":         "preview counts are finalized by the audience resolver",
	})
}

func (a *API) handleSubspaceResolve(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	dedupeKey := strings.TrimSuffix(strings.Trim(rest, "/"), "/resolve")
	identity := IdentityFromRequest(r, a.AllowAll)
	tenant, _ := tenantPartition(r)
	if !identity.Can(CapSubspaceWrite, tenant, "") && !a.isServiceToken(r) {
		writeError(w, http.StatusForbidden, "subspace resolve requires an admin or service token")
		return
	}
	alert, err := a.Service.ResolveSubspace(r.Context(), tenant, dedupeKey)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, alert)
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

func (a *API) handleAlertSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, _ := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	summaries, err := a.Store.ListAlertSummaries(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	filtered := make([]PartitionAlertSummary, 0, len(summaries))
	for _, summary := range summaries {
		if !identity.Can(CapAlertsRead, summary.Tenant, summary.Partition) {
			continue
		}
		filtered = append(filtered, summary)
	}
	writeJSON(w, http.StatusOK, map[string]any{"partitions": filtered})
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
			{"name": "guardian", "mode": "push/pull", "status": "configured"},
			{"name": "aws", "mode": "pull", "status": "configured"},
			{"name": "gcp", "mode": "pull", "status": "configured"},
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

func (a *API) handleGCPPolicy(w http.ResponseWriter, r *http.Request) {
	identity := IdentityFromRequest(r, a.AllowAll)
	tenant, partition := tenantPartition(r)
	switch r.Method {
	case http.MethodGet:
		if !identity.Can(CapSourcesRead, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		policy, err := a.Store.GetGCPPolicy(r.Context())
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
		var policy GCPPolicy
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&policy); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.Store.PutGCPPolicy(r.Context(), policy); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "policy": policy})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleGuardianPolicy(w http.ResponseWriter, r *http.Request) {
	identity := IdentityFromRequest(r, a.AllowAll)
	tenant, partition := tenantPartition(r)
	if tenant == "" && a.Service != nil {
		tenant = strings.TrimSpace(a.Service.DefaultTenant)
	}
	if tenant == "" {
		tenant = "default"
	}
	if partition == "" {
		writeError(w, http.StatusBadRequest, "partition is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !identity.Can(CapSourcesRead, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		policy, err := a.Store.GetGuardianPolicy(r.Context(), tenant, partition)
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
		var policy GuardianPolicy
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&policy); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.Store.PutGuardianPolicy(r.Context(), tenant, partition, policy); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		policy.Tenant = tenant
		policy.Partition = partition
		if policy.Version == 0 {
			policy.Version = 1
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
	if partition == "" {
		// Legacy, tenant-wide view. New clients always send a partition.
		switch r.Method {
		case http.MethodGet:
			if !identity.Can(CapReceiversRead, tenant, "") && !identity.AllowAll && !identity.Admin {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
			cfg, err := a.Store.GetReceiverConfig(r.Context())
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if cfg.Receivers == nil {
				cfg.Receivers = []Receiver{}
			}
			if cfg.Routes == nil {
				cfg.Routes = []Route{}
			}
			writeJSON(w, http.StatusOK, cfg)
			return
		case http.MethodPut:
			if !identity.Admin && !identity.AllowAll {
				writeError(w, http.StatusForbidden, "tenant-wide receiver config requires an admin")
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
			return
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		if !identity.Can(CapReceiversRead, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		cfg, err := a.Store.GetPartitionRouting(r.Context(), tenant, partition)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if cfg.Receivers == nil {
			cfg.Receivers = []Receiver{}
		}
		if cfg.Routes == nil {
			cfg.Routes = []Route{}
		}
		if cfg.AlertRoutes == nil {
			cfg.AlertRoutes = []AlertRoute{}
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPut:
		if !identity.Can(CapReceiversWrite, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		var cfg PartitionRouting
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&cfg); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.Store.PutPartitionRouting(r.Context(), tenant, partition, cfg); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// A route may be added while an alert is already firing (Guardian
		// partition alerts are a common example). Deliver those alerts now
		// rather than waiting for the next state transition.
		if a.Service != nil {
			if _, err := a.Service.ReconcileRouting(r.Context(), tenant, partition); err != nil && a.Logger != nil {
				a.Logger.Printf("sentinel: reconcile routing for %s/%s: %v", tenant, partition, err)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success"})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleRoutingOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, _ := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.AllowAll && !identity.Admin {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	configs, err := a.Store.ListPartitionRouting(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type partitionSummary struct {
		Tenant            string   `json:"tenant"`
		Partition         string   `json:"partition"`
		Receivers         int      `json:"receivers"`
		Routes            int      `json:"routes"`
		PartitionReceiver string   `json:"partitionReceiver,omitempty"`
		DefaultReceiver   string   `json:"defaultReceiver,omitempty"`
		ReceiverNames     []string `json:"receiverNames"`
	}
	out := make([]partitionSummary, 0, len(configs))
	for _, cfg := range configs {
		names := make([]string, 0, len(cfg.Receivers))
		for _, receiver := range cfg.Receivers {
			names = append(names, receiver.Name)
		}
		sort.Strings(names)
		out = append(out, partitionSummary{
			Tenant:            cfg.Tenant,
			Partition:         cfg.Partition,
			Receivers:         len(cfg.Receivers),
			Routes:            len(cfg.Routes),
			PartitionReceiver: cfg.PartitionReceiver,
			DefaultReceiver:   cfg.DefaultReceiver,
			ReceiverNames:     names,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"partitions": out})
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

func (a *API) handleRules(w http.ResponseWriter, r *http.Request) {
	tenant, partition := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.Can(CapRulesRead, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	refs, err := a.Store.ListRuleGroups(r.Context(), tenant)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type ruleSummary struct {
		Alert    string   `json:"alert"`
		Mode     string   `json:"mode,omitempty"`
		Signal   string   `json:"signal,omitempty"`
		Severity string   `json:"severity,omitempty"`
		Notify   []string `json:"notify,omitempty"`
		Summary  string   `json:"summary,omitempty"`
	}
	type summary struct {
		Tenant    string        `json:"tenant"`
		Partition string        `json:"partition"`
		Group     string        `json:"group"`
		Interval  string        `json:"interval"`
		Rules     int           `json:"rules"`
		Alerts    []ruleSummary `json:"alerts"`
	}
	out := make([]summary, 0, len(refs))
	for _, ref := range refs {
		if partition != "" && ref.Partition != partition {
			continue
		}
		interval := ""
		rules := 0
		alerts := make([]ruleSummary, 0)
		if ref.File != nil && len(ref.File.Groups) > 0 {
			group := ref.File.Groups[0]
			interval = formatDuration(group.IntervalOrDefault())
			rules = len(group.Rules)
			for _, rule := range group.Rules {
				alerts = append(alerts, ruleSummary{
					Alert:    rule.Alert,
					Mode:     rule.ModeOrDefault(),
					Signal:   rule.Annotations["sentinel.signal"],
					Severity: rule.Labels["severity"],
					Notify:   append([]string(nil), rule.Notify...),
					Summary:  rule.Annotations["summary"],
				})
			}
		}
		out = append(out, summary{Tenant: ref.Tenant, Partition: ref.Partition, Group: ref.Group, Interval: interval, Rules: rules, Alerts: alerts})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

func (a *API) handleRule(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 3 {
		writeError(w, http.StatusBadRequest, "path must be /v1/alerts/rules/{tenant}/{partition}/{group}")
		return
	}
	tenant, partition, group := parts[0], parts[1], parts[2]
	identity := IdentityFromRequest(r, a.AllowAll)
	switch r.Method {
	case http.MethodGet:
		if !identity.Can(CapRulesRead, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		file, err := a.Store.GetRuleGroup(r.Context(), tenant, partition, group)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json") {
			writeJSON(w, http.StatusOK, file)
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
		if !identity.Can(CapRulesWrite, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		file, err := ParseRuleFile(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.Store.PutRuleGroup(r.Context(), tenant, partition, group, file); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "tenant": tenant, "partition": partition, "group": group})
	case http.MethodDelete:
		if !identity.Can(CapRulesWrite, tenant, partition) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if err := a.Store.DeleteRuleGroup(r.Context(), tenant, partition, group); err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success"})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleRuleCompile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, partition := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.Can(CapRulesRead, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var req struct {
		Spec *AlertSpec `json:"spec"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Spec == nil {
		writeError(w, http.StatusBadRequest, "spec is required")
		return
	}
	expr, err := CompileSpec(req.Spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expr": expr})
}

func (a *API) handleRulePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, partition := tenantPartition(r)
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.Can(CapRulesRead, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if a.Service.Querier == nil {
		writeError(w, http.StatusServiceUnavailable, "query backend is not configured")
		return
	}
	var req struct {
		Spec *AlertSpec `json:"spec"`
		Expr string     `json:"expr"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	expr := strings.TrimSpace(req.Expr)
	if req.Spec != nil {
		compiled, err := CompileSpec(req.Spec)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		expr = compiled
	}
	if expr == "" {
		writeError(w, http.StatusBadRequest, "spec or expr is required")
		return
	}
	data, partial, err := a.Service.Querier.PromInstant(r.Context(), expr, a.now())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	instances := make([]map[string]any, 0)
	for _, item := range prometheusResultSlice(data) {
		mapItem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		labels := map[string]string{}
		if metric, ok := mapItem["metric"].(map[string]any); ok {
			for key, value := range metric {
				if str, ok := value.(string); ok {
					labels[key] = str
				}
			}
		}
		value, _, valueErr := promSampleValue(mapItem)
		instance := map[string]any{"labels": labels}
		if valueErr == nil {
			instance["value"] = formatNumber(value)
		}
		instances = append(instances, instance)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"expr":      expr,
		"data":      data,
		"partial":   partial,
		"instances": instances,
		"wouldFire": len(instances) > 0,
	})
}

func (a *API) handleRuleTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenant, partition := tenantPartition(r)
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.Can(CapRulesRead, tenant, partition) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	file, err := a.Store.GetRuleGroup(r.Context(), tenant, partition, group)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	results := make([]RuleEvalResult, 0)
	for _, spec := range file.Groups {
		results = append(results, a.Service.EvaluateRuleGroup(r.Context(), spec)...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *API) isServiceToken(r *http.Request) bool {
	if strings.TrimSpace(a.ServiceToken) == "" {
		return false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	return strings.EqualFold(auth, "Bearer "+a.ServiceToken)
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
