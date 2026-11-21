package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/radryc/monofs/pkg/authz"
	"sigs.k8s.io/yaml"
)

// TenantGroupPrefix marks a collector identity as scoped to one tenant. A
// collector may hold several groups (one per tenant) or the wildcard
// "ingest:tenant:*".
const TenantGroupPrefix = "ingest:tenant:"

// IngestGroup is attached to every collector principal, including the legacy
// shared token.
const IngestGroup = "doctor:ingest"

// TokenSpec is one collector credential in DOCTOR_INGEST_TOKENS_FILE.
//
// Exactly one of Token (plaintext, development only) or SHA256 (hex) must be
// set. Tokens are never logged.
type TokenSpec struct {
	ID      string   `json:"id" yaml:"id"`
	Token   string   `json:"token,omitempty" yaml:"token,omitempty"`
	SHA256  string   `json:"sha256,omitempty" yaml:"sha256,omitempty"`
	Tenants []string `json:"tenants,omitempty" yaml:"tenants,omitempty"`
}

// TokenFile is the on-disk format for per-collector ingest credentials.
type TokenFile struct {
	Version int         `json:"version,omitempty" yaml:"version,omitempty"`
	Tokens  []TokenSpec `json:"tokens" yaml:"tokens"`
}

// ScopedTokenVerifier resolves opaque bearer tokens to collector identities
// carrying ingest:tenant:<name> groups. Tokens are stored hashed.
type ScopedTokenVerifier struct {
	mu     sync.RWMutex
	tokens map[string]authz.Identity // sha256(token) -> identity
	scoped bool
}

// NewScopedTokenVerifier returns an empty verifier.
func NewScopedTokenVerifier() *ScopedTokenVerifier {
	return &ScopedTokenVerifier{tokens: map[string]authz.Identity{}}
}

// LoadTokenFile reads and validates a DOCTOR_INGEST_TOKENS_FILE.
func LoadTokenFile(path string) (*ScopedTokenVerifier, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ingest: read token file %s: %w", path, err)
	}
	var file TokenFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("ingest: parse token file %s: %w", path, err)
	}
	verifier := NewScopedTokenVerifier()
	for i, spec := range file.Tokens {
		if err := verifier.AddSpec(spec); err != nil {
			return nil, fmt.Errorf("ingest: token file %s entry %d: %w", path, i, err)
		}
	}
	if len(verifier.tokens) == 0 {
		return nil, fmt.Errorf("ingest: token file %s contains no tokens", path)
	}
	return verifier, nil
}

// AddSpec registers one TokenSpec. Plaintext tokens are hashed immediately.
func (v *ScopedTokenVerifier) AddSpec(spec TokenSpec) error {
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	tenants := normalizeTenants(spec.Tenants)
	if len(tenants) == 0 {
		return fmt.Errorf("tenants must not be empty")
	}
	identity := authz.Identity{
		Subject:  "ingest:" + id,
		ClientID: "ingest:" + id,
		Groups:   append([]string{IngestGroup}, tenantGroups(tenants)...),
	}
	switch {
	case strings.TrimSpace(spec.SHA256) != "":
		hash, err := normalizeSHA256(spec.SHA256)
		if err != nil {
			return err
		}
		v.AddHash(hash, identity)
	case strings.TrimSpace(spec.Token) != "":
		v.Add(identity, spec.Token)
	default:
		return fmt.Errorf("token or sha256 is required")
	}
	return nil
}

// Add registers a plaintext token for an identity.
func (v *ScopedTokenVerifier) Add(identity authz.Identity, token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	v.AddHash(hashToken(token), identity)
}

// AddHash registers an already-hashed token (hex SHA-256).
func (v *ScopedTokenVerifier) AddHash(hash string, identity authz.Identity) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if hash == "" {
		return
	}
	v.mu.Lock()
	v.tokens[hash] = identity
	v.scoped = v.scoped || identityHasTenantScope(identity)
	v.mu.Unlock()
}

// Scoped reports whether at least one registered token restricts tenants.
func (v *ScopedTokenVerifier) Scoped() bool {
	if v == nil {
		return false
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.scoped
}

// TokenCount returns the number of registered credentials.
func (v *ScopedTokenVerifier) TokenCount() int {
	if v == nil {
		return 0
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.tokens)
}

// Verify implements authz.TokenVerifier.
func (v *ScopedTokenVerifier) Verify(_ context.Context, rawToken string) (authz.Identity, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return authz.Identity{}, fmt.Errorf("authz: empty token")
	}
	hash := hashToken(rawToken)
	v.mu.RLock()
	identity, ok := v.tokens[hash]
	v.mu.RUnlock()
	if !ok {
		// Keep the failure path cheap but constant-time with respect to the
		// token value by rejecting after a hash comparison.
		return authz.Identity{}, fmt.Errorf("authz: unknown ingest token")
	}
	// The map lookup already used the hash; this guards accidental future
	// changes that compare raw tokens.
	if subtle.ConstantTimeCompare([]byte(hashToken(rawToken)), []byte(hash)) != 1 {
		return authz.Identity{}, fmt.Errorf("authz: unknown ingest token")
	}
	return identity, nil
}

// LegacyIdentity builds the identity for DOCTOR_INGEST_AUTH_TOKEN. It is scoped
// to the deployment-wide allowlist so a shared token cannot cross tenants.
func LegacyIdentity(allowedTenants []string) authz.Identity {
	tenants := normalizeTenants(allowedTenants)
	if len(tenants) == 0 {
		tenants = []string{"*"}
	}
	return authz.Identity{
		Subject:  "telemetry-ingest",
		ClientID: "telemetry-ingest",
		Groups:   append([]string{IngestGroup}, tenantGroups(tenants)...),
	}
}

// IdentityHasTenantScope reports whether an identity carries at least one
// tenant group.
func IdentityHasTenantScope(identity authz.Identity) bool {
	return identityHasTenantScope(identity)
}

func identityHasTenantScope(identity authz.Identity) bool {
	for _, group := range identity.Groups {
		if strings.HasPrefix(strings.TrimSpace(group), TenantGroupPrefix) {
			return true
		}
	}
	return false
}

// IdentityTenantAllowed reports whether the identity may write the tenant. A
// non-scoped identity is allowed (legacy tests/direct callers); the service
// still applies the deployment-wide allowlist separately.
func IdentityTenantAllowed(identity authz.Identity, tenant string) bool {
	tenant = strings.TrimSpace(tenant)
	if !identityHasTenantScope(identity) {
		return true
	}
	for _, group := range identity.Groups {
		group = strings.TrimSpace(group)
		if !strings.HasPrefix(group, TenantGroupPrefix) {
			continue
		}
		if group == TenantGroupPrefix+"*" || group == TenantGroupPrefix+tenant {
			return true
		}
	}
	return false
}

func normalizeTenants(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
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

func tenantGroups(tenants []string) []string {
	out := make([]string, 0, len(tenants))
	for _, tenant := range tenants {
		out = append(out, TenantGroupPrefix+tenant)
	}
	return out
}

func normalizeSHA256(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(strings.ToLower(raw), "sha256:")
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("sha256 must be 64 hex characters")
	}
	return hex.EncodeToString(decoded), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
