package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/store"
)

// LeaderLease is a MonoFS CAS-backed controller lease. It avoids running two
// catalog publishers against the same snapshot root.
type LeaderLease struct {
	Files     CatalogFileStore
	Token     string
	Path      string
	Holder    string
	TTL       time.Duration
	Principal string
	Now       func() time.Time
}

type leaderLeaseRecord struct {
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewLeaderLease builds a lease at <root>/<tenant>/controller/leader.json.
func NewLeaderLease(files CatalogFileStore, token, root, tenant, holder string, ttl time.Duration) *LeaderLease {
	if root == "" {
		root = "/doctor/v1/federation"
	}
	if tenant == "" {
		tenant = "default"
	}
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	return &LeaderLease{
		Files:     files,
		Token:     token,
		Path:      path.Join("/"+trimSlashes(root), tenant, "controller", "leader.json"),
		Holder:    strings.TrimSpace(holder),
		TTL:       ttl,
		Principal: "doctor-federation-controller",
		Now:       time.Now,
	}
}

// Acquire attempts to take or renew the lease. It returns false when another
// live holder owns it.
func (l *LeaderLease) Acquire(ctx context.Context) (bool, error) {
	if l == nil || l.Files == nil {
		return false, fmt.Errorf("leader lease file store is required")
	}
	if l.Holder == "" {
		return false, fmt.Errorf("leader lease holder is required")
	}
	now := l.now()
	current, version, err := l.read(ctx)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err == nil && strings.TrimSpace(current.Holder) != l.Holder && current.ExpiresAt.After(now) {
		return false, nil
	}
	record := leaderLeaseRecord{Holder: l.Holder, ExpiresAt: now.Add(l.TTL), UpdatedAt: now}
	raw, marshalErr := json.Marshal(record)
	if marshalErr != nil {
		return false, marshalErr
	}
	expected := version
	if expected == "" {
		expected = "absent"
	}
	if _, writeErr := l.Files.UpsertPaths(ctx, l.Token, []store.PathWrite{{
		LogicalPath:       l.Path,
		Content:           raw,
		ExpectedVersionID: expected,
	}}, store.MutationContext{PrincipalID: l.Principal, Reason: "federation controller leader lease"}); writeErr != nil {
		return false, writeErr
	}
	return true, nil
}

// Renew extends the lease only while this holder still owns it.
func (l *LeaderLease) Renew(ctx context.Context) (bool, error) {
	return l.Acquire(ctx)
}

// Release expires the lease when this holder owns it.
func (l *LeaderLease) Release(ctx context.Context) error {
	if l == nil || l.Files == nil {
		return nil
	}
	current, version, err := l.read(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(current.Holder) != l.Holder {
		return nil
	}
	now := l.now()
	record := leaderLeaseRecord{Holder: l.Holder, ExpiresAt: now.Add(-time.Second), UpdatedAt: now}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = l.Files.UpsertPaths(ctx, l.Token, []store.PathWrite{{
		LogicalPath:       l.Path,
		Content:           raw,
		ExpectedVersionID: version,
	}}, store.MutationContext{PrincipalID: l.Principal, Reason: "federation controller leader release"})
	return err
}

func (l *LeaderLease) read(ctx context.Context) (leaderLeaseRecord, string, error) {
	raw, err := l.Files.ReadFile(ctx, l.Path)
	if err != nil {
		return leaderLeaseRecord{}, "", err
	}
	var record leaderLeaseRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return leaderLeaseRecord{}, "", fmt.Errorf("parse leader lease: %w", err)
	}
	version := ""
	if versions, err := l.Files.ListVersions(ctx, l.Token, l.Path); err == nil && len(versions) > 0 {
		version = versions[0].VersionID
	}
	return record, version, nil
}

func (l *LeaderLease) now() time.Time {
	if l != nil && l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}
