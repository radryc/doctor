// Package access decides whether an authenticated identity may query an
// endpoint. Resolvers are pluggable; the default resolver reads OWNERS files
// from MonoFS.
package access

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

// ErrAccessDenied reports that access enforcement excluded every backend for a
// signal.
var ErrAccessDenied = errors.New("access denied")

// Signal identifies one telemetry signal.
type Signal string

const (
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
	SignalTraces  Signal = "traces"
)

// Endpoint is the access-controlled unit exposed by the proxy.
type Endpoint struct {
	Name       string
	Signals    []Signal
	OwnersPath string
	Public     bool
}

// Decision is the outcome of one access evaluation.
type Decision struct {
	Endpoint string
	Signal   Signal
	Allowed  bool
	Resolver string
	Reason   string
	Observed bool
}

// Resolver decides whether an identity may query an endpoint for a signal.
type Resolver interface {
	Allowed(ctx context.Context, id authz.Identity, ep Endpoint, signal Signal) (Decision, error)
	ResolverName() string
}

// AllowAll permits every request.
type AllowAll struct{}

// Allowed implements Resolver.
func (AllowAll) Allowed(_ context.Context, _ authz.Identity, ep Endpoint, signal Signal) (Decision, error) {
	return Decision{Endpoint: ep.Name, Signal: signal, Allowed: true, Resolver: "allow-all"}, nil
}

// ResolverName implements Resolver.
func (AllowAll) ResolverName() string { return "allow-all" }

// Static permits or denies explicit subjects and groups.
type Static struct {
	AllowSubjects map[string]struct{}
	AllowGroups   map[string]struct{}
	DenySubjects  map[string]struct{}
}

// NewStatic builds a static resolver from comma-separated lists.
func NewStatic(allowSubjects, allowGroups, denySubjects string) *Static {
	return &Static{
		AllowSubjects: toSet(allowSubjects),
		AllowGroups:   toSet(allowGroups),
		DenySubjects:  toSet(denySubjects),
	}
}

func toSet(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out[strings.ToLower(trimmed)] = struct{}{}
		}
	}
	return out
}

// Allowed implements Resolver.
func (s *Static) Allowed(_ context.Context, id authz.Identity, ep Endpoint, signal Signal) (Decision, error) {
	decision := Decision{Endpoint: ep.Name, Signal: signal, Resolver: "static"}
	if id.IsAnonymous() {
		decision.Reason = "anonymous"
		return decision, nil
	}
	if _, denied := s.DenySubjects[strings.ToLower(id.Subject)]; denied {
		decision.Reason = "subject denied"
		return decision, nil
	}
	if _, denied := s.DenySubjects[strings.ToLower(id.Email)]; denied && id.Email != "" {
		decision.Reason = "email denied"
		return decision, nil
	}
	if _, ok := s.AllowSubjects[strings.ToLower(id.Subject)]; ok {
		decision.Allowed = true
		return decision, nil
	}
	if id.Email != "" {
		if _, ok := s.AllowSubjects[strings.ToLower(id.Email)]; ok {
			decision.Allowed = true
			return decision, nil
		}
	}
	for group := range s.AllowGroups {
		if id.HasGroup(group) {
			decision.Allowed = true
			return decision, nil
		}
	}
	decision.Reason = "no matching static grant"
	return decision, nil
}

// ResolverName implements Resolver.
func (s *Static) ResolverName() string { return "static" }

// Chain tries resolvers in order and returns the first allow.
type Chain struct {
	resolvers []Resolver
}

// NewChain builds a resolver chain.
func NewChain(resolvers ...Resolver) *Chain {
	return &Chain{resolvers: resolvers}
}

// Allowed implements Resolver.
func (c *Chain) Allowed(ctx context.Context, id authz.Identity, ep Endpoint, signal Signal) (Decision, error) {
	var last Decision
	for _, resolver := range c.resolvers {
		decision, err := resolver.Allowed(ctx, id, ep, signal)
		if err != nil {
			return Decision{}, err
		}
		last = decision
		if decision.Allowed {
			return decision, nil
		}
	}
	if last.Resolver == "" {
		last = Decision{Endpoint: ep.Name, Signal: signal, Resolver: c.ResolverName(), Reason: "no resolver allowed"}
	}
	return last, nil
}

// ResolverName implements Resolver.
func (c *Chain) ResolverName() string { return "chain" }

// Gate applies enforcement policy and records decisions. Metrics are never
// enforced.
type Gate struct {
	resolver Resolver
	enforce  map[Signal]bool
	logger   *log.Logger
	observe  func(Decision)
}

// NewGate builds an enforcement gate. When enforce is nil or a signal is
// absent, decisions are observed but not enforced.
func NewGate(resolver Resolver, enforce map[Signal]bool, logger *log.Logger, observe func(Decision)) *Gate {
	if enforce == nil {
		enforce = map[Signal]bool{}
	}
	return &Gate{resolver: resolver, enforce: enforce, logger: logger, observe: observe}
}

// ResolverName exposes the underlying resolver name.
func (g *Gate) ResolverName() string {
	if g == nil || g.resolver == nil {
		return "none"
	}
	return g.resolver.ResolverName()
}

// Allowed evaluates access and applies enforcement. enforced reports whether
// the decision is binding.
func (g *Gate) Allowed(ctx context.Context, id authz.Identity, ep Endpoint, signal Signal) (bool, Decision, bool) {
	if g == nil || g.resolver == nil {
		return true, Decision{Endpoint: ep.Name, Signal: signal, Allowed: true, Resolver: "none"}, false
	}
	decision, err := g.resolver.Allowed(ctx, id, ep, signal)
	if err != nil {
		decision = Decision{Endpoint: ep.Name, Signal: signal, Resolver: g.resolver.ResolverName(), Reason: err.Error()}
	}
	enforced := signal != SignalMetrics && g.enforce[signal]
	if !enforced {
		decision.Observed = true
	}
	if g.observe != nil {
		g.observe(decision)
	}
	if g.logger != nil && !decision.Allowed {
		mode := "observe"
		if enforced {
			mode = "enforce"
		}
		g.logger.Printf("access: %s denied endpoint=%s signal=%s subject=%q resolver=%s reason=%s",
			mode, ep.Name, signal, id.Subject, decision.Resolver, decision.Reason)
	}
	if !enforced {
		return true, decision, false
	}
	return decision.Allowed, decision, true
}

// Decide evaluates a decision without enforcement, for listing endpoints.
func (g *Gate) Decide(ctx context.Context, id authz.Identity, ep Endpoint, signal Signal) Decision {
	if g == nil || g.resolver == nil {
		return Decision{Endpoint: ep.Name, Signal: signal, Allowed: true, Resolver: "none"}
	}
	decision, err := g.resolver.Allowed(ctx, id, ep, signal)
	if err != nil {
		return Decision{Endpoint: ep.Name, Signal: signal, Resolver: g.resolver.ResolverName(), Reason: err.Error()}
	}
	return decision
}

// TTLDuration normalizes a cache TTL.
func TTLDuration(value time.Duration) time.Duration {
	if value <= 0 {
		return 60 * time.Second
	}
	return value
}
