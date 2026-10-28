package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/radryc/doctor/internal/access"
	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/monofs/pkg/authz"
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
	// MaxFanoutBackends bounds how many remote backends a single pull query
	// may fan out to after label/partition pruning. Defaults to 32.
	MaxFanoutBackends int
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

	mu                sync.RWMutex
	explicit          map[string]upstreamEntry
	explicitRev       uint64
	cachedSnap        []Backend
	cachedSnapRev     uint64
	cachedSnapRefresh time.Time
	built             map[string]upstreamEntry
	breakers          map[string]*breaker
}

// NewRouter builds a router over a registry.
func NewRouter(cfg RouterConfig, registry Registry) *Router {
	maxInflight := cfg.MaxInflight
	if maxInflight <= 0 {
		maxInflight = 32
	}
	maxFanout := cfg.MaxFanoutBackends
	if maxFanout <= 0 {
		maxFanout = 32
	}
	timeout := cfg.BackendTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Router{
		cfg: RouterConfig{
			MaxInflight:       maxInflight,
			MaxFanoutBackends: maxFanout,
			BackendTimeout:    timeout,
			Logger:            cfg.Logger,
			Access:            cfg.Access,
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
	r.explicitRev++
	r.cachedSnap = nil
	return nil
}

// EndpointSummary aggregates fleet-wide health and dimension counts across all
// registered/discovered endpoints without serializing 50,000 rows.
type EndpointSummary struct {
	Total     int            `json:"total"`
	Healthy   int            `json:"healthy"`
	Unhealthy int            `json:"unhealthy"`
	ByKind    map[string]int `json:"byKind"`
	ByStatus  map[string]int `json:"byStatus"`
	ByRegion  map[string]int `json:"byRegion"`
	ByAccount map[string]int `json:"byAccount"`
	ByCluster map[string]int `json:"byCluster"`
	ByVersion map[string]int `json:"byVersion"`
}

// EndpointPageFilter scopes and paginates the endpoint list for 50,000-pusher fleets.
type EndpointPageFilter struct {
	Kind      string
	Status    string
	Region    string
	Account   string
	Cluster   string
	Version   string
	Query     string
	Outliers  bool
	Unhealthy bool
	Limit     int
	Cursor    string
}

// EndpointPage holds one page of EndpointInfo plus fleet summary.
type EndpointPage struct {
	Endpoints  []EndpointInfo  `json:"endpoints"`
	NextCursor string          `json:"nextCursor,omitempty"`
	TotalMatch int             `json:"totalMatched"`
	Summary    EndpointSummary `json:"summary"`
}

// SummarizeEndpoints computes fleet-wide counts in O(N) memory-only pass.
func (r *Router) SummarizeEndpoints() EndpointSummary {
	now := time.Now()
	backends := r.snapshot()
	summary := EndpointSummary{
		Total:     len(backends),
		ByKind:    make(map[string]int),
		ByStatus:  make(map[string]int),
		ByRegion:  make(map[string]int),
		ByAccount: make(map[string]int),
		ByCluster: make(map[string]int),
		ByVersion: make(map[string]int),
	}
	for _, b := range backends {
		if r.backendHealthy(b, now) {
			summary.Healthy++
		} else {
			summary.Unhealthy++
		}
		if b.Kind != "" {
			summary.ByKind[b.Kind]++
		}
		status := b.Status
		if status == "" {
			status = "active"
		}
		summary.ByStatus[status]++
		if b.Region != "" {
			summary.ByRegion[b.Region]++
		}
		if b.Account != "" {
			summary.ByAccount[b.Account]++
		}
		if b.Cluster != "" {
			summary.ByCluster[b.Cluster]++
		}
		if b.Version != "" {
			summary.ByVersion[b.Version]++
		}
	}
	return summary
}

// EndpointsPage returns a filtered, optionally outlier-sorted page of endpoints
// along with the fleet summary. Only the page slice evaluates access decisions.
func (r *Router) EndpointsPage(ctx context.Context, filter EndpointPageFilter) EndpointPage {
	now := time.Now()
	backends := r.snapshot()
	summary := r.SummarizeEndpoints()

	kindFilter := strings.ToLower(strings.TrimSpace(filter.Kind))
	statusFilter := strings.ToLower(strings.TrimSpace(filter.Status))
	regionFilter := strings.ToLower(strings.TrimSpace(filter.Region))
	accountFilter := strings.ToLower(strings.TrimSpace(filter.Account))
	clusterFilter := strings.ToLower(strings.TrimSpace(filter.Cluster))
	versionFilter := strings.ToLower(strings.TrimSpace(filter.Version))
	qFilter := strings.ToLower(strings.TrimSpace(filter.Query))

	matched := make([]Backend, 0, 256)
	for _, b := range backends {
		healthy := r.backendHealthy(b, now)
		if filter.Unhealthy && healthy {
			continue
		}
		if kindFilter != "" && !strings.EqualFold(b.Kind, kindFilter) {
			continue
		}
		if statusFilter != "" && !strings.EqualFold(b.Status, statusFilter) {
			continue
		}
		if regionFilter != "" && !strings.EqualFold(b.Region, regionFilter) {
			continue
		}
		if accountFilter != "" && !strings.EqualFold(b.Account, accountFilter) {
			continue
		}
		if clusterFilter != "" && !strings.EqualFold(b.Cluster, clusterFilter) {
			continue
		}
		if versionFilter != "" && !strings.EqualFold(b.Version, versionFilter) {
			continue
		}
		if qFilter != "" {
			if !strings.Contains(strings.ToLower(b.Name), qFilter) &&
				!strings.Contains(strings.ToLower(b.Account), qFilter) &&
				!strings.Contains(strings.ToLower(b.Region), qFilter) &&
				!strings.Contains(strings.ToLower(b.Cluster), qFilter) {
				continue
			}
		}
		matched = append(matched, b)
	}

	if filter.Outliers {
		sort.SliceStable(matched, func(i, j int) bool {
			hi := r.backendHealthy(matched[i], now)
			hj := r.backendHealthy(matched[j], now)
			if hi != hj {
				return !hi // unhealthy first
			}
			si := endpointStatusRank(matched[i].Status)
			sj := endpointStatusRank(matched[j].Status)
			if si != sj {
				return si < sj
			}
			if !matched[i].LastHeartbeat.Equal(matched[j].LastHeartbeat) {
				if matched[i].LastHeartbeat.IsZero() {
					return false
				}
				if matched[j].LastHeartbeat.IsZero() {
					return true
				}
				return matched[i].LastHeartbeat.Before(matched[j].LastHeartbeat)
			}
			return matched[i].Name < matched[j].Name
		})
	}

	totalMatched := len(matched)
	offset := 0
	if filter.Cursor != "" {
		if parsed, err := strconv.Atoi(filter.Cursor); err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	if offset > totalMatched {
		offset = totalMatched
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	end := offset + limit
	var nextCursor string
	if end < totalMatched {
		nextCursor = strconv.Itoa(end)
	} else {
		end = totalMatched
	}

	pageSlice := matched[offset:end]
	out := make([]EndpointInfo, 0, len(pageSlice))
	for _, backend := range pageSlice {
		out = append(out, r.endpointInfoFor(ctx, backend, now))
	}
	return EndpointPage{
		Endpoints:  out,
		NextCursor: nextCursor,
		TotalMatch: totalMatched,
		Summary:    summary,
	}
}

func endpointStatusRank(status string) int {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "failed", "unhealthy", "degraded", "offline":
		return 0
	case "stale", "warning":
		return 1
	case "active", "healthy", "ready", "ok", "":
		return 2
	default:
		return 1
	}
}

func endpointHealthLabel(healthy bool, status string) string {
	if !healthy {
		return "offline"
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "degraded", "attention", "warning", "stale", "drifted", "drift":
		return "degraded"
	case "offline", "down", "failed", "error", "unhealthy":
		return "offline"
	default:
		return "healthy"
	}
}

// Endpoints returns the registry snapshot with health and access state.
func (r *Router) Endpoints(ctx context.Context) []EndpointInfo {
	now := time.Now()
	backends := r.snapshot()
	out := make([]EndpointInfo, 0, len(backends))
	for _, backend := range backends {
		out = append(out, r.endpointInfoFor(ctx, backend, now))
	}
	return out
}

func (r *Router) endpointInfoFor(ctx context.Context, backend Backend, now time.Time) EndpointInfo {
	info := EndpointInfo{
		Name:          backend.Name,
		Kind:          backend.Kind,
		Signals:       append([]Signal(nil), backend.Signals...),
		OwnersPath:    backend.OwnersPath,
		Origin:        backend.Origin,
		Public:        backend.Public,
		Healthy:       r.backendHealthy(backend, now),
		Allowed:       true,
		Resolver:      "none",
		Provider:      backend.Provider,
		Version:       backend.Version,
		Cluster:       backend.Cluster,
		Account:       backend.Account,
		Region:        backend.Region,
		Capabilities:  append([]string(nil), backend.Capabilities...),
		Status:        backend.Status,
		LastHeartbeat: backend.LastHeartbeat,
	}
	info.Health = endpointHealthLabel(info.Healthy, backend.Status)
	if r.access != nil {
		identity, _ := authz.IdentityFromContext(ctx)
		decision := r.access.Decide(ctx, identity, EndpointFor(backend), gatedSignal(backend))
		info.Allowed = decision.Allowed
		info.Resolver = decision.Resolver
		info.Reason = decision.Reason
	}
	return info
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
	// Health is a presentation-friendly rollup of Healthy and Status. It keeps
	// older UI clients (which read health instead of healthy) working.
	Health        string    `json:"health"`
	Allowed       bool      `json:"allowed"`
	Resolver      string    `json:"resolver,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	Version       string    `json:"version,omitempty"`
	Cluster       string    `json:"cluster,omitempty"`
	Account       string    `json:"account,omitempty"`
	Region        string    `json:"region,omitempty"`
	Capabilities  []string  `json:"capabilities,omitempty"`
	Status        string    `json:"status,omitempty"`
	LastHeartbeat time.Time `json:"lastHeartbeat,omitempty"`
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
	criteria := parseSelectorCriteria(pushdown.Selector)
	targets, accessWarnings, err := r.logTargets(ctx, criteria)
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
	targets, capWarn := r.capLogTargets(targets)
	if capWarn != nil {
		accessWarnings = append(accessWarnings, *capWarn)
	}

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
			if !r.backendAllowed(target.backend, time.Now()) {
				results[i] = result{index: i, err: fmt.Errorf("backend %s circuit breaker open", target.backend.Name)}
				return
			}

			callCtx, cancel := context.WithTimeout(ctx, r.backendCallTimeout(ctx, target.backend))
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
			r.noteBackendFailure(target.backend, res.err)
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

// logTargets selects logs backends, applying partition/label pruning and access
// enforcement. Denied backends produce warnings in observe mode and are excluded
// in enforce mode.
func (r *Router) logTargets(ctx context.Context, criteria selectorCriteria) ([]logTarget, []Warning, error) {
	targets := make([]logTarget, 0)
	var warnings []Warning
	enforced := 0
	denied := 0
	for _, backend := range r.snapshot() {
		if !backend.Supports(SignalLogs) {
			continue
		}
		if !backendMatchesCriteria(backend, criteria) {
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

func (r *Router) capLogTargets(targets []logTarget) ([]logTarget, *Warning) {
	maxRemote := r.cfg.MaxFanoutBackends
	if maxRemote <= 0 || len(targets) <= maxRemote {
		return targets, nil
	}
	out := make([]logTarget, 0, maxRemote+1)
	remoteCount := 0
	totalRemote := 0
	for _, t := range targets {
		if isLocalBackend(t.backend) {
			out = append(out, t)
			continue
		}
		totalRemote++
		if remoteCount < maxRemote {
			out = append(out, t)
			remoteCount++
		}
	}
	if totalRemote <= maxRemote {
		return targets, nil
	}
	return out, &Warning{
		Backend: "router",
		Signal:  SignalLogs,
		Message: fmt.Sprintf("remote fan-out capped at %d of %d matching backends; narrow query by region, account, cluster, or partition", maxRemote, totalRemote),
	}
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
	criteria := parseSelectorCriteria(query)
	var (
		primary  []metricsTarget
		fallback []metricsTarget
		local    *metricsTarget
	)
	for _, backend := range r.snapshot() {
		if !backend.Supports(SignalMetrics) {
			continue
		}
		if !backendMatchesCriteria(backend, criteria) {
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

	var capWarnings []Warning
	maxRemote := r.cfg.MaxFanoutBackends
	if maxRemote > 0 && len(primary) > maxRemote {
		total := len(primary)
		primary = primary[:maxRemote]
		capWarnings = append(capWarnings, Warning{
			Backend: "router",
			Signal:  SignalMetrics,
			Message: fmt.Sprintf("remote fan-out capped at %d of %d matching backends; narrow query by region, account, cluster, or partition", maxRemote, total),
		})
	}
	if maxRemote > 0 && len(fallback) > maxRemote {
		total := len(fallback)
		fallback = fallback[:maxRemote]
		capWarnings = append(capWarnings, Warning{
			Backend: "router",
			Signal:  SignalMetrics,
			Message: fmt.Sprintf("remote fallback fan-out capped at %d of %d matching backends; narrow query by region, account, cluster, or partition", maxRemote, total),
		})
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
				if !r.backendAllowed(target.backend, time.Now()) {
					results[i] = result{target: target, err: fmt.Errorf("backend %s circuit breaker open", target.backend.Name)}
					return
				}

				callCtx, cancel := context.WithTimeout(ctx, r.backendCallTimeout(ctx, target.backend))
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
		warnings   = append([]Warning(nil), capWarnings...)
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
				r.noteBackendFailure(res.target.backend, res.err)
				// A pure AWS query has no local fallback. Propagate the scope
				// error so handlers can return 422 instead of an empty 200.
				if mode == AWSQueryOnly && len(primary) == 1 {
					return nil, warnings, res.err
				}
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
				r.noteBackendFailure(res.target.backend, res.err)
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
	r.mu.RLock()
	explicitCount := len(r.explicit)
	explicitRev := r.explicitRev
	cached := r.cachedSnap
	cachedRev := r.cachedSnapRev
	cachedRefresh := r.cachedSnapRefresh
	r.mu.RUnlock()

	if explicitCount == 0 {
		if r.registry == nil {
			return nil
		}
		return r.registry.Snapshot()
	}

	var regRefresh time.Time
	if r.registry != nil {
		regRefresh = r.registry.LastRefresh()
	}
	if cached != nil && cachedRev == explicitRev && !regRefresh.IsZero() && cachedRefresh.Equal(regRefresh) {
		return cached
	}

	seen := make(map[string]struct{}, explicitCount)
	out := make([]Backend, 0, explicitCount)
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

	if !regRefresh.IsZero() {
		r.mu.Lock()
		if r.explicitRev == explicitRev {
			r.cachedSnap = out
			r.cachedSnapRev = explicitRev
			r.cachedSnapRefresh = regRefresh
		}
		r.mu.Unlock()
	}
	return out
}

// selectorCriteria holds normalized equality/alternation values extracted from
// PromQL, LogQL, or TraceQL/TraceSearch selectors to prune 50,000 remote pushers
// before fan-out.
type selectorCriteria struct {
	region    map[string]struct{}
	account   map[string]struct{}
	cluster   map[string]struct{}
	partition map[string]struct{}
	backend   map[string]struct{}
}

func (c selectorCriteria) isEmpty() bool {
	return len(c.region) == 0 && len(c.account) == 0 && len(c.cluster) == 0 && len(c.partition) == 0 && len(c.backend) == 0
}

var (
	scopingLabelRe   = regexp.MustCompile(`\b(region|aws_region|account|aws_account|cluster|partition|guardian_partition|shard|pusher|backend)\s*(=|=~)\s*"([^"]+)"`)
	scopingLogGrpRe  = regexp.MustCompile(`\baws_log_group\s*(=|=~)\s*"([^"]+)"`)
	simpleAltValueRe = regexp.MustCompile(`^[a-zA-Z0-9_.:/-]+(\|[a-zA-Z0-9_.:/-]+)*$`)
)

func parseSelectorCriteria(selector string) selectorCriteria {
	var c selectorCriteria
	if strings.TrimSpace(selector) == "" {
		return c
	}
	addValues := func(dst *map[string]struct{}, op, raw string) {
		if op == "=~" && !simpleAltValueRe.MatchString(raw) {
			return
		}
		for _, part := range strings.Split(raw, "|") {
			val := strings.ToLower(strings.TrimSpace(part))
			if val == "" {
				continue
			}
			if *dst == nil {
				*dst = make(map[string]struct{}, 2)
			}
			(*dst)[val] = struct{}{}
		}
	}
	for _, m := range scopingLabelRe.FindAllStringSubmatch(selector, -1) {
		if len(m) != 4 {
			continue
		}
		key, op, val := m[1], m[2], m[3]
		switch key {
		case "region", "aws_region":
			addValues(&c.region, op, val)
		case "account", "aws_account":
			addValues(&c.account, op, val)
		case "cluster":
			addValues(&c.cluster, op, val)
		case "partition", "guardian_partition":
			addValues(&c.partition, op, val)
		case "shard", "pusher", "backend":
			addValues(&c.backend, op, val)
		}
	}
	for _, m := range scopingLogGrpRe.FindAllStringSubmatch(selector, -1) {
		if len(m) != 3 {
			continue
		}
		for _, grp := range strings.Split(m[2], "|") {
			acct, reg, part := parseGuardianLogGroupScope(grp)
			if acct != "" {
				addValues(&c.account, "=", acct)
			}
			if reg != "" {
				addValues(&c.region, "=", reg)
			}
			if part != "" {
				addValues(&c.partition, "=", part)
			}
		}
	}
	return c
}

func parseGuardianLogGroupScope(logGroup string) (account, region, partition string) {
	logGroup = strings.TrimSpace(logGroup)
	if idx := strings.Index(logGroup, "guardian/"); idx >= 0 {
		logGroup = logGroup[idx:]
	}
	parts := strings.Split(strings.Trim(logGroup, "/"), "/")
	if len(parts) < 4 || parts[0] != "guardian" {
		return "", "", ""
	}
	return strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2]), strings.TrimSpace(parts[3])
}

