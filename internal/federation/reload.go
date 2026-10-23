package federation

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// CatalogReloader periodically checks a snapshot store and atomically swaps the
// manager's catalog when the version changes.
type CatalogReloader struct {
	Store    SnapshotStore
	Manager  *Manager
	Source   string
	Interval time.Duration
	Logger   *log.Logger
	Metrics  *CatalogMetrics
}

func NewCatalogReloader(store SnapshotStore, manager *Manager, source string, interval time.Duration, logger *log.Logger) *CatalogReloader {
	if logger == nil {
		logger = log.Default()
	}
	if strings.TrimSpace(source) == "" {
		source = "catalog"
	}
	return &CatalogReloader{
		Store:    store,
		Manager:  manager,
		Source:   source,
		Interval: interval,
		Logger:   logger,
	}
}

// Run reloads on a ticker until the context is canceled.
func (r *CatalogReloader) Run(ctx context.Context) {
	if r == nil || r.Store == nil || r.Manager == nil {
		return
	}
	interval := r.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Reload(ctx); err != nil {
				r.Logger.Printf("federation catalog reload failed: %v", err)
			}
		}
	}
}

// Reload performs one version check and swap. It is exported so tests and an
// admin refresh endpoint can trigger it synchronously.
func (r *CatalogReloader) Reload(ctx context.Context) (err error) {
	if r == nil || r.Store == nil || r.Manager == nil {
		return errors.New("federation reloader is not configured")
	}
	start := time.Now()
	changed := false
	defer func() {
		if r.Metrics == nil {
			return
		}
		switch {
		case err != nil:
			r.Metrics.RecordReload("error", time.Since(start))
		case changed:
			r.Metrics.RecordReload("success", time.Since(start))
		}
	}()

	current := r.Manager.Version()
	if versioner, ok := r.Store.(SnapshotVersioner); ok {
		version, versionErr := versioner.Version(ctx)
		if versionErr != nil {
			if errors.Is(versionErr, ErrSnapshotNotFound) {
				return nil
			}
			return versionErr
		}
		if strings.TrimSpace(version) == current {
			return nil
		}
	}
	changed = true

	snapshot, err := r.Store.Load(ctx)
	if err != nil {
		if errors.Is(err, ErrSnapshotNotFound) {
			changed = false
			return nil
		}
		return err
	}
	if strings.TrimSpace(snapshot.Version) == current {
		changed = false
		return nil
	}
	catalog, err := NewCatalog(snapshot.Targets, snapshot.Version)
	if err != nil {
		return fmt.Errorf("build reloaded catalog: %w", err)
	}
	return r.Manager.Reload(catalog, fmt.Sprintf("%s:%s", r.Source, snapshot.Version))
}
