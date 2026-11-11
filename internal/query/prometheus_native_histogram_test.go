package query

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/histogram"
	promlabels "github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/radryc/doctor/internal/segment"
)

func TestQueryNeedsNativeHistograms(t *testing.T) {
	cases := map[string]bool{
		"histogram_avg(foo)":                         true,
		"histogram_count(rate(foo[5m]))":             true,
		"sum(histogram_stddev(foo))":                 true,
		"histogram_quantile(0.95, foo)":              true,
		"histogram_fraction(0, 1, foo)":              true,
		"rate(foo[5m])":                              false,
		"sum(foo)":                                   false,
		"histogram_avg(foo) + histogram_stdvar(bar)": true,
	}
	for query, want := range cases {
		expr, err := promparser.NewParser(promparser.Options{}).ParseExpr(query)
		if err != nil {
			t.Fatalf("%s: parse: %v", query, err)
		}
		if got := queryNeedsNativeHistograms(expr); got != want {
			t.Fatalf("%s: got %t, want %t", query, got, want)
		}
	}
}

func nativeHistogramTestBackend(base time.Time) *fakeLogEngineBackend {
	records := []segment.MetricPointRecord{
		nativeHistogramTestPoint("latency_seconds_bucket", base, map[string]string{"le": "0.1"}, 1),
		nativeHistogramTestPoint("latency_seconds_bucket", base, map[string]string{"le": "0.5"}, 3),
		nativeHistogramTestPoint("latency_seconds_bucket", base, map[string]string{"le": "1"}, 4),
		nativeHistogramTestPoint("latency_seconds_bucket", base, map[string]string{"le": "+Inf"}, 4),
		nativeHistogramTestPoint("latency_seconds_count", base, nil, 4),
		nativeHistogramTestPoint("latency_seconds_sum", base, nil, 1.2),
		nativeHistogramTestPoint("temperature_celsius", base, nil, 21.5),
	}
	return &fakeLogEngineBackend{metrics: records}
}

func nativeHistogramTestPoint(name string, ts time.Time, attributes map[string]string, value float64) segment.MetricPointRecord {
	return segment.MetricPointRecord{
		Tenant:     "default",
		Service:    "api",
		MetricName: name,
		Timestamp:  ts,
		Attributes: attributes,
		HasValue:   true,
		Value:      value,
		MetricType: "histogram",
	}
}

func nativeHistogramTestVectorValue(t *testing.T, data map[string]any) float64 {
	t.Helper()
	result, ok := data["result"].([]any)
	if !ok || len(result) != 1 {
		t.Fatalf("result = %#v, want one vector sample", data["result"])
	}
	entry, ok := result[0].(map[string]any)
	if !ok {
		t.Fatalf("result[0] = %#v, want map", result[0])
	}
	value, ok := entry["value"].([]any)
	if !ok || len(value) != 2 {
		t.Fatalf("value = %#v, want [timestamp, value]", entry["value"])
	}
	number, ok := value[1].(string)
	if !ok {
		t.Fatalf("value[1] = %#v, want string", value[1])
	}
	parsed, err := strconv.ParseFloat(number, 64)
	if err != nil {
		t.Fatalf("parse value %q: %v", number, err)
	}
	return parsed
}

func nativeHistogramTestQuery(t *testing.T, svc *Service, query string, at time.Time) map[string]any {
	t.Helper()
	data, err, parseErr := svc.execPrometheusInstant(context.Background(), "default", "", query, at)
	if err != nil {
		t.Fatalf("%s: err = %v (parse=%t)", query, err, parseErr)
	}
	return data
}

