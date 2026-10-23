package federation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/ruler"
	"github.com/rydzu/ainfra/doctor/internal/store"
)

func TestEnvTokenResolver(t *testing.T) {
	t.Setenv("TEST_FEDERATION_TOKEN", "env-token")
	resolver := EnvTokenResolver{}
	token, err := resolver.Resolve(context.Background(), Target{
		ID:       "aws-1",
		TokenEnv: "TEST_FEDERATION_TOKEN",
	})
	if err != nil || token != "env-token" {
		t.Fatalf("Resolve() = %q, %v", token, err)
	}
}

func TestGuardianTokenResolverCachesAndInvalidates(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("X-Guardian-Internal-Token"); got != "internal" {
			t.Fatalf("internal token = %q", got)
		}
		if r.URL.Path != "/api/m2m/pushers/aws-div-1" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"aws-div-1","token":"pusher-token"}`))
	}))
	defer server.Close()

	resolver := NewGuardianTokenResolver(GuardianTokenResolverOptions{
		BaseURL: server.URL,
		Token:   "internal",
	})
	target := Target{ID: "aws-1", TokenRef: "guardian-pusher://aws-div-1"}
	for i := 0; i < 2; i++ {
		token, err := resolver.Resolve(context.Background(), target)
		if err != nil || token != "pusher-token" {
			t.Fatalf("Resolve() = %q, %v", token, err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
	resolver.Invalidate(target)
	if _, err := resolver.Resolve(context.Background(), target); err != nil {
		t.Fatalf("Resolve after invalidate: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests after invalidate = %d, want 2", requests.Load())
	}
}

func TestGuardianTokenResolverRejectsEnvTarget(t *testing.T) {
	resolver := NewGuardianTokenResolver(GuardianTokenResolverOptions{})
	_, err := resolver.Resolve(context.Background(), Target{ID: "aws-1", TokenEnv: "SOME_TOKEN"})
	if err != ErrTokenRefUnsupported {
		t.Fatalf("error = %v, want ErrTokenRefUnsupported", err)
	}
}

func TestBundleTokenResolverShardedReadAndInvalidation(t *testing.T) {
	files := ruler.NewMemoryStore()
	resolver := NewBundleTokenResolver(BundleTokenResolverOptions{
		Files:      files,
		Root:       "/doctor/v1/federation/tokens",
		Tenant:     "default",
		ShardCount: 16,
		CacheTTL:   time.Minute,
	})
	target := Target{ID: "aws-1", TokenRef: "monofs-token://default"}
	writeBundleShard(t, files, resolver, target, map[string]string{target.ID: "bundle-token"})

	token, err := resolver.Resolve(context.Background(), target)
	if err != nil || token != "bundle-token" {
		t.Fatalf("Resolve() = %q, %v", token, err)
	}

	resolver.Invalidate(target)
	writeBundleShard(t, files, resolver, target, map[string]string{target.ID: "rotated-token"})
	token, err = resolver.Resolve(context.Background(), target)
	if err != nil || token != "rotated-token" {
		t.Fatalf("Resolve after invalidation = %q, %v", token, err)
	}
}

func TestChainTokenResolverFallsBackToBundle(t *testing.T) {
	files := ruler.NewMemoryStore()
	bundle := NewBundleTokenResolver(BundleTokenResolverOptions{
		Files:      files,
		Root:       "/doctor/v1/federation/tokens",
		Tenant:     "default",
		ShardCount: 4,
	})
	target := Target{ID: "aws-1", TokenRef: "monofs-token://default"}
	writeBundleShard(t, files, bundle, target, map[string]string{target.ID: "fallback-token"})

	guardian := NewGuardianTokenResolver(GuardianTokenResolverOptions{BaseURL: "http://127.0.0.1:1"})
	chain := ChainTokenResolver{guardian, bundle}
	token, err := chain.Resolve(context.Background(), target)
	if err != nil || token != "fallback-token" {
		t.Fatalf("chain Resolve() = %q, %v", token, err)
	}
}

func TestBundleTokenResolverSupportsGuardianPusherRefs(t *testing.T) {
	files := ruler.NewMemoryStore()
	bundle := NewBundleTokenResolver(BundleTokenResolverOptions{
		Files:      files,
		Root:       "/doctor/v1/federation/tokens",
		Tenant:     "default",
		ShardCount: 4,
	})
	target := Target{ID: "aws-1", TokenRef: "guardian-pusher://aws-1"}
	writeBundleShard(t, files, bundle, target, map[string]string{target.ID: "bundle-token"})

	token, err := bundle.Resolve(context.Background(), target)
	if err != nil || token != "bundle-token" {
		t.Fatalf("Resolve() = %q, %v", token, err)
	}
}

func writeBundleShard(t *testing.T, files *ruler.MemoryStore, resolver *BundleTokenResolver, target Target, tokens map[string]string) {
	t.Helper()
	shard := resolver.shardFor(target.ID)
	logicalPath := path.Join(resolver.root, resolver.tenant, fmt.Sprintf("%04d.json", shard))
	body := fmt.Sprintf(`{"tokens":{%q:%q}}`, target.ID, tokens[target.ID])
	if _, err := files.UpsertPaths(context.Background(), "", []store.PathWrite{{
		LogicalPath:       logicalPath,
		Content:           []byte(body),
		ExpectedVersionID: "",
	}}, store.MutationContext{}); err != nil {
		t.Fatalf("write bundle shard: %v", err)
	}
}