func backendMatchesCriteria(backend Backend, c selectorCriteria) bool {
	if c.isEmpty() || isLocalBackend(backend) || backend.Federated {
		// Federated backends own target selection internally; pruning them by
		// static account/region fields would defeat their planner.
		return true
	}
	if !backendMatchesField(backend, "backend", c.backend) {
		return false
	}
	if !backendMatchesField(backend, "region", c.region) {
		return false
	}
	if !backendMatchesField(backend, "account", c.account) {
		return false
	}
	if !backendMatchesField(backend, "cluster", c.cluster) {
		return false
	}
	if !backendMatchesField(backend, "partition", c.partition) {
		return false
	}
	return true
}

func backendMatchesField(backend Backend, field string, allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		return true
	}
	var candidates []string
	add := func(v string) {
		if trimmed := strings.ToLower(strings.TrimSpace(v)); trimmed != "" {
			candidates = append(candidates, trimmed)
		}
	}
	switch field {
	case "backend":
		add(backend.Name)
	case "region":
		add(backend.Region)
		add(backend.Labels["region"])
		add(backend.Labels["aws_region"])
	case "account":
		add(backend.Account)
		add(backend.Labels["account"])
		add(backend.Labels["aws_account"])
	case "cluster":
		add(backend.Cluster)
		add(backend.Labels["cluster"])
	case "partition":
		add(backend.Labels["partition"])
		add(backend.Labels["guardian_partition"])
		add(backend.Cluster)
		add(backend.Region)
		add(backend.Name)
	}
	if len(candidates) == 0 {
		if backend.Origin == "guardian-pusher" || isAWSBackend(backend) {
			return false
		}
		return true
	}
	for _, cand := range candidates {
		if _, ok := allowed[cand]; ok {
			return true
		}
	}
	return false
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

