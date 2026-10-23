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
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

// Options configures the federation manager.
type Options struct {
	MaxFanout     int
	Timeout       time.Duration
	ClientFactory ClientFactory
	TokenResolver TokenResolver
	Logger        *log.Logger
}

// Manager implements proxy.AWSClient by resolving a query to a bounded set of
// catalog targets, fanning out, and merging the results.
type Manager struct {
	catalog       *Catalog
	planner       *Planner
	factory       ClientFactory
	tokenResolver TokenResolver
	timeout       time.Duration
	logger        *log.Logger

	mu      sync.Mutex
	clients map[string]proxy.AWSClient
}

// NewManager builds a federation manager.
func NewManager(catalog *Catalog, opts Options) *Manager {
	if opts.MaxFanout <= 0 {
		opts.MaxFanout = 32
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
	return &Manager{
		catalog:       catalog,
		planner:       NewPlanner(catalog, opts.MaxFanout),
		factory:       opts.ClientFactory,
		tokenResolver: opts.TokenResolver,
		timeout:       opts.Timeout,
		logger:        opts.Logger,
		clients:       make(map[string]proxy.AWSClient),
	}
}

// Enabled reports whether federation is configured.
func (m *Manager) Enabled() bool {
	return m != nil && m.catalog != nil && m.catalog.Len() > 0
}

// Planner exposes the underlying planner for admin/debug endpoints.
func (m *Manager) Planner() *Planner {
	if m == nil {
		return nil
	}
	return m.planner
}

// statusMaxPartitionEntries bounds the partition map in the status response.
// The aggregate count remains complete; the map exists for the small demo and
// operator convenience, not for serializing 150k rows.
const statusMaxPartitionEntries = 512

// Status summarizes the federation catalog for the admin/status endpoint.
func (m *Manager) Status() map[string]any {
	if m == nil || m.catalog == nil {
		return map[string]any{"enabled": false}
	}
	targets := m.catalog.Targets()
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
	return map[string]any{
		"enabled":              m.Enabled(),
		"targets":              len(targets),
		"max_fanout":           m.planner.MaxFanout(),
		"version":              m.catalog.Version(),
		"regions":              regions,
		"partitions":           partitions,
		"partition_count":      len(allPartitions),
		"partitions_truncated": len(allPartitions) > len(partitions),
	}
}

// CheckScope implements ScopeChecker.
func (m *Manager) CheckScope(ctx context.Context, q Query) error {
	if m == nil || m.planner == nil {
		return errors.New("federation manager is not configured")
	}
	return m.planner.CheckScope(ctx, q)
}

func (m *Manager) clientFor(target Target) (proxy.AWSClient, error) {
	m.mu.Lock()
	if client, ok := m.clients[target.ID]; ok {
		m.mu.Unlock()
		return client, nil
	}
	m.mu.Unlock()

	client, err := m.factory(target)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if existing, ok := m.clients[target.ID]; ok {
		m.mu.Unlock()
		return existing, nil
	}
	m.clients[target.ID] = client
	m.mu.Unlock()
	return client, nil
}

func (m *Manager) plan(ctx context.Context, q Query) ([]Target, error) {
	if m == nil || m.planner == nil {
		return nil, errors.New("federation manager is not configured")
	}
	plan, err := m.planner.Plan(ctx, q)
	if err != nil {
		return nil, err
	}
	if plan.Mode != PlanPull {
		return nil, &ScopeError{Err: scopeError(plan.Reason), Matched: plan.Matched, MaxFanout: m.planner.MaxFanout()}
	}
	return plan.Targets, nil
}

// ListMetrics returns the union of cached metric descriptors. A small catalog
// without cached names falls back to live discovery; fleet-scale catalogs must
// be populated by the catalog builder.
func (m *Manager) ListMetrics(ctx context.Context, namespace, metric string) ([]awsstore.MetricDescriptor, error) {
	if !m.Enabled() {
		return nil, errors.New("aws federation is disabled")
	}
	targets := m.catalog.Targets()
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
	if len(targets) > m.planner.MaxFanout() {
		return nil, nil
	}

	var out []awsstore.MetricDescriptor
	var failures int
	for _, target := range targets {
		client, err := m.clientFor(target)
		if err != nil {
			failures++
			continue
		}
		descriptors, err := client.ListMetrics(ctx, namespace, metric)
		if err != nil {
			failures++
			m.logger.Printf("federation: ListMetrics target=%s: %v", target.ID, err)
			continue
		}
		out = append(out, descriptors...)
	}
	if len(out) == 0 && failures == len(targets) {
		return nil, fmt.Errorf("aws federation: all %d targets failed ListMetrics", failures)
	}
	return dedupeMetricDescriptors(out), nil
}

// ListServices returns the union of cached X-Ray services.
func (m *Manager) ListServices(ctx context.Context) ([]awsstore.ServiceInfo, error) {
	if !m.Enabled() {
		return nil, errors.New("aws federation is disabled")
	}
	targets := m.catalog.Targets()
	cached := make([]awsstore.ServiceInfo, 0)
	for _, target := range targets {
		for _, name := range target.XrayServices {
			cached = append(cached, awsstore.ServiceInfo{Name: name, Region: target.Region, Account: target.Account})
		}
	}
	if len(cached) > 0 {
		return dedupeServices(cached), nil
	}
	if len(targets) > m.planner.MaxFanout() {
		return nil, nil
	}

	var out []awsstore.ServiceInfo
	var failures int
	for _, target := range targets {
		client, err := m.clientFor(target)
		if err != nil {
			failures++
			continue
		}
		services, err := client.ListServices(ctx)
		if err != nil {
			failures++
			m.logger.Printf("federation: ListServices target=%s: %v", target.ID, err)
			continue
		}
		out = append(out, services...)
	}
	if len(out) == 0 && failures == len(targets) {
		return nil, fmt.Errorf("aws federation: all %d targets failed ListServices", failures)
	}
	return dedupeServices(out), nil
}

// ListLogGroups returns the union of cached log groups.
func (m *Manager) ListLogGroups(ctx context.Context) ([]string, error) {
	if !m.Enabled() {
		return nil, errors.New("aws federation is disabled")
	}
	targets := m.catalog.Targets()
	cached := make([]string, 0)
	for _, target := range targets {
		cached = append(cached, target.LogGroups...)
	}
	if len(cached) > 0 {
		return dedupeStrings(cached), nil
	}
	if len(targets) > m.planner.MaxFanout() {
		return nil, nil
	}

	var out []string
	var failures int
	for _, target := range targets {
		client, err := m.clientFor(target)
		if err != nil {
			failures++
			continue
		}
		groups, err := client.ListLogGroups(ctx)
		if err != nil {
			failures++
			m.logger.Printf("federation: ListLogGroups target=%s: %v", target.ID, err)
			continue
		}
		out = append(out, groups...)
	}
	if len(out) == 0 && failures == len(targets) {
		return nil, fmt.Errorf("aws federation: all %d targets failed ListLogGroups", failures)
	}
	return dedupeStrings(out), nil
}

// ResolveLogGroups resolves a concrete regex pattern to log groups known to
// the catalog. It lets proxy.AWSLogsForLoki avoid listing millions of groups.
func (m *Manager) ResolveLogGroups(ctx context.Context, pattern string) ([]string, error) {
	if !m.Enabled() {
		return nil, errors.New("aws federation is disabled")
	}
	if strings.TrimSpace(pattern) == "" {
		return nil, nil
	}
	if groups := m.catalog.LogGroupsMatching(pattern); len(groups) > 0 {
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
	targets, err := m.plan(ctx, Query{
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

	type result struct {
		target  Target
		records []awsstore.LogRecord
		err     error
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			client, clientErr := m.clientFor(target)
			if clientErr != nil {
				results[i] = result{target: target, err: clientErr}
				return
			}
			groups := logGroupsForTarget(target, q.LogGroups, m.catalog)
			if len(groups) == 0 {
				results[i] = result{target: target}
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, m.timeout)
			defer cancel()
			records, callErr := client.QueryLogs(callCtx, awsstore.LogQuery{
				LogGroups: groups,
				Query:     q.Query,
				From:      q.From,
				To:        q.To,
				Limit:     perTarget,
			})
			results[i] = result{target: target, records: records, err: callErr}
		}(i, target)
	}
	wg.Wait()

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
		records := m.convertLogRecords(res.records, res.target)
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

	type result struct {
		target Target
		items  []awsstore.TraceSummary
		err    error
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			client, clientErr := m.clientFor(target)
			if clientErr != nil {
				results[i] = result{target: target, err: clientErr}
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, m.timeout)
			defer cancel()
			items, callErr := client.QueryTraces(callCtx, q)
			results[i] = result{target: target, items: items, err: callErr}
		}(i, target)
	}
	wg.Wait()

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
		for _, item := range res.items {
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
	var out []awsstore.Span
	var firstErr error
	for _, target := range targets {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			firstErr = clientErr
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, m.timeout)
		spans, callErr := client.GetTrace(callCtx, traceID)
		cancel()
		if callErr != nil {
			firstErr = callErr
			continue
		}
		for i := range spans {
			if spans[i].Account == "" {
				spans[i].Account = target.Account
			}
			if spans[i].Region == "" {
				spans[i].Region = target.Region
			}
		}
		out = append(out, spans...)
		if len(out) > 0 {
			return out, nil
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
	remotes := make([]proxy.PromRemote, 0, len(targets))
	var firstErr error
	for _, target := range targets {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			firstErr = clientErr
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, m.timeout)
		resp, callErr := client.PrometheusQuery(callCtx, forwarded, at)
		cancel()
		if callErr != nil {
			m.logger.Printf("federation: PrometheusQuery target=%s: %v", target.ID, callErr)
			if firstErr == nil {
				firstErr = callErr
			}
			continue
		}
		remotes = append(remotes, proxy.PromRemote{Source: proxy.SourceAWS, Backend: target.ID, Data: proxy.ExtractPrometheusData(resp)})
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
	remotes := make([]proxy.PromRemote, 0, len(targets))
	var firstErr error
	for _, target := range targets {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			firstErr = clientErr
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, m.timeout+5*time.Second)
		resp, callErr := client.PrometheusQueryRange(callCtx, forwarded, start, end, step)
		cancel()
		if callErr != nil {
			m.logger.Printf("federation: PrometheusQueryRange target=%s: %v", target.ID, callErr)
			if firstErr == nil {
				firstErr = callErr
			}
			continue
		}
		remotes = append(remotes, proxy.PromRemote{Source: proxy.SourceAWS, Backend: target.ID, Data: proxy.ExtractPrometheusData(resp)})
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
	seen := make(map[string]map[string]string)
	for _, target := range targets {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, m.timeout)
		resp, callErr := client.PrometheusSeries(callCtx, match, start, end)
		cancel()
		if callErr != nil {
			m.logger.Printf("federation: PrometheusSeries target=%s: %v", target.ID, callErr)
			continue
		}
		for _, series := range proxy.ExtractPrometheusSeries(resp) {
			if series["source"] == "" {
				series["source"] = proxy.SourceAWS
			}
			seen[seriesKey(series)] = series
		}
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
	seen := make(map[string]struct{})
	for _, target := range targets {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, m.timeout)
		resp, callErr := client.PrometheusLabels(callCtx, match, start, end)
		cancel()
		if callErr != nil {
			continue
		}
		for _, value := range proxy.ExtractPrometheusStringList(resp) {
			seen[value] = struct{}{}
		}
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
	seen := make(map[string]struct{})
	for _, target := range targets {
		client, clientErr := m.clientFor(target)
		if clientErr != nil {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, m.timeout)
		resp, callErr := client.PrometheusLabelValues(callCtx, label, match, start, end)
		cancel()
		if callErr != nil {
			continue
		}
		for _, value := range proxy.ExtractPrometheusStringList(resp) {
			seen[value] = struct{}{}
		}
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
