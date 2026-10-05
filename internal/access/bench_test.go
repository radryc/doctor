package access

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

func TestOwnersWarmDecisionIsCached(t *testing.T) {
	// Sanity check that repeated decisions do not reload owners.
	source := &fakeOwnersSource{files: map[string]*authz.OwnersFile{
		"doctor/v1/endpoints/e0": ownerFile([]string{"alice"}, nil, nil),
	}}
	resolver := NewOwners(source, nil, "", time.Minute)
	endpoint := Endpoint{Name: "e0", OwnersPath: "doctor/v1/endpoints/e0"}
	id := authz.Identity{Subject: "alice"}
	if _, err := resolver.Allowed(context.Background(), id, endpoint, SignalLogs); err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if source.loads["doctor/v1/endpoints/e0"] != 1 {
		t.Fatalf("loads = %d, want 1", source.loads["doctor/v1/endpoints/e0"])
	}
}

func BenchmarkOwnersResolve10kEndpoints(b *testing.B) {
	const endpointCount = 10_000
	files := make(map[string]*authz.OwnersFile, endpointCount)
	endpoints := make([]Endpoint, 0, endpointCount)
	for i := 0; i < endpointCount; i++ {
		name := fmt.Sprintf("endpoint-%d", i)
		path := "doctor/v1/endpoints/" + name
		files[path] = ownerFile([]string{"alice"}, nil, nil)
		endpoints = append(endpoints, Endpoint{Name: name, OwnersPath: path})
	}
	resolver := NewOwners(&fakeOwnersSource{files: files}, nil, "", time.Hour)
	ctx := context.Background()
	id := authz.Identity{Subject: "alice"}

	// Warm the cache.
	for _, endpoint := range endpoints {
		if _, err := resolver.Allowed(ctx, id, endpoint, SignalLogs); err != nil {
			b.Fatalf("warm Allowed: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, endpoint := range endpoints {
			decision, err := resolver.Allowed(ctx, id, endpoint, SignalLogs)
			if err != nil || !decision.Allowed {
				b.Fatalf("Allowed: %v %#v", err, decision)
			}
		}
	}
}
