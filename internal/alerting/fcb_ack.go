package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const fcbAckMaxBody = 64 << 10

// IncidentAckFunc applies a page acknowledgement to an incident. It reports
// whether the incident was found. Sentinel wires this to the incident service
// so the alerting package does not have to depend on the incident package.
type IncidentAckFunc func(ctx context.Context, tenant, id, actor string, acked bool) (bool, error)

// FCBAckPayload is the doctor.android.ack/v1 callback emitted by
// notfify/fcm-bridge when a paged device acks or nacks a notification. The
// bridge only sends the fields it knows; tenant, partition, and kind are
// optional hints for multi-tenant deployments.
type FCBAckPayload struct {
	SchemaVersion string `json:"schemaVersion"`
	PageID        string `json:"pageId"`
	AlertID       string `json:"alertId"`
	Kind          string `json:"kind,omitempty"`
	Tenant        string `json:"tenant,omitempty"`
	Partition     string `json:"partition,omitempty"`
	Title         string `json:"title,omitempty"`
	Action        string `json:"action"`
	Status        string `json:"status,omitempty"`
	DeviceID      string `json:"deviceId,omitempty"`
	DeviceName    string `json:"deviceName,omitempty"`
	Owner         string `json:"owner,omitempty"`
	TargetID      string `json:"targetId,omitempty"`
	Note          string `json:"note,omitempty"`
	RespondedAt   string `json:"respondedAt,omitempty"`
}

// handleFCBAck receives a device ack/nack from the fcm-bridge. The bridge signs
// the exact body with X-Doctor-Signature. An "ack" acknowledges the matching
// alert or incident; a "nack" clears a previous acknowledgement so Doctor stays
// consistent with the page state after a responder changes their mind.
func (a *API) handleFCBAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(a.FCBSecret) == 0 && !a.AllowAll {
		writeError(w, http.StatusServiceUnavailable, "fcb ack callback is not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, fcbAckMaxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(a.FCBSecret) > 0 && !VerifyGuardianSignature(a.FCBSecret, body, r.Header.Get("X-Doctor-Signature")) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	var payload FCBAckPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	action := strings.ToLower(strings.TrimSpace(payload.Action))
	if action != "ack" && action != "nack" {
		writeError(w, http.StatusBadRequest, `action must be "ack" or "nack"`)
		return
	}
	alertID := strings.TrimSpace(payload.AlertID)
	if alertID == "" {
		writeError(w, http.StatusBadRequest, "alertId is required")
		return
	}

	tenant := strings.TrimSpace(payload.Tenant)
	if tenant == "" {
		tenant = a.Service.DefaultTenant
	}
	if tenant == "" {
		tenant = "default"
	}
	actor := firstNonEmpty(
		strings.TrimSpace(payload.Owner),
		strings.TrimSpace(payload.DeviceName),
		strings.TrimSpace(payload.DeviceID),
		"fcb",
	)
	kind := strings.ToLower(strings.TrimSpace(payload.Kind))
	acked := action == "ack"

	if kind != EventKindIncident {
		alert, err := a.findAlertForAck(r.Context(), tenant, strings.TrimSpace(payload.Partition), alertID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if alert != nil {
			updated, err := a.Service.Ack(r.Context(), alert.Tenant, alert.Partition, alert.Fingerprint, actor, acked)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status": "accepted",
				"kind":   EventKindAlert,
				"id":     updated.Fingerprint,
				"pageId": payload.PageID,
				"acked":  updated.Acked,
				"actor":  actor,
			})
			return
		}
	}

	if a.AckIncident != nil && kind != EventKindAlert {
		found, err := a.AckIncident(r.Context(), tenant, alertID, actor, acked)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if found {
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status": "accepted",
				"kind":   EventKindIncident,
				"id":     alertID,
				"pageId": payload.PageID,
				"acked":  acked,
				"actor":  actor,
			})
			return
		}
	}

	writeError(w, http.StatusNotFound, "alert not found")
}

// findAlertForAck locates the alert behind a page by fingerprint or ID. When
// the callback carries no partition, the tenant is scanned once. Ack callbacks
// are low-volume, so this avoids storing a second lookup index.
func (a *API) findAlertForAck(ctx context.Context, tenant, partition, id string) (*Alert, error) {
	if partition != "" {
		alert, err := a.Store.GetAlert(ctx, tenant, partition, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, nil
			}
			return nil, err
		}
		return alert, nil
	}
	alerts, err := a.Store.ListTenantAlerts(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, alert := range alerts {
		if alert.Fingerprint == id || alert.ID == id {
			found := alert
			return &found, nil
		}
	}
	return nil, nil
}
