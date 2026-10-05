package proxy

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/yaml"
)

// Registry supplies immutable backend snapshots. Implementations must be safe
// for concurrent use.
type Registry interface {
	// Snapshot returns the current backends sorted by (Priority, Name).
	Snapshot() []Backend
	// LastRefresh reports when the snapshot was last rebuilt.
	LastRefresh() time.Time
}

// SliceRegistry is an in-memory registry, used for tests and explicit wiring.
type SliceRegistry struct {
	snapshot atomic.Pointer[[]Backend]
}

// NewSliceRegistry builds a registry from an explicit backend list.
func NewSliceRegistry(backends []Backend) *SliceRegistry {
	r := &SliceRegistry{}
	r.store(backends)
	return r
}

// Snapshot implements Registry.
func (r *SliceRegistry) Snapshot() []Backend {
	if snapshot := r.snapshot.Load(); snapshot != nil {
		return *snapshot
	}
	return nil
}

// LastRefresh implements Registry.
func (r *SliceRegistry) LastRefresh() time.Time { return time.Time{} }

func (r *SliceRegistry) store(backends []Backend) {
	sorted := append([]Backend(nil), backends...)
	SortBackends(sorted)
	r.snapshot.Store(&sorted)
}

// StaticRegistryConfig configures the file-backed registry.
type StaticRegistryConfig struct {
	Path     string
	Interval time.Duration
	Logger   *log.Logger
}

// StaticRegistry loads a backend list from a YAML/JSON file and reloads it when
// the file changes. Invalid reloads keep the previous snapshot.
type StaticRegistry struct {
	path     string
	interval time.Duration
	logger   *log.Logger

	mu          sync.Mutex
	snapshot    atomic.Pointer[[]Backend]
	lastRefresh atomic.Int64
	modTime     atomic.Int64
}

// NewStaticRegistry loads the file immediately and returns the registry.
func NewStaticRegistry(cfg StaticRegistryConfig) (*StaticRegistry, error) {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil, fmt.Errorf("proxy: static registry path is required")
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	r := &StaticRegistry{
		path:     path,
		interval: interval,
		logger:   cfg.Logger,
	}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Snapshot implements Registry.
func (r *StaticRegistry) Snapshot() []Backend {
	if snapshot := r.snapshot.Load(); snapshot != nil {
		return *snapshot
	}
	return nil
}

// LastRefresh implements Registry.
func (r *StaticRegistry) LastRefresh() time.Time {
	value := r.lastRefresh.Load()
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

// Run reloads the file when its modification time changes until ctx is done.
func (r *StaticRegistry) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.ReloadIfChanged(); err != nil && r.logger != nil {
				r.logger.Printf("proxy: static registry reload failed: %v", err)
			}
		}
	}
}

// ReloadIfChanged reloads only when the file modification time moved.
func (r *StaticRegistry) ReloadIfChanged() error {
	info, err := os.Stat(r.path)
	if err != nil {
		return err
	}
	if info.ModTime().UnixNano() == r.modTime.Load() {
		return nil
	}
	return r.Reload()
}

// Reload parses the file and atomically replaces the snapshot.
func (r *StaticRegistry) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("proxy: read registry file: %w", err)
	}
	backends, err := ParseBackends(data)
	if err != nil {
		return err
	}
	info, err := os.Stat(r.path)
	if err == nil {
		r.modTime.Store(info.ModTime().UnixNano())
	}
	sorted := append([]Backend(nil), backends...)
	SortBackends(sorted)
	r.snapshot.Store(&sorted)
	r.lastRefresh.Store(time.Now().UTC().UnixNano())
	return nil
}

// ParseBackends decodes and validates a registry document.
func ParseBackends(data []byte) ([]Backend, error) {
	var doc registryFile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("proxy: parse registry: %w", err)
	}
	if doc.Version != 0 && doc.Version != 1 {
		return nil, fmt.Errorf("proxy: unsupported registry version %d (expected 1)", doc.Version)
	}
	backends := make([]Backend, 0, len(doc.Backends))
	seen := make(map[string]struct{}, len(doc.Backends))
	for i, entry := range doc.Backends {
		backend, err := entry.toBackend()
		if err != nil {
			return nil, fmt.Errorf("proxy: backend[%d]: %w", i, err)
		}
		if _, exists := seen[backend.Name]; exists {
			return nil, fmt.Errorf("proxy: duplicate backend name %q", backend.Name)
		}
		seen[backend.Name] = struct{}{}
		backends = append(backends, backend)
	}
	return backends, nil
}

