package federation

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricapi "go.opentelemetry.io/otel/metric"
)

// CatalogMetrics owns the observable catalog metrics and the reload counters.
type CatalogMetrics struct {
	registration   metricapi.Registration
	reloads        metricapi.Int64Counter
	reloadDuration metricapi.Float64Histogram
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
		return nil
	}, targets, partitions, targetsByRegion, stale, loadedAt, info)
	if err != nil {
		return nil, err
	}
	return &CatalogMetrics{
		registration:   registration,
		reloads:        reloads,
		reloadDuration: reloadDuration,
	}, nil
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

// Unregister removes the observable callbacks.
func (m *CatalogMetrics) Unregister() error {
	if m == nil || m.registration == nil {
		return nil
	}
	return m.registration.Unregister()
}
