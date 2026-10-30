package alerting

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GenericAlert is an Alertmanager-compatible intake alert.
type GenericAlert struct {
	Status       string            `json:"status,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	StartsAt     time.Time         `json:"startsAt,omitempty"`
	EndsAt       time.Time         `json:"endsAt,omitempty"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
	Fingerprint  string            `json:"fingerprint,omitempty"`
	Tenant       string            `json:"tenant,omitempty"`
	Partition    string            `json:"partition,omitempty"`
	Severity     string            `json:"severity,omitempty"`
	Delivery     string            `json:"delivery,omitempty"`
}

type genericEnvelope struct {
	Alerts []GenericAlert `json:"alerts"`
}

func (a *API) handleGenericIntake(w http.ResponseWriter, r *http.Request, source string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity := IdentityFromRequest(r, a.AllowAll)
	if !identity.Admin && !a.AllowAll && !a.isServiceToken(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	alerts, err := decodeGenericAlerts(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tenant, partition := tenantPartition(r)
	if tenant == "" {
		tenant = a.Service.DefaultTenant
	}
	if tenant == "" {
		tenant = "default"
	}
	accepted := 0
	for _, item := range alerts {
		alert, ok := a.genericAlert(source, tenant, partition, item)
		if !ok {
			continue
		}
		if _, err := a.Service.Apply(r.Context(), alert); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		accepted++
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "accepted": accepted})
}

func (a *API) handleGenericPreview(w http.ResponseWriter, r *http.Request, source string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	alerts, err := decodeGenericAlerts(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tenant, partition := tenantPartition(r)
	if tenant == "" {
		tenant = a.Service.DefaultTenant
	}
	out := make([]Alert, 0, len(alerts))
	for _, item := range alerts {
		alert, ok := a.genericAlert(source, tenant, partition, item)
		if ok {
			out = append(out, alert)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": out, "count": len(out)})
}

func decodeGenericAlerts(body []byte) ([]GenericAlert, error) {
	var envelope genericEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Alerts) > 0 {
		return envelope.Alerts, nil
	}
	var single GenericAlert
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, err
	}
	if single.Labels == nil && single.Annotations == nil && single.Status == "" {
		return nil, fmt.Errorf("alerting: intake payload has no alerts")
	}
	return []GenericAlert{single}, nil
}

func (a *API) genericAlert(source, tenant, partition string, item GenericAlert) (Alert, bool) {
	if item.Tenant != "" {
		tenant = item.Tenant
	}
	if item.Partition != "" {
		partition = item.Partition
	}
	if partition == "" {
		partition = firstNonEmpty(item.Labels["partition"], item.Labels["guardian_partition"])
	}
	if partition == "" || tenant == "" {
		return Alert{}, false
	}
	status := StatusFiring
	switch strings.ToLower(strings.TrimSpace(item.Status)) {
	case "resolved":
		status = StatusResolved
	case "pending":
		status = StatusPending
	case "no_data":
		status = StatusNoData
	case "error":
		status = StatusError
	}
	labels := map[string]string{}
	for key, value := range item.Labels {
		labels[key] = value
	}
	labels["source"] = source
	labels["partition"] = partition
	if labels["alertname"] == "" {
		labels["alertname"] = firstNonEmpty(item.Fingerprint, "Alert")
	}
	severity := firstNonEmpty(item.Severity, labels["severity"], "warning")
	annotations := map[string]string{}
	for key, value := range item.Annotations {
		annotations[key] = value
	}
	if annotations["summary"] == "" {
		annotations["summary"] = labels["alertname"]
	}
	startsAt := item.StartsAt
	if startsAt.IsZero() {
		startsAt = a.now()
	}
	alert := Alert{
		Fingerprint: Fingerprint(source, tenant, partition, labels),
		Source:      source,
		Tenant:      tenant,
		Partition:   partition,
		Status:      status,
		Severity:    severity,
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    startsAt,
		Delivery:    firstNonEmpty(item.Delivery, DeliverySentinel),
	}
	alert.ID = alert.Fingerprint
	if status == StatusResolved {
		if item.EndsAt.IsZero() {
			now := a.now()
			alert.EndsAt = &now
		} else {
			alert.EndsAt = &item.EndsAt
		}
	}
	return alert, true
}
