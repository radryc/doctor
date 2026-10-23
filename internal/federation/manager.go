package federation

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

// Options configures the federation manager.
type Options struct {
	MaxFanout            int
	MaxInflight          int
	MaxConcurrentTargets int
	MaxAttempts          int
	ClientPoolMax        int
	ClientIdleTTL        time.Duration
	BreakerThreshold     int
	BreakerCooldown      time.Duration
	MetadataTTL          time.Duration
	Timeout              time.Duration
	StaleAfter           time.Duration
	Source               string
	ClientFactory        ClientFactory
	TokenResolver        TokenResolver
	Logger               *log.Logger
}

type managerState struct {
	catalog  *Catalog
	planner  *Planner
	source   string
	loadedAt time.Time
}

// Manager implements proxy.AWSClient by resolving a query to a bounded set of
// catalog targets, fanning out, and merging the results.
type Manager struct {
	state                atomic.Pointer[managerState]
	factory              ClientFactory
	tokenResolver        TokenResolver
	timeout              time.Duration
	staleAfter           time.Duration
	maxFanout            int
	maxConcurrentTargets int
	maxAttempts          int
	metrics              *CatalogMetrics
	breakerThreshold     int
	breakerCooldown      time.Duration
	metadataTTL          time.Duration
	logger               *log.Logger

	inflight chan struct{}

	discoveryMu        sync.Mutex
	servicesCache      []awsstore.ServiceInfo
	servicesCachedAt   time.Time
	servicesCacheValid bool
	pool               *clientPool

	breakerMu sync.Mutex
	breakers  map[string]*targetBreaker
}

// NewManager builds a federation manager.
func NewManager(catalog *Catalog, opts Options) *Manager {
	if opts.MaxFanout <= 0 {
		opts.MaxFanout = 32
	}
	if opts.MaxInflight <= 0 {
		opts.MaxInflight = 64
	}
	if opts.MaxConcurrentTargets <= 0 {
		opts.MaxConcurrentTargets = 8
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 2
	}
	if opts.ClientPoolMax <= 0 {
		opts.ClientPoolMax = 10000
	}
	if opts.ClientIdleTTL <= 0 {
		opts.ClientIdleTTL = 15 * time.Minute
	}
	if opts.BreakerThreshold <= 0 {
		opts.BreakerThreshold = 5
	}
	if opts.BreakerCooldown <= 0 {
		opts.BreakerCooldown = 30 * time.Second
	}
	if opts.MetadataTTL <= 0 {
		opts.MetadataTTL = time.Hour
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.ClientFactory == nil {
		opts.ClientFactory = func(target Target) (proxy.AWSClient, error) {
			if target.Token != "" {
				return awsstore.NewClient(target.Endpoint, target.Token), nil
			}
			if opts.TokenResolver == nil {
				return nil, fmt.Errorf("federation target %q has no token and no token resolver is configured", target.ID)
			}
			return awsstore.NewClientWithTokenSource(target.Endpoint, targetTokenSource{
				resolver: opts.TokenResolver,
				target:   target,
			}), nil
		}
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.StaleAfter < 0 {
		opts.StaleAfter = 0
	}
	manager := &Manager{
		factory:              opts.ClientFactory,
		tokenResolver:        opts.TokenResolver,
		timeout:              opts.Timeout,
		staleAfter:           opts.StaleAfter,
		maxFanout:            opts.MaxFanout,
		maxConcurrentTargets: opts.MaxConcurrentTargets,
		maxAttempts:          opts.MaxAttempts,
		breakerThreshold:     opts.BreakerThreshold,
		breakerCooldown:      opts.BreakerCooldown,
		metadataTTL:          opts.MetadataTTL,
		logger:               opts.Logger,
		inflight:             make(chan struct{}, opts.MaxInflight),
		pool:                 newClientPool(opts.ClientPoolMax, opts.ClientIdleTTL),
		breakers:             make(map[string]*targetBreaker),
	}
	manager.state.Store(&managerState{
		catalog:  catalog,
		planner:  NewPlanner(catalog, opts.MaxFanout),
		source:   strings.TrimSpace(opts.Source),
		loadedAt: time.Now().UTC(),
	})
	return manager
}

// Run expires idle clients and keeps the pool bounded until the context ends.
func (m *Manager) Run(ctx context.Context) {
	if m == nil || m.pool == nil {
		return
	}
	interval := m.pool.idleTTL / 2
	if interval <= 0 || interval > time.Minute {
		interval = time.Minute
	}
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pool.prune(func(Target) bool { return true })
		}
	}
}

