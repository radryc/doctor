package access

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

// OwnersSource loads OWNERS files for a directory. A directory without an
// OWNERS file returns (nil, nil).
type OwnersSource interface {
	LoadOwners(ctx context.Context, dir string) (*authz.OwnersFile, error)
	// Version returns a change token for a directory, or "" when unavailable.
	Version(ctx context.Context, dir string) (string, error)
}

// Owners resolves access from OWNERS files. View access is granted when the
// identity appears in the viewers, ingesters, or maintainers roles of the
// nearest governing OWNERS file or any ancestor file.
type Owners struct {
	Source   OwnersSource
	Mapping  authz.TeamMapping
	Root     string
	CacheTTL time.Duration

	mu    sync.Mutex
	cache map[string]*ownersEntry
}

type ownersEntry struct {
	version  string
	grants   []authz.Grant
	loadedAt time.Time
}

// NewOwners builds an OWNERS resolver.
func NewOwners(source OwnersSource, mapping authz.TeamMapping, root string, cacheTTL time.Duration) *Owners {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" {
		root = "doctor/v1/endpoints"
	}
	return &Owners{
		Source:   source,
		Mapping:  mapping,
		Root:     root,
		CacheTTL: TTLDuration(cacheTTL),
		cache:    make(map[string]*ownersEntry),
	}
}

// ResolverName implements Resolver.
func (o *Owners) ResolverName() string { return "owners" }

// Allowed implements Resolver.
func (o *Owners) Allowed(ctx context.Context, id authz.Identity, ep Endpoint, signal Signal) (Decision, error) {
	decision := Decision{Endpoint: ep.Name, Signal: signal, Resolver: o.ResolverName()}
	if id.IsAnonymous() {
		decision.Reason = "anonymous"
		return decision, nil
	}
	if o.Source == nil {
		decision.Reason = "no owners source configured"
		return decision, nil
	}
	for _, dir := range ancestorDirs(o.ownersPath(ep)) {
		grants, err := o.grantsFor(ctx, dir, ep.Name)
		if err != nil {
			return decision, err
		}
		if grants == nil {
			continue
		}
		if grantsAllow(grants, id, ep.Name) {
			decision.Allowed = true
			decision.Reason = "listed in OWNERS at " + dir
			return decision, nil
		}
	}
	decision.Reason = "not listed in any governing OWNERS file"
	return decision, nil
}

func (o *Owners) ownersPath(ep Endpoint) string {
	path := strings.Trim(strings.TrimSpace(ep.OwnersPath), "/")
	if path != "" {
		return path
	}
	return o.Root + "/" + strings.Trim(ep.Name, "/")
}

func (o *Owners) grantsFor(ctx context.Context, dir, partition string) ([]authz.Grant, error) {
	version, _ := o.Source.Version(ctx, dir)
	now := time.Now()

	o.mu.Lock()
	if entry := o.cache[dir]; entry != nil && entry.version == version && now.Sub(entry.loadedAt) < o.CacheTTL {
		grants := entry.grants
		o.mu.Unlock()
		return grants, nil
	}
	o.mu.Unlock()

	owners, err := o.Source.LoadOwners(ctx, dir)
	if err != nil {
		return nil, err
	}
	var grants []authz.Grant
	if owners != nil {
		grants, _ = authz.CompileGrants(partition, owners, o.Mapping)
	}

	o.mu.Lock()
	o.cache[dir] = &ownersEntry{version: version, grants: grants, loadedAt: now}
	o.mu.Unlock()
	return grants, nil
}

// Invalidate drops the cache, forcing a reload on the next decision.
func (o *Owners) Invalidate() {
	o.mu.Lock()
	o.cache = make(map[string]*ownersEntry)
	o.mu.Unlock()
}

func grantsAllow(grants []authz.Grant, id authz.Identity, partition string) bool {
	for _, grant := range grants {
		if grant.Partition != authz.WildcardPartition && grant.Partition != partition {
			continue
		}
		if !grant.Role.Allows(authz.ActionView) {
			continue
		}
		if grant.Subject != "" {
			if grant.Subject == id.Subject || grant.Subject == id.ClientID || (id.Email != "" && grant.Subject == id.Email) {
				return true
			}
			continue
		}
		if grant.Group != "" && id.HasGroup(grant.Group) {
			return true
		}
	}
	return false
}

// ancestorDirs returns path and each ancestor, deepest first, ending at "".
func ancestorDirs(path string) []string {
	p := strings.Trim(strings.TrimSpace(path), "/")
	dirs := make([]string, 0, strings.Count(p, "/")+1)
	for {
		dirs = append(dirs, p)
		if p == "" {
			break
		}
		if idx := strings.LastIndexByte(p, '/'); idx >= 0 {
			p = p[:idx]
		} else {
			p = ""
		}
	}
	return dirs
}
