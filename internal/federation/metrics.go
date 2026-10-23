package federation

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricapi "go.opentelemetry.io/otel/metric"
)

// CatalogMetrics owns federation query-plane instruments and the observable
// catalog/circuit/token gauges.
type CatalogMetrics struct {
	registration   metricapi.Registration
	reloads        metricapi.Int64Counter
	reloadDuration metricapi.Float64Histogram

	planTotal       metricapi.Int64Counter
	planMatched     metricapi.Float64Histogram
	planSelected    metricapi.Float64Histogram
	planDuration    metricapi.Float64Histogram
	fanoutRequests  metricapi.Int64Counter
	fanoutErrors    metricapi.Int64Counter
	fanoutDuration  metricapi.Float64Histogram
	partials        metricapi.Int64Counter
	tokenResolves   metricapi.Int64Counter
	tokenErrors     metricapi.Int64Counter
	tokenInvalidate metricapi.Int64Counter
}

var activeCatalogMetrics atomic.Pointer[CatalogMetrics]

// SetActiveMetrics exposes the metrics to resolvers that are constructed before
// the manager registration (token sources, legacy clients).
func SetActiveMetrics(metrics *CatalogMetrics) {
	activeCatalogMetrics.Store(metrics)
}

func activeMetrics() *CatalogMetrics {
	return activeCatalogMetrics.Load()
}