func (m *Manager) current() *managerState {
	if m == nil {
		return nil
	}
	return m.state.Load()
}

// Reload atomically swaps the catalog and planner. Existing target clients are
// retained unless their endpoint or token reference changed.
func (m *Manager) Reload(catalog *Catalog, source string) error {
	if m == nil {
		return errors.New("federation manager is nil")
	}
	if catalog == nil {
		return errors.New("federation catalog is nil")
	}
	next := &managerState{
		catalog:  catalog,
		planner:  NewPlanner(catalog, m.maxFanout),
		source:   strings.TrimSpace(source),
		loadedAt: time.Now().UTC(),
	}
	previous := m.state.Swap(next)
	m.invalidateDiscoveryCache()
	m.pruneClients(catalog)
	if previous != nil && previous.catalog != nil {
		m.logger.Printf("federation catalog reloaded: %s -> %s (%d targets)",
			previous.catalog.Version(), catalog.Version(), catalog.Len())
	} else {
		m.logger.Printf("federation catalog loaded: %s (%d targets)", catalog.Version(), catalog.Len())
	}
	return nil
}

func (m *Manager) invalidateDiscoveryCache() {
	m.discoveryMu.Lock()
	m.servicesCache = nil
	m.servicesCachedAt = time.Time{}
	m.servicesCacheValid = false
	m.discoveryMu.Unlock()
}

func (m *Manager) cachedServices() ([]awsstore.ServiceInfo, bool) {
	m.discoveryMu.Lock()
	defer m.discoveryMu.Unlock()
	if !m.servicesCacheValid {
		return nil, false
	}
	if m.metadataTTL > 0 && time.Since(m.servicesCachedAt) > m.metadataTTL {
		m.servicesCacheValid = false
		return nil, false
	}
	return append([]awsstore.ServiceInfo(nil), m.servicesCache...), true
}

func (m *Manager) storeCachedServices(services []awsstore.ServiceInfo) {
	m.discoveryMu.Lock()
	m.servicesCache = append([]awsstore.ServiceInfo(nil), services...)
	m.servicesCachedAt = time.Now().UTC()
	m.servicesCacheValid = true
	m.discoveryMu.Unlock()
}

func (m *Manager) pruneClients(catalog *Catalog) {
	if catalog == nil || m.pool == nil {
		return
	}
	m.pool.prune(func(target Target) bool {
		next, ok := catalog.ByID(target.ID)
		return ok && sameClientTarget(next, target)
	})
}

func sameClientTarget(left, right Target) bool {
	return left.Endpoint == right.Endpoint &&
		left.TokenRef == right.TokenRef &&
		left.TokenEnv == right.TokenEnv &&
		left.Token == right.Token
}

// Enabled reports whether federation is configured.
func (m *Manager) Enabled() bool {
	state := m.current()
	return state != nil && state.catalog != nil && state.catalog.Len() > 0
}

// Version returns the current catalog version.
func (m *Manager) Version() string {
	state := m.current()
	if state == nil || state.catalog == nil {
		return ""
	}
	return state.catalog.Version()
}

// SetMetrics attaches query-plane telemetry after registration.
func (m *Manager) SetMetrics(metrics *CatalogMetrics) {
	if m != nil {
		m.metrics = metrics
	}
}

// Planner exposes the underlying planner for admin/debug endpoints.
func (m *Manager) Planner() *Planner {
	state := m.current()
	if state == nil {
		return nil
	}
	return state.planner
}

// statusMaxPartitionEntries bounds the partition map in the status response.
// The aggregate count remains complete; the map exists for the small demo and
// operator convenience, not for serializing 150k rows.
const statusMaxPartitionEntries = 512

