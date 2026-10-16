package auth

import (
	"log"
	"net/http"
	"strings"

	"github.com/radryc/monofs/pkg/authz"
)

// TenantTrust attaches a synthetic machine identity to requests that carry a
// tenant header (or tenant query parameter) when no verified identity exists.
//
// It exists for header-authenticated integrations such as Grafana's provisioned
// datasources, which send X-Doctor-Tenant but no bearer token. The synthetic
// identity is:
//
//	ClientID: tenant:<name>
//	Groups:   [doctor:tenant]
//
// Access resolvers decide whether that principal may read data, so operators
// stay in control: allow it with DOCTOR_ACCESS_STATIC_ALLOW_GROUPS=doctor:tenant
// (chain resolver) or by listing tenant:<name> in an OWNERS file.
//
// Enabling this trusts the header, so it must only be used when every caller
// reaching doctor-query is itself trusted (cluster-internal service, gateway
// that strips client-supplied copies of the header).
type TenantTrust struct {
	// Header is the tenant header name (DOCTOR_TENANT_HEADER).
	Header string
	// Logger receives a one-time-per-request debug line when a synthetic
	// identity is attached.
	Logger *log.Logger
}

// NewTenantTrust builds the middleware. A blank header falls back to
// X-Doctor-Tenant.
func NewTenantTrust(header string, logger *log.Logger) *TenantTrust {
	if strings.TrimSpace(header) == "" {
		header = "X-Doctor-Tenant"
	}
	return &TenantTrust{Header: header, Logger: logger}
}

// Wrap implements the HTTP middleware.
func (t *TenantTrust) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := strings.TrimSpace(r.Header.Get(t.Header))
		if tenant == "" {
			tenant = strings.TrimSpace(r.URL.Query().Get("tenant"))
		}
		if tenant == "" {
			next.ServeHTTP(w, r)
			return
		}
		if identity, ok := authz.IdentityFromContext(r.Context()); ok && !identity.IsAnonymous() {
			// A verified bearer identity always wins over the header.
			next.ServeHTTP(w, r)
			return
		}
		synthetic := authz.Identity{
			ClientID: "tenant:" + tenant,
			Groups:   []string{"doctor:tenant"},
		}
		if t.Logger != nil {
			t.Logger.Printf("access: trusting tenant header %s=%q as machine identity %s", t.Header, tenant, synthetic.ClientID)
		}
		next.ServeHTTP(w, r.WithContext(authz.ContextWithIdentity(r.Context(), synthetic)))
	})
}
