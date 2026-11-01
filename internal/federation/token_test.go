package federation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/ruler"
	"github.com/radryc/doctor/internal/store"
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

func TestTokenBundleEncryptionRoundTrip(t *testing.T) {
	files := ruler.NewMemoryStore()
	writer := NewTokenBundleWriter(files, "", "/doctor/v1/federation/tokens", "default", 4)
	writer.Key = []byte("operator-secret")
	target := Target{ID: "aws-1", TokenRef: "guardian-pusher://aws-1"}
	if _, err := writer.Write(context.Background(), map[string]string{target.ID: "bundle-token"}); err != nil {
		t.Fatalf("Write encrypted token: %v", err)
	}
	raw, err := files.ReadFile(context.Background(), writer.shardPath(writer.shardFor(target.ID)))
	if err != nil {
		t.Fatalf("read shard: %v", err)
	}
	if strings.Contains(string(raw), "bundle-token") {
		t.Fatalf("encrypted shard contains plaintext token: %s", raw)
	}
	resolver := NewBundleTokenResolver(BundleTokenResolverOptions{
		Files:      files,
		Root:       "/doctor/v1/federation/tokens",
		Tenant:     "default",
		ShardCount: 4,
	})
	resolver.Key = []byte("operator-secret")
	if token, err := resolver.Resolve(context.Background(), target); err != nil || token != "bundle-token" {
		t.Fatalf("Resolve = %q, %v", token, err)
	}
	resolver.Invalidate(target)
	resolver.Key = []byte("wrong-secret")
	if _, err := resolver.Resolve(context.Background(), target); err == nil {
		t.Fatal("Resolve with wrong key succeeded")
	}
}

func TestTokenBundleWriterMigratesPlaintextShards(t *testing.T) {
	files := ruler.NewMemoryStore()
	writer := NewTokenBundleWriter(files, "", "/doctor/v1/federation/tokens", "default", 4)
	target := Target{ID: "aws-1", TokenRef: "guardian-pusher://aws-1"}
	if _, err := writer.Write(context.Background(), map[string]string{target.ID: "bundle-token"}); err != nil {
		t.Fatalf("Write plaintext token: %v", err)
	}
	writer.Key = []byte("operator-secret")
	if _, err := writer.Write(context.Background(), map[string]string{target.ID: "bundle-token"}); err != nil {
		t.Fatalf("Write with key: %v", err)
	}
	raw, err := files.ReadFile(context.Background(), writer.shardPath(writer.shardFor(target.ID)))
	if err != nil {
		t.Fatalf("read shard: %v", err)
	}
	if strings.Contains(string(raw), "bundle-token") {
		t.Fatalf("plaintext was not migrated: %s", raw)
	}
}

func TestTokenBundleWriterRevokesRemovedTargets(t *testing.T) {
	files := ruler.NewMemoryStore()
	writer := NewTokenBundleWriter(files, "", "/doctor/v1/federation/tokens", "default", 4)
	target := Target{ID: "aws-1", TokenRef: "guardian-pusher://aws-1"}
	if _, err := writer.Write(context.Background(), map[string]string{target.ID: "bundle-token"}); err != nil {
		t.Fatalf("Write initial token: %v", err)
	}
	resolver := NewBundleTokenResolver(BundleTokenResolverOptions{
		Files:      files,
		Root:       "/doctor/v1/federation/tokens",
		Tenant:     "default",
		ShardCount: 4,
		CacheTTL:   time.Minute,
	})
	if token, err := resolver.Resolve(context.Background(), target); err != nil || token != "bundle-token" {
		t.Fatalf("Resolve = %q, %v", token, err)
	}
	if _, err := writer.Write(context.Background(), map[string]string{}); err != nil {
		t.Fatalf("Write empty token map: %v", err)
	}
	resolver.Invalidate(target)
	if _, err := resolver.Resolve(context.Background(), target); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("Resolve after revocation error = %v, want ErrTokenNotFound", err)
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
