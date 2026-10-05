package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
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
// partition-derived endpoints are best-effort.
func (d *GuardianDiscovery) Refresh(ctx context.Context) error {
	if d.baseURL == "" {
		return fmt.Errorf("proxy: guardian discovery requires a base URL")
	}
	pushers, err := d.fetchPushers(ctx)
	if err != nil {
		d.setError(err)
		return err
	}

	backends := make([]Backend, 0, len(pushers))
	for _, pusher := range pushers {
		backend, err := d.backendFromPusher(pusher)
		if err != nil {
			if d.logger != nil {
				d.logger.Printf("proxy: skipping pusher %q: %v", pusher.Name, err)
			}
			continue
		}
		backends = append(backends, backend)
	}

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
	d.lastRefresh.Store(time.Now().UTC().UnixNano())
	d.lastError.Store(nil)
	return nil
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
	ObservedAPIURL string            `json:"observedApiUrl"`
	Token          string            `json:"token"`
	TokenEnv       string            `json:"tokenEnv"`
	Labels         map[string]string `json:"labels"`
	Region         string            `json:"region"`
	Account        string            `json:"account"`
}

type guardianPusherList struct {
	Pushers []guardianPusher `json:"pushers"`
}

func (d *GuardianDiscovery) fetchPushers(ctx context.Context) ([]guardianPusher, error) {
	var response guardianPusherList
	if err := d.getJSON(ctx, "/api/pushers", &response); err != nil {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
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
	kind := strings.TrimSpace(strings.ToLower(labels["kind"]))
	if kind == "" {
		kind = KindAWS
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
		Name:       name,
		Kind:       kind,
		BaseURL:    strings.TrimRight(strings.TrimSpace(pusher.ObservedAPIURL), "/"),
		Token:      token,
		Labels:     labels,
		Signals:    signals,
		OwnersPath: ownersPath,
		Policy:     policyFromLabels(labels, kind),
		Origin:     "guardian-pusher",
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
