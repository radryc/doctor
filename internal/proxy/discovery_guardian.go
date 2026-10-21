package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxGuardianDiscoveryBodyBytes = 64 << 20 // 64 MiB for 50K pusher payloads
	fullReconcileInterval         = 10 * time.Minute
)

// GuardianDiscoveryConfig configures dynamic backend discovery from Guardian.
type GuardianDiscoveryConfig struct {
	// BaseURL is the Guardian API base URL (e.g. http://guardian:19090).
	BaseURL string
	// Token is attached as a bearer token to Guardian API calls. It is
	// required when Guardian UI auth is enforced.
	Token string
	// Interval between discovery polls. Defaults to 30s, matching Guardian's
	// own observed registry resolver.
	Interval time.Duration
	// OwnersRoot is the MonoFS path prefix used when a pusher does not declare
	// an explicit ownersPath.
	OwnersRoot string
	// HTTPClient overrides the default client (used by tests).
	HTTPClient *http.Client
	// Logger receives discovery errors.
	Logger *log.Logger
}

// GuardianDiscovery polls Guardian for observability sources and publishes them
// as backend snapshots. Failed polls keep the previous snapshot.
type GuardianDiscovery struct {
	baseURL    string
	token      string
	interval   time.Duration
	ownersRoot string
	client     *http.Client
	logger     *log.Logger

	snapshot    atomic.Pointer[[]Backend]
	lastRefresh atomic.Int64
	lastError   atomic.Pointer[string]

	mu             sync.Mutex
	knownPushers   map[string]Backend
	lastFullSync   time.Time
	lastSyncCursor time.Time
}

// NewGuardianDiscovery builds a discovery loop. Call Run to start polling.
func NewGuardianDiscovery(cfg GuardianDiscoveryConfig) *GuardianDiscovery {
	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	ownersRoot := strings.TrimRight(strings.TrimSpace(cfg.OwnersRoot), "/")
	if ownersRoot == "" {
		ownersRoot = "/doctor/v1/endpoints"
	}
	return &GuardianDiscovery{
		baseURL:    strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		token:      cfg.Token,
		interval:   interval,
		ownersRoot: ownersRoot,
		client:     client,
		logger:     cfg.Logger,
	}
}

// Run polls until ctx is done. It refreshes once immediately.
func (d *GuardianDiscovery) Run(ctx context.Context) {
	if err := d.Refresh(ctx); err != nil && d.logger != nil {
		d.logger.Printf("proxy: guardian discovery failed: %v", err)
	}
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.Refresh(ctx); err != nil && d.logger != nil {
				d.logger.Printf("proxy: guardian discovery failed: %v", err)
			}
		}
	}
}

// Snapshot implements Registry.
func (d *GuardianDiscovery) Snapshot() []Backend {
	if snapshot := d.snapshot.Load(); snapshot != nil {
		return *snapshot
	}
	return nil
}

