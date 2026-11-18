package proxy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/radryc/doctor/internal/telemetrylabels"
)

// discoveryTarget is one backend selected for a labels/series request.
type discoveryTarget struct {
	backend   Backend
	upstream  MetricsUpstream
	selectors []string
}

type discoveryResult[T any] struct {
	target discoveryTarget
	value  T
	err    error
}

// MetricsSeries fans a /api/v1/series request out to every metric backend and
// merges the label sets. Backends explicitly excluded by a `source` matcher are
// skipped; remote failures become warnings so Grafana still gets partial data.
func (r *Router) MetricsSeries(ctx context.Context, tenant string, match []string, start, end time.Time, partition string) ([]map[string]string, []Warning, error) {
	targets, warnings := r.discoveryTargets(match, partition)
	results, runWarnings, err := runDiscovery(r, ctx, targets, func(callCtx context.Context, target discoveryTarget) ([]map[string]string, error) {
		return target.upstream.PromSeries(callCtx, tenant, target.selectors, start, end)
	})
	warnings = append(warnings, runWarnings...)
	if err != nil {
		return nil, warnings, err
	}
	groups := make([]SeriesGroup, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			warnings = append(warnings, Warning{Backend: result.target.backend.Name, Signal: SignalMetrics, Message: result.err.Error()})
			r.noteBackendFailure(result.target.backend, result.err)
			continue
		}
		r.recordSuccess(result.target.backend.Name)
		source := sourceForKind(result.target.backend.Kind)
		groups = append(groups, SeriesGroup{
			Source:  source,
			Backend: result.target.backend.Name,
			Series:  StampSeriesSource(result.value, source),
		})
	}
	return MergePrometheusSeries(groups), warnings, nil
}

// MetricsLabels fans a /api/v1/labels request out to every metric backend and
// unions the label names.
func (r *Router) MetricsLabels(ctx context.Context, tenant string, match []string, start, end time.Time, partition string) ([]string, []Warning, error) {
	targets, warnings := r.discoveryTargets(match, partition)
	results, runWarnings, err := runDiscovery(r, ctx, targets, func(callCtx context.Context, target discoveryTarget) ([]string, error) {
		return target.upstream.PromLabels(callCtx, tenant, target.selectors, start, end)
	})
	warnings = append(warnings, runWarnings...)
	if err != nil {
		return nil, warnings, err
	}
	lists := make([][]string, 0, len(results)+1)
	successful := 0
	for _, result := range results {
		if result.err != nil {
			warnings = append(warnings, Warning{Backend: result.target.backend.Name, Signal: SignalMetrics, Message: result.err.Error()})
			r.noteBackendFailure(result.target.backend, result.err)
			continue
		}
		r.recordSuccess(result.target.backend.Name)
		successful++
		lists = append(lists, result.value)
	}
	if successful > 0 {
		// Every merged series carries a provenance label, so it is also a
		// discoverable label name even when no backend stores it explicitly.
		lists = append(lists, []string{LabelSource})
	}
	return MergeStringLists(lists...), warnings, nil
}

// MetricsLabelValues fans a /api/v1/label/{name}/values request out to every
// metric backend. Each value keeps the provider that produced it so the metric
// catalog can preserve a balanced Doctor/AWS/GCP mix.
func (r *Router) MetricsLabelValues(ctx context.Context, tenant, label string, match []string, start, end time.Time, partition string) ([]LabelValue, []Warning, error) {
	targets, warnings := r.discoveryTargets(match, partition)
	results, runWarnings, err := runDiscovery(r, ctx, targets, func(callCtx context.Context, target discoveryTarget) ([]string, error) {
		return target.upstream.PromLabelValues(callCtx, tenant, label, target.selectors, start, end)
	})
	warnings = append(warnings, runWarnings...)
	if err != nil {
		return nil, warnings, err
	}
	groups := make([]ValuesGroup, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			warnings = append(warnings, Warning{Backend: result.target.backend.Name, Signal: SignalMetrics, Message: result.err.Error()})
			r.noteBackendFailure(result.target.backend, result.err)
			continue
		}
		r.recordSuccess(result.target.backend.Name)
		groups = append(groups, ValuesGroup{
			Source:  sourceForKind(result.target.backend.Kind),
			Backend: result.target.backend.Name,
			Values:  result.value,
		})
	}
	return MergeLabelValues(groups), warnings, nil
}

// MetricSources returns the provenance values that metric backends can
// contribute. The discovery handlers use it to decide whether a provider's
// source label should be advertised without probing every backend.
func (r *Router) MetricSources() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, backend := range r.snapshot() {
		if !backend.Supports(SignalMetrics) {
			continue
		}
		source := sourceForKind(backend.Kind)
		if source == "" {
			continue
		}
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		out = append(out, source)
	}
	return out
}

func (r *Router) discoveryTargets(match []string, partition string) ([]discoveryTarget, []Warning) {
	criteria := parseSelectorCriteria(joinMatches(match))
	targets := make([]discoveryTarget, 0)
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
		selectors, ok := DiscoverySelectorsFor(backend, match)
		if !ok {
			continue
		}
		if partition != "" && len(selectors) > 0 {
			if provider := providerForBackend(backend); provider != "" {
				for i, selector := range selectors {
					selectors[i] = telemetrylabels.ApplyPartition(selector, provider, partition)
				}
			}
		}
		targets = append(targets, discoveryTarget{backend: backend, upstream: upstream, selectors: selectors})
	}

	var warnings []Warning
	if max := r.cfg.MaxFanoutBackends; max > 0 && len(targets) > max {
		total := len(targets)
		targets = targets[:max]
		warnings = append(warnings, Warning{
			Backend: "router",
			Signal:  SignalMetrics,
			Message: fmt.Sprintf("discovery fan-out capped at %d of %d matching backends; narrow the selector by source, region, account, or partition", max, total),
		})
	}
	return targets, warnings
}

func runDiscovery[T any](
	r *Router,
	ctx context.Context,
	targets []discoveryTarget,
	exec func(context.Context, discoveryTarget) (T, error),
) ([]discoveryResult[T], []Warning, error) {
	results := make([]discoveryResult[T], len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, r.cfg.MaxInflight)
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target discoveryTarget) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = discoveryResult[T]{target: target, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			if !r.backendAllowed(target.backend, time.Now()) {
				results[i] = discoveryResult[T]{target: target, err: fmt.Errorf("backend %s circuit breaker open", target.backend.Name)}
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, r.backendCallTimeout(ctx, target.backend))
			defer cancel()
			value, err := exec(callCtx, target)
			results[i] = discoveryResult[T]{target: target, value: value, err: err}
		}(i, target)
	}
	wg.Wait()

	var warnings []Warning
	for _, result := range results {
		if result.err == nil || !isLocalBackend(result.target.backend) {
			continue
		}
		// Local discovery failures are fatal, matching the query path.
		return results, warnings, result.err
	}
	return results, warnings, nil
}

func joinMatches(match []string) string {
	if len(match) == 0 {
		return ""
	}
	out := ""
	for i, selector := range match {
		if i > 0 {
			out += " or "
		}
		out += selector
	}
	return out
}
