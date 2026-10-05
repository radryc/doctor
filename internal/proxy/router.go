package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/rydzu/ainfra/doctor/internal/access"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// Warning reports a non-fatal backend failure. Partial results are returned
// alongside warnings so callers can surface degraded responses.
type Warning struct {
	Backend string `json:"backend"`
	Signal  Signal `json:"signal,omitempty"`
	Message string `json:"message"`
}

// QueryError distinguishes parse failures (HTTP 400) from execution failures.
type QueryError struct {
	Err   error
	Parse bool
}

// Error implements error.
func (e *QueryError) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying error.
func (e *QueryError) Unwrap() error { return e.Err }

// IsParseError reports whether err is a query parse failure.
func IsParseError(err error) bool {
	var queryErr *QueryError
	return errors.As(err, &queryErr) && queryErr.Parse
}

// RouterConfig tunes fan-out behavior.
type RouterConfig struct {
	// MaxInflight bounds concurrent upstream calls across all requests.
	MaxInflight int
	// BackendTimeout bounds each upstream call.
	BackendTimeout time.Duration
	// Logger receives upstream failures.
	Logger *log.Logger
	// Access applies endpoint authorization. Nil allows everything.
	Access *access.Gate
}

// Upstreams groups the adapters registered for one backend.
type Upstreams struct {
	Metrics MetricsUpstream
	Logs    LogsUpstream
	Traces  TracesUpstream
}

type upstreamEntry struct {
	backend     Backend
	upstreams   Upstreams
	fingerprint string
}

// Router fans queries out to registered backends and merges the results into
// one dataspace. The registry may change between requests; upstreams for
// discovered backends are built lazily and cached until the backend changes.
type Router struct {
	cfg      RouterConfig
	registry Registry
	logger   *log.Logger
	access   *access.Gate

	mu       sync.RWMutex
	explicit map[string]upstreamEntry
	built    map[string]upstreamEntry
	breakers map[string]*breaker
}

// NewRouter builds a router over a registry.
func NewRouter(cfg RouterConfig, registry Registry) *Router {
	maxInflight := cfg.MaxInflight
	if maxInflight <= 0 {
		maxInflight = 32
	}
	timeout := cfg.BackendTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Router{
		cfg: RouterConfig{
			MaxInflight:    maxInflight,
			BackendTimeout: timeout,
			Logger:         cfg.Logger,
			Access:         cfg.Access,
		},
		registry: registry,
		logger:   cfg.Logger,
		access:   cfg.Access,
		explicit: make(map[string]upstreamEntry),
		built:    make(map[string]upstreamEntry),
		breakers: make(map[string]*breaker),
	}
}

// EndpointFor converts a backend into its access-control endpoint.
func EndpointFor(backend Backend) access.Endpoint {
	signals := make([]access.Signal, 0, len(backend.Signals))
	for _, signal := range backend.Signals {
		signals = append(signals, access.Signal(signal))
	}
	return access.Endpoint{
		Name:       backend.Name,
		Signals:    signals,
		OwnersPath: backend.OwnersPath,
		Public:     backend.Public,
	}
}

// accessDecision evaluates the gate for a backend and signal.
func (r *Router) accessDecision(ctx context.Context, backend Backend, signal access.Signal) (bool, access.Decision, bool) {
	if r.access == nil {
		return true, access.Decision{Endpoint: backend.Name, Signal: signal, Allowed: true, Resolver: "none"}, false
	}
	identity, _ := authz.IdentityFromContext(ctx)
	return r.access.Allowed(ctx, identity, EndpointFor(backend), signal)
}

func accessWarning(decision access.Decision) Warning {
	mode := "observed"
	if !decision.Observed {
		mode = "enforced"
	}
	message := fmt.Sprintf("access denied (%s): %s", mode, decision.Reason)
	return Warning{Backend: decision.Endpoint, Message: message}
}

// Register installs explicit upstreams for a backend (local storage, AWS, ...).
// Explicit registrations take precedence over lazily built HTTP upstreams.
func (r *Router) Register(backend Backend, upstreams Upstreams) error {
	if err := backend.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.explicit[backend.Name] = upstreamEntry{backend: backend, upstreams: upstreams}
	return nil
}