// Status summarizes the federation catalog for the admin/status endpoint.
func (m *Manager) Status() map[string]any {
	state := m.current()
	if state == nil || state.catalog == nil {
		return map[string]any{"enabled": false}
	}
	targets := state.catalog.Targets()
	regions := make(map[string]int)
	partitions := make(map[string]int, statusMaxPartitionEntries)
	allPartitions := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		regions[target.Region]++
		for _, partition := range target.Partitions {
			allPartitions[partition] = struct{}{}
			if _, included := partitions[partition]; included {
				partitions[partition]++
			} else if len(partitions) < statusMaxPartitionEntries {
				partitions[partition] = 1
			}
		}
	}
	stale := m.staleAfter > 0 && !state.loadedAt.IsZero() && time.Since(state.loadedAt) > m.staleAfter
	openCircuits, circuits := m.circuitStatus()
	recentErrors := make([]string, 0, len(circuits))
	for _, circuit := range circuits {
		if circuit.LastError != "" {
			recentErrors = append(recentErrors, circuit.Target+": "+circuit.LastError)
		}
		if len(recentErrors) >= 16 {
			break
		}
	}
	return map[string]any{
		"enabled":              m.Enabled(),
		"targets":              len(targets),
		"max_fanout":           state.planner.MaxFanout(),
		"version":              state.catalog.Version(),
		"source":               state.source,
		"loaded_at":            state.loadedAt,
		"stale":                stale,
		"regions":              regions,
		"partitions":           partitions,
		"partition_count":      len(allPartitions),
		"partitions_truncated": len(allPartitions) > len(partitions),
		"open_circuits":        openCircuits,
		"circuits":             circuits,
		"recent_errors":        recentErrors,
		"client_pool_size":     m.pool.len(),
	}
}

// CheckScope implements ScopeChecker.
func (m *Manager) CheckScope(ctx context.Context, q Query) error {
	state := m.current()
	if state == nil || state.planner == nil {
		return errors.New("federation manager is not configured")
	}
	return state.planner.CheckScope(ctx, q)
}

func (m *Manager) clientFor(target Target) (proxy.AWSClient, error) {
	if m.pool != nil {
		if client, ok := m.pool.get(target); ok {
			return client, nil
		}
	}
	client, err := m.factory(target)
	if err != nil {
		return nil, err
	}
	if m.pool != nil {
		return m.pool.put(target, client), nil
	}
	return client, nil
}

func (m *Manager) plan(ctx context.Context, q Query) ([]Target, error) {
	return m.planWith(m.current(), ctx, q)
}

func (m *Manager) planWith(state *managerState, ctx context.Context, q Query) ([]Target, error) {
	if state == nil || state.planner == nil {
		return nil, errors.New("federation manager is not configured")
	}
	started := time.Now()
	plan, err := state.planner.Plan(ctx, q)
	selected := len(plan.Targets)
	if meta := RequestMetaFromContext(ctx); meta != nil && state.catalog != nil {
		meta.SetPlan(state.catalog.Version(), plan.Matched, selected)
	}
	if m.metrics != nil {
		m.metrics.RecordPlan(string(q.Signal), string(plan.Mode), plan.Matched, selected, time.Since(started))
	}
	if err != nil {
		return nil, err
	}
	if plan.Mode != PlanPull {
		return nil, &ScopeError{Err: scopeError(plan.Reason), Matched: plan.Matched, MaxFanout: state.planner.MaxFanout()}
	}
	return plan.Targets, nil
}