func (r *Router) breakerAllow(name string, now time.Time) bool {
	r.mu.RLock()
	b, ok := r.breakers[name]
	r.mu.RUnlock()
	if !ok {
		return true
	}
	return b.allow(now)
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

// noteBackendFailure records an upstream failure for circuit-breaker purposes.
// Client errors (query parse failures) and the in-process local backend must
// not mark a healthy backend as degraded: the local store is this process, and
// a bad query says nothing about backend availability.
func (r *Router) noteBackendFailure(backend Backend, err error) {
	if err == nil || isLocalBackend(backend) || IsParseError(err) {
		return
	}
	// Scope and fan-out rejection are client/query errors. A dashboard that
	// asks for an unscoped live-AWS query should get a 422, but must not mark
	// the AWS backend unhealthy or open its circuit breaker.
	if errors.Is(err, ErrScopeRequired) || errors.Is(err, ErrTooBroad) || errors.Is(err, ErrNoTargets) {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	r.recordFailure(backend.Name, time.Now())
}

// backendHealthy reports circuit-breaker health for the endpoint UI. The local
// in-process backend is healthy whenever the query service is serving requests.
func (r *Router) backendHealthy(backend Backend, now time.Time) bool {
	if isLocalBackend(backend) {
		return true
	}
	return r.breakerAllow(backend.Name, now)
}

// backendAllowed is the query-path equivalent of backendHealthy.
func (r *Router) backendAllowed(backend Backend, now time.Time) bool {
	if isLocalBackend(backend) {
		return true
	}
	return r.breakerAllow(backend.Name, now)
}

func (r *Router) recordSuccess(name string) {
	r.mu.RLock()
	b, ok := r.breakers[name]
	r.mu.RUnlock()
	if ok {
		b.recordSuccess()
	}
}

// ReportBackendHealth lets an active health probe update the endpoint status
// immediately. A nil error clears the circuit; a non-nil error opens it.
func (r *Router) ReportBackendHealth(name string, err error) {
	name = strings.TrimSpace(name)
	if r == nil || name == "" {
		return
	}
	if err == nil {
		r.recordSuccess(name)
		return
	}
	r.breakerFor(name).forceOpen(time.Now())
}

func (r *Router) backendTimeout(backend Backend) time.Duration {
	if backend.Timeout > 0 {
		return backend.Timeout
	}
	return r.cfg.BackendTimeout
}

// backendCallTimeout bounds one upstream call. A caller that explicitly
// requests a longer deadline (for example a PromQL ?timeout=2m query) must not
// be clamped back to the default short backend timeout.
func (r *Router) backendCallTimeout(ctx context.Context, backend Backend) time.Duration {
	timeout := r.backendTimeout(backend)
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > timeout {
			return remaining
		}
	}
	return timeout
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

// forceOpen opens the circuit immediately. Active health probes use this so
// the status view does not wait for the retry threshold.
func (b *breaker) forceOpen(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = breakerThreshold
	b.openUntil = now.Add(breakerCooldown)
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
