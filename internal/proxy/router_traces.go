package proxy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/access"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/tempo"
)

// ListServices returns the union of trace service names across backends.
func (r *Router) ListServices(ctx context.Context, tenant string) ([]string, []Warning, error) {
	targets, accessWarnings, err := r.traceTargets(ctx)
	if err != nil {
		return nil, accessWarnings, err
	}
	names := make(map[string]struct{})
	warnings := append([]Warning(nil), accessWarnings...)
	for _, target := range targets {
		upstream := target.upstreams.Traces
		if upstream == nil {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, r.backendTimeout(target.backend))
		services, err := upstream.ListServices(callCtx, tenant)
		cancel()
		if err != nil {
			r.noteBackendFailure(target.backend, err)
			warnings = append(warnings, Warning{Backend: target.backend.Name, Signal: SignalTraces, Message: err.Error()})
			continue
		}
		r.recordSuccess(target.backend.Name)
		for _, service := range services {
			if service != "" {
				names[service] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, warnings, nil
}

// SearchTraces fans a trace search out and merges the summaries.
func (r *Router) SearchTraces(ctx context.Context, tenant string, search TraceSearch) ([]TraceSummary, []Warning, error) {
	criteria := parseTraceSearchCriteria(search)
	targets, accessWarnings, err := r.traceTargetsWithCriteria(ctx, criteria)
	if err != nil {
		return nil, accessWarnings, err
	}
	if search.TraceQL != "" {
		capable := false
		for _, target := range targets {
			if _, ok := target.upstreams.Traces.(TraceQLUpstream); ok {
				capable = true
				break
			}
		}
		if capable {
			groups := make([][]TraceSummary, len(targets))
			warnings, err := r.forEachTraceTarget(ctx, targets, accessWarnings, func(ctx context.Context, target traceTarget) error {
				upstream, ok := target.upstreams.Traces.(TraceQLUpstream)
				if !ok {
					return nil
				}
				summaries, queryErr := upstream.SearchTraceQL(ctx, tenant, search.TraceQL, search.From, search.To, search.Limit)
				if queryErr != nil {
					return queryErr
				}
				groups[target.index] = summaries
				return nil
			})
			if err != nil {
				return nil, warnings, err
			}
			return MergeTraceSummaries(groups...), warnings, nil
		}

		// No TraceQL-capable backend: fall back to the shared subset matcher
		// for local/AWS/Elasticsearch sources.
		if err := search.ApplyTraceQL(); err != nil {
			return nil, accessWarnings, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		if search.MatchNone {
			return nil, accessWarnings, nil
		}
	}
	if len(targets) == 0 {
		return nil, nil, nil
	}

	groups := make([][]TraceSummary, len(targets))
	warnings, err := r.forEachTraceTarget(ctx, targets, accessWarnings, func(ctx context.Context, target traceTarget) error {
		summaries, queryErr := target.upstreams.Traces.SearchTraces(ctx, tenant, search)
		if queryErr != nil {
			return queryErr
		}
		groups[target.index] = summaries
		return nil
	})
	if err != nil {
		return nil, warnings, err
	}
	return MergeTraceSummaries(groups...), warnings, nil
}

// GetTrace fans a trace lookup out and merges the spans. found is false when
// no backend returned spans.
func (r *Router) GetTrace(ctx context.Context, tenant, traceID string) ([]segment.TraceRecord, []Warning, error) {
	return r.GetTraceWindow(ctx, tenant, traceID, time.Time{}, time.Time{})
}

// GetTraceWindow is GetTrace with an explicit time window for backends that
// can use it. It keeps trace detail from scanning the entire local store.
func (r *Router) GetTraceWindow(ctx context.Context, tenant, traceID string, from, to time.Time) ([]segment.TraceRecord, []Warning, error) {
	targets, accessWarnings, err := r.traceTargets(ctx)
	if err != nil {
		return nil, accessWarnings, err
	}
	if len(targets) == 0 {
		return nil, accessWarnings, nil
	}
	type result struct {
		index   int
		records []segment.TraceRecord
		found   bool
		err     error
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, r.cfg.MaxInflight)
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target traceTarget) {
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
			callCtx, cancel := context.WithTimeout(ctx, r.backendTimeout(target.backend))
			defer cancel()
			var (
				records []segment.TraceRecord
				found   bool
				err     error
			)
			if windowSource, ok := target.upstreams.Traces.(TraceWindowSource); ok {
				records, found, err = windowSource.GetTraceWindow(callCtx, tenant, traceID, from, to)
			} else {
				records, found, err = target.upstreams.Traces.GetTrace(callCtx, tenant, traceID)
			}
			results[i] = result{index: i, records: records, found: found, err: err}
		}(i, target)
	}
	wg.Wait()

	var (
		local    []segment.TraceRecord
		remotes  []TraceRemote
		warnings = append([]Warning(nil), accessWarnings...)
		localErr error
	)
	found := false
	for _, res := range results {
		target := targets[res.index]
		if res.err != nil {
			r.noteBackendFailure(target.backend, res.err)
			if isLocalBackend(target.backend) {
				localErr = res.err
				warnings = append(warnings, Warning{Backend: target.backend.Name, Signal: SignalTraces, Message: res.err.Error()})
				continue
			}
			warnings = append(warnings, Warning{Backend: target.backend.Name, Signal: SignalTraces, Message: res.err.Error()})
			continue
		}
		r.recordSuccess(target.backend.Name)
		if !res.found {
			continue
		}
		found = true
		if isLocalBackend(target.backend) {
			local = res.records
			continue
		}
		remotes = append(remotes, TraceRemote{
			Source:  sourceForKind(target.backend.Kind),
			Backend: target.backend.Name,
			Records: res.records,
		})
	}
	if !found {
		if localErr != nil {
			return nil, warnings, localErr
		}
		return nil, warnings, nil
	}
	return MergeTraceRecords(local, remotes...), warnings, nil
}

// TraceTags returns the union of tag names across tag-aware backends.
func (r *Router) TraceTags(ctx context.Context, tenant string) ([]string, []Warning, error) {
	targets, accessWarnings, err := r.traceTargets(ctx)
	if err != nil {
		return nil, accessWarnings, err
	}
	tags := make(map[string]struct{})
	warnings := append([]Warning(nil), accessWarnings...)
	for _, target := range targets {
		source, ok := target.upstreams.Traces.(TraceTagSource)
		if !ok {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, r.backendTimeout(target.backend))
		names, err := source.TraceTags(callCtx, tenant)
		cancel()
		if err != nil {
			warnings = append(warnings, Warning{Backend: target.backend.Name, Signal: SignalTraces, Message: err.Error()})
			continue
		}
		for _, name := range names {
			if name != "" {
				tags[name] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(tags))
	for tag := range tags {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out, warnings, nil
}

// TraceTagValues returns the union of values for one tag across tag-aware
// backends.
func (r *Router) TraceTagValues(ctx context.Context, tenant, tag string) ([]string, []Warning, error) {
	targets, accessWarnings, err := r.traceTargets(ctx)
	if err != nil {
		return nil, accessWarnings, err
	}
	values := make(map[string]struct{})
	warnings := append([]Warning(nil), accessWarnings...)
	for _, target := range targets {
		source, ok := target.upstreams.Traces.(TraceTagSource)
		if !ok {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, r.backendTimeout(target.backend))
		found, err := source.TraceTagValues(callCtx, tenant, tag)
		cancel()
		if err != nil {
			warnings = append(warnings, Warning{Backend: target.backend.Name, Signal: SignalTraces, Message: err.Error()})
			continue
		}
		for _, value := range found {
			if value != "" {
				values[value] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out, warnings, nil
}

// SearchSpanSets returns the matched spans of a search grouped by trace ID.
// Only span-capable upstreams contribute; the trace access gate applies.
func (r *Router) SearchSpanSets(ctx context.Context, tenant string, search TraceSearch, spss int) (map[string]tempo.SpanSet, []Warning, error) {
	criteria := parseTraceSearchCriteria(search)
	targets, accessWarnings, err := r.traceTargetsWithCriteria(ctx, criteria)
	if err != nil {
		return nil, accessWarnings, err
	}
	results := make([]map[string]tempo.SpanSet, len(targets))
	warnings, err := r.forEachTraceTarget(ctx, targets, accessWarnings, func(ctx context.Context, target traceTarget) error {
		source, ok := target.upstreams.Traces.(TraceSpanSource)
		if !ok {
			return nil
		}
		sets, queryErr := source.SearchSpanSets(ctx, tenant, search, spss)
		if queryErr != nil {
			return queryErr
		}
		results[target.index] = sets
		return nil
	})
	if err != nil {
		return nil, warnings, err
	}
	merged := make(map[string]tempo.SpanSet)
	for _, sets := range results {
		mergeSpanSets(merged, sets)
	}
	return merged, warnings, nil
}

func mergeSpanSets(dst, src map[string]tempo.SpanSet) {
	for traceID, incoming := range src {
		existing, ok := dst[traceID]
		if !ok {
			dst[traceID] = incoming
			continue
		}
		seen := make(map[string]struct{}, len(existing.Spans))
		for _, span := range existing.Spans {
			seen[span.SpanID] = struct{}{}
		}
		for _, span := range incoming.Spans {
			if _, duplicate := seen[span.SpanID]; duplicate {
				continue
			}
			existing.Spans = append(existing.Spans, span)
			seen[span.SpanID] = struct{}{}
		}
		if incoming.Matched > existing.Matched {
			existing.Matched = incoming.Matched
		}
		dst[traceID] = existing
	}
}

// TraceTagScopes returns the union of scoped tag names across scope-aware
// backends, falling back to classifying the plain tag list for backends that
// only implement TraceTagSource.
func (r *Router) TraceTagScopes(ctx context.Context, tenant string) ([]tempo.TagScope, []Warning, error) {
	targets, accessWarnings, err := r.traceTargets(ctx)
	if err != nil {
		return nil, accessWarnings, err
	}
	scopeTags := map[string]map[string]struct{}{
		"intrinsic": {},
		"resource":  {},
		"span":      {},
	}
	warnings := append([]Warning(nil), accessWarnings...)
	for _, target := range targets {
		callCtx, cancel := context.WithTimeout(ctx, r.backendTimeout(target.backend))
		if scopeSource, ok := target.upstreams.Traces.(TraceTagScopeSource); ok {
			scopes, scopeErr := scopeSource.TraceTagScopes(callCtx, tenant)
			cancel()
			if scopeErr != nil {
				warnings = append(warnings, Warning{Backend: target.backend.Name, Signal: SignalTraces, Message: scopeErr.Error()})
				continue
			}
			for _, scope := range scopes {
				collectTagScope(scopeTags, scope.Name, scope.Tags)
			}
			continue
		}
		if tagSource, ok := target.upstreams.Traces.(TraceTagSource); ok {
			names, tagErr := tagSource.TraceTags(callCtx, tenant)
			cancel()
			if tagErr != nil {
				warnings = append(warnings, Warning{Backend: target.backend.Name, Signal: SignalTraces, Message: tagErr.Error()})
				continue
			}
			for _, name := range names {
				collectTagScope(scopeTags, classifyTagScope(name), []string{name})
			}
			continue
		}
		cancel()
	}
	scopes := make([]tempo.TagScope, 0, len(scopeTags))
	for _, name := range []string{"intrinsic", "resource", "span"} {
		tags := sortedKeys(scopeTags[name])
		if len(tags) == 0 {
			continue
		}
		scopes = append(scopes, tempo.TagScope{Name: name, Tags: tags})
	}
	return scopes, warnings, nil
}

func collectTagScope(scopeTags map[string]map[string]struct{}, scope string, tags []string) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		scope = "span"
	}
	bucket := scopeTags[scope]
	if bucket == nil {
		bucket = map[string]struct{}{}
		scopeTags[scope] = bucket
	}
	for _, tag := range tags {
		if tag != "" {
			bucket[tag] = struct{}{}
		}
	}
}

// intrinsicTagNames are the span intrinsics Grafana's TraceQL editors offer.
var intrinsicTagNames = map[string]struct{}{
	"name": {}, "status": {}, "kind": {}, "duration": {},
}

// resourceTagPrefixes are attribute prefixes that identify resource attributes
// when an upstream can only report flat tag names.
var resourceTagPrefixes = []string{
	"k8s.", "host.", "os.", "cloud.", "container.", "process.", "telemetry.",
	"guardian_", "aws_", "service.", "device.", "sdk.", "deployment.", "kube.",
}

// classifyTagScope best-effort maps a flat tag name onto a Tempo scope.
func classifyTagScope(name string) string {
	if _, ok := intrinsicTagNames[name]; ok {
		return "intrinsic"
	}
	if name == "service.name" || name == "service" {
		return "resource"
	}
	for _, prefix := range resourceTagPrefixes {
		if strings.HasPrefix(name, prefix) {
			return "resource"
		}
	}
	return "span"
}

func sortedKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// LocalTraceAccessError returns access.ErrAccessDenied when the trace gate
// enforces a denial for the local (doctor) backend. Handlers that read the
// local store directly must call this before scanning.
func (r *Router) LocalTraceAccessError(ctx context.Context) error {
	for _, backend := range r.snapshot() {
		if !backend.Supports(SignalTraces) || !isLocalBackend(backend) {
			continue
		}
		allowed, _, enforced := r.accessDecision(ctx, backend, access.SignalTraces)
		if enforced && !allowed {
			return access.ErrAccessDenied
		}
		return nil
	}
	return nil
}

// traceTarget is an upstreamEntry with its position for deterministic merging.
type traceTarget struct {
	upstreamEntry
	index int
}

func (r *Router) traceTargets(ctx context.Context) ([]traceTarget, []Warning, error) {
	return r.traceTargetsWithCriteria(ctx, selectorCriteria{})
}

func (r *Router) traceTargetsWithCriteria(ctx context.Context, criteria selectorCriteria) ([]traceTarget, []Warning, error) {
	entries := make([]traceTarget, 0)
	var warnings []Warning
	enforced := 0
	denied := 0
	for _, backend := range r.snapshot() {
		if !backend.Supports(SignalTraces) {
			continue
		}
		if !backendMatchesCriteria(backend, criteria) {
			continue
		}
		entry := r.entryFor(backend)
		if entry.upstreams.Traces == nil {
			continue
		}
		_, decision, isEnforced := r.accessDecision(ctx, backend, access.SignalTraces)
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
		entries = append(entries, traceTarget{upstreamEntry: entry})
	}
	if enforced > 0 && denied == enforced {
		return nil, warnings, access.ErrAccessDenied
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return targetRank(entries[i].backend) < targetRank(entries[j].backend)
	})
	maxRemote := r.cfg.MaxFanoutBackends
	if maxRemote > 0 && len(entries) > maxRemote {
		capped := make([]traceTarget, 0, maxRemote+1)
		remoteCount := 0
		totalRemote := 0
		for _, e := range entries {
			if isLocalBackend(e.backend) {
				capped = append(capped, e)
				continue
			}
			totalRemote++
			if remoteCount < maxRemote {
				capped = append(capped, e)
				remoteCount++
			}
		}
		if totalRemote > maxRemote {
			entries = capped
			warnings = append(warnings, Warning{
				Backend: "router",
				Signal:  SignalTraces,
				Message: fmt.Sprintf("remote fan-out capped at %d of %d matching backends; narrow query by region, account, cluster, or partition", maxRemote, totalRemote),
			})
		}
	}
	for i := range entries {
		entries[i].index = i
	}
	return entries, warnings, nil
}

func parseTraceSearchCriteria(search TraceSearch) selectorCriteria {
	c := parseSelectorCriteria(search.TraceQL)
	add := func(dst *map[string]struct{}, v string) {
		val := strings.ToLower(strings.TrimSpace(v))
		if val == "" {
			return
		}
		if *dst == nil {
			*dst = make(map[string]struct{}, 2)
		}
		(*dst)[val] = struct{}{}
	}
	for _, tag := range search.Tags {
		if tag.Regex != nil {
			continue
		}
		key := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(tag.Key)), "resource."), "span.")
		switch key {
		case "region", "aws_region", "cloud.region":
			add(&c.region, tag.Value)
		case "account", "aws_account", "cloud.account.id":
			add(&c.account, tag.Value)
		case "cluster", "k8s.cluster.name":
			add(&c.cluster, tag.Value)
		case "partition", "guardian_partition":
			add(&c.partition, tag.Value)
		case "shard", "pusher", "backend":
			add(&c.backend, tag.Value)
		}
	}
	return c
}

// forEachTraceTarget runs fn for each trace target with bounded concurrency.
// Local backend failures are fatal; remote failures become warnings.
func (r *Router) forEachTraceTarget(ctx context.Context, targets []traceTarget, baseWarnings []Warning, fn func(ctx context.Context, target traceTarget) error) ([]Warning, error) {
	type result struct {
		target traceTarget
		err    error
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, r.cfg.MaxInflight)
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target traceTarget) {
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
			callCtx, cancel := context.WithTimeout(ctx, r.backendTimeout(target.backend))
			defer cancel()
			results[i] = result{target: target, err: fn(callCtx, target)}
		}(i, target)
	}
	wg.Wait()

	warnings := append([]Warning(nil), baseWarnings...)
	for _, res := range results {
		if res.err == nil {
			r.recordSuccess(res.target.backend.Name)
			continue
		}
		r.noteBackendFailure(res.target.backend, res.err)
		if isLocalBackend(res.target.backend) {
			return warnings, res.err
		}
		warnings = append(warnings, Warning{Backend: res.target.backend.Name, Signal: SignalTraces, Message: res.err.Error()})
	}
	return warnings, nil
}