// ListMetrics returns the union of cached metric descriptors. A small catalog
// without cached names falls back to live discovery; fleet-scale catalogs must
// be populated by the catalog builder.
func (m *Manager) ListMetrics(ctx context.Context, namespace, metric string) ([]awsstore.MetricDescriptor, error) {
	state := m.current()
	if state == nil || state.catalog == nil || state.catalog.Len() == 0 {
		return nil, errors.New("aws federation is disabled")
	}
	targets := state.catalog.Targets()
	if len(targets) == 0 {
		return nil, nil
	}

	cached := make([]awsstore.MetricDescriptor, 0)
	for _, target := range targets {
		for _, name := range target.MetricNames {
			if metric != "" && name != metric {
				continue
			}
			cached = append(cached, awsstore.MetricDescriptor{MetricName: name, Region: target.Region})
		}
	}
	if len(cached) > 0 {
		return dedupeMetricDescriptors(cached), nil
	}
	if len(targets) > state.planner.MaxFanout() {
		return nil, nil
	}

	results := fanoutTargets(m, ctx, targets, SignalMetrics, func(callCtx context.Context, target Target) ([]awsstore.MetricDescriptor, error) {
		client, err := m.clientFor(target)
		if err != nil {
			return nil, err
		}
		return client.ListMetrics(callCtx, namespace, metric)
	})
	out := make([]awsstore.MetricDescriptor, 0)
	failures := 0
	for _, result := range results {
		if result.err != nil {
			failures++
			m.logger.Printf("federation: ListMetrics target=%s: %v", result.target.ID, result.err)
			continue
		}
		out = append(out, result.value...)
	}
	if len(out) == 0 && failures == len(targets) {
		return nil, fmt.Errorf("aws federation: all %d targets failed ListMetrics", failures)
	}
	return dedupeMetricDescriptors(out), nil
}

// ListServices returns the union of cached X-Ray services.
func (m *Manager) ListServices(ctx context.Context) ([]awsstore.ServiceInfo, error) {
	state := m.current()
	if state == nil || state.catalog == nil || state.catalog.Len() == 0 {
		return nil, errors.New("aws federation is disabled")
	}
	if services, ok := m.cachedServices(); ok {
		return services, nil
	}
	targets := state.catalog.Targets()
	now := time.Now().UTC()

	cached := make([]awsstore.ServiceInfo, 0)
	stale := make([]Target, 0)
	for _, target := range targets {
		for _, name := range target.XrayServices {
			cached = append(cached, awsstore.ServiceInfo{Name: name, Region: target.Region, Account: target.Account})
		}
		if target.MetadataFetchedAt.IsZero() || now.Sub(target.MetadataFetchedAt) > m.metadataTTL {
			stale = append(stale, target)
		}
	}
	if len(stale) == 0 {
		result := dedupeServices(cached)
		m.storeCachedServices(result)
		return result, nil
	}
	if len(stale) > state.planner.MaxFanout() {
		// The controller should populate metadata for fleet-scale catalogs.
		// Never turn an empty discovery cache into an unbounded request fan-out.
		return dedupeServices(cached), nil
	}

	results := fanoutTargets(m, ctx, stale, SignalTraces, func(callCtx context.Context, target Target) ([]awsstore.ServiceInfo, error) {
		client, err := m.clientFor(target)
		if err != nil {
			return nil, err
		}
		return client.ListServices(callCtx)
	})
	out := append([]awsstore.ServiceInfo(nil), cached...)
	failures := 0
	for _, result := range results {
		if result.err != nil {
			failures++
			m.logger.Printf("federation: ListServices target=%s: %v", result.target.ID, result.err)
			continue
		}
		out = append(out, result.value...)
	}
	if len(out) == 0 && failures == len(stale) {
		return nil, fmt.Errorf("aws federation: all %d targets failed ListServices", failures)
	}
	result := dedupeServices(out)
	m.storeCachedServices(result)
	return result, nil
}

