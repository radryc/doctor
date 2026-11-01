package auth

import (
	"net/http"
	"strings"
)

// TenantGuard bounds which tenant names a deployment accepts. It is a
// defence-in-depth check in front of the storage layer: the strongest isolation
// still requires tenant-scoped storage keys, but this prevents callers from
// freely switching to another tenant by editing a query parameter or header.
type TenantGuard struct {
	Header        string
	DefaultTenant string
	// AllowedTenants is an explicit allowlist. Empty means all names are
	// accepted unless SingleTenant is set.
	AllowedTenants []string
	// SingleTenant forces every request to this tenant. Empty disables the
	// single-tenant constraint.
	SingleTenant string
}

// NewTenantGuard builds the middleware.
func NewTenantGuard(header, defaultTenant string, allowed []string, singleTenant string) *TenantGuard {
	if strings.TrimSpace(header) == "" {
		header = "X-Doctor-Tenant"
	}
	if strings.TrimSpace(defaultTenant) == "" {
		defaultTenant = "default"
	}
	return &TenantGuard{
		Header:         header,
		DefaultTenant:  strings.TrimSpace(defaultTenant),
		AllowedTenants: normalizeTenantList(allowed),
		SingleTenant:   strings.TrimSpace(singleTenant),
	}
}

func normalizeTenantList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// Wrap implements the HTTP middleware.
func (g *TenantGuard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
		if tenant == "" {
			tenant = strings.TrimSpace(r.Header.Get(g.Header))
		}
		if tenant == "" {
			tenant = g.DefaultTenant
		}
		if g.SingleTenant != "" && tenant != g.SingleTenant {
			writeTenantError(w, "tenant is fixed to "+g.SingleTenant)
			return
		}
		if len(g.AllowedTenants) > 0 {
			allowed := false
			for _, candidate := range g.AllowedTenants {
				if candidate == "*" || candidate == tenant {
					allowed = true
					break
				}
			}
			if !allowed {
				writeTenantError(w, "tenant is not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeTenantError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"status":"error","errorType":"forbidden","error":"` + message + `"}`))
}
