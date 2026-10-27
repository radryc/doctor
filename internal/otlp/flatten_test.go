package otlp

import (
	"encoding/hex"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func float64Ptr(value float64) *float64 {
	return &value
}

func hexMustDecode(t *testing.T, value string) []byte {
	t.Helper()
	b, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode hex %q: %v", value, err)
	}
	return b
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

func TestFlattenMetricsCapturesExemplarTraceSpan(t *testing.T) {
	t.Helper()

	baseTime := time.Date(2026, 4, 30, 11, 0, 0, 0, time.UTC)
	traceID := "0123456789abcdef0123456789abcdef"
	spanID := "89abcdef01234567"
	req := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
				Key:   "service.name",
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api"}},
			}}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "request_total",
					Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
						AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano:      uint64(baseTime.UnixNano()),
							StartTimeUnixNano: uint64(baseTime.Add(-time.Minute).UnixNano()),
							Value:             &metricspb.NumberDataPoint_AsInt{AsInt: 42},
							Exemplars: []*metricspb.Exemplar{{
								TraceId: hexMustDecode(t, traceID),
								SpanId:  hexMustDecode(t, spanID),
							}},
						}},
					}},
				}},
			}},
		}},
	}

	records := FlattenMetrics(req, "tenant-a", nil)
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	if got := records[0].TraceID; got != traceID {
		t.Fatalf("trace_id = %q, want %q", got, traceID)
	}
	if got := records[0].SpanID; got != spanID {
		t.Fatalf("span_id = %q, want %q", got, spanID)
	}
}

func TestFlattenMetricsInfersTelemetryIdentityFromCommonResourceLabels(t *testing.T) {
	t.Helper()

	baseTime := time.Date(2026, 4, 30, 11, 0, 0, 0, time.UTC)
	req := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{
					Key:   "app.kubernetes.io/name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "payments-api"}},
				},
				{
					Key:   "k8s.namespace.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "checkout"}},
				},
				{
					Key:   "app.kubernetes.io/component",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "backend"}},
				},
				{
					Key:   "k8s.pod.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "payments-api-7c9f4d8b7b-kzq9p"}},
				},
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "request_total",
					Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
						AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano:      uint64(baseTime.UnixNano()),
							StartTimeUnixNano: uint64(baseTime.Add(-time.Minute).UnixNano()),
							Value:             &metricspb.NumberDataPoint_AsInt{AsInt: 42},
						}},
					}},
				}},
			}},
		}},
	}

	records := FlattenMetrics(req, "tenant-a", nil)
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	record := records[0]
	if record.Service != "checkout.backend.payments-api-7c9f4d8b7b-kzq9p" {
		t.Fatalf("service = %q, want %q", record.Service, "checkout.backend.payments-api-7c9f4d8b7b-kzq9p")
	}
	if record.Partition != "checkout" {
		t.Fatalf("partition = %q, want %q", record.Partition, "checkout")
	}
	if record.Intent != "backend" {
		t.Fatalf("intent = %q, want %q", record.Intent, "backend")
	}
	if record.Asset != "payments-api-7c9f4d8b7b-kzq9p" {
		t.Fatalf("asset = %q, want %q", record.Asset, "payments-api-7c9f4d8b7b-kzq9p")
	}
}

func TestFlattenMetricsPromotesScrapedGuardianLabels(t *testing.T) {
	t.Helper()

	baseTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	req := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{
					Key:   "service.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "guardiand"}},
				},
				{
					Key:   "k8s.namespace.name",
					Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "guardian"}},
				},
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "monofs_server_ops_total",
					Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
						AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano:      uint64(baseTime.UnixNano()),
							StartTimeUnixNano: uint64(baseTime.Add(-time.Minute).UnixNano()),
							Value:             &metricspb.NumberDataPoint_AsInt{AsInt: 1},
							Attributes: []*commonpb.KeyValue{
								{
									Key:   "guardian_partition",
									Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "monofs"}},
								},
								{
									Key:   "guardian_intent",
									Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "monofs-storage"}},
								},
								{
									Key:   "guardian_asset",
									Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "node-a"}},
								},
							},
						}},
					}},
				}},
			}},
		}},
	}

	records := FlattenMetrics(req, "tenant-a", nil)
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	record := records[0]
	if record.Service != "monofs.monofs-storage.node-a" {
		t.Fatalf("service = %q, want %q", record.Service, "monofs.monofs-storage.node-a")
	}
	if record.Partition != "monofs" || record.Intent != "monofs-storage" || record.Asset != "node-a" {
		t.Fatalf("identity = %s/%s/%s", record.Partition, record.Intent, record.Asset)
	}
	if record.Attributes["service.alias"] != "guardiand" {
		t.Fatalf("service.alias = %q, want guardiand", record.Attributes["service.alias"])
	}
}

func strAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func TestFlattenMetricsKeepsProducerWhenPointLabelsDescribeTarget(t *testing.T) {
	baseTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	req := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "guardian.pusher.k8s-main"),
				strAttr("guardian.pusher", "k8s-main"),
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "guardian.pusher.task.executions",
					Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
						AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano:      uint64(baseTime.UnixNano()),
							StartTimeUnixNano: uint64(baseTime.Add(-time.Minute).UnixNano()),
							Value:             &metricspb.NumberDataPoint_AsInt{AsInt: 1},
							Attributes: []*commonpb.KeyValue{
								strAttr("guardian.partition", "doctor"),
								strAttr("guardian.intent", "ingest"),
								strAttr("guardian.operation", "DIFF"),
							},
						}},
					}},
				}},
			}},
		}},
	}

	record := FlattenMetrics(req, "tenant-a", nil)[0]
	if record.Service != "guardian.pusher.k8s-main" {
		t.Fatalf("service = %q, want guardian.pusher.k8s-main", record.Service)
	}
	if record.Partition != "guardian" || record.Intent != "pusher" || record.Asset != "k8s-main" {
		t.Fatalf("producer identity = %s/%s/%s", record.Partition, record.Intent, record.Asset)
	}
	if record.Attributes["guardian.target.partition"] != "doctor" || record.Attributes["guardian.target.intent"] != "ingest" {
		t.Fatalf("target identity missing: %#v", record.Attributes)
	}
	if _, exists := record.Attributes["guardian.partition"]; exists {
		t.Fatalf("legacy target label was not renamed: %#v", record.Attributes)
	}
}

func TestFlattenMetricsUsesExplicitPointServiceAsSubject(t *testing.T) {
	baseTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	req := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "guardian-configs.guardian.guardiand"),
				strAttr("guardian.partition", "guardian-configs"),
				strAttr("guardian.intent", "guardian"),
				strAttr("guardian.asset", "guardiand"),
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "guardian_asset_observation_current",
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano: uint64(baseTime.UnixNano()),
							Value:        &metricspb.NumberDataPoint_AsInt{AsInt: 1},
							Attributes: []*commonpb.KeyValue{
								strAttr("service", "agent.secrets.deepseek-api-key"),
								strAttr("guardian_partition", "agent"),
								strAttr("guardian_intent", "secrets"),
								strAttr("guardian_asset", "deepseek-api-key"),
								strAttr("status", "Healthy"),
							},
						}},
					}},
				}},
			}},
		}},
	}

	record := FlattenMetrics(req, "tenant-a", nil)[0]
	if record.Service != "agent.secrets.deepseek-api-key" {
		t.Fatalf("service = %q, want the observed asset", record.Service)
	}
	if record.Partition != "agent" || record.Intent != "secrets" || record.Asset != "deepseek-api-key" {
		t.Fatalf("subject identity = %s/%s/%s", record.Partition, record.Intent, record.Asset)
	}
	if record.Attributes["guardian.producer.service"] != "guardian-configs.guardian.guardiand" {
		t.Fatalf("producer service not preserved: %#v", record.Attributes)
	}
}

func TestFlattenLogsCanonicalizesExplicitGuardianIdentity(t *testing.T) {
	req := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "lagent-backend"),
				strAttr("guardian.partition", "agent"),
				strAttr("guardian.intent", "agent"),
				strAttr("guardian.asset", "lagent-backend"),
			}},
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello"}}}},
			}},
		}},
	}

	record := FlattenLogs(req, "tenant-a", nil)[0]
	if record.Service != "agent.agent.lagent-backend" {
		t.Fatalf("service = %q, want agent.agent.lagent-backend", record.Service)
	}
	if record.ResourceAttributes["service.alias"] != "lagent-backend" {
		t.Fatalf("service.alias = %q, want lagent-backend", record.ResourceAttributes["service.alias"])
	}
}

func TestFlattenLogsDoesNotInventIdentityFromPodName(t *testing.T) {
	req := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "lagent-backend"),
				strAttr("k8s.namespace.name", "agent"),
				strAttr("k8s.pod.name", "lagent-backend-7c9f4d8b7b-kzq9p"),
			}},
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello"}}}},
			}},
		}},
	}

	record := FlattenLogs(req, "tenant-a", nil)[0]
	if record.Service != "lagent-backend" {
		t.Fatalf("service = %q, want the producer-provided short name", record.Service)
	}
}