// ListLogGroups returns the union of cached log groups.
func (m *Manager) ListLogGroups(ctx context.Context) ([]string, error) {
	state := m.current()
	if state == nil || state.catalog == nil || state.catalog.Len() == 0 {
		return nil, errors.New("aws federation is disabled")
	}
	targets := state.catalog.Targets()
	cached := make([]string, 0)
	for _, target := range targets {
		cached = append(cached, target.LogGroups...)
	}
	if len(cached) > 0 {
		return dedupeStrings(cached), nil
	}
	if len(targets) > state.planner.MaxFanout() {
		return nil, nil
	}

	results := fanoutTargets(m, ctx, targets, SignalLogs, func(callCtx context.Context, target Target) ([]string, error) {
		client, err := m.clientFor(target)
		if err != nil {
			return nil, err
		}
		return client.ListLogGroups(callCtx)
	})
	out := make([]string, 0)
	failures := 0
	for _, result := range results {
		if result.err != nil {
			failures++
			m.logger.Printf("federation: ListLogGroups target=%s: %v", result.target.ID, result.err)
			continue
		}
		out = append(out, result.value...)
	}
	if len(out) == 0 && failures == len(targets) {
		return nil, fmt.Errorf("aws federation: all %d targets failed ListLogGroups", failures)
	}
	return dedupeStrings(out), nil
}

// ResolveLogGroups resolves a concrete regex pattern to log groups known to
// the catalog. It lets proxy.AWSLogsForLoki avoid listing millions of groups.
func (m *Manager) ResolveLogGroups(ctx context.Context, pattern string) ([]string, error) {
	state := m.current()
	if state == nil || state.catalog == nil || state.catalog.Len() == 0 {
		return nil, errors.New("aws federation is disabled")
	}
	if strings.TrimSpace(pattern) == "" {
		return nil, nil
	}
	if groups := state.catalog.LogGroupsMatching(pattern); len(groups) > 0 {
		return groups, nil
	}
	groups, err := m.ListLogGroups(ctx)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for _, group := range groups {
		if re.MatchString(group) {
			out = append(out, group)
		}
	}
	return out, nil
}

// QueryLogs fans a CloudWatch Logs Insights query out to the targets that own
// the requested log groups.
func (m *Manager) QueryLogs(ctx context.Context, q awsstore.LogQuery) ([]awsstore.LogRecord, error) {
	state := m.current()
	targets, err := m.planWith(state, ctx, Query{
		Signal: SignalLogs, LogGroups: q.LogGroups, Expr: q.Query,
		From: q.From, To: q.To, Limit: q.Limit,
	})
	if err != nil {
		return nil, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	perTarget := limit / len(targets)
	if perTarget < 1 {
		perTarget = 1
	}

	results := fanoutTargets(m, ctx, targets, SignalLogs, func(callCtx context.Context, target Target) ([]awsstore.LogRecord, error) {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			return nil, clientErr
		}
		var catalog *Catalog
		if state != nil {
			catalog = state.catalog
		}
		groups := logGroupsForTarget(target, q.LogGroups, catalog)
		if len(groups) == 0 {
			return nil, nil
		}
		return client.QueryLogs(callCtx, awsstore.LogQuery{
			LogGroups: groups,
			Query:     q.Query,
			From:      q.From,
			To:        q.To,
			Limit:     perTarget,
		})
	})

	remotes := make([]proxy.LogRemote, 0, len(results))
	var firstErr error
	for _, res := range results {
		if res.err != nil {
			m.logger.Printf("federation: QueryLogs target=%s: %v", res.target.ID, res.err)
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		records := m.convertLogRecords(res.value, res.target)
		if len(records) > 0 {
			remotes = append(remotes, proxy.LogRemote{Source: proxy.SourceAWS, Backend: res.target.ID, Records: records})
		}
		m.recordLatency(res.target, "logs", 0)
	}
	if len(remotes) == 0 && firstErr != nil {
		return nil, firstErr
	}
	merged := proxy.MergeLogs(nil, remotes...)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return recordsToAWS(merged), nil
}

// QueryTraces fans X-Ray trace summaries out to the targets that can serve the
// requested service.
func (m *Manager) QueryTraces(ctx context.Context, q awsstore.TraceQuery) ([]awsstore.TraceSummary, error) {
	targets, err := m.plan(ctx, Query{
		Signal: SignalTraces, Service: q.Service, From: q.From, To: q.To, Limit: q.Limit,
	})
	if err != nil {
		return nil, err
	}

	results := fanoutTargets(m, ctx, targets, SignalTraces, func(callCtx context.Context, target Target) ([]awsstore.TraceSummary, error) {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			return nil, clientErr
		}
		return client.QueryTraces(callCtx, q)
	})

	byTrace := make(map[string]awsstore.TraceSummary)
	var firstErr error
	for _, res := range results {
		if res.err != nil {
			m.logger.Printf("federation: QueryTraces target=%s: %v", res.target.ID, res.err)
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		for _, item := range res.value {
			if item.TraceID == "" {
				continue
			}
			if item.Account == "" {
				item.Account = res.target.Account
			}
			if item.Region == "" {
				item.Region = res.target.Region
			}
			if existing, ok := byTrace[item.TraceID]; ok {
				if item.SpanCount > existing.SpanCount {
					byTrace[item.TraceID] = item
				}
				continue
			}
			byTrace[item.TraceID] = item
		}
	}
	if len(byTrace) == 0 && firstErr != nil {
		return nil, firstErr
	}
	out := make([]awsstore.TraceSummary, 0, len(byTrace))
	for _, item := range byTrace {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartTime.Equal(out[j].StartTime) {
			return out[i].TraceID < out[j].TraceID
		}
		return out[i].StartTime.After(out[j].StartTime)
	})
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetTrace queries the target that owns a trace ID. Without a trace index, a
// small catalog is tried; fleet-scale catalogs return a scope error.
func (m *Manager) GetTrace(ctx context.Context, traceID string) ([]awsstore.Span, error) {
	targets, err := m.plan(ctx, Query{Signal: SignalTraces, TraceID: traceID})
	if err != nil {
		return nil, err
	}
	results := fanoutTargets(m, ctx, targets, SignalTraces, func(callCtx context.Context, target Target) ([]awsstore.Span, error) {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			return nil, clientErr
		}
		return client.GetTrace(callCtx, traceID)
	})
	out := make([]awsstore.Span, 0)
	seen := make(map[string]struct{})
	var firstErr error
	for _, result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		for _, span := range result.value {
			if span.Account == "" {
				span.Account = result.target.Account
			}
			if span.Region == "" {
				span.Region = result.target.Region
			}
			key := span.TraceID + "\x00" + span.SpanID
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, span)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, ErrNoTargets
}

