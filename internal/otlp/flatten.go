package otlp

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/rydzu/ainfra/doctor/internal/privacy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

func FlattenTraces(req *coltracepb.ExportTraceServiceRequest, tenant string, engine *privacy.Engine) []segment.TraceRecord {
	out := make([]segment.TraceRecord, 0)
	if req == nil {
		return out
	}
	for _, resourceSpans := range req.GetResourceSpans() {
		resourceAttrs := attributesMap(resourceSpans.GetResource().GetAttributes(), engine)
		service := resourceAttrs["service.name"]
		if service == "" {
			service = "unknown"
		}
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			for _, span := range scopeSpans.GetSpans() {
				record := segment.TraceRecord{
					Tenant:             tenant,
					TraceID:            hex.EncodeToString(span.GetTraceId()),
					SpanID:             hex.EncodeToString(span.GetSpanId()),
					ParentSpanID:       hex.EncodeToString(span.GetParentSpanId()),
					Name:               engine.SanitizeText(span.GetName()),
					Kind:               span.GetKind().String(),
					Service:            service,
					StartTime:          unixNano(span.GetStartTimeUnixNano()),
					EndTime:            unixNano(span.GetEndTimeUnixNano()),
					StatusCode:         span.GetStatus().GetCode().String(),
					StatusMessage:      engine.SanitizeText(span.GetStatus().GetMessage()),
					ResourceAttributes: resourceAttrs,
					Attributes:         attributesMap(span.GetAttributes(), engine),
				}
				for _, event := range span.GetEvents() {
					record.Events = append(record.Events, segment.TraceEvent{
						Name:       engine.SanitizeText(event.GetName()),
						Timestamp:  unixNano(event.GetTimeUnixNano()),
						Attributes: attributesMap(event.GetAttributes(), engine),
					})
				}
				out = append(out, record)
			}
		}
	}
	return out
}

func FlattenLogs(req *collogspb.ExportLogsServiceRequest, tenant string, engine *privacy.Engine) []segment.LogRecord {
	out := make([]segment.LogRecord, 0)
	if req == nil {
		return out
	}
	for _, resourceLogs := range req.GetResourceLogs() {
		resourceAttrs := attributesMap(resourceLogs.GetResource().GetAttributes(), engine)
		service := resourceAttrs["service.name"]
		if service == "" {
			service = "unknown"
		}
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			for _, record := range scopeLogs.GetLogRecords() {
				out = append(out, segment.LogRecord{
					Tenant:             tenant,
					Service:            service,
					Timestamp:          unixNano(record.GetTimeUnixNano()),
					SeverityNumber:     int32(record.GetSeverityNumber()),
					SeverityText:       record.GetSeverityText(),
					Body:               sanitizeAnyValue(record.GetBody(), engine),
					TraceID:            hex.EncodeToString(record.GetTraceId()),
					SpanID:             hex.EncodeToString(record.GetSpanId()),
					ResourceAttributes: resourceAttrs,
					Attributes:         attributesMap(record.GetAttributes(), engine),
				})
			}
		}
	}
	return out
}

