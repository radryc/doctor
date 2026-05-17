package otlp

import (
	"testing"
	"time"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func float64Ptr(value float64) *float64 {
	return &value
}

func TestFlattenMetricsExpandsHistogramAndSummary(t *testing.T) {
	t.Helper()

	baseTime := time.Date(2026, 4, 30, 11, 0, 0, 0, time.UTC)
	req := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{
			{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
					Key:   "service.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api"}},
				}}},
				ScopeMetrics: []*metricspb.ScopeMetrics{{
					Metrics: []*metricspb.Metric{
						{
							Name: "request_duration_seconds",
							Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
								AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
								DataPoints: []*metricspb.HistogramDataPoint{{
									TimeUnixNano:      uint64(baseTime.UnixNano()),
									StartTimeUnixNano: uint64(baseTime.Add(-time.Minute).UnixNano()),
									Count:             6,
									Sum:               float64Ptr(12),
									BucketCounts:      []uint64{2, 3, 1},
									ExplicitBounds:    []float64{0.5, 1},
								}},
							}},
						},
						{
							Name: "request_size_bytes",
							Data: &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: []*metricspb.SummaryDataPoint{{
								TimeUnixNano:      uint64(baseTime.UnixNano()),
								StartTimeUnixNano: uint64(baseTime.Add(-time.Minute).UnixNano()),
								Count:             4,
								Sum:               100,
								QuantileValues: []*metricspb.SummaryDataPoint_ValueAtQuantile{{
									Quantile: 0.5,
									Value:    20,
								}, {
									Quantile: 0.9,
									Value:    35,
								}},
							}}}},
						},
					},
				}},
			},
		},
	}

	records := FlattenMetrics(req, "tenant-a", nil)
	if len(records) != 9 {
		t.Fatalf("record count = %d, want 9", len(records))
	}

	values := make(map[string]float64)
	quantiles := make(map[string]float64)
	buckets := make(map[string]float64)
	for _, record := range records {
		if record.Service != "api" {
			t.Fatalf("service = %q, want api", record.Service)
		}
		values[record.MetricName] = record.Value
		if le := record.Attributes["le"]; le != "" {
			buckets[le] = record.Value
		}
		if quantile := record.Attributes["quantile"]; quantile != "" {
			quantiles[quantile] = record.Value
		}
	}

	if got := buckets["0.5"]; got != 2 {
		t.Fatalf("bucket le=0.5 = %v, want 2", got)
	}
	if got := buckets["1"]; got != 5 {
		t.Fatalf("bucket le=1 = %v, want 5", got)
	}
	if got := buckets["+Inf"]; got != 6 {
		t.Fatalf("bucket le=+Inf = %v, want 6", got)
	}
	if got := values["request_duration_seconds_count"]; got != 6 {
		t.Fatalf("histogram count = %v, want 6", got)
	}
	if got := values["request_duration_seconds_sum"]; got != 12 {
		t.Fatalf("histogram sum = %v, want 12", got)
	}
	if got := quantiles["0.5"]; got != 20 {
		t.Fatalf("summary quantile 0.5 = %v, want 20", got)
	}
	if got := quantiles["0.9"]; got != 35 {
		t.Fatalf("summary quantile 0.9 = %v, want 35", got)
	}
	if got := values["request_size_bytes_count"]; got != 4 {
		t.Fatalf("summary count = %v, want 4", got)
	}
	if got := values["request_size_bytes_sum"]; got != 100 {
		t.Fatalf("summary sum = %v, want 100", got)
	}
}
