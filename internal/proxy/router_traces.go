package proxy

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/access"
	"github.com/rydzu/ainfra/doctor/internal/proxy/translate"
	"github.com/rydzu/ainfra/doctor/internal/segment"
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
			r.recordFailure(target.backend.Name, time.Now())
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
	targets, accessWarnings, err := r.traceTargets(ctx)
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
		translated, err := translate.ParseTraceQL(search.TraceQL)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		search.TraceQL = ""
		if translated.Service != "" {
			search.Service = translated.Service
		}
		for _, tag := range translated.Tags {
			search.Tags = append(search.Tags, TraceTag{Key: tag.Key, Value: tag.Value})
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
			if !r.breakerFor(target.backend.Name).allow(time.Now()) {
				results[i] = result{index: i, err: fmt.Errorf("backend %s circuit breaker open", target.backend.Name)}
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, r.backendTimeout(target.backend))
			defer cancel()
			records, found, err := target.upstreams.Traces.GetTrace(callCtx, tenant, traceID)
			results[i] = result{index: i, records: records, found: found, err: err}
		}(i, target)
	}
	wg.Wait()

	var (
		local    []segment.TraceRecord
		remotes  []TraceRemote
		warnings = append([]Warning(nil), accessWarnings...)
	)
	found := false
	for _, res := range results {
		target := targets[res.index]
		if res.err != nil {
			r.recordFailure(target.backend.Name, time.Now())
			if isLocalBackend(target.backend) {
				return nil, warnings, res.err
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

// traceTarget is an upstreamEntry with its position for deterministic merging.
type traceTarget struct {
	upstreamEntry
	index int
}

func (r *Router) traceTargets(ctx context.Context) ([]traceTarget, []Warning, error) {
	entries := make([]traceTarget, 0)
	var warnings []Warning
	enforced := 0
	denied := 0
	for _, backend := range r.snapshot() {
		if !backend.Supports(SignalTraces) {
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
		entries = append(entries, traceTarget{upstreamEntry: entry, index: len(entries)})
	}
	if enforced > 0 && denied == enforced {
		return nil, warnings, access.ErrAccessDenied
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return targetRank(entries[i].backend) < targetRank(entries[j].backend)
	})
	return entries, warnings, nil
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
			if !r.breakerFor(target.backend.Name).allow(time.Now()) {
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
		r.recordFailure(res.target.backend.Name, time.Now())
		if isLocalBackend(res.target.backend) {
			return warnings, res.err
		}
		warnings = append(warnings, Warning{Backend: res.target.backend.Name, Signal: SignalTraces, Message: res.err.Error()})
	}
	return warnings, nil
}
