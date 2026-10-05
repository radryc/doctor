package access

import (
	"context"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

type fakeOwnersSource struct {
	files map[string]*authz.OwnersFile
	loads map[string]int
}

func (f *fakeOwnersSource) LoadOwners(_ context.Context, dir string) (*authz.OwnersFile, error) {
	if f.loads == nil {
		f.loads = map[string]int{}
	}
	f.loads[dir]++
	return f.files[dir], nil
}

func (f *fakeOwnersSource) Version(context.Context, string) (string, error) { return "", nil }

func ownerFile(viewers, ingesters, maintainers []string) *authz.OwnersFile {
	toRefs := func(raw []string) []authz.OwnerRef {
		out := make([]authz.OwnerRef, 0, len(raw))
		for _, value := range raw {
			if len(value) > 0 && value[0] == '@' {
				out = append(out, authz.OwnerRef{Team: value[1:]})
			} else {
				out = append(out, authz.OwnerRef{Subject: value})
			}
		}
		return out
	}
	return &authz.OwnersFile{
		Version:     1,
		Viewers:     toRefs(viewers),
		Ingesters:   toRefs(ingesters),
		Maintainers: toRefs(maintainers),
	}
}

func TestOwnersAllowsViewerAndAncestorMaintainer(t *testing.T) {
	source := &fakeOwnersSource{files: map[string]*authz.OwnersFile{
		"doctor/v1/endpoints/loki-edge": ownerFile([]string{"alice"}, nil, nil),
		"doctor/v1":                     ownerFile(nil, nil, []string{"@platform"}),
	}}
	resolver := NewOwners(source, authz.TeamMapping{"platform": "platform-team"}, "", time.Minute)
	endpoint := Endpoint{Name: "loki-edge", OwnersPath: "doctor/v1/endpoints/loki-edge"}

	decision, err := resolver.Allowed(context.Background(), authz.Identity{Subject: "alice"}, endpoint, SignalLogs)
	if err != nil || !decision.Allowed {
		t.Fatalf("viewer decision = %#v, err = %v", decision, err)
	}

	// A maintainer of an ancestor grants access to the child endpoint.
	decision, err = resolver.Allowed(context.Background(), authz.Identity{Subject: "bob", Groups: []string{"platform-team"}}, endpoint, SignalLogs)
	if err != nil || !decision.Allowed {
		t.Fatalf("ancestor maintainer decision = %#v, err = %v", decision, err)
	}

	decision, err = resolver.Allowed(context.Background(), authz.Identity{Subject: "mallory"}, endpoint, SignalLogs)
	if err != nil || decision.Allowed {
		t.Fatalf("unlisted decision = %#v, err = %v", decision, err)
	}
}

func TestOwnersCachesCompiledGrants(t *testing.T) {
	source := &fakeOwnersSource{files: map[string]*authz.OwnersFile{
		"doctor/v1/endpoints/loki-edge": ownerFile([]string{"alice"}, nil, nil),
	}}
	resolver := NewOwners(source, nil, "", time.Minute)
	endpoint := Endpoint{Name: "loki-edge", OwnersPath: "doctor/v1/endpoints/loki-edge"}
	id := authz.Identity{Subject: "alice"}

	for i := 0; i < 5; i++ {
		if _, err := resolver.Allowed(context.Background(), id, endpoint, SignalLogs); err != nil {
			t.Fatalf("Allowed: %v", err)
		}
	}
	if got := source.loads["doctor/v1/endpoints/loki-edge"]; got != 1 {
		t.Fatalf("owners loads = %d, want 1 (cached)", got)
	}
}

func TestOwnersAnonymousDenied(t *testing.T) {
	resolver := NewOwners(&fakeOwnersSource{}, nil, "", time.Minute)
	decision, err := resolver.Allowed(context.Background(), authz.Identity{}, Endpoint{Name: "x"}, SignalLogs)
	if err != nil || decision.Allowed {
		t.Fatalf("anonymous decision = %#v, err = %v", decision, err)
	}
}

func TestStaticResolver(t *testing.T) {
	resolver := NewStatic("alice, bob@example.com", "platform-team", "mallory")
	if decision, _ := resolver.Allowed(context.Background(), authz.Identity{Subject: "alice"}, Endpoint{Name: "x"}, SignalLogs); !decision.Allowed {
		t.Fatalf("alice denied")
	}
	if decision, _ := resolver.Allowed(context.Background(), authz.Identity{Subject: "s", Email: "bob@example.com"}, Endpoint{Name: "x"}, SignalLogs); !decision.Allowed {
		t.Fatalf("bob denied")
	}
	if decision, _ := resolver.Allowed(context.Background(), authz.Identity{Subject: "carol", Groups: []string{"platform-team"}}, Endpoint{Name: "x"}, SignalLogs); !decision.Allowed {
		t.Fatalf("group denied")
	}
	if decision, _ := resolver.Allowed(context.Background(), authz.Identity{Subject: "mallory"}, Endpoint{Name: "x"}, SignalLogs); decision.Allowed {
		t.Fatalf("denied subject allowed")
	}
}

func TestChainFirstAllowWins(t *testing.T) {
	chain := NewChain(
		NewStatic("", "", ""),
		NewStatic("alice", "", ""),
	)
	decision, err := chain.Allowed(context.Background(), authz.Identity{Subject: "alice"}, Endpoint{Name: "x"}, SignalLogs)
	if err != nil || !decision.Allowed || decision.Resolver != "static" {
		t.Fatalf("decision = %#v, err = %v", decision, err)
	}
}

func TestGateObserveThenEnforce(t *testing.T) {
	deny := NewStatic("", "", "")
	endpoint := Endpoint{Name: "loki-edge"}

	observe := NewGate(deny, nil, nil, nil)
	allowed, decision, enforced := observe.Allowed(context.Background(), authz.Identity{Subject: "alice"}, endpoint, SignalLogs)
	if !allowed || enforced || !decision.Observed {
		t.Fatalf("observe: allowed=%v enforced=%v decision=%#v", allowed, enforced, decision)
	}

	enforce := NewGate(deny, map[Signal]bool{SignalLogs: true}, nil, nil)
	allowed, decision, enforced = enforce.Allowed(context.Background(), authz.Identity{Subject: "alice"}, endpoint, SignalLogs)
	if allowed || !enforced || decision.Observed {
		t.Fatalf("enforce: allowed=%v enforced=%v decision=%#v", allowed, enforced, decision)
	}

	// Metrics are never enforced, even with the flag set.
	allowed, _, enforced = enforce.Allowed(context.Background(), authz.Identity{Subject: "alice"}, endpoint, SignalMetrics)
	if !allowed || enforced {
		t.Fatalf("metrics: allowed=%v enforced=%v", allowed, enforced)
	}
}

func TestGrantsResolver(t *testing.T) {
	store, err := authz.NewGrantStore("")
	if err != nil {
		t.Fatalf("NewGrantStore: %v", err)
	}
	if err := store.Add(authz.Grant{Subject: "alice", Partition: "loki-edge", Role: authz.RoleViewer}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	resolver := &Grants{Store: store}
	if decision, _ := resolver.Allowed(context.Background(), authz.Identity{Subject: "alice"}, Endpoint{Name: "loki-edge"}, SignalLogs); !decision.Allowed {
		t.Fatalf("alice denied by grants")
	}
	if decision, _ := resolver.Allowed(context.Background(), authz.Identity{Subject: "bob"}, Endpoint{Name: "loki-edge"}, SignalLogs); decision.Allowed {
		t.Fatalf("bob allowed by grants")
	}
}
