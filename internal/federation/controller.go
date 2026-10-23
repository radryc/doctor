package federation

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"path"
	"strings"
	"time"
)

// ControllerConfig configures the federation catalog controller.
type ControllerConfig struct {
	GuardianURL   string
	GuardianToken string

	SnapshotRoot string
	Tenant       string
	MonoFSToken  string

	Interval             time.Duration
	MetadataTTL          time.Duration
	MetadataTimeout      time.Duration
	MetadataConcurrency  int
	SnapshotShardTargets int
	TokenShardCount      int
	LeaseTTL             time.Duration
	Holder               string
	DryRun               bool
	Logger               *log.Logger
}

// GuardianSource is the machine feed consumed by the catalog controller.
type GuardianSource interface {
	Pushers(ctx context.Context) ([]PusherInfo, map[string]string, error)
	Partitions(ctx context.Context) ([]PartitionInfo, error)
}

// MetadataSource fetches discovery metadata for one pusher.
type MetadataSource interface {
	Fetch(ctx context.Context, endpoint, token string) (TargetMeta, error)
}

// Controller rebuilds and publishes the federation catalog from Guardian and
// pusher metadata on a fixed cadence.
type Controller struct {
	Config        ControllerConfig
	Guardian      GuardianSource
	Metadata      MetadataSource
	Files         CatalogFileStore
	Store         SnapshotStore
	Tokens        *TokenBundleWriter
	Lease         *LeaderLease
	MetadataCache *MetadataCache
}

// NewController wires the default production dependencies.
func NewController(cfg ControllerConfig, files CatalogFileStore) (*Controller, error) {
	if files == nil {
		return nil, errors.New("federation controller file store is required")
	}
	if strings.TrimSpace(cfg.SnapshotRoot) == "" {
		cfg.SnapshotRoot = "/doctor/v1/federation"
	}
	cfg.SnapshotRoot = "/" + trimSlashes(cfg.SnapshotRoot)
	if strings.TrimSpace(cfg.Tenant) == "" {
		cfg.Tenant = "default"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.MetadataTTL <= 0 {
		cfg.MetadataTTL = time.Hour
	}
	if cfg.MetadataTimeout <= 0 {
		cfg.MetadataTimeout = 5 * time.Second
	}
	if cfg.MetadataConcurrency <= 0 {
		cfg.MetadataConcurrency = 32
	}
	if cfg.SnapshotShardTargets <= 0 {
		cfg.SnapshotShardTargets = 500
	}
	if cfg.TokenShardCount <= 0 {
		cfg.TokenShardCount = 256
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = cfg.Interval * 2
	}
	if cfg.LeaseTTL < 90*time.Second {
		cfg.LeaseTTL = 90 * time.Second
	}
	if strings.TrimSpace(cfg.Holder) == "" {
		cfg.Holder = "doctor-federation-controller"
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}

	snapshotStore := NewMonoFSSnapshotStore(files, cfg.MonoFSToken, cfg.SnapshotRoot, cfg.Tenant)
	snapshotStore.ShardTargets = cfg.SnapshotShardTargets
	cache, err := LoadMetadataCache(context.Background(), files, path.Join(cfg.SnapshotRoot, cfg.Tenant, "controller", "metadata.json"))
	if err != nil {
		cfg.Logger.Printf("federation controller: metadata cache reset: %v", err)
		cache = &MetadataCache{entries: make(map[string]MetadataCacheEntry)}
	}
	controller := &Controller{
		Config:        cfg,
		Guardian:      NewGuardianFeed(cfg.GuardianURL, cfg.GuardianToken, nil),
		Metadata:      NewMetadataFetcher(nil),
		Files:         files,
		Store:         snapshotStore,
		Tokens:        NewTokenBundleWriter(files, cfg.MonoFSToken, path.Join(cfg.SnapshotRoot, "tokens"), cfg.Tenant, cfg.TokenShardCount),
		Lease:         NewLeaderLease(files, cfg.MonoFSToken, cfg.SnapshotRoot, cfg.Tenant, cfg.Holder, cfg.LeaseTTL),
		MetadataCache: cache,
	}
	return controller, nil
}

// Run acquires the leader lease and rebuilds the catalog until the context is
// canceled. A once-only sync is available for CronJob/CI use.
func (c *Controller) Run(ctx context.Context, once bool) error {
	if c == nil {
		return errors.New("federation controller is nil")
	}
	if once {
		acquired, err := c.Lease.Acquire(ctx)
		if err != nil {
			return err
		}
		if !acquired {
			return fmt.Errorf("federation controller leader lease is held by another instance")
		}
		defer func() { _ = c.Lease.Release(context.Background()) }()
		return c.Sync(ctx)
	}

	for {
		acquired, err := c.Lease.Acquire(ctx)
		if err != nil {
			c.Config.Logger.Printf("federation controller: leader lease failed: %v", err)
		} else if !acquired {
			c.Config.Logger.Printf("federation controller: another replica holds the leader lease")
		} else if err := c.Sync(ctx); err != nil {
			c.Config.Logger.Printf("federation controller: sync failed: %v", err)
		}
		wait := c.Config.Interval
		if jitterRange := wait / 10; jitterRange > 0 {
			wait += time.Duration(rand.Int63n(int64(jitterRange)))
		}
		select {
		case <-ctx.Done():
			_ = c.Lease.Release(context.Background())
			return nil
		case <-time.After(wait):
		}
	}
}

// Sync performs one Guardian -> metadata -> catalog -> publish cycle.
func (c *Controller) Sync(ctx context.Context) error {
	if c == nil || c.Guardian == nil {
		return errors.New("federation controller Guardian feed is required")
	}
	start := time.Now()
	pushers, tokensByPusher, err := c.Guardian.Pushers(ctx)
	if err != nil {
		return err
	}
	partitions, err := c.Guardian.Partitions(ctx)
	if err != nil {
		return err
	}
	if len(partitions) == 0 {
		return errors.New("guardian partition feed returned no partitions")
	}

	meta, warnings := c.refreshMetadata(ctx, pushers, tokensByPusher)
	if err := ctx.Err(); err != nil {
		return err
	}
	now := c.now()
	result, err := BuildCatalog(BuildInput{
		Version:    fmt.Sprintf("controller-%d", now.UnixNano()),
		Partitions: partitions,
		Pushers:    pushers,
		Meta:       meta,
	})
	if err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		warnings = append(warnings, warning)
	}
	for _, warning := range warnings {
		c.Config.Logger.Printf("federation controller warning: %s", warning)
	}

	targetTokens := targetTokens(result.Snapshot, pushers, tokensByPusher)
	if c.Config.DryRun {
		c.Config.Logger.Printf("federation controller dry-run: targets=%d partitions=%d tokens=%d warnings=%d",
			len(result.Snapshot.Targets), len(partitions), len(targetTokens), len(warnings))
		return nil
	}

	changedShards, err := c.Tokens.Write(ctx, targetTokens)
	if err != nil {
		return err
	}
	expected, err := c.expectedVersion(ctx)
	if err != nil {
		return err
	}
	version, err := c.Store.Save(ctx, result.Snapshot, expected)
	if err != nil {
		return err
	}
	if err := c.saveMetadataCache(ctx); err != nil {
		c.Config.Logger.Printf("federation controller: metadata cache save failed: %v", err)
	}
	c.Config.Logger.Printf("federation controller: published version=%s targets=%d partitions=%d token_shards=%d warnings=%d elapsed=%s",
		version, len(result.Snapshot.Targets), len(partitions), changedShards, len(warnings), time.Since(start).Round(time.Millisecond))
	return nil
}