// RegisterCatalogMetrics installs the federation catalog metrics callbacks.
func RegisterCatalogMetrics(manager *Manager) (*CatalogMetrics, error) {
	if manager == nil {
		return nil, errors.New("federation manager is required for catalog metrics")
	}
	meter := otel.Meter("doctor.federation")
	targets, err := meter.Int64ObservableGauge(
		"doctor.federation.catalog.targets",
		metricapi.WithDescription("Current number of federation targets."),
	)
	if err != nil {
		return nil, err
	}
	partitions, err := meter.Int64ObservableGauge(
		"doctor.federation.catalog.partitions",
		metricapi.WithDescription("Current number of distinct partitions served by the federation catalog."),
	)
	if err != nil {
		return nil, err
	}
	targetsByRegion, err := meter.Int64ObservableGauge(
		"doctor.federation.catalog.targets.by_region",
		metricapi.WithDescription("Current federation targets by region."),
	)
	if err != nil {
		return nil, err
	}
	stale, err := meter.Int64ObservableGauge(
		"doctor.federation.catalog.stale",
		metricapi.WithDescription("1 when the active catalog is older than the configured stale threshold."),
	)
	if err != nil {
		return nil, err
	}
	loadedAt, err := meter.Int64ObservableGauge(
		"doctor.federation.catalog.loaded_timestamp_seconds",
		metricapi.WithDescription("Unix timestamp when the active catalog was loaded."),
	)
	if err != nil {
		return nil, err
	}
	info, err := meter.Int64ObservableGauge(
		"doctor.federation.catalog.info",
		metricapi.WithDescription("Active catalog identity labels; the value is always 1."),
	)
	if err != nil {
		return nil, err
	}
	circuitOpen, err := meter.Int64ObservableGauge(
		"doctor.federation.circuit.open",
		metricapi.WithDescription("1 when a target circuit is open."),
	)
	if err != nil {
		return nil, err
	}
	tokenCache, err := meter.Int64ObservableGauge(
		"doctor.federation.token.cache.entries",
		metricapi.WithDescription("Cached token entries or shards in the active resolver."),
	)
	if err != nil {
		return nil, err
	}
	reloads, err := meter.Int64Counter(
		"doctor.federation.catalog.reloads",
		metricapi.WithDescription("Federation catalog reload attempts by result."),
	)
	if err != nil {
		return nil, err
	}
	reloadDuration, err := meter.Float64Histogram(
		"doctor.federation.catalog.reload.duration_seconds",
		metricapi.WithDescription("Federation catalog reload duration."),
	)
	if err != nil {
		return nil, err
	}
	planTotal, err := meter.Int64Counter(
		"doctor.federation.plan.total",
		metricapi.WithDescription("Federation plans by signal and mode."),
	)
	if err != nil {
		return nil, err
	}
	planMatched, err := meter.Float64Histogram(
		"doctor.federation.plan.matched",
		metricapi.WithDescription("Targets matched by federation plans."),
	)
	if err != nil {
		return nil, err
	}
	planSelected, err := meter.Float64Histogram(
		"doctor.federation.plan.selected",
		metricapi.WithDescription("Targets selected for fan-out by federation plans."),
	)
	if err != nil {
		return nil, err
	}
	planDuration, err := meter.Float64Histogram(
		"doctor.federation.plan.duration_seconds",
		metricapi.WithDescription("Federation plan duration."),
	)
	if err != nil {
		return nil, err
	}
	fanoutRequests, err := meter.Int64Counter(
		"doctor.federation.fanout.requests",
		metricapi.WithDescription("Federation target requests by signal and result."),
	)
	if err != nil {
		return nil, err
	}
	fanoutErrors, err := meter.Int64Counter(
		"doctor.federation.fanout.errors",
		metricapi.WithDescription("Federation target failures by signal and reason."),
	)
	if err != nil {
		return nil, err
	}
	fanoutDuration, err := meter.Float64Histogram(
		"doctor.federation.fanout.duration_seconds",
		metricapi.WithDescription("Federation target call duration."),
	)
	if err != nil {
		return nil, err
	}
	partials, err := meter.Int64Counter(
		"doctor.federation.partial",
		metricapi.WithDescription("Federation responses with partial target failures."),
	)
	if err != nil {
		return nil, err
	}
	tokenResolves, err := meter.Int64Counter(
		"doctor.federation.token.resolves",
		metricapi.WithDescription("Federation token resolution attempts by result."),
	)
	if err != nil {
		return nil, err
	}
	tokenErrors, err := meter.Int64Counter(
		"doctor.federation.token.errors",
		metricapi.WithDescription("Federation token resolution errors."),
	)
	if err != nil {
		return nil, err
	}
	tokenInvalidate, err := meter.Int64Counter(
		"doctor.federation.token.invalidations",
		metricapi.WithDescription("Federation token cache invalidations."),
	)
	if err != nil {
		return nil, err
	}

	registration, err := meter.RegisterCallback(func(_ context.Context, observer metricapi.Observer) error {
		state := manager.current()
		if state == nil || state.catalog == nil {
			return nil
		}
		stats := state.catalog.Stats()
		observer.ObserveInt64(targets, int64(stats.Targets))
		observer.ObserveInt64(partitions, int64(stats.Partitions))
		for region, count := range stats.Regions {
			observer.ObserveInt64(targetsByRegion, int64(count), metricapi.WithAttributes(
				attribute.String("region", region),
			))
		}
		staleValue := int64(0)
		if manager.staleAfter > 0 && !state.loadedAt.IsZero() && time.Since(state.loadedAt) > manager.staleAfter {
			staleValue = 1
		}
		observer.ObserveInt64(stale, staleValue)
		if !state.loadedAt.IsZero() {
			observer.ObserveInt64(loadedAt, state.loadedAt.Unix())
		}
		observer.ObserveInt64(info, 1, metricapi.WithAttributes(
			attribute.String("version", state.catalog.Version()),
			attribute.String("source", state.source),
		))
		_, circuits := manager.circuitStatus()
		for _, circuit := range circuits {
			if circuit.Open {
				observer.ObserveInt64(circuitOpen, 1, metricapi.WithAttributes(
					attribute.String("target", circuit.Target),
				))
			}
		}
		if sizer, ok := manager.tokenResolver.(interface{ CacheSize() int }); ok {
			observer.ObserveInt64(tokenCache, int64(sizer.CacheSize()))
		}
		return nil
	}, targets, partitions, targetsByRegion, stale, loadedAt, info, circuitOpen, tokenCache)
	if err != nil {
		return nil, err
	}
	metrics := &CatalogMetrics{
		registration:    registration,
		reloads:         reloads,
		reloadDuration:  reloadDuration,
		planTotal:       planTotal,
		planMatched:     planMatched,
		planSelected:    planSelected,
		planDuration:    planDuration,
		fanoutRequests:  fanoutRequests,
		fanoutErrors:    fanoutErrors,
		fanoutDuration:  fanoutDuration,
		partials:        partials,
		tokenResolves:   tokenResolves,
		tokenErrors:     tokenErrors,
		tokenInvalidate: tokenInvalidate,
	}
	SetActiveMetrics(metrics)
	return metrics, nil
}

