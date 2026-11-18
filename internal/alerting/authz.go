package alerting

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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

// identitySigV2Prefix marks a signature that also covers the caller's
// partition scope. Legacy signatures omit the prefix and never bind the
// X-Doctor-Partition-Access header.
const identitySigV2Prefix = "v2:"

// SignIdentity computes the legacy HMAC-SHA256 hex signature over
// user + "|" + admin + "|" + groups for X-Doctor-Identity-Sig.
//
// It does not bind partition access. New callers that set
// X-Doctor-Partition-Access must use SignIdentityWithPartitions.
func SignIdentity(secret, user, admin, groups string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strings.TrimSpace(user) + "|" + strings.TrimSpace(admin) + "|" + strings.TrimSpace(groups)))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignIdentityWithPartitions computes an HMAC-SHA256 signature over
// user + "|" + admin + "|" + groups + "|" + partitions and prefixes it with
// "v2:". The partition header is included so a signature cannot be replayed
// with a wider X-Doctor-Partition-Access scope.
func SignIdentityWithPartitions(secret, user, admin, groups, partitions string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strings.TrimSpace(user) + "|" + strings.TrimSpace(admin) + "|" + strings.TrimSpace(groups) + "|" + strings.TrimSpace(partitions)))
	return identitySigV2Prefix + hex.EncodeToString(mac.Sum(nil))
}

func verifyIdentitySig(secret, user, admin, groups, partitions, sig string) bool {
	if strings.TrimSpace(secret) == "" {
		return false
	}
	sig = strings.TrimSpace(sig)
	sig = strings.TrimPrefix(sig, "sha256=")
	if sig == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(sig), identitySigV2Prefix) {
		want := SignIdentityWithPartitions(secret, user, admin, groups, partitions)
		return hmac.Equal([]byte(strings.ToLower(sig)), []byte(want))
	}
	// Legacy signatures did not cover partition access. Refuse them when the
	// request carries a partition scope so an old token cannot widen it.
	if strings.TrimSpace(partitions) != "" {
		return false
	}
	want := SignIdentity(secret, user, admin, groups)
	return hmac.Equal([]byte(strings.ToLower(sig)), []byte(want))
}

func validServiceToken(r *http.Request, serviceToken string) bool {
	serviceToken = strings.TrimSpace(serviceToken)
	if serviceToken == "" {
		return false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) < len("Bearer ") || !strings.EqualFold(auth[:len("Bearer ")], "Bearer ") {
		return false
	}
	token := strings.TrimSpace(auth[len("Bearer "):])
	return subtle.ConstantTimeCompare([]byte(token), []byte(serviceToken)) == 1
}

// IdentityFromRequest reads the trusted identity headers. Direct anonymous
// access is allowed only when allowAll is true (local development). When
// allowAll is false, unsigned identity headers are ignored.
func IdentityFromRequest(r *http.Request, allowAll bool) Identity {
	return IdentityFromRequestWithAuth(r, allowAll, "", "")
}

// IdentityFromRequestWithAuth reads identity headers when allowAll is true, a
// valid serviceToken is presented, or X-Doctor-Identity-Sig matches
// HMAC-SHA256(identityHMACSecret, user + "|" + admin + "|" + groups).
func IdentityFromRequestWithAuth(r *http.Request, allowAll bool, identityHMACSecret, serviceToken string) Identity {
	identity := Identity{
		AllowAll:   allowAll,
		Partitions: map[string]bool{},
	}
	if !allowAll {
		isSvc := validServiceToken(r, serviceToken)
		userHdr := r.Header.Get("X-Doctor-User")
		adminHdr := r.Header.Get("X-Doctor-Admin")
		groupsHdr := r.Header.Get("X-Doctor-Groups")
		partitionsHdr := r.Header.Get("X-Doctor-Partition-Access")
		sigOK := verifyIdentitySig(identityHMACSecret, userHdr, adminHdr, groupsHdr, partitionsHdr, r.Header.Get("X-Doctor-Identity-Sig"))
		if !isSvc && !sigOK {
			return identity
		}
		// A service token authenticates the service, not an arbitrary end
		// user. If identity headers are asserted they must also be signed, so
		// a compromised service credential cannot forge a wider partition
		// scope for a named operator.
		hasIdentityHeaders := userHdr != "" || adminHdr != "" || groupsHdr != "" || partitionsHdr != ""
		if isSvc && hasIdentityHeaders && !sigOK {
			return identity
		}
		identity = parseIdentityHeaders(r, allowAll)
		if isSvc && identity.User == "" && !identity.Admin {
			identity.User = "service"
			identity.Admin = true
		}
		return identity
	}
	identity = parseIdentityHeaders(r, allowAll)
	if identity.User == "" && identity.AllowAll {
		identity.User = "local-dev"
		identity.Admin = true
	}
	return identity
}

func parseIdentityHeaders(r *http.Request, allowAll bool) Identity {
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