type registryFile struct {
	Version  int           `json:"version" yaml:"version"`
	Backends []backendFile `json:"backends" yaml:"backends"`
}

type backendFile struct {
	Name       string            `json:"name" yaml:"name"`
	Kind       string            `json:"kind" yaml:"kind"`
	BaseURL    string            `json:"baseUrl" yaml:"baseUrl"`
	Token      string            `json:"token" yaml:"token"`
	Tenant     string            `json:"tenant" yaml:"tenant"`
	Labels     map[string]string `json:"labels" yaml:"labels"`
	Signals    []string          `json:"signals" yaml:"signals"`
	OwnersPath string            `json:"ownersPath" yaml:"ownersPath"`
	Public     *bool             `json:"public" yaml:"public"`
	Timeout    string            `json:"timeout" yaml:"timeout"`
	Priority   int               `json:"priority" yaml:"priority"`
	Modes      map[string]string `json:"modes" yaml:"modes"`
	Policy     string            `json:"policy" yaml:"policy"`
	Index      string            `json:"index" yaml:"index"`
	Schema     map[string]string `json:"schema" yaml:"schema"`
	Project    string            `json:"project" yaml:"project"`
	Dataset    string            `json:"dataset" yaml:"dataset"`
	Table      string            `json:"table" yaml:"table"`
}

func (entry backendFile) toBackend() (Backend, error) {
	signals := make([]Signal, 0, len(entry.Signals))
	for _, raw := range entry.Signals {
		signals = append(signals, Signal(strings.TrimSpace(strings.ToLower(raw))))
	}
	modes := make(map[Signal]Mode, len(entry.Modes))
	for rawSignal, rawMode := range entry.Modes {
		modes[Signal(strings.TrimSpace(strings.ToLower(rawSignal)))] = Mode(strings.TrimSpace(strings.ToLower(rawMode)))
	}
	var timeout time.Duration
	if strings.TrimSpace(entry.Timeout) != "" {
		parsed, err := time.ParseDuration(entry.Timeout)
		if err != nil {
			return Backend{}, fmt.Errorf("invalid timeout %q: %w", entry.Timeout, err)
		}
		timeout = parsed
	}
	backend := Backend{
		Name:       strings.TrimSpace(entry.Name),
		Kind:       strings.TrimSpace(strings.ToLower(entry.Kind)),
		BaseURL:    strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/"),
		Token:      entry.Token,
		Tenant:     entry.Tenant,
		Labels:     entry.Labels,
		Signals:    signals,
		OwnersPath: strings.TrimSpace(entry.OwnersPath),
		Timeout:    timeout,
		Priority:   entry.Priority,
		Modes:      modes,
		Policy:     QueryPolicy(strings.TrimSpace(strings.ToLower(entry.Policy))),
		Origin:     "config",
		Index:      strings.TrimSpace(entry.Index),
		Schema:     entry.Schema,
		Project:    strings.TrimSpace(entry.Project),
		Dataset:    strings.TrimSpace(entry.Dataset),
		Table:      strings.TrimSpace(entry.Table),
	}
	if entry.Public != nil {
		backend.Public = *entry.Public
	}
	if err := backend.Validate(); err != nil {
		return Backend{}, err
	}
	return backend, nil
}

// CompositeRegistry merges multiple registries by backend name. Earlier
// registries win, so static configuration can pin a discovered backend.
type CompositeRegistry struct {
	registries []Registry
}

// NewCompositeRegistry builds a composite over the given registries.
func NewCompositeRegistry(registries ...Registry) *CompositeRegistry {
	filtered := make([]Registry, 0, len(registries))
	for _, registry := range registries {
		if registry != nil {
			filtered = append(filtered, registry)
		}
	}
	return &CompositeRegistry{registries: filtered}
}

// Snapshot implements Registry.
func (c *CompositeRegistry) Snapshot() []Backend {
	seen := make(map[string]struct{})
	out := make([]Backend, 0)
	for _, registry := range c.registries {
		for _, backend := range registry.Snapshot() {
			if _, exists := seen[backend.Name]; exists {
				continue
			}
			seen[backend.Name] = struct{}{}
			out = append(out, backend)
		}
	}
	SortBackends(out)
	return out
}

// LastRefresh implements Registry.
func (c *CompositeRegistry) LastRefresh() time.Time {
	var latest time.Time
	for _, registry := range c.registries {
		if refreshed := registry.LastRefresh(); refreshed.After(latest) {
			latest = refreshed
		}
	}
	return latest
}