// Endpoints returns the registry snapshot with health and access state.
func (r *Router) Endpoints(ctx context.Context) []EndpointInfo {
	now := time.Now()
	backends := r.snapshot()
	out := make([]EndpointInfo, 0, len(backends))
	for _, backend := range backends {
		info := EndpointInfo{
			Name:       backend.Name,
			Kind:       backend.Kind,
			Signals:    append([]Signal(nil), backend.Signals...),
			OwnersPath: backend.OwnersPath,
			Origin:     backend.Origin,
			Public:     backend.Public,
			Healthy:    r.breakerFor(backend.Name).allow(now),
			Allowed:    true,
			Resolver:   "none",
		}
		if r.access != nil {
			identity, _ := authz.IdentityFromContext(ctx)
			decision := r.access.Decide(ctx, identity, EndpointFor(backend), gatedSignal(backend))
			info.Allowed = decision.Allowed
			info.Resolver = decision.Resolver
			info.Reason = decision.Reason
		}
		out = append(out, info)
	}
	return out
}

// EndpointInfo is the public view of a backend.
type EndpointInfo struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Signals    []Signal `json:"signals"`
	OwnersPath string   `json:"ownersPath,omitempty"`
	Origin     string   `json:"origin,omitempty"`
	Public     bool     `json:"public"`
	Healthy    bool     `json:"healthy"`
	Allowed    bool     `json:"allowed"`
	Resolver   string   `json:"resolver,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// gatedSignal picks the signal used for endpoint-list decisions: logs and
// traces are gated, metrics are public.
func gatedSignal(backend Backend) access.Signal {
	if backend.Supports(SignalLogs) {
		return access.SignalLogs
	}
	if backend.Supports(SignalTraces) {
		return access.SignalTraces
	}
	return access.SignalMetrics
}

// MetricsInstant fans an instant PromQL query out and merges the results.
func (r *Router) MetricsInstant(ctx context.Context, tenant, query string, at time.Time) (map[string]any, []Warning, error) {
	return r.metricsQuery(ctx, tenant, query, "vector", func(ctx context.Context, upstream MetricsUpstream, query string) (map[string]any, error) {
		return upstream.PromInstant(ctx, tenant, query, at)
	})
}

// MetricsRange fans a range PromQL query out and merges the results.
func (r *Router) MetricsRange(ctx context.Context, tenant, query string, start, end time.Time, step time.Duration) (map[string]any, []Warning, error) {
	return r.metricsQuery(ctx, tenant, query, "matrix", func(ctx context.Context, upstream MetricsUpstream, query string) (map[string]any, error) {
		return upstream.PromRange(ctx, tenant, query, start, end, step)
	})
}

// Logs fans a reduced LogQL query out and merges the records.
func (r *Router) Logs(ctx context.Context, tenant string, pushdown LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, []Warning, error) {
	targets, accessWarnings, err := r.logTargets(ctx)
	if err != nil {
		return nil, accessWarnings, err
	}
	if len(targets) == 0 {
		return nil, accessWarnings, nil
	}
	// Local storage is merged first so its records retain their original order.
	sort.SliceStable(targets, func(i, j int) bool {
		return targetRank(targets[i].backend) < targetRank(targets[j].backend)
	})

	type result struct {
		index   int
		records []segment.LogRecord
		err     error
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, r.cfg.MaxInflight)
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target logTarget) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = result{index: i, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			if !r.breakerFor(target.backend.Name).allow(time.Now()) {
				results[i] = result{index: i, err: fmt.Errorf("backend %s circuit breaker open", target.backend.Name)}
				return
			}

			callCtx, cancel := context.WithTimeout(ctx, r.backendTimeout(target.backend))
			defer cancel()
			records, err := target.upstream.QueryLogs(callCtx, tenant, pushdown, from, to, limit)
			results[i] = result{index: i, records: records, err: err}
		}(i, target)
	}
	wg.Wait()

	var (
		localRecords []segment.LogRecord
		remotes      []LogRemote
		warnings     = append([]Warning(nil), accessWarnings...)
	)
	for _, res := range results {
		target := targets[res.index]
		if res.err != nil {
			r.recordFailure(target.backend.Name, time.Now())
			if isLocalBackend(target.backend) {
				return nil, warnings, res.err
			}
			warnings = append(warnings, Warning{Backend: target.backend.Name, Signal: SignalLogs, Message: res.err.Error()})
			continue
		}
		r.recordSuccess(target.backend.Name)
		if isLocalBackend(target.backend) {
			localRecords = res.records
			continue
		}
		remotes = append(remotes, LogRemote{Source: sourceForKind(target.backend.Kind), Backend: target.backend.Name, Records: res.records})
	}
	return MergeLogs(localRecords, remotes...), warnings, nil
}

type logTarget struct {
	backend  Backend
	upstream LogsUpstream
}

// logTargets selects logs backends, applying access enforcement. Denied
// backends produce warnings in observe mode and are excluded in enforce mode.
func (r *Router) logTargets(ctx context.Context) ([]logTarget, []Warning, error) {
	targets := make([]logTarget, 0)
	var warnings []Warning
	enforced := 0
	denied := 0
	for _, backend := range r.snapshot() {
		if !backend.Supports(SignalLogs) {
			continue
		}
		upstream := r.logsUpstream(backend)
		if upstream == nil {
			continue
		}
		_, decision, isEnforced := r.accessDecision(ctx, backend, access.SignalLogs)
		if isEnforced {
			enforced++
		}
		if !decision.Allowed {
			warnings = append(warnings, accessWarning(decision))
			if isEnforced {
				denied++
				continue
			}
		}
		targets = append(targets, logTarget{backend: backend, upstream: upstream})
	}
	if enforced > 0 && denied == enforced {
		return nil, warnings, access.ErrAccessDenied
	}
	return targets, warnings, nil
}

type metricsTarget struct {
	backend  Backend
	upstream MetricsUpstream
	query    string
	fallback bool
}

func (r *Router) metricsQuery(
	ctx context.Context,
	tenant, query, defaultType string,
	exec func(ctx context.Context, upstream MetricsUpstream, query string) (map[string]any, error),
) (map[string]any, []Warning, error) {
	stripped, mode := AWSQueryMode(query)
	var (
		primary  []metricsTarget
		fallback []metricsTarget
		local    *metricsTarget
	)
	for _, backend := range r.snapshot() {
		if !backend.Supports(SignalMetrics) {
			continue
		}
		upstream := r.metricsUpstream(backend)
		if upstream == nil {
			continue
		}
		target := metricsTarget{backend: backend, upstream: upstream, query: query}
		switch mode {
		case AWSQueryOnly:
			if !isAWSBackend(backend) {
				continue
			}
			target.query = stripped
			primary = append(primary, target)
			continue
		case AWSQueryMixed:
			if isAWSBackend(backend) {
				target.query = stripped
			}
		}
		if isLocalBackend(backend) {
			local = &target
			continue
		}
		if backend.QueryPolicy() == PolicyFallback && mode == AWSQueryNone {
			target.fallback = true
			fallback = append(fallback, target)
			continue
		}
		primary = append(primary, target)
	}

	type result struct {
		target metricsTarget
		data   map[string]any
		err    error
	}

	run := func(targets []metricsTarget) ([]result, error) {
		results := make([]result, len(targets))
		var wg sync.WaitGroup
		sem := make(chan struct{}, r.cfg.MaxInflight)
		for i, target := range targets {
			wg.Add(1)
			go func(i int, target metricsTarget) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					results[i] = result{target: target, err: ctx.Err()}
					return
				}
				defer func() { <-sem }()
				if !r.breakerFor(target.backend.Name).allow(time.Now()) {
					results[i] = result{target: target, err: fmt.Errorf("backend %s circuit breaker open", target.backend.Name)}
					return
				}

				callCtx, cancel := context.WithTimeout(ctx, r.backendTimeout(target.backend))
				defer cancel()
				data, err := exec(callCtx, target.upstream, target.query)
				results[i] = result{target: target, data: data, err: err}
			}(i, target)
		}
		wg.Wait()
		for _, res := range results {
			if res.err == nil || !isLocalBackend(res.target.backend) {
				continue
			}
			// Local failures are fatal: a parse error is a client error, any
			// other failure aborts the query (matching the legacy path).
			return results, res.err
		}
		return results, nil
	}

	var (
		warnings   []Warning
		localData  map[string]any
		localEmpty = true
		remoteData []PromRemote
	)

	if local != nil {
		results, err := run([]metricsTarget{*local})
		if err != nil {
			return nil, nil, err
		}
		if results[0].err != nil {
			return nil, nil, results[0].err
		}
		localData = results[0].data
		localEmpty = PrometheusResultEmpty(localData)
	}

	if len(primary) > 0 {
		results, err := run(primary)
		if err != nil {
			return nil, nil, err
		}
		for _, res := range results {
			if res.err != nil {
				r.recordFailure(res.target.backend.Name, time.Now())
				warnings = append(warnings, Warning{Backend: res.target.backend.Name, Signal: SignalMetrics, Message: res.err.Error()})
				continue
			}
			r.recordSuccess(res.target.backend.Name)
			remoteData = append(remoteData, PromRemote{
				Source:  sourceForKind(res.target.backend.Kind),
				Backend: res.target.backend.Name,
				Data:    res.data,
			})
		}
	}

	if len(fallback) > 0 && localEmpty {
		results, err := run(fallback)
		if err != nil {
			return nil, nil, err
		}
		for _, res := range results {
			if res.err != nil {
				r.recordFailure(res.target.backend.Name, time.Now())
				warnings = append(warnings, Warning{Backend: res.target.backend.Name, Signal: SignalMetrics, Message: res.err.Error()})
				continue
			}
			r.recordSuccess(res.target.backend.Name)
			remoteData = append(remoteData, PromRemote{
				Source:  sourceForKind(res.target.backend.Kind),
				Backend: res.target.backend.Name,
				Data:    res.data,
			})
		}
	}

	merged := MergePrometheusQueryData(localData, defaultType, remoteData...)
	return merged, warnings, nil
}

func (r *Router) snapshot() []Backend {
	seen := make(map[string]struct{})
	out := make([]Backend, 0)
	r.mu.RLock()
	for name, entry := range r.explicit {
		seen[name] = struct{}{}
		out = append(out, entry.backend)
	}
	r.mu.RUnlock()
	if r.registry != nil {
		for _, backend := range r.registry.Snapshot() {
			if _, exists := seen[backend.Name]; exists {
				continue
			}
			out = append(out, backend)
		}
	}
	SortBackends(out)
	return out
}

func (r *Router) metricsUpstream(backend Backend) MetricsUpstream {
	entry := r.entryFor(backend)
	return entry.upstreams.Metrics
}

func (r *Router) logsUpstream(backend Backend) LogsUpstream {
	entry := r.entryFor(backend)
	return entry.upstreams.Logs
}

func (r *Router) entryFor(backend Backend) upstreamEntry {
	r.mu.RLock()
	if entry, ok := r.explicit[backend.Name]; ok {
		r.mu.RUnlock()
		return entry
	}
	r.mu.RUnlock()

	fingerprint := backendFingerprint(backend)
	r.mu.RLock()
	if entry, ok := r.built[backend.Name]; ok && entry.fingerprint == fingerprint {
		r.mu.RUnlock()
		return entry
	}
	r.mu.RUnlock()

	entry := upstreamEntry{backend: backend, fingerprint: fingerprint}
	switch backend.Kind {
	case KindPrometheus:
		entry.upstreams.Metrics = NewPromHTTPUpstream(backend, nil)
	case KindLoki:
		entry.upstreams.Logs = NewLokiHTTPUpstream(backend, nil)
	case KindTempo:
		entry.upstreams.Traces = NewTempoHTTPUpstream(backend, nil)
	case KindElasticsearch:
		entry.upstreams.Logs = NewElasticsearchUpstream(backend, nil)
	case KindBigQuery:
		entry.upstreams.Logs = NewBigQueryUpstream(backend, nil)
	}

	r.mu.Lock()
	r.built[backend.Name] = entry
	r.mu.Unlock()
	return entry
}

func (r *Router) breakerFor(name string) *breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.breakers[name]
	if !ok {
		b = &breaker{}
		r.breakers[name] = b
	}
	return b
}

func (r *Router) recordFailure(name string, now time.Time) {
	r.breakerFor(name).recordFailure(now)
}

func (r *Router) recordSuccess(name string) {
	r.breakerFor(name).recordSuccess()
}

func (r *Router) backendTimeout(backend Backend) time.Duration {
	if backend.Timeout > 0 {
		return backend.Timeout
	}
	return r.cfg.BackendTimeout
}

func sourceForKind(kind string) string {
	if kind == KindMonoFS {
		return SourceDoctor
	}
	return kind
}

func isLocalBackend(backend Backend) bool {
	return backend.Kind == KindMonoFS
}

func isAWSBackend(backend Backend) bool {
	return backend.Kind == KindAWS
}

func targetRank(backend Backend) int {
	if isLocalBackend(backend) {
		return 0
	}
	return 1
}

func backendFingerprint(backend Backend) string {
	return strings.Join([]string{backend.Name, backend.Kind, backend.BaseURL, backend.Token, backend.Tenant}, "|")
}

const (
	breakerThreshold = 5
	breakerCooldown  = 30 * time.Second
)

type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

func (b *breaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if now.Before(b.openUntil) {
		return false
	}
	b.openUntil = time.Time{}
	b.failures = 0
	return true
}

func (b *breaker) recordFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= breakerThreshold {
		b.openUntil = now.Add(breakerCooldown)
	}
}

func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
}

// ErrUnsupported reports that an upstream does not implement a signal.
var ErrUnsupported = errors.New("proxy: upstream does not support this query")

func unsupported(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}