func TestFlattenMetricsDoesNotReattributePodMetricsToThePod(t *testing.T) {
	baseTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	req := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "k8s-top.k8s-top.k8s-top"),
				strAttr("guardian.partition", "k8s-top"),
				strAttr("guardian.intent", "k8s-top"),
				strAttr("guardian.asset", "k8s-top"),
				strAttr("guardian.asset.type", "Compute"),
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "k8s.top.pod.cpu.usage.millicores",
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano: uint64(baseTime.UnixNano()),
							Value:        &metricspb.NumberDataPoint_AsInt{AsInt: 42},
							Attributes: []*commonpb.KeyValue{
								strAttr("guardian.partition", "agent"),
								strAttr("guardian.intent", "agent"),
								strAttr("guardian.asset", "lagent-backend"),
							},
						}},
					}},
				}},
			}},
		}},
	}

	record := FlattenMetrics(req, "tenant-a", nil)[0]
	if record.Service != "k8s-top.k8s-top.k8s-top" {
		t.Fatalf("service = %q, want the k8s-top producer", record.Service)
	}
	if record.Partition != "k8s-top" || record.Intent != "k8s-top" || record.Asset != "k8s-top" {
		t.Fatalf("producer identity = %s/%s/%s", record.Partition, record.Intent, record.Asset)
	}
	if record.Attributes["guardian.target.partition"] != "agent" ||
		record.Attributes["guardian.target.asset"] != "lagent-backend" {
		t.Fatalf("pod target labels missing: %#v", record.Attributes)
	}
}

func TestFlattenMetricsScrapedProducerKeepsTargetLabels(t *testing.T) {
	baseTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	req := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "guardiand"),
				strAttr("k8s.namespace.name", "guardian"),
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "guardian_results_executions",
					Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
						AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano:      uint64(baseTime.UnixNano()),
							StartTimeUnixNano: uint64(baseTime.Add(-time.Minute).UnixNano()),
							Value:             &metricspb.NumberDataPoint_AsInt{AsInt: 1},
							Attributes: []*commonpb.KeyValue{
								strAttr("guardian_partition", "guardian-configs"),
								strAttr("guardian_intent", "guardian"),
								strAttr("guardian_asset", "guardiand"),
								strAttr("guardian_target_partition", "doctor"),
								strAttr("guardian_target_intent", "ingest"),
							},
						}},
					}},
				}},
			}},
		}},
	}

	record := FlattenMetrics(req, "tenant-a", nil)[0]
	if record.Service != "guardian-configs.guardian.guardiand" {
		t.Fatalf("service = %q, want canonical guardiand", record.Service)
	}
	if record.Attributes["guardian_target_partition"] != "doctor" {
		t.Fatalf("target partition = %q", record.Attributes["guardian_target_partition"])
	}
	if record.Attributes["service.alias"] != "guardiand" {
		t.Fatalf("service.alias = %q, want guardiand", record.Attributes["service.alias"])
	}
}

func TestFlattenMetricsScrapedObservationUsesPointServiceAsSubject(t *testing.T) {
	baseTime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	req := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "guardiand"),
				strAttr("k8s.namespace.name", "guardian"),
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "guardian_asset_observation_current",
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano: uint64(baseTime.UnixNano()),
							Value:        &metricspb.NumberDataPoint_AsInt{AsInt: 1},
							Attributes: []*commonpb.KeyValue{
								strAttr("service", "agent.secrets.deepseek-api-key"),
								strAttr("guardian_partition", "guardian-configs"),
								strAttr("guardian_intent", "guardian"),
								strAttr("guardian_asset", "guardiand"),
								strAttr("guardian_target_partition", "agent"),
								strAttr("guardian_target_intent", "secrets"),
								strAttr("guardian_target_asset", "deepseek-api-key"),
								strAttr("guardian_asset_type", "Secret"),
							},
						}},
					}},
				}},
			}},
		}},
	}

	record := FlattenMetrics(req, "tenant-a", nil)[0]
	if record.Service != "agent.secrets.deepseek-api-key" {
		t.Fatalf("service = %q, want observed asset", record.Service)
	}
	if record.Partition != "agent" || record.Intent != "secrets" || record.Asset != "deepseek-api-key" {
		t.Fatalf("subject identity = %s/%s/%s", record.Partition, record.Intent, record.Asset)
	}
	if record.Attributes["guardian.producer.service"] != "guardian-configs.guardian.guardiand" {
		t.Fatalf("producer service = %q", record.Attributes["guardian.producer.service"])
	}
}