func TestNativeHistogramFunctionsUseClassicBuckets(t *testing.T) {
	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	svc := &Service{defaultTenant: "default", logEngine: nativeHistogramTestBackend(base)}

	cases := []struct {
		query string
		want  float64
		tol   float64
	}{
		{"histogram_count(latency_seconds)", 4, 1e-9},
		{"histogram_sum(latency_seconds)", 1.2, 1e-9},
		{"histogram_avg(latency_seconds)", 0.3, 1e-9},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got := nativeHistogramTestVectorValue(t, nativeHistogramTestQuery(t, svc, tc.query, base))
			if math.Abs(got-tc.want) > tc.tol {
				t.Fatalf("value = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNativeHistogramStdDevAndVariance(t *testing.T) {
	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	svc := &Service{defaultTenant: "default", logEngine: nativeHistogramTestBackend(base)}

	for _, query := range []string{
		"histogram_stddev(latency_seconds)",
		"histogram_stdvar(latency_seconds)",
	} {
		t.Run(query, func(t *testing.T) {
			data := nativeHistogramTestQuery(t, svc, query, base)
			got := nativeHistogramTestVectorValue(t, data)
			if math.IsNaN(got) || math.IsInf(got, 0) || got <= 0 {
				t.Fatalf("value = %v, want a positive finite number", got)
			}
		})
	}
}

func TestNativeHistogramRateFunction(t *testing.T) {
	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	backend := nativeHistogramTestBackend(base)
	// A second sample one minute later makes rate() happy.
	for _, record := range []segment.MetricPointRecord{
		nativeHistogramTestPoint("latency_seconds_bucket", base.Add(time.Minute), map[string]string{"le": "0.1"}, 2),
		nativeHistogramTestPoint("latency_seconds_bucket", base.Add(time.Minute), map[string]string{"le": "0.5"}, 6),
		nativeHistogramTestPoint("latency_seconds_bucket", base.Add(time.Minute), map[string]string{"le": "1"}, 8),
		nativeHistogramTestPoint("latency_seconds_bucket", base.Add(time.Minute), map[string]string{"le": "+Inf"}, 8),
		nativeHistogramTestPoint("latency_seconds_count", base.Add(time.Minute), nil, 8),
		nativeHistogramTestPoint("latency_seconds_sum", base.Add(time.Minute), nil, 2.4),
	} {
		backend.metrics = append(backend.metrics, record)
	}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	data := nativeHistogramTestQuery(t, svc, "histogram_avg(rate(latency_seconds[5m]))", base.Add(time.Minute))
	got := nativeHistogramTestVectorValue(t, data)
	if math.Abs(got-0.3) > 1e-9 {
		t.Fatalf("histogram_avg(rate(...)) = %v, want 0.3", got)
	}
}

func TestNativeHistogramQuantileAndFractionUseBaseName(t *testing.T) {
	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	svc := &Service{defaultTenant: "default", logEngine: nativeHistogramTestBackend(base)}

	quantile := nativeHistogramTestVectorValue(t, nativeHistogramTestQuery(t, svc, "histogram_quantile(0.95, latency_seconds)", base))
	if math.IsNaN(quantile) || math.IsInf(quantile, 0) || quantile <= 0 {
		t.Fatalf("histogram_quantile = %v, want a positive finite number", quantile)
	}

	fraction := nativeHistogramTestVectorValue(t, nativeHistogramTestQuery(t, svc, "histogram_fraction(0, 0.5, latency_seconds)", base))
	if math.Abs(fraction-0.75) > 1e-9 {
		t.Fatalf("histogram_fraction = %v, want 0.75", fraction)
	}
}

func TestNativeHistogramSynthesisLeavesClassicQueriesAlone(t *testing.T) {
	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	svc := &Service{defaultTenant: "default", logEngine: nativeHistogramTestBackend(base)}

	data := nativeHistogramTestQuery(t, svc, "histogram_quantile(0.95, sum by (le) (rate(latency_seconds_bucket[5m])))", base)
	if len(data) == 0 {
		t.Fatal("classic histogram_quantile returned no data")
	}
}

func TestNativeHistogramSynthesisLeavesFloatMetricsAlone(t *testing.T) {
	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	svc := &Service{defaultTenant: "default", logEngine: nativeHistogramTestBackend(base)}

	data := nativeHistogramTestQuery(t, svc, "temperature_celsius", base)
	got := nativeHistogramTestVectorValue(t, data)
	if got != 21.5 {
		t.Fatalf("temperature = %v, want 21.5", got)
	}
}

func TestMarshalPrometheusHistogramShape(t *testing.T) {
	base := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	value := promql.Vector{{
		T: base.UnixMilli(),
		H: &histogram.FloatHistogram{
			Schema:          histogram.CustomBucketsSchema,
			CustomValues:    []float64{0.1, 0.5, 1},
			PositiveSpans:   []histogram.Span{{Offset: 0, Length: 4}},
			PositiveBuckets: []float64{1, 2, 1, 0},
			Count:           4,
			Sum:             1.2,
		},
		Metric: promlabels.FromStrings("__name__", "latency_seconds"),
	}}

	data, err := marshalPrometheusValue(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	result := data["result"].([]any)
	entry := result[0].(map[string]any)
	point := entry["histogram"].([]any)
	if len(point) != 2 {
		t.Fatalf("histogram point = %#v", point)
	}
	encoded := point[1].(map[string]any)
	if encoded["count"] != "4" || encoded["sum"] != "1.2" {
		t.Fatalf("histogram = %#v", encoded)
	}
	buckets, ok := encoded["buckets"].([]any)
	if !ok || len(buckets) == 0 {
		t.Fatalf("buckets = %#v", encoded["buckets"])
	}
}