// PrometheusQuery fans an instant PromQL query out to matching targets.
func (m *Manager) PrometheusQuery(ctx context.Context, query string, at time.Time) (map[string]any, error) {
	forwarded, _ := proxy.AWSQueryMode(query)
	targets, err := m.plan(ctx, Query{Signal: SignalMetrics, Expr: forwarded})
	if err != nil {
		return nil, err
	}
	forwarded = stripTargetLabels(forwarded)
	if strings.TrimSpace(forwarded) == "" {
		return emptyPrometheusResponse("vector"), nil
	}
	results := fanoutTargets(m, ctx, targets, SignalMetrics, func(callCtx context.Context, target Target) (map[string]any, error) {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			return nil, clientErr
		}
		resp, callErr := client.PrometheusQuery(callCtx, forwarded, at)
		if callErr != nil {
			return nil, callErr
		}
		return proxy.ExtractPrometheusData(resp), nil
	})
	remotes := make([]proxy.PromRemote, 0, len(results))
	var firstErr error
	for _, result := range results {
		if result.err != nil {
			m.logger.Printf("federation: PrometheusQuery target=%s: %v", result.target.ID, result.err)
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		remotes = append(remotes, proxy.PromRemote{Source: proxy.SourceAWS, Backend: result.target.ID, Data: result.value})
	}
	if len(remotes) == 0 && firstErr != nil {
		return nil, firstErr
	}
	data := proxy.MergePrometheusQueryData(nil, "vector", remotes...)
	return map[string]any{"status": "success", "data": data}, nil
}

