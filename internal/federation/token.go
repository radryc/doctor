package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

var (
	// ErrTokenRefUnsupported means this resolver does not handle the target's
	// token reference scheme. A ChainTokenResolver uses it to try the next
	// resolver.
	ErrTokenRefUnsupported = errors.New("federation token reference is not supported by this resolver")
	// ErrTokenNotFound means the resolver understood the reference but no
	// token was available.
	ErrTokenNotFound = errors.New("federation target token not found")
)

// TokenResolver resolves a target's bearer token without putting tokens in the
// catalog snapshot. Implementations must be safe for concurrent use.
type TokenResolver interface {
	Resolve(ctx context.Context, target Target) (string, error)
	Invalidate(target Target)
}

// targetTokenSource adapts a TokenResolver to the awsstore.TokenSource
// interface for one target.
type targetTokenSource struct {
	resolver TokenResolver
	target   Target
}

func (s targetTokenSource) Token(ctx context.Context) (string, error) {
	return s.resolver.Resolve(ctx, s.target)
}

func (s targetTokenSource) Invalidate() {
	s.resolver.Invalidate(s.target)
}

// EnvTokenResolver is the development/demo resolver for token_env. It is not
// suitable for a 50k fleet because it requires one environment variable per
// target.
type EnvTokenResolver struct{}

func (EnvTokenResolver) Resolve(_ context.Context, target Target) (string, error) {
	if target.Token != "" {
		return target.Token, nil
	}
	if target.TokenEnv == "" {
		return "", ErrTokenRefUnsupported
	}
	value := strings.TrimSpace(os.Getenv(target.TokenEnv))
	if value == "" {
		return "", fmt.Errorf("%w: environment variable %s is empty", ErrTokenNotFound, target.TokenEnv)
	}
	return value, nil
}

func (EnvTokenResolver) Invalidate(Target) {}

// ChainTokenResolver tries resolvers in order. Unsupported references and
// not-found results fall through; other errors also fall through so a bundle
// can remain available when Guardian is temporarily down.
type ChainTokenResolver []TokenResolver

