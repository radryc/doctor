package ruler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/radryc/monofs/pkg/authz"
)

func TestLoadRulesTokenFileScopesPartitions(t *testing.T) {
	hash := sha256.Sum256([]byte("team-a-token"))
	path := filepath.Join(t.TempDir(), "rules-tokens.yaml")
	content := "version: 1\ntokens:\n" +
		"  - id: team-a\n    sha256: " + hex.EncodeToString(hash[:]) + "\n    partitions: [a]\n    role: maintainer\n" +
		"  - id: viewer\n    token: viewer-token\n    partitions: [b]\n    role: viewer\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	verifier, err := LoadRulesTokenFile(path)
	if err != nil {
		t.Fatalf("LoadRulesTokenFile: %v", err)
	}
	if verifier.Count() != 2 {
		t.Fatalf("count = %d", verifier.Count())
	}

	maintainer, err := verifier.Verify(context.Background(), "team-a-token")
	if err != nil {
		t.Fatalf("verify maintainer: %v", err)
	}
	ctx := authz.ContextWithIdentity(context.Background(), maintainer)
	authorizer := ScopedRulesAuthorizer{}
	if !authorizer.CanModify(ctx, "a", "g") {
		t.Fatal("maintainer should modify partition a")
	}
	if authorizer.CanModify(ctx, "b", "g") {
		t.Fatal("maintainer must not modify partition b")
	}
	if !authorizer.CanRead(ctx, "a") || authorizer.CanRead(ctx, "b") {
		t.Fatal("read scope mismatch")
	}

	viewer, err := verifier.Verify(context.Background(), "viewer-token")
	if err != nil {
		t.Fatalf("verify viewer: %v", err)
	}
	viewerCtx := authz.ContextWithIdentity(context.Background(), viewer)
	if authorizer.CanModify(viewerCtx, "b", "g") {
		t.Fatal("viewer must not modify")
	}
	if !authorizer.CanRead(viewerCtx, "b") {
		t.Fatal("viewer should read partition b")
	}
}

func TestScopedRulesAuthorizerAdmin(t *testing.T) {
	identity := authz.Identity{Subject: "root", Groups: []string{RulesAdminGroup}}
	ctx := authz.ContextWithIdentity(context.Background(), identity)
	authorizer := ScopedRulesAuthorizer{}
	if !authorizer.CanModify(ctx, "anything", "g") || !authorizer.CanRead(ctx, "anything") {
		t.Fatal("admin should have full access")
	}
}
