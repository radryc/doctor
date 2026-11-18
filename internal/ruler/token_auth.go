package ruler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/radryc/monofs/pkg/authz"
	"sigs.k8s.io/yaml"
)

// Rules authorization groups.
const (
	RulesAdminGroup      = "rules:admin"
	RulesWriteGroupPrefix = "rules:write:"
	RulesReadGroupPrefix  = "rules:read:"
)

// RulesTokenSpec is one credentials entry in DOCTOR_RULES_TOKENS_FILE.
type RulesTokenSpec struct {
	ID         string   `json:"id" yaml:"id"`
	Token      string   `json:"token,omitempty" yaml:"token,omitempty"`
	SHA256     string   `json:"sha256,omitempty" yaml:"sha256,omitempty"`
	Partitions []string `json:"partitions,omitempty" yaml:"partitions,omitempty"`
	// Role is admin, maintainer (default), or viewer.
	Role string `json:"role,omitempty" yaml:"role,omitempty"`
}

// RulesTokenFile is the on-disk credential document.
type RulesTokenFile struct {
	Version int               `json:"version,omitempty" yaml:"version,omitempty"`
	Tokens  []RulesTokenSpec  `json:"tokens" yaml:"tokens"`
}

// ScopedRulesVerifier resolves bearer tokens to principals carrying
// rules:write:<partition> / rules:read:<partition> groups.
type ScopedRulesVerifier struct {
	mu     sync.RWMutex
	tokens map[string]authz.Identity
}

// NewScopedRulesVerifier returns an empty verifier.
func NewScopedRulesVerifier() *ScopedRulesVerifier {
	return &ScopedRulesVerifier{tokens: map[string]authz.Identity{}}
}

// LoadRulesTokenFile reads DOCTOR_RULES_TOKENS_FILE.
func LoadRulesTokenFile(path string) (*ScopedRulesVerifier, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ruler: read token file %s: %w", path, err)
	}
	var file RulesTokenFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("ruler: parse token file %s: %w", path, err)
	}
	verifier := NewScopedRulesVerifier()
	for i, spec := range file.Tokens {
		if err := verifier.AddSpec(spec); err != nil {
			return nil, fmt.Errorf("ruler: token file %s entry %d: %w", path, i, err)
		}
	}
	if verifier.Count() == 0 {
		return nil, fmt.Errorf("ruler: token file %s contains no tokens", path)
	}
	return verifier, nil
}

// AddSpec registers one token. Plaintext tokens are hashed immediately.
func (v *ScopedRulesVerifier) AddSpec(spec RulesTokenSpec) error {
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	partitions := normalizePartitions(spec.Partitions)
	if len(partitions) == 0 {
		return fmt.Errorf("partitions must not be empty")
	}
	role := strings.ToLower(strings.TrimSpace(spec.Role))
	if role == "" {
		role = "maintainer"
	}
	groups := []string{"doctor:rules"}
	switch role {
	case "admin":
		groups = append(groups, RulesAdminGroup)
	case "viewer":
		for _, partition := range partitions {
			groups = append(groups, RulesReadGroupPrefix+partition)
		}
	case "maintainer":
		for _, partition := range partitions {
			groups = append(groups, RulesWriteGroupPrefix+partition)
		}
	default:
		return fmt.Errorf("unknown role %q (want admin, maintainer, or viewer)", role)
	}
	identity := authz.Identity{
		Subject:  "rules:" + id,
		ClientID: "rules:" + id,
		Groups:   groups,
	}
	switch {
	case strings.TrimSpace(spec.SHA256) != "":
		hash, err := normalizeRulesSHA256(spec.SHA256)
		if err != nil {
			return err
		}
		v.addHash(hash, identity)
	case strings.TrimSpace(spec.Token) != "":
		v.addHash(rulesTokenHash(spec.Token), identity)
	default:
		return fmt.Errorf("token or sha256 is required")
	}
	return nil
}

// AddToken registers a plaintext token with an explicit identity. Used for the
// legacy DOCTOR_RULES_API_TOKEN (admin during migration).
func (v *ScopedRulesVerifier) AddToken(token string, identity authz.Identity) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	v.addHash(rulesTokenHash(token), identity)
}

func (v *ScopedRulesVerifier) addHash(hash string, identity authz.Identity) {
	v.mu.Lock()
	v.tokens[hash] = identity
	v.mu.Unlock()
}

// Count returns the number of registered credentials.
func (v *ScopedRulesVerifier) Count() int {
	if v == nil {
		return 0
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.tokens)
}

// Verify implements authz.TokenVerifier.
func (v *ScopedRulesVerifier) Verify(_ context.Context, rawToken string) (authz.Identity, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return authz.Identity{}, fmt.Errorf("authz: empty token")
	}
	hash := rulesTokenHash(rawToken)
	v.mu.RLock()
	identity, ok := v.tokens[hash]
	v.mu.RUnlock()
	if !ok {
		return authz.Identity{}, fmt.Errorf("authz: unknown rules token")
	}
	return identity, nil
}

// ScopedRulesAuthorizer enforces the groups produced by ScopedRulesVerifier.
// A nil authorizer allows everything, matching the pre-authorization behavior.
type ScopedRulesAuthorizer struct{}

// CanModify reports whether the caller may write, delete, or evaluate a group.
func (ScopedRulesAuthorizer) CanModify(ctx context.Context, partition, _ string) bool {
	id, ok := authz.IdentityFromContext(ctx)
	if !ok || id.IsAnonymous() {
		return false
	}
	if id.HasGroup(RulesAdminGroup) {
		return true
	}
	partition = strings.TrimSpace(partition)
	return id.HasGroup(RulesWriteGroupPrefix+partition) || id.HasGroup(RulesWriteGroupPrefix+"*")
}

// CanRead reports whether the caller may read a group.
func (ScopedRulesAuthorizer) CanRead(ctx context.Context, partition string) bool {
	id, ok := authz.IdentityFromContext(ctx)
	if !ok || id.IsAnonymous() {
		return false
	}
	if id.HasGroup(RulesAdminGroup) {
		return true
	}
	partition = strings.TrimSpace(partition)
	return id.HasGroup(RulesWriteGroupPrefix+partition) ||
		id.HasGroup(RulesReadGroupPrefix+partition) ||
		id.HasGroup(RulesWriteGroupPrefix+"*") ||
		id.HasGroup(RulesReadGroupPrefix+"*")
}

func normalizePartitions(values []string) []string {
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

func normalizeRulesSHA256(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(strings.ToLower(raw), "sha256:")
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("sha256 must be 64 hex characters")
	}
	return hex.EncodeToString(decoded), nil
}

func rulesTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