// PrometheusQueryRange fans a range PromQL query out to matching targets.
func (m *Manager) PrometheusQueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	forwarded, _ := proxy.AWSQueryMode(query)
	targets, err := m.plan(ctx, Query{Signal: SignalMetrics, Expr: forwarded, From: start, To: end})
	if err != nil {
		return nil, err
	}
	forwarded = stripTargetLabels(forwarded)
	if strings.TrimSpace(forwarded) == "" {
		return emptyPrometheusResponse("matrix"), nil
	}
	results := fanoutTargets(m, ctx, targets, SignalMetrics, func(callCtx context.Context, target Target) (map[string]any, error) {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			return nil, clientErr
		}
		resp, callErr := client.PrometheusQueryRange(callCtx, forwarded, start, end, step)
		if callErr != nil {
			return nil, callErr
		}
		return proxy.ExtractPrometheusData(resp), nil
	})
	remotes := make([]proxy.PromRemote, 0, len(results))
	var firstErr error
	for _, result := range results {
		if result.err != nil {
			m.logger.Printf("federation: PrometheusQueryRange target=%s: %v", result.target.ID, result.err)
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		remotes = append(remotes, proxy.PromRemote{Source: proxy.SourceAWS, Backend: result.target.ID, Data: result.value})
	}
	if len(remotes) == 0 && firstErr != nil {
		return nil, firstErr
	}
	data := proxy.MergePrometheusQueryData(nil, "matrix", remotes...)
	return map[string]any{"status": "success", "data": data}, nil
}

// PrometheusSeries unions series across matching targets.
func (m *Manager) PrometheusSeries(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	targets, err := m.planMatches(ctx, match, start, end)
	if err != nil {
		return nil, err
	}
	results := fanoutTargets(m, ctx, targets, SignalMetrics, func(callCtx context.Context, target Target) ([]map[string]string, error) {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			return nil, clientErr
		}
		resp, callErr := client.PrometheusSeries(callCtx, match, start, end)
		if callErr != nil {
			return nil, callErr
		}
		return proxy.ExtractPrometheusSeries(resp), nil
	})
	seen := make(map[string]map[string]string)
	var firstErr error
	for _, result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		for _, series := range result.value {
			if series["source"] == "" {
				series["source"] = proxy.SourceAWS
			}
			seen[seriesKey(series)] = series
		}
	}
	if len(seen) == 0 && firstErr != nil {
		return nil, firstErr
	}
	data := make([]any, 0, len(seen))
	for _, series := range seen {
		data = append(data, series)
	}
	return map[string]any{"status": "success", "data": data}, nil
}

// PrometheusLabels unions label names across matching targets.
func (m *Manager) PrometheusLabels(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	targets, err := m.planMatches(ctx, match, start, end)
	if err != nil {
		return nil, err
	}
	results := fanoutTargets(m, ctx, targets, SignalMetrics, func(callCtx context.Context, target Target) ([]string, error) {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			return nil, clientErr
		}
		resp, callErr := client.PrometheusLabels(callCtx, match, start, end)
		if callErr != nil {
			return nil, callErr
		}
		return proxy.ExtractPrometheusStringList(resp), nil
	})
	seen := make(map[string]struct{})
	var firstErr error
	for _, result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		for _, value := range result.value {
			seen[value] = struct{}{}
		}
	}
	if len(seen) == 0 && firstErr != nil {
		return nil, firstErr
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return map[string]any{"status": "success", "data": out}, nil
}

// PrometheusLabelValues unions label values across matching targets.
func (m *Manager) PrometheusLabelValues(ctx context.Context, label string, match []string, start, end time.Time) (map[string]any, error) {
	targets, err := m.planMatches(ctx, match, start, end)
	if err != nil {
		return nil, err
	}
	results := fanoutTargets(m, ctx, targets, SignalMetrics, func(callCtx context.Context, target Target) ([]string, error) {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			return nil, clientErr
		}
		resp, callErr := client.PrometheusLabelValues(callCtx, label, match, start, end)
		if callErr != nil {
			return nil, callErr
		}
		return proxy.ExtractPrometheusStringList(resp), nil
	})
	seen := make(map[string]struct{})
	var firstErr error
	for _, result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		for _, value := range result.value {
			seen[value] = struct{}{}
		}
	}
	if len(seen) == 0 && firstErr != nil {
		return nil, firstErr
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return map[string]any{"status": "success", "data": out}, nil
}

