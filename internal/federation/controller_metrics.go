package federation

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricapi "go.opentelemetry.io/otel/metric"
)

type controllerInstruments struct {
	syncs           metricapi.Int64Counter
	publishes       metricapi.Int64Counter
	metadataFetch   metricapi.Int64Counter
	buildDuration   metricapi.Float64Histogram
	publishDuration metricapi.Float64Histogram
}

var (
	controllerMetricsOnce      sync.Once
	controllerInstrumentsValue *controllerInstruments
)

func controllerInstrumentsFor() *controllerInstruments {
	controllerMetricsOnce.Do(func() {
		meter := otel.Meter("doctor.federation.controller")
		inst := &controllerInstruments{}
		inst.syncs, _ = meter.Int64Counter(
			"doctor.federation.controller.syncs",
			metricapi.WithDescription("Federation controller sync attempts by result."),
		)
		inst.publishes, _ = meter.Int64Counter(
			"doctor.federation.controller.publishes",
			metricapi.WithDescription("Federation catalog publish attempts by result."),
		)
		inst.metadataFetch, _ = meter.Int64Counter(
			"doctor.federation.controller.metadata_fetches",
			metricapi.WithDescription("Pusher metadata fetch attempts by result."),
		)
		inst.buildDuration, _ = meter.Float64Histogram(
			"doctor.federation.controller.build.duration_seconds",
			metricapi.WithDescription("Federation controller catalog build/publish duration."),
		)
		inst.publishDuration, _ = meter.Float64Histogram(
			"doctor.federation.controller.publish.duration_seconds",
			metricapi.WithDescription("Federation controller snapshot publish duration."),
		)
		controllerInstrumentsValue = inst
	})
	return controllerInstrumentsValue
}

// RecordControllerSync records one controller sync outcome.
func RecordControllerSync(result string, elapsed time.Duration) {
	inst := controllerInstrumentsFor()
	if inst == nil || inst.syncs == nil {
		return
	}
	attrs := metricapi.WithAttributes(attribute.String("result", result))
	inst.syncs.Add(context.Background(), 1, attrs)
	if inst.buildDuration != nil {
		inst.buildDuration.Record(context.Background(), elapsed.Seconds(), attrs)
	}
}

// RecordControllerPublish records one snapshot publish outcome.
func RecordControllerPublish(result string, elapsed time.Duration) {
	inst := controllerInstrumentsFor()
	if inst == nil || inst.publishes == nil {
		return
	}
	attrs := metricapi.WithAttributes(attribute.String("result", result))
	inst.publishes.Add(context.Background(), 1, attrs)
	if inst.publishDuration != nil {
		inst.publishDuration.Record(context.Background(), elapsed.Seconds(), attrs)
	}
}

// RecordMetadataFetch records one pusher metadata fetch outcome.
func RecordMetadataFetch(result string) {
	inst := controllerInstrumentsFor()
	if inst == nil || inst.metadataFetch == nil {
		return
	}
	inst.metadataFetch.Add(context.Background(), 1, metricapi.WithAttributes(attribute.String("result", result)))
}