// RecordReload records one reload attempt.
func (m *CatalogMetrics) RecordReload(result string, elapsed time.Duration) {
	if m == nil || m.reloads == nil {
		return
	}
	attrs := metricapi.WithAttributes(attribute.String("result", result))
	m.reloads.Add(context.Background(), 1, attrs)
	if m.reloadDuration != nil {
		m.reloadDuration.Record(context.Background(), elapsed.Seconds(), attrs)
	}
}

// RecordPlan records one planner decision.
func (m *CatalogMetrics) RecordPlan(signal, mode string, matched, selected int, elapsed time.Duration) {
	if m == nil || m.planTotal == nil {
		return
	}
	ctx := context.Background()
	attrs := metricapi.WithAttributes(
		attribute.String("signal", signal),
		attribute.String("mode", mode),
	)
	m.planTotal.Add(ctx, 1, attrs)
	if m.planMatched != nil {
		m.planMatched.Record(ctx, float64(matched), attrs)
	}
	if m.planSelected != nil {
		m.planSelected.Record(ctx, float64(selected), attrs)
	}
	if m.planDuration != nil {
		m.planDuration.Record(ctx, elapsed.Seconds(), attrs)
	}
}

// RecordFanout records one target call result.
func (m *CatalogMetrics) RecordFanout(signal, result string, elapsed time.Duration) {
	if m == nil || m.fanoutRequests == nil {
		return
	}
	ctx := context.Background()
	attrs := metricapi.WithAttributes(
		attribute.String("signal", signal),
		attribute.String("result", result),
	)
	m.fanoutRequests.Add(ctx, 1, attrs)
	if m.fanoutDuration != nil {
		m.fanoutDuration.Record(ctx, elapsed.Seconds(), attrs)
	}
	if result != "success" && result != "canceled" && m.fanoutErrors != nil {
		m.fanoutErrors.Add(ctx, 1, attrs)
	}
}

// RecordPartial records a response that contains partial target failures.
func (m *CatalogMetrics) RecordPartial() {
	if m == nil || m.partials == nil {
		return
	}
	m.partials.Add(context.Background(), 1)
}

// RecordTokenResolve records one resolver attempt.
func (m *CatalogMetrics) RecordTokenResolve(result string) {
	if m == nil {
		return
	}
	ctx := context.Background()
	if m.tokenResolves != nil {
		m.tokenResolves.Add(ctx, 1, metricapi.WithAttributes(attribute.String("result", result)))
	}
	if result != "success" && m.tokenErrors != nil {
		m.tokenErrors.Add(ctx, 1)
	}
}

// RecordTokenInvalidate records a cache invalidation after a 401.
func (m *CatalogMetrics) RecordTokenInvalidate() {
	if m == nil || m.tokenInvalidate == nil {
		return
	}
	m.tokenInvalidate.Add(context.Background(), 1)
}

// Unregister removes the observable callbacks.
func (m *CatalogMetrics) Unregister() error {
	if m == nil || m.registration == nil {
		return nil
	}
	if activeMetrics() == m {
		activeCatalogMetrics.Store(nil)
	}
	return m.registration.Unregister()
}