func FlattenMetrics(req *colmetricspb.ExportMetricsServiceRequest, tenant string, engine *privacy.Engine) []segment.MetricPointRecord {
	out := make([]segment.MetricPointRecord, 0)
	if req == nil {
		return out
	}
	for _, resourceMetrics := range req.GetResourceMetrics() {
		resourceAttrs := attributesMap(resourceMetrics.GetResource().GetAttributes(), engine)
		service := resourceAttrs["service.name"]
		if service == "" {
			service = "unknown"
		}
		for _, scopeMetrics := range resourceMetrics.GetScopeMetrics() {
			for _, metric := range scopeMetrics.GetMetrics() {
				base := metricBase(metric, tenant, service, resourceAttrs, engine)
				switch typed := metric.Data.(type) {
				case *metricspb.Metric_Gauge:
					for _, point := range typed.Gauge.GetDataPoints() {
						out = append(out, numberPointRecord(base, "gauge", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), "", point.GetAttributes(), point))
					}
				case *metricspb.Metric_Sum:
					temporality := typed.Sum.GetAggregationTemporality().String()
					for _, point := range typed.Sum.GetDataPoints() {
						out = append(out, numberPointRecord(base, "sum", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, point.GetAttributes(), point))
					}
				case *metricspb.Metric_Histogram:
					temporality := typed.Histogram.GetAggregationTemporality().String()
					for _, point := range typed.Histogram.GetDataPoints() {
						out = append(out, histogramPointRecords(base, temporality, point)...)
					}
				case *metricspb.Metric_ExponentialHistogram:
					temporality := typed.ExponentialHistogram.GetAggregationTemporality().String()
					for _, point := range typed.ExponentialHistogram.GetDataPoints() {
						out = append(out, exponentialHistogramPointRecords(base, temporality, point)...)
					}
				case *metricspb.Metric_Summary:
					for _, point := range typed.Summary.GetDataPoints() {
						out = append(out, summaryPointRecords(base, point)...)
					}
				}
			}
		}
	}
	return out
}

type metricBaseRecord struct {
	tenant             string
	service            string
	name               string
	description        string
	unit               string
	resourceAttributes map[string]string
	engine             *privacy.Engine
}

func metricBase(metric *metricspb.Metric, tenant, service string, resourceAttrs map[string]string, engine *privacy.Engine) metricBaseRecord {
	return metricBaseRecord{
		tenant:             tenant,
		service:            service,
		name:               metric.GetName(),
		description:        metric.GetDescription(),
		unit:               metric.GetUnit(),
		resourceAttributes: resourceAttrs,
		engine:             engine,
	}
}

func numberPointRecord(base metricBaseRecord, metricType string, ts, start uint64, temporality string, attrs []*commonpb.KeyValue, point *metricspb.NumberDataPoint) segment.MetricPointRecord {
	value, hasValue := numberDataPointValue(point)
	return segment.MetricPointRecord{
		Tenant:             base.tenant,
		Service:            base.service,
		MetricName:         base.name,
		Description:        base.description,
		Unit:               base.unit,
		Type:               metricType,
		Timestamp:          unixNano(ts),
		StartTime:          unixNano(start),
		Temporality:        temporality,
		Attributes:         attributesMap(attrs, base.engine),
		ResourceAttributes: base.resourceAttributes,
		HasValue:           hasValue,
		Value:              value,
	}
}

func rawPointRecord(base metricBaseRecord, metricType string, ts, start uint64, temporality string, attrs []*commonpb.KeyValue, point proto.Message) segment.MetricPointRecord {
	raw, _ := protojson.Marshal(point)
	return segment.MetricPointRecord{
		Tenant:             base.tenant,
		Service:            base.service,
		MetricName:         base.name,
		Description:        base.description,
		Unit:               base.unit,
		Type:               metricType,
		Timestamp:          unixNano(ts),
		StartTime:          unixNano(start),
		Temporality:        temporality,
		Attributes:         attributesMap(attrs, base.engine),
		ResourceAttributes: base.resourceAttributes,
		Raw:                raw,
	}
}

func histogramPointRecords(base metricBaseRecord, temporality string, point *metricspb.HistogramDataPoint) []segment.MetricPointRecord {
	attrs := attributesMap(point.GetAttributes(), base.engine)
	records := make([]segment.MetricPointRecord, 0, len(point.GetBucketCounts())+2)

	var cumulativeCount uint64
	for index, bucketCount := range point.GetBucketCounts() {
		cumulativeCount += bucketCount
		bucketAttrs := copyAttributes(attrs)
		bucketAttrs["le"] = histogramBoundLabel(index, point.GetExplicitBounds())
		records = append(records, floatPointRecord(
			base,
			base.name+"_bucket",
			"histogram_bucket",
			point.GetTimeUnixNano(),
			point.GetStartTimeUnixNano(),
			temporality,
			bucketAttrs,
			float64(cumulativeCount),
		))
	}

	records = append(records,
		floatPointRecord(base, base.name+"_count", "histogram_count", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, attrs, float64(point.GetCount())),
		floatPointRecord(base, base.name+"_sum", "histogram_sum", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, attrs, point.GetSum()),
	)
	return records
}