// LastRefresh implements Registry.
func (d *GuardianDiscovery) LastRefresh() time.Time {
	value := d.lastRefresh.Load()
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

// LastError returns the most recent discovery error, if any.
func (d *GuardianDiscovery) LastError() error {
	if message := d.lastError.Load(); message != nil && *message != "" {
		return fmt.Errorf("%s", *message)
	}
	return nil
}

// Refresh performs one discovery poll. The pusher registry is authoritative;
// partition-derived endpoints are best-effort. After the initial full hydration,
// steady-state polls use ?updated_since= delta queries to avoid transferring
// all 50,000 pusher records every 30s.
func (d *GuardianDiscovery) Refresh(ctx context.Context) error {
	if d.baseURL == "" {
		return fmt.Errorf("proxy: guardian discovery requires a base URL")
	}

	d.mu.Lock()
	now := time.Now().UTC()
	fullSync := d.knownPushers == nil || d.lastFullSync.IsZero() || now.Sub(d.lastFullSync) >= fullReconcileInterval
	since := d.lastSyncCursor
	d.mu.Unlock()

	var (
		pushers []guardianPusher
		err     error
	)
	if fullSync {
		pushers, err = d.fetchPushers(ctx, time.Time{})
	} else {
		pushers, err = d.fetchPushers(ctx, since)
	}
	if err != nil {
		d.setError(err)
		return err
	}

	d.mu.Lock()
	if fullSync || d.knownPushers == nil {
		d.knownPushers = make(map[string]Backend, len(pushers))
		d.lastFullSync = now
	}
	maxUpdated := since
	for _, pusher := range pushers {
		if ts := parsePusherTimestamp(pusher.UpdatedAt, pusher.LastHeartbeat); ts.After(maxUpdated) {
			maxUpdated = ts
		}
		backend, bErr := d.backendFromPusher(pusher)
		if bErr != nil {
			if d.logger != nil {
				d.logger.Printf("proxy: skipping pusher %q: %v", pusher.Name, bErr)
			}
			continue
		}
		d.knownPushers[backend.Name] = backend
	}
	if maxUpdated.IsZero() {
		d.lastSyncCursor = now.Add(-2 * time.Second)
	} else {
		d.lastSyncCursor = maxUpdated
	}
	backends := make([]Backend, 0, len(d.knownPushers))
	for _, b := range d.knownPushers {
		backends = append(backends, b)
	}
	d.mu.Unlock()

	if partitionBackends, partitionErr := d.fetchPartitionBackends(ctx); partitionErr != nil {
		if d.logger != nil {
			d.logger.Printf("proxy: guardian partition discovery failed: %v", partitionErr)
		}
	} else {
		backends = append(backends, partitionBackends...)
	}

	deduped := dedupeBackends(backends)
	SortBackends(deduped)
	d.snapshot.Store(&deduped)
	d.lastRefresh.Store(now.UnixNano())
	d.lastError.Store(nil)
	return nil
}

func parsePusherTimestamp(updatedAt, lastHeartbeat string) time.Time {
	for _, raw := range []string{updatedAt, lastHeartbeat} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (d *GuardianDiscovery) setError(err error) {
	if err == nil {
		return
	}
	message := err.Error()
	d.lastError.Store(&message)
}

type guardianPusher struct {
	Name           string            `json:"name"`
	Provider       string            `json:"provider"`
	Version        string            `json:"version"`
	Cluster        string            `json:"cluster"`
	Account        string            `json:"account"`
	Region         string            `json:"region"`
	Capabilities   []string          `json:"capabilities"`
	Status         string            `json:"status"`
	LastHeartbeat  string            `json:"lastHeartbeat"`
	UpdatedAt      string            `json:"updatedAt"`
	ObservedAPIURL string            `json:"observedApiUrl"`
	Token          string            `json:"token"`
	TokenEnv       string            `json:"tokenEnv"`
	Labels         map[string]string `json:"labels"`
}

type guardianPusherList struct {
	Pushers []guardianPusher `json:"pushers"`
}

func (d *GuardianDiscovery) fetchPushers(ctx context.Context, updatedSince time.Time) ([]guardianPusher, error) {
	path := "/api/pushers"
	if !updatedSince.IsZero() {
		path = "/api/pushers?updated_since=" + url.QueryEscape(updatedSince.UTC().Format(time.RFC3339Nano))
	}
	var response guardianPusherList
	if err := d.getJSON(ctx, path, &response); err != nil {
		return nil, err
	}
	return response.Pushers, nil
}

type guardianPartition struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

func (d *GuardianDiscovery) fetchPartitionBackends(ctx context.Context) ([]Backend, error) {
	var partitions []guardianPartition
	if err := d.getJSON(ctx, "/api/m2m/partitions", &partitions); err != nil {
		return nil, err
	}
	backends := make([]Backend, 0)
	for _, partition := range partitions {
		backend, ok := d.backendFromPartition(partition)
		if !ok {
			continue
		}
		backends = append(backends, backend)
	}
	return backends, nil
}

func (d *GuardianDiscovery) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(d.token) != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("proxy: guardian GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("proxy: guardian GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGuardianDiscoveryBodyBytes)).Decode(out); err != nil {
		return fmt.Errorf("proxy: decode guardian %s: %w", path, err)
	}
	return nil
}

