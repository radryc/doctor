package access

import (
	"context"

	"github.com/radryc/monofs/pkg/authz"
)

// Grants resolves access from an authz.GrantStore.
type Grants struct {
	Store *authz.GrantStore
}

// ResolverName implements Resolver.
func (g *Grants) ResolverName() string { return "grants" }

// Allowed implements Resolver.
func (g *Grants) Allowed(ctx context.Context, id authz.Identity, ep Endpoint, signal Signal) (Decision, error) {
	decision := Decision{Endpoint: ep.Name, Signal: signal, Resolver: g.ResolverName()}
	if g.Store == nil {
		decision.Reason = "no grant store configured"
		return decision, nil
	}
	if id.IsAnonymous() {
		decision.Reason = "anonymous"
		return decision, nil
	}
	if g.Store.Can(ctx, id, ep.Name, authz.ActionView) {
		decision.Allowed = true
		return decision, nil
	}
	decision.Reason = "no grant for endpoint"
	return decision, nil
}