func (m *Manager) planMatches(ctx context.Context, match []string, start, end time.Time) ([]Target, error) {
	forwarded := make([]string, 0, len(match))
	for _, selector := range match {
		stripped, _ := proxy.AWSQueryMode(selector)
		forwarded = append(forwarded, stripped)
	}
	expr := strings.Join(forwarded, " or ")
	return m.plan(ctx, Query{Signal: SignalMetrics, Expr: expr, From: start, To: end})
}

func (m *Manager) convertLogRecords(records []awsstore.LogRecord, target Target) []segment.LogRecord {
	return proxy.AWSLogRecords(records, target.Tenant, "", target.Account, target.Region)
}

func (m *Manager) recordLatency(_ Target, _ string, _ time.Duration) {}

func logGroupsForTarget(target Target, groups []string, catalog *Catalog) []string {
	if len(groups) == 0 {
		return nil
	}
	out := make([]string, 0, len(groups))
	for _, group := range groups {
		if account, _, _, ok := parseGuardianLogGroup(group); ok {
			if account == target.Account {
				out = append(out, group)
			}
			continue
		}
		if catalog != nil {
			owners := catalog.ByLogGroups([]string{group})
			if len(owners) > 0 {
				for _, owner := range owners {
					if owner.ID == target.ID {
						out = append(out, group)
						break
					}
				}
				continue
			}
		}
		out = append(out, group)
	}
	return out
}

func dedupeMetricDescriptors(in []awsstore.MetricDescriptor) []awsstore.MetricDescriptor {
	seen := make(map[string]struct{}, len(in))
	out := make([]awsstore.MetricDescriptor, 0, len(in))
	for _, item := range in {
		key := item.Namespace + "\x00" + item.MetricName + "\x00" + item.Region
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace == out[j].Namespace {
			return out[i].MetricName < out[j].MetricName
		}
		return out[i].Namespace < out[j].Namespace
	})
	return out
}

func dedupeServices(in []awsstore.ServiceInfo) []awsstore.ServiceInfo {
	seen := make(map[string]struct{}, len(in))
	out := make([]awsstore.ServiceInfo, 0, len(in))
	for _, item := range in {
		key := item.Name + "\x00" + item.Account + "\x00" + item.Region
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func seriesKey(series map[string]string) string {
	keys := make([]string, 0, len(series))
	for key := range series {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(series[key])
		builder.WriteByte(0)
	}
	return builder.String()
}

// recordsToAWS converts merged segment logs back to the awsstore shape expected
// by proxy.AWSClient. The manager uses segment records internally for merging.
var targetLabelMatcher = regexp.MustCompile(`(?i)\b(?:account|aws_account|region|aws_region|partition|guardian_partition)\s*(?:=|!=|=~|!~)\s*"[^"]*"\s*,?\s*`)

// stripTargetLabels removes Doctor-only target-selection labels before a
// PromQL query is forwarded to CloudWatch. The planner has already used those
// labels to choose the account/region/partition targets; CloudWatch does not
// expose them as metric labels.
func stripTargetLabels(query string) string {
	out := targetLabelMatcher.ReplaceAllString(query, "")
	out = strings.ReplaceAll(out, "{,", "{")
	out = strings.ReplaceAll(out, ",}", "}")
	out = strings.ReplaceAll(out, ",,", ",")
	out = strings.ReplaceAll(out, "{}", "")
	out = strings.TrimSpace(out)
	return out
}

func emptyPrometheusResponse(resultType string) map[string]any {
	return map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": resultType,
			"result":     []any{},
		},
	}
}

func recordsToAWS(records []segment.LogRecord) []awsstore.LogRecord {
	out := make([]awsstore.LogRecord, 0, len(records))
	for _, record := range records {
		fields := make(map[string]string, len(record.Attributes))
		for key, value := range record.Attributes {
			fields[key] = value
		}
		out = append(out, awsstore.LogRecord{
			Timestamp: record.Timestamp,
			LogGroup:  fields["aws_log_group"],
			LogStream: fields["aws_log_stream"],
			Message:   record.Body,
			Fields:    fields,
			Region:    fields["aws_region"],
			Account:   fields["aws_account"],
		})
	}
	return out
}