func (d *GuardianDiscovery) backendFromPusher(pusher guardianPusher) (Backend, error) {
	name := strings.TrimSpace(strings.ToLower(pusher.Name))
	if name == "" {
		return Backend{}, fmt.Errorf("empty pusher name")
	}
	labels := pusher.Labels
	kind := strings.TrimSpace(strings.ToLower(pusher.Provider))
	if kind == "" {
		kind = strings.TrimSpace(strings.ToLower(labels["kind"]))
	}
	if kind == "" && strings.TrimSpace(pusher.Account) != "" {
		kind = KindAWS
	}
	if kind == "" {
		return Backend{}, fmt.Errorf("pusher %q has no provider", pusher.Name)
	}
	signals := signalsFromLabels(labels, kind)
	ownersPath := strings.TrimSpace(labels["ownersPath"])
	if ownersPath == "" {
		ownersPath = d.ownersRoot + "/" + name
	}
	token := pusher.Token
	if token == "" && pusher.TokenEnv != "" {
		token = os.Getenv(pusher.TokenEnv)
	}
	backend := Backend{
		Name:         name,
		Kind:         kind,
		BaseURL:      strings.TrimRight(strings.TrimSpace(pusher.ObservedAPIURL), "/"),
		Token:        token,
		Labels:       labels,
		Signals:      signals,
		OwnersPath:   ownersPath,
		Policy:       policyFromLabels(labels, kind),
		Origin:       "guardian-pusher",
		Provider:     pusher.Provider,
		Version:      pusher.Version,
		Cluster:      pusher.Cluster,
		Account:      pusher.Account,
		Region:       pusher.Region,
		Capabilities: append([]string(nil), pusher.Capabilities...),
		Status:       pusher.Status,
	}
	if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(pusher.LastHeartbeat)); err == nil {
		backend.LastHeartbeat = parsed
	}
	if err := backend.Validate(); err != nil {
		return Backend{}, err
	}
	return backend, nil
}

func (d *GuardianDiscovery) backendFromPartition(partition guardianPartition) (Backend, bool) {
	labels := partition.Labels
	kind := strings.TrimSpace(strings.ToLower(labels["kind"]))
	endpoint := strings.TrimSpace(labels["endpoint"])
	if kind == "" || endpoint == "" {
		return Backend{}, false
	}
	name := strings.TrimSpace(strings.ToLower(labels["backendName"]))
	if name == "" {
		name = strings.TrimSpace(strings.ToLower(partition.Name))
	}
	ownersPath := strings.TrimSpace(labels["ownersPath"])
	if ownersPath == "" {
		ownersPath = d.ownersRoot + "/" + name
	}
	backend := Backend{
		Name:       name,
		Kind:       kind,
		BaseURL:    strings.TrimRight(endpoint, "/"),
		Labels:     labels,
		Signals:    signalsFromLabels(labels, kind),
		OwnersPath: ownersPath,
		Policy:     policyFromLabels(labels, kind),
		Origin:     "guardian-partition",
	}
	if err := backend.Validate(); err != nil {
		return Backend{}, false
	}
	return backend, true
}

// DefaultSignals maps a backend kind to the signals it can serve.
func DefaultSignals(kind string) []Signal {
	switch kind {
	case KindPrometheus:
		return []Signal{SignalMetrics}
	case KindLoki:
		return []Signal{SignalLogs}
	case KindTempo:
		return []Signal{SignalTraces}
	case KindElasticsearch:
		return []Signal{SignalLogs, SignalTraces}
	case KindBigQuery:
		return []Signal{SignalMetrics, SignalLogs, SignalTraces}
	case KindMonoFS, KindAWS:
		return []Signal{SignalMetrics, SignalLogs, SignalTraces}
	default:
		return nil
	}
}

func signalsFromLabels(labels map[string]string, kind string) []Signal {
	raw := strings.TrimSpace(strings.ToLower(labels["signals"]))
	if raw == "" {
		return DefaultSignals(kind)
	}
	parts := strings.Split(raw, ",")
	signals := make([]Signal, 0, len(parts))
	for _, part := range parts {
		signal := Signal(strings.TrimSpace(part))
		if signal.Valid() {
			signals = append(signals, signal)
		}
	}
	if len(signals) == 0 {
		return DefaultSignals(kind)
	}
	return signals
}

func policyFromLabels(labels map[string]string, kind string) QueryPolicy {
	switch QueryPolicy(strings.TrimSpace(strings.ToLower(labels["policy"]))) {
	case PolicyAlways:
		return PolicyAlways
	case PolicyFallback:
		return PolicyFallback
	}
	// Preserve the legacy AWS federation behavior for discovered AWS pushers.
	if kind == KindAWS {
		return PolicyFallback
	}
	return PolicyAlways
}

func dedupeBackends(backends []Backend) []Backend {
	seen := make(map[string]struct{}, len(backends))
	out := make([]Backend, 0, len(backends))
	for _, backend := range backends {
		if _, exists := seen[backend.Name]; exists {
			continue
		}
		seen[backend.Name] = struct{}{}
		out = append(out, backend)
	}
	return out
}