func (c *Controller) expectedVersion(ctx context.Context) (string, error) {
	if versioner, ok := c.Store.(SnapshotVersioner); ok {
		version, err := versioner.Version(ctx)
		if err == nil {
			return version, nil
		}
		if errors.Is(err, ErrSnapshotNotFound) {
			return "absent", nil
		}
		return "", err
	}
	snapshot, err := c.Store.Load(ctx)
	if err != nil {
		if errors.Is(err, ErrSnapshotNotFound) {
			return "absent", nil
		}
		return "", err
	}
	return snapshot.Version, nil
}

func targetTokens(snapshot Snapshot, pushers []PusherInfo, tokens map[string]string) map[string]string {
	out := make(map[string]string)
	for _, target := range snapshot.Targets {
		best, ok := bestPusherFor(pushers, target.Account, target.Region)
		if !ok {
			continue
		}
		if token := strings.TrimSpace(tokens[best.Name]); token != "" {
			out[target.ID] = token
		}
	}
	return out
}

func bestPusherFor(pushers []PusherInfo, account, region string) (PusherInfo, bool) {
	var best PusherInfo
	found := false
	for _, pusher := range pushers {
		if !strings.EqualFold(strings.TrimSpace(pusher.Account), strings.TrimSpace(account)) ||
			!strings.EqualFold(strings.TrimSpace(pusher.Region), strings.TrimSpace(region)) {
			continue
		}
		if pusher.Endpoint == "" {
			continue
		}
		if !found || betterPusher(pusher, best) {
			best = pusher
			found = true
		}
	}
	return best, found
}

func (c *Controller) now() time.Time {
	return time.Now().UTC()
}
