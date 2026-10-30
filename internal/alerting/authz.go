package alerting

import (
	"net/http"
	"strings"
)

// Capability is one permission checked by the Sentinel API.
type Capability string

const (
	CapAlertsRead             Capability = "alerts.read"
	CapAlertsAck              Capability = "alerts.ack"
	CapAlertsSilence          Capability = "alerts.silence"
	CapAlertsSilencePartition Capability = "alerts.silence.partition"
	CapRulesRead              Capability = "alerts.rules.read"
	CapRulesWrite             Capability = "alerts.rules.write"
	CapSourcesRead            Capability = "alerts.sources.read"
	CapSourcesWrite           Capability = "alerts.sources.write"
	CapReceiversRead          Capability = "alerts.receivers.read"
	CapReceiversWrite         Capability = "alerts.receivers.write"
	CapDeliveryRead           Capability = "alerts.delivery.read"
	CapDeliveryRetry          Capability = "alerts.delivery.retry"
	CapSubspaceRead           Capability = "alerts.subspace.read"
	CapSubspaceWrite          Capability = "alerts.subspace.write"
	CapSubspaceSilence        Capability = "alerts.subspace.silence"
)

// Identity is the authenticated caller. doctor-query forwards these values
// after verifying OIDC; direct service callers use a service token.
type Identity struct {
	User       string
	Groups     []string
	Admin      bool
	AllowAll   bool
	Partitions map[string]bool
}

// IdentityFromRequest reads the trusted identity headers. Direct anonymous
// access is allowed only when allowAll is true (local development).
func IdentityFromRequest(r *http.Request, allowAll bool) Identity {
	identity := Identity{
		User:       strings.TrimSpace(r.Header.Get("X-Doctor-User")),
		Admin:      strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Doctor-Admin")), "true"),
		AllowAll:   allowAll,
		Partitions: map[string]bool{},
	}
	for _, group := range splitCSV(r.Header.Get("X-Doctor-Groups")) {
		identity.Groups = append(identity.Groups, group)
	}
	for _, partition := range splitCSV(r.Header.Get("X-Doctor-Partition-Access")) {
		identity.Partitions[partition] = true
	}
	for _, group := range identity.Groups {
		switch strings.ToLower(group) {
		case "sentinel-admin", "sentinel-admins", "doctor-admin", "doctor-admins":
			identity.Admin = true
		}
	}
	if identity.User == "" && identity.AllowAll {
		identity.User = "local-dev"
		identity.Admin = true
	}
	return identity
}

// Can reports whether the identity has capability on the partition.
func (i Identity) Can(capability Capability, tenant, partition string) bool {
	if i.AllowAll || i.Admin {
		return true
	}
	if !i.canSeePartition(partition) {
		return false
	}
	if i.hasGroup("sentinel-responder", "sentinel-responders", "sentinel-oncall", "sentinel-on-call") {
		switch capability {
		case CapAlertsRead, CapAlertsAck, CapAlertsSilence, CapRulesRead, CapSourcesRead, CapReceiversRead, CapDeliveryRead, CapSubspaceRead:
			return true
		}
	}
	if i.hasGroup("sentinel-rule-editor", "sentinel-rule-editors", "sentinel-engineer", "sentinel-engineers") {
		switch capability {
		case CapAlertsRead, CapRulesRead, CapRulesWrite, CapSourcesRead, CapReceiversRead, CapDeliveryRead, CapSubspaceRead:
			return true
		}
	}
	switch capability {
	case CapAlertsRead, CapRulesRead, CapSourcesRead, CapReceiversRead, CapDeliveryRead, CapSubspaceRead:
		return i.canSeePartition(partition)
	default:
		return false
	}
}

// Capabilities returns the effective capabilities for a partition.
func (i Identity) Capabilities(tenant, partition string) []Capability {
	all := []Capability{
		CapAlertsRead, CapAlertsAck, CapAlertsSilence, CapAlertsSilencePartition,
		CapRulesRead, CapRulesWrite, CapSourcesRead, CapSourcesWrite,
		CapReceiversRead, CapReceiversWrite, CapDeliveryRead, CapDeliveryRetry,
		CapSubspaceRead, CapSubspaceWrite, CapSubspaceSilence,
	}
	out := make([]Capability, 0, len(all))
	for _, capability := range all {
		if i.Can(capability, tenant, partition) {
			out = append(out, capability)
		}
	}
	return out
}

func (i Identity) canSeePartition(partition string) bool {
	if i.AllowAll || i.Admin {
		return true
	}
	if partition == "" {
		return false
	}
	return i.Partitions[partition] || i.Partitions["*"]
}

func (i Identity) hasGroup(groups ...string) bool {
	for _, actual := range i.Groups {
		for _, want := range groups {
			if strings.EqualFold(actual, want) {
				return true
			}
		}
	}
	return false
}

func splitCSV(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
