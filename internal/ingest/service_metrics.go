package ingest

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricapi "go.opentelemetry.io/otel/metric"
)

var (
	ingestMetricsOnce      sync.Once
	ingestMetricsPrimed    sync.Once
	ingestRequestsAccepted metricapi.Int64Counter
	ingestRecordsAccepted  metricapi.Int64Counter
)

var ingestSignals = []string{"traces", "logs", "metrics"}

func recordAcceptedIngest(ctx context.Context, signal string, records int) {
	if signal == "" || records < 0 {
		return
	}
	requests, acceptedRecords := ingestInstruments()
	attrs := metricapi.WithAttributes(attribute.String("signal", signal))
	requests.Add(ctx, 1, attrs)
	acceptedRecords.Add(ctx, int64(records), attrs)
}

func primeAcceptedIngestMetrics() {
	ingestMetricsPrimed.Do(func() {
		requests, acceptedRecords := ingestInstruments()
		ctx := context.Background()
		for _, signal := range ingestSignals {
			attrs := metricapi.WithAttributes(attribute.String("signal", signal))
			requests.Add(ctx, 0, attrs)
			acceptedRecords.Add(ctx, 0, attrs)
		}
	})
}

func ingestInstruments() (metricapi.Int64Counter, metricapi.Int64Counter) {
	ingestMetricsOnce.Do(func() {
		meter := otel.Meter("doctor/ingest")
		ingestRequestsAccepted, _ = meter.Int64Counter(
			"doctor.ingest.requests.accepted",
			metricapi.WithDescription("Total accepted ingest requests, by signal."),
		)
		ingestRecordsAccepted, _ = meter.Int64Counter(
			"doctor.ingest.records.accepted",
			metricapi.WithDescription("Total accepted telemetry records, by signal."),
		)
	})
	return ingestRequestsAccepted, ingestRecordsAccepted
}
