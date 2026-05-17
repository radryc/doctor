package ingest

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewServiceWithBuffersPrimesAcceptedIngestMetrics(t *testing.T) {
	resetIngestMetricsForTest()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previousProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetMeterProvider(previousProvider)
		resetIngestMetricsForTest()
	})

	_ = NewServiceWithBuffers("X-Doctor-Tenant", "default", nil, nil, nil, nil, nil)

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	assertSignalsPresent(t, metrics, "doctor.ingest.requests.accepted")
	assertSignalsPresent(t, metrics, "doctor.ingest.records.accepted")
}

func assertSignalsPresent(t *testing.T, metrics metricdata.ResourceMetrics, name string) {
	t.Helper()
	wantSignals := map[string]struct{}{
		"logs":    {},
		"metrics": {},
		"traces":  {},
	}
	gotSignals := map[string]int64{}
	for _, scopeMetrics := range metrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s has unexpected type %T", name, metric.Data)
			}
			for _, point := range sum.DataPoints {
				signalValue, ok := point.Attributes.Value(attribute.Key("signal"))
				if !ok {
					continue
				}
				gotSignals[signalValue.AsString()] = point.Value
			}
		}
	}
	for signal := range wantSignals {
		value, ok := gotSignals[signal]
		if !ok {
			t.Fatalf("metric %s missing signal %q, got %v", name, signal, gotSignals)
		}
		if value != 0 {
			t.Fatalf("metric %s signal %q = %d, want 0", name, signal, value)
		}
	}
}

func resetIngestMetricsForTest() {
	ingestMetricsOnce = sync.Once{}
	ingestMetricsPrimed = sync.Once{}
	ingestRequestsAccepted = nil
	ingestRecordsAccepted = nil
}