func exponentialHistogramPointRecords(base metricBaseRecord, temporality string, point *metricspb.ExponentialHistogramDataPoint) []segment.MetricPointRecord {
	attrs := attributesMap(point.GetAttributes(), base.engine)
	return []segment.MetricPointRecord{
		floatPointRecord(base, base.name+"_count", "exp_histogram_count", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, attrs, float64(point.GetCount())),
		floatPointRecord(base, base.name+"_sum", "exp_histogram_sum", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, attrs, point.GetSum()),
	}
}

func summaryPointRecords(base metricBaseRecord, point *metricspb.SummaryDataPoint) []segment.MetricPointRecord {
	attrs := attributesMap(point.GetAttributes(), base.engine)
	records := make([]segment.MetricPointRecord, 0, len(point.GetQuantileValues())+2)
	for _, quantileValue := range point.GetQuantileValues() {
		quantileAttrs := copyAttributes(attrs)
		quantileAttrs["quantile"] = formatMetricLabelValue(quantileValue.GetQuantile())
		records = append(records, floatPointRecord(
			base,
			base.name,
			"summary_quantile",
			point.GetTimeUnixNano(),
			point.GetStartTimeUnixNano(),
			"",
			quantileAttrs,
			quantileValue.GetValue(),
		))
	}
	records = append(records,
		floatPointRecord(base, base.name+"_count", "summary_count", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), "", attrs, float64(point.GetCount())),
		floatPointRecord(base, base.name+"_sum", "summary_sum", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), "", attrs, point.GetSum()),
	)
	return records
}

func floatPointRecord(base metricBaseRecord, metricName, metricType string, ts, start uint64, temporality string, attrs map[string]string, value float64) segment.MetricPointRecord {
	return segment.MetricPointRecord{
		Tenant:             base.tenant,
		Service:            base.service,
		MetricName:         metricName,
		Description:        base.description,
		Unit:               base.unit,
		Type:               metricType,
		Timestamp:          unixNano(ts),
		StartTime:          unixNano(start),
		Temporality:        temporality,
		Attributes:         attrs,
		ResourceAttributes: base.resourceAttributes,
		HasValue:           true,
		Value:              value,
	}
}

func copyAttributes(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return map[string]string{}
	}
	dup := make(map[string]string, len(attrs)+1)
	for key, value := range attrs {
		dup[key] = value
	}
	return dup
}

func histogramBoundLabel(index int, explicitBounds []float64) string {
	if index >= len(explicitBounds) {
		return "+Inf"
	}
	return formatMetricLabelValue(explicitBounds[index])
}

func formatMetricLabelValue(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func numberDataPointValue(point *metricspb.NumberDataPoint) (float64, bool) {
	switch value := point.Value.(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		return value.AsDouble, true
	case *metricspb.NumberDataPoint_AsInt:
		return float64(value.AsInt), true
	default:
		return 0, false
	}
}

func attributesMap(attrs []*commonpb.KeyValue, engine *privacy.Engine) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		if attr == nil {
			continue
		}
		out[attr.GetKey()] = sanitizeAnyValue(attr.GetValue(), engine)
	}
	if engine == nil {
		return out
	}
	return engine.SanitizeAttributes(out)
}

func sanitizeAnyValue(value *commonpb.AnyValue, engine *privacy.Engine) string {
	text := anyValueString(value)
	if engine == nil {
		return text
	}
	return engine.SanitizeText(text)
}

func anyValueString(value *commonpb.AnyValue) string {
	if value == nil {
		return ""
	}
	switch typed := value.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return typed.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(typed.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(typed.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(typed.DoubleValue, 'f', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(typed.BytesValue)
	case *commonpb.AnyValue_ArrayValue, *commonpb.AnyValue_KvlistValue:
		raw, _ := protojson.Marshal(value)
		return string(raw)
	default:
		return ""
	}
}

func unixNano(value uint64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(value)).UTC()
}
