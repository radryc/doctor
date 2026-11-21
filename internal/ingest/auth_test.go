package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/radryc/monofs/pkg/authz"
)

func TestLoadTokenFileScopesCollectors(t *testing.T) {
	hash := sha256.Sum256([]byte("edge-token"))
	path := filepath.Join(t.TempDir(), "ingest-tokens.yaml")
	content := "version: 1\ntokens:\n" +
		"  - id: collector-edge\n    sha256: " + hex.EncodeToString(hash[:]) + "\n    tenants: [edge]\n" +
		"  - id: collector-prod\n    token: prod-token\n    tenants: [prod, edge]\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	verifier, err := LoadTokenFile(path)
	if err != nil {
		t.Fatalf("LoadTokenFile: %v", err)
	}
	if verifier.TokenCount() != 2 {
		t.Fatalf("TokenCount = %d, want 2", verifier.TokenCount())
	}
	if !verifier.Scoped() {
		t.Fatal("expected a tenant-scoped verifier")
	}

	identity, err := verifier.Verify(context.Background(), "edge-token")
	if err != nil {
		t.Fatalf("Verify(edge): %v", err)
	}
	if identity.ClientID != "ingest:collector-edge" {
		t.Fatalf("client id = %q", identity.ClientID)
	}
	if !identity.HasGroup(TenantGroupPrefix + "edge") {
		t.Fatalf("groups = %v, want edge scope", identity.Groups)
	}
	if IdentityTenantAllowed(identity, "prod") {
		t.Fatal("edge collector must not write prod")
	}
	if !IdentityTenantAllowed(identity, "edge") {
		t.Fatal("edge collector must write edge")
	}

	if _, err := verifier.Verify(context.Background(), "missing"); err == nil {
		t.Fatal("expected unknown token to fail")
	}
}

func TestLoadTokenFileRejectsBadEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ingest-tokens.yaml")
	if err := os.WriteFile(path, []byte("tokens:\n  - id: no-secret\n    tenants: [edge]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTokenFile(path); err == nil {
		t.Fatal("expected missing token/sha256 to fail")
	}
}

func TestServiceEnforcesCollectorTenantScope(t *testing.T) {
	svc := NewServiceWithBuffers("X-Doctor-Tenant", "default", nil, nil, nil, nil, nil, nil)
	svc.SetAllowedTenants([]string{"edge", "prod"})

	identity := authz.Identity{
		Subject:  "ingest:collector-edge",
		ClientID: "ingest:collector-edge",
		Groups:   []string{IngestGroup, TenantGroupPrefix + "edge"},
	}
	ctx := authz.ContextWithIdentity(context.Background(), identity)
	if err := svc.ingestTraces(ctx, "prod", &coltracepb.ExportTraceServiceRequest{}); err == nil {
		t.Fatal("expected cross-tenant ingest to be denied")
	}
	if !svc.tenantAllowedForContext(ctx, "edge") {
		t.Fatal("edge collector must be allowed to write edge")
	}
	if svc.tenantAllowedForContext(ctx, "prod") {
		t.Fatal("edge collector must not be allowed to write prod")
	}
}

func TestServiceWithoutIdentityKeepsAllowlistBehavior(t *testing.T) {
	svc := NewServiceWithBuffers("X-Doctor-Tenant", "default", nil, nil, nil, nil, nil, nil)
	svc.SetAllowedTenants([]string{"edge"})
	if !svc.tenantAllowedForContext(context.Background(), "edge") {
		t.Fatal("direct callers should keep the allowlist behavior")
	}
	if svc.tenantAllowedForContext(context.Background(), "prod") {
		t.Fatal("direct callers must still honor the allowlist")
	}
}

func TestLegacyIdentityIsScopedToAllowlist(t *testing.T) {
	identity := LegacyIdentity([]string{"default", "edge"})
	if !IdentityTenantAllowed(identity, "edge") {
		t.Fatal("legacy identity should allow edge")
	}
	if IdentityTenantAllowed(identity, "prod") {
		t.Fatal("legacy identity should not allow prod")
	}
	wildcard := LegacyIdentity(nil)
	if !IdentityTenantAllowed(wildcard, "anything") {
		t.Fatal("empty allowlist should be represented as wildcard")
	}
}