func (chain ChainTokenResolver) Resolve(ctx context.Context, target Target) (string, error) {
	var lastErr, notFoundErr error
	for _, resolver := range chain {
		if resolver == nil {
			continue
		}
		token, err := resolver.Resolve(ctx, target)
		if err == nil {
			return token, nil
		}
		if errors.Is(err, ErrTokenNotFound) && notFoundErr == nil {
			notFoundErr = err
		}
		if !errors.Is(err, ErrTokenRefUnsupported) {
			lastErr = err
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	if notFoundErr != nil {
		return "", notFoundErr
	}
	return "", ErrTokenRefUnsupported
}

func (chain ChainTokenResolver) Invalidate(target Target) {
	for _, resolver := range chain {
		if resolver != nil {
			resolver.Invalidate(target)
		}
	}
}

// GuardianTokenResolverOptions configures the lazy Guardian pusher-token
// resolver.
type GuardianTokenResolverOptions struct {
	BaseURL     string
	Token       string
	HTTPClient  *http.Client
	CacheTTL    time.Duration
	NegativeTTL time.Duration
}

// GuardianTokenResolver fetches one pusher token on demand from Guardian's
// machine-to-machine pusher endpoint. It never loads all 50k tokens.
type GuardianTokenResolver struct {
	baseURL     string
	token       string
	httpClient  *http.Client
	cacheTTL    time.Duration
	negativeTTL time.Duration

	mu       sync.Mutex
	cache    map[string]guardianTokenEntry
	inflight map[string]chan struct{}
}

type guardianTokenEntry struct {
	token   string
	err     error
	expires time.Time
}

// NewGuardianTokenResolver builds a Guardian token resolver.
func NewGuardianTokenResolver(opts GuardianTokenResolverOptions) *GuardianTokenResolver {
	cacheTTL := opts.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = 5 * time.Minute
	}
	negativeTTL := opts.NegativeTTL
	if negativeTTL <= 0 {
		negativeTTL = 30 * time.Second
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &GuardianTokenResolver{
		baseURL:     strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"),
		token:       strings.TrimSpace(opts.Token),
		httpClient:  client,
		cacheTTL:    cacheTTL,
		negativeTTL: negativeTTL,
		cache:       make(map[string]guardianTokenEntry),
		inflight:    make(map[string]chan struct{}),
	}
}

func (r *GuardianTokenResolver) Resolve(ctx context.Context, target Target) (string, error) {
	if target.Token != "" {
		return target.Token, nil
	}
	if target.TokenRef == "" && target.TokenEnv != "" {
		return "", ErrTokenRefUnsupported
	}
	name, err := guardianPusherName(target)
	if err != nil {
		return "", err
	}
	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.cache[name]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.token, entry.err
	}
	if ch, ok := r.inflight[name]; ok {
		r.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return r.Resolve(ctx, target)
	}
	ch := make(chan struct{})
	r.inflight[name] = ch
	r.mu.Unlock()

	token, fetchErr := r.fetch(ctx, name)
	expires := time.Now().Add(r.cacheTTL)
	if fetchErr != nil {
		expires = time.Now().Add(r.negativeTTL)
	}

	r.mu.Lock()
	r.cache[name] = guardianTokenEntry{token: token, err: fetchErr, expires: expires}
	delete(r.inflight, name)
	close(ch)
	r.mu.Unlock()
	return token, fetchErr
}

func (r *GuardianTokenResolver) Invalidate(target Target) {
	name, err := guardianPusherName(target)
	if err != nil {
		return
	}
	r.mu.Lock()
	delete(r.cache, name)
	r.mu.Unlock()
}

func (r *GuardianTokenResolver) fetch(ctx context.Context, name string) (string, error) {
	if r.baseURL == "" {
		return "", fmt.Errorf("%w: guardian API URL is not configured", ErrTokenNotFound)
	}
	requestURL := r.baseURL + "/api/m2m/pushers/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if r.token != "" {
		req.Header.Set("X-Guardian-Internal-Token", r.token)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("%w: pusher %s", ErrTokenNotFound, name)
	default:
		return "", fmt.Errorf("guardian pusher token HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Token    string `json:"token"`
		TokenEnv string `json:"tokenEnv"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode guardian pusher token: %w", err)
	}
	if token := strings.TrimSpace(payload.Token); token != "" {
		return token, nil
	}
	if env := strings.TrimSpace(payload.TokenEnv); env != "" {
		if token := strings.TrimSpace(os.Getenv(env)); token != "" {
			return token, nil
		}
	}
	return "", fmt.Errorf("%w: pusher %s has no token", ErrTokenNotFound, name)
}

func guardianPusherName(target Target) (string, error) {
	ref := strings.TrimSpace(target.TokenRef)
	if ref == "" {
		if target.TokenEnv != "" {
			return "", ErrTokenRefUnsupported
		}
		// Compatibility for catalogs that use the target ID as the pusher ID.
		return strings.TrimSpace(target.ID), nil
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("parse token_ref %q: %w", ref, err)
	}
	if parsed.Scheme != "" && !strings.EqualFold(parsed.Scheme, "guardian-pusher") {
		return "", ErrTokenRefUnsupported
	}
	name := strings.Trim(strings.TrimSpace(parsed.Host+parsed.Path), "/")
	if name == "" {
		return "", fmt.Errorf("%w: empty guardian pusher reference", ErrTokenRefUnsupported)
	}
	return name, nil
}

// BundleTokenResolverOptions configures the sharded MonoFS bundle fallback.
type BundleTokenResolverOptions struct {
	Files      CatalogFileStore
	Root       string
	Tenant     string
	ShardCount int
	CacheTTL   time.Duration
}

// BundleTokenResolver reads sharded token bundles from MonoFS. It is a
// fallback/cache path, not the primary secret store.
type BundleTokenResolver struct {
	files      CatalogFileStore
	root       string
	tenant     string
	shardCount int
	cacheTTL   time.Duration

	mu      sync.Mutex
	shards  map[int]bundleTokenShard
	loading map[int]chan struct{}
}

type bundleTokenShard struct {
	tokens  map[string]string
	expires time.Time
}

// NewBundleTokenResolver builds a sharded MonoFS bundle resolver.
func NewBundleTokenResolver(opts BundleTokenResolverOptions) *BundleTokenResolver {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		root = "/doctor/v1/federation/tokens"
	}
	tenant := strings.TrimSpace(opts.Tenant)
	if tenant == "" {
		tenant = "default"
	}
	shardCount := opts.ShardCount
	if shardCount <= 0 {
		shardCount = 256
	}
	cacheTTL := opts.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = 5 * time.Minute
	}
	return &BundleTokenResolver{
		files:      opts.Files,
		root:       "/" + strings.Trim(root, "/"),
		tenant:     tenant,
		shardCount: shardCount,
		cacheTTL:   cacheTTL,
		shards:     make(map[int]bundleTokenShard),
		loading:    make(map[int]chan struct{}),
	}
}

func (r *BundleTokenResolver) Resolve(ctx context.Context, target Target) (string, error) {
	if target.Token != "" {
		return target.Token, nil
	}
	if target.TokenEnv != "" {
		return "", ErrTokenRefUnsupported
	}
	if ref := strings.TrimSpace(target.TokenRef); ref != "" {
		parsed, err := url.Parse(ref)
		if err != nil {
			return "", fmt.Errorf("parse token_ref %q: %w", ref, err)
		}
		if parsed.Scheme != "" && !strings.EqualFold(parsed.Scheme, "monofs-token") {
			return "", ErrTokenRefUnsupported
		}
	}
	if r.files == nil {
		return "", fmt.Errorf("%w: MonoFS file store is not configured", ErrTokenNotFound)
	}
	shard := r.shardFor(target.ID)
	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.shards[shard]; ok && now.Before(entry.expires) {
		token := entry.tokens[target.ID]
		r.mu.Unlock()
		if token == "" {
			return "", fmt.Errorf("%w: target %s in shard %d", ErrTokenNotFound, target.ID, shard)
		}
		return token, nil
	}
	if ch, ok := r.loading[shard]; ok {
		r.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return r.Resolve(ctx, target)
	}
	ch := make(chan struct{})
	r.loading[shard] = ch
	r.mu.Unlock()

	tokens, loadErr := r.loadShard(ctx, shard)
	r.mu.Lock()
	if loadErr == nil {
		r.shards[shard] = bundleTokenShard{tokens: tokens, expires: time.Now().Add(r.cacheTTL)}
	}
	delete(r.loading, shard)
	close(ch)
	r.mu.Unlock()
	if loadErr != nil {
		return "", loadErr
	}
	token := tokens[target.ID]
	if token == "" {
		return "", fmt.Errorf("%w: target %s in shard %d", ErrTokenNotFound, target.ID, shard)
	}
	return token, nil
}

func (r *BundleTokenResolver) Invalidate(target Target) {
	shard := r.shardFor(target.ID)
	r.mu.Lock()
	delete(r.shards, shard)
	r.mu.Unlock()
}

func (r *BundleTokenResolver) shardFor(targetID string) int {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(targetID))
	return int(hash.Sum32() % uint32(r.shardCount))
}

func (r *BundleTokenResolver) loadShard(ctx context.Context, shard int) (map[string]string, error) {
	logicalPath := path.Join(r.root, r.tenant, fmt.Sprintf("%04d.json", shard))
	raw, err := r.files.ReadFile(ctx, logicalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: shard %s", ErrTokenNotFound, logicalPath)
		}
		return nil, fmt.Errorf("read token shard %s: %w", logicalPath, err)
	}
	var bundle struct {
		Tokens map[string]string `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return nil, fmt.Errorf("parse token shard %s: %w", logicalPath, err)
	}
	if bundle.Tokens == nil {
		bundle.Tokens = map[string]string{}
	}
	return bundle.Tokens, nil
}
