package proxy

import (
	"context"
	"testing"

	"github.com/radryc/doctor/internal/access"
)

func TestAccessDecisionBypassesOwnersForLocalStorage(t *testing.T) {
	// A resolver that allows nobody: this is what the OWNERS resolver does for a
	// non-admin when no governing OWNERS file lists them.
	gate := access.NewGate(
		access.NewStatic("", "", ""),
		map[access.Signal]bool{access.SignalTraces: true, access.SignalLogs: true},
		nil,
		nil,
	)
	router := &Router{access: gate}

	local := Backend{Name: "monofs", Kind: KindMonoFS, Origin: "local"}
	for _, signal := range []access.Signal{access.SignalTraces, access.SignalLogs} {
		allowed, decision, enforced := router.accessDecision(context.Background(), local, signal)
		if !allowed {
			t.Fatalf("%s: local backend denied: %#v", signal, decision)
		}
		if enforced {
			t.Fatalf("%s: local backend decision should not be enforced by the proxy gate", signal)
		}
		if decision.Resolver != "local-policy" {
			t.Fatalf("%s: resolver = %q, want local-policy", signal, decision.Resolver)
		}
	}

	remote := Backend{Name: "loki-edge", Kind: KindLoki, Origin: "config"}
	allowed, decision, enforced := router.accessDecision(context.Background(), remote, access.SignalLogs)
	if allowed {
		t.Fatalf("remote backend should stay gated: %#v", decision)
	}
	if !enforced {
		t.Fatal("remote backend denial should be enforced")
	}
}

func TestTraceTargetsKeepLocalWhenRemoteDenied(t *testing.T) {
	gate := access.NewGate(
		access.NewStatic("", "", ""),
		map[access.Signal]bool{access.SignalTraces: true},
		nil,
		nil,
	)
	local := Backend{
		Name: "monofs", Kind: KindMonoFS, Origin: "local",
		Signals: []Signal{SignalTraces},
	}
	remote := Backend{
		Name: "tempo-edge", Kind: KindTempo, Origin: "config",
		BaseURL: "http://tempo.example",
		Signals: []Signal{SignalTraces},
	}
	router := newTestRouter(t,
		map[string]Backend{"monofs": local, "tempo-edge": remote},
		map[string]Upstreams{
			"monofs":     {Traces: &fakeTracesUpstream{name: "monofs"}},
			"tempo-edge": {Traces: &fakeTracesUpstream{name: "tempo-edge"}},
		},
	)
	router.access = gate

	targets, warnings, err := router.traceTargets(context.Background())
	if err != nil {
		t.Fatalf("traceTargets: %v (warnings=%#v)", err, warnings)
	}
	if len(targets) != 1 || targets[0].backend.Name != "monofs" {
		t.Fatalf("targets = %#v, want only the local backend", targets)
	}
	if len(warnings) == 0 {
		t.Fatal("expected a warning for the denied remote backend")
	}
}
