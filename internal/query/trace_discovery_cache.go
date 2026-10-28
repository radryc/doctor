package query

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/proxy"
	"github.com/radryc/doctor/internal/tempo"
)

const defaultTraceDiscoveryCacheTTL = 30 * time.Second

type traceTagValuesCacheEntry struct {
	values   map[string]struct{}
	warnings []proxy.Warning
	expires  time.Time
}

type traceTagsCacheEntry struct {
	tags     map[string]struct{}
	warnings []proxy.Warning
	expires  time.Time
}

type traceScopesCacheEntry struct {
	scopes   []tempo.TagScope
	warnings []proxy.Warning
	expires  time.Time
}

// SetTraceDiscoveryCacheTTL overrides the trace tag/service discovery cache
// TTL. Non-positive values leave the default in place.
func (s *Service) SetTraceDiscoveryCacheTTL(ttl time.Duration) {
	if s != nil && ttl > 0 {
		s.traceDiscoveryCacheTTL = ttl
	}
}

// SetTraceDiscoveryWindow bounds the span/manifest discovery scan used when
// MonoFS cannot answer from its trace-discovery index. Non-positive values
// leave the default in place.
func (s *Service) SetTraceDiscoveryWindow(window time.Duration) {
	if s != nil && window > 0 {
		s.traceDiscoveryWindow = window
	}
}

func (s *Service) traceWindow() time.Duration {
	if s == nil || s.traceDiscoveryWindow <= 0 {
		return time.Hour
	}
	return s.traceDiscoveryWindow
}

func (s *Service) traceDiscoveryTTL() time.Duration {
	if s == nil || s.traceDiscoveryCacheTTL <= 0 {
		return defaultTraceDiscoveryCacheTTL
	}
	return s.traceDiscoveryCacheTTL
}

func traceDiscoveryIdentityKey(ctx context.Context) string {
	id := identityFromContext(ctx)
	groups := append([]string(nil), id.Groups...)
	sort.Strings(groups)
	return strings.Join([]string{id.Subject, id.Email, id.ClientID, strings.Join(groups, ",")}, "\x00")
}

func (s *Service) traceDiscoveryCacheKey(ctx context.Context, tenant, kind, extra string) string {
	return kind + "\x00" + tenant + "\x00" + traceDiscoveryIdentityKey(ctx) + "\x00" + extra
}

func cloneStringSet(in map[string]struct{}) map[string]struct{} {
	if len(in) == 0 {
		return map[string]struct{}{}
	}
	out := make(map[string]struct{}, len(in))
	for key := range in {
		out[key] = struct{}{}
	}
	return out
}

func cloneWarnings(in []proxy.Warning) []proxy.Warning {
	return append([]proxy.Warning(nil), in...)
}

func (s *Service) cachedTraceTagValues(ctx context.Context, tenant, tag string) (map[string]struct{}, []proxy.Warning, bool) {
	if s == nil {
		return nil, nil, false
	}
	key := s.traceDiscoveryCacheKey(ctx, tenant, "tag-values", tag)
	now := time.Now()
	s.traceDiscoveryMu.Lock()
	defer s.traceDiscoveryMu.Unlock()
	entry, ok := s.traceTagValuesCache[key]
	if !ok || now.After(entry.expires) {
		if ok {
			delete(s.traceTagValuesCache, key)
		}
		return nil, nil, false
	}
	return cloneStringSet(entry.values), cloneWarnings(entry.warnings), true
}

func (s *Service) storeTraceTagValues(ctx context.Context, tenant, tag string, values map[string]struct{}, warnings []proxy.Warning) {
	if s == nil {
		return
	}
	key := s.traceDiscoveryCacheKey(ctx, tenant, "tag-values", tag)
	s.traceDiscoveryMu.Lock()
	defer s.traceDiscoveryMu.Unlock()
	if s.traceTagValuesCache == nil {
		s.traceTagValuesCache = make(map[string]traceTagValuesCacheEntry)
	}
	s.traceTagValuesCache[key] = traceTagValuesCacheEntry{
		values:   cloneStringSet(values),
		warnings: cloneWarnings(warnings),
		expires:  time.Now().Add(s.traceDiscoveryTTL()),
	}
}

func (s *Service) cachedTraceTags(ctx context.Context, tenant string) (map[string]struct{}, []proxy.Warning, bool) {
	if s == nil {
		return nil, nil, false
	}
	key := s.traceDiscoveryCacheKey(ctx, tenant, "tags", "")
	now := time.Now()
	s.traceDiscoveryMu.Lock()
	defer s.traceDiscoveryMu.Unlock()
	entry, ok := s.traceTagsCache[key]
	if !ok || now.After(entry.expires) {
		if ok {
			delete(s.traceTagsCache, key)
		}
		return nil, nil, false
	}
	return cloneStringSet(entry.tags), cloneWarnings(entry.warnings), true
}

func (s *Service) storeTraceTags(ctx context.Context, tenant string, tags map[string]struct{}, warnings []proxy.Warning) {
	if s == nil {
		return
	}
	key := s.traceDiscoveryCacheKey(ctx, tenant, "tags", "")
	s.traceDiscoveryMu.Lock()
	defer s.traceDiscoveryMu.Unlock()
	if s.traceTagsCache == nil {
		s.traceTagsCache = make(map[string]traceTagsCacheEntry)
	}
	s.traceTagsCache[key] = traceTagsCacheEntry{
		tags:     cloneStringSet(tags),
		warnings: cloneWarnings(warnings),
		expires:  time.Now().Add(s.traceDiscoveryTTL()),
	}
}

func (s *Service) cachedTraceScopes(ctx context.Context, tenant string) ([]tempo.TagScope, []proxy.Warning, bool) {
	if s == nil {
		return nil, nil, false
	}
	key := s.traceDiscoveryCacheKey(ctx, tenant, "scopes", "")
	now := time.Now()
	s.traceDiscoveryMu.Lock()
	defer s.traceDiscoveryMu.Unlock()
	entry, ok := s.traceScopesCache[key]
	if !ok || now.After(entry.expires) {
		if ok {
			delete(s.traceScopesCache, key)
		}
		return nil, nil, false
	}
	return append([]tempo.TagScope(nil), entry.scopes...), cloneWarnings(entry.warnings), true
}

func (s *Service) storeTraceScopes(ctx context.Context, tenant string, scopes []tempo.TagScope, warnings []proxy.Warning) {
	if s == nil {
		return
	}
	key := s.traceDiscoveryCacheKey(ctx, tenant, "scopes", "")
	s.traceDiscoveryMu.Lock()
	defer s.traceDiscoveryMu.Unlock()
	if s.traceScopesCache == nil {
		s.traceScopesCache = make(map[string]traceScopesCacheEntry)
	}
	s.traceScopesCache[key] = traceScopesCacheEntry{
		scopes:   append([]tempo.TagScope(nil), scopes...),
		warnings: cloneWarnings(warnings),
		expires:  time.Now().Add(s.traceDiscoveryTTL()),
	}
}
