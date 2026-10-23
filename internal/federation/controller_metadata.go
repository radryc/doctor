package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/store"
)

// MetadataFetcher retrieves discovery metadata from one pusher's read API.
type MetadataFetcher struct {
	Client     *http.Client
	MaxRetries int
}

// NewMetadataFetcher builds a pusher metadata client.
func NewMetadataFetcher(client *http.Client) *MetadataFetcher {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &MetadataFetcher{Client: client, MaxRetries: 2}
}

// Fetch reads /v1/aws/meta with bounded retries for transient failures.
func (f *MetadataFetcher) Fetch(ctx context.Context, endpoint, token string) (TargetMeta, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return TargetMeta{}, fmt.Errorf("pusher endpoint is required")
	}
	requestURL := endpoint + "/v1/aws/meta"
	var lastErr error
	attempts := 1
	if f != nil && f.MaxRetries > 0 {
		attempts += f.MaxRetries
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return TargetMeta{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 150 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return TargetMeta{}, err
		}
		req.Header.Set("Accept", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		client := f.Client
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var meta TargetMeta
			if err := json.Unmarshal(body, &meta); err != nil {
				return TargetMeta{}, fmt.Errorf("parse pusher metadata: %w", err)
			}
			return meta, nil
		}
		lastErr = fmt.Errorf("pusher metadata HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return TargetMeta{}, lastErr
		}
	}
	return TargetMeta{}, lastErr
}

// MetadataCacheEntry is one pusher's last successful discovery result.
type MetadataCacheEntry struct {
	Meta          TargetMeta `json:"meta"`
	FetchedAt     time.Time  `json:"fetched_at"`
	PusherVersion string     `json:"pusher_version,omitempty"`
}

// MetadataCache is the controller's durable last-known metadata cache.
type MetadataCache struct {
	mu      sync.Mutex
	entries map[string]MetadataCacheEntry
	dirty   bool
}

type metadataCacheDocument struct {
	Entries map[string]MetadataCacheEntry `json:"entries"`
}

// LoadMetadataCache reads the last-known metadata, returning an empty cache if
// no file exists yet. A corrupt cache is treated as empty and surfaced as a
// warning by the controller.
func LoadMetadataCache(ctx context.Context, files CatalogFileStore, logicalPath string) (*MetadataCache, error) {
	cache := &MetadataCache{entries: make(map[string]MetadataCacheEntry)}
	if files == nil || strings.TrimSpace(logicalPath) == "" {
		return cache, nil
	}
	raw, err := files.ReadFile(ctx, logicalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cache, nil
		}
		return cache, err
	}
	var doc metadataCacheDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return cache, fmt.Errorf("parse metadata cache: %w", err)
	}
	for name, entry := range doc.Entries {
		cache.entries[name] = entry
	}
	return cache, nil
}

// Get returns a cached entry.
func (c *MetadataCache) Get(name string) (MetadataCacheEntry, bool) {
	if c == nil {
		return MetadataCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[name]
	return entry, ok
}

// Put stores an entry and marks the cache dirty.
func (c *MetadataCache) Put(name string, entry MetadataCacheEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[name] = entry
	c.dirty = true
}

// Snapshot returns a copy suitable for saving.
func (c *MetadataCache) Snapshot() map[string]MetadataCacheEntry {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]MetadataCacheEntry, len(c.entries))
	for key, value := range c.entries {
		out[key] = value
	}
	return out
}

// Dirty reports whether the cache changed since the last save.
func (c *MetadataCache) Dirty() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirty
}

// MarkSaved clears the dirty flag.
func (c *MetadataCache) MarkSaved() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirty = false
}

func metadataTTL(base time.Duration, name string) time.Duration {
	if base <= 0 {
		base = time.Hour
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	jitterRange := base / 10
	if jitterRange <= 0 {
		return base
	}
	return base + time.Duration(h.Sum32()%uint32(jitterRange))
}

// refreshMetadata refreshes stale entries with a bounded worker pool and keeps
// the last-known metadata when a pusher is unavailable.
func (c *Controller) refreshMetadata(ctx context.Context, pushers []PusherInfo, tokens map[string]string) (map[string]TargetMeta, []string) {
	cache := c.MetadataCache
	if cache == nil {
		cache = &MetadataCache{entries: make(map[string]MetadataCacheEntry)}
		c.MetadataCache = cache
	}
	now := c.now()
	warnings := make([]string, 0)
	type job struct {
		pusher PusherInfo
		token  string
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	workers := c.Config.MetadataConcurrency
	if workers <= 0 {
		workers = 32
	}
	if workers > len(pushers) {
		workers = len(pushers)
	}
	if workers < 1 {
		workers = 1
	}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				if item.pusher.Endpoint == "" || item.pusher.Account == "" || item.pusher.Region == "" {
					continue
				}
				entry, ok := cache.Get(item.pusher.Name)
				if ok && entry.PusherVersion == item.pusher.Version && now.Before(entry.FetchedAt.Add(metadataTTL(c.Config.MetadataTTL, item.pusher.Name))) {
					continue
				}
				fetchCtx, cancel := context.WithTimeout(ctx, c.Config.MetadataTimeout)
				meta, err := c.Metadata.Fetch(fetchCtx, item.pusher.Endpoint, item.token)
				cancel()
				mu.Lock()
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("metadata %s: %v", item.pusher.Name, err))
					mu.Unlock()
					continue
				}
				cache.Put(item.pusher.Name, MetadataCacheEntry{
					Meta:          meta,
					FetchedAt:     c.now(),
					PusherVersion: item.pusher.Version,
				})
				mu.Unlock()
			}
		}()
	}
	for _, pusher := range pushers {
		item := job{pusher: pusher, token: tokens[pusher.Name]}
		select {
		case jobs <- item:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, append(warnings, ctx.Err().Error())
		}
	}
	close(jobs)
	wg.Wait()

	out := make(map[string]TargetMeta, len(pushers))
	for _, pusher := range pushers {
		if entry, ok := cache.Get(pusher.Name); ok {
			out[pusher.Name] = entry.Meta
		}
	}
	return out, warnings
}

// saveMetadataCache persists the cache through the catalog file store. The
// whole document is small enough for the metadata TTL window; catalog shards
// remain the large object.
func (c *Controller) saveMetadataCache(ctx context.Context) error {
	if c.MetadataCache == nil || !c.MetadataCache.Dirty() {
		return nil
	}
	doc := metadataCacheDocument{Entries: c.MetadataCache.Snapshot()}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := c.writeFile(ctx, c.metadataPath(), raw, "federation controller metadata cache"); err != nil {
		return err
	}
	c.MetadataCache.MarkSaved()
	return nil
}

func (c *Controller) metadataPath() string {
	return path.Join(c.Config.SnapshotRoot, c.Config.Tenant, "controller", "metadata.json")
}

// writeFile writes one controller-owned file with a file-version CAS.
func (c *Controller) writeFile(ctx context.Context, logicalPath string, content []byte, reason string) error {
	if c.Files == nil {
		return errors.New("federation controller file store is required")
	}
	expected := "absent"
	if versions, err := c.Files.ListVersions(ctx, c.Config.MonoFSToken, logicalPath); err == nil && len(versions) > 0 {
		expected = versions[0].VersionID
	}
	_, err := c.Files.UpsertPaths(ctx, c.Config.MonoFSToken, []store.PathWrite{{
		LogicalPath:       logicalPath,
		Content:           content,
		ExpectedVersionID: expected,
	}}, store.MutationContext{PrincipalID: c.Config.Holder, Reason: reason})
	return err
}
