package otlp

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/rydzu/ainfra/doctor/internal/identity"
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
		service, partition, intent, asset := telemetryIdentity(resourceAttrs)
		service, partition, intent, asset = canonicalizeResourceIdentity(service, partition, intent, asset, resourceAttrs)
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
					Partition:          partition,
					Intent:             intent,
					Asset:              asset,
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
		service, partition, intent, asset := telemetryIdentity(resourceAttrs)
		service, partition, intent, asset = canonicalizeResourceIdentity(service, partition, intent, asset, resourceAttrs)
		for _, scopeLogs := range resourceLogs.GetScopeLogs() {
			for _, record := range scopeLogs.GetLogRecords() {
				out = append(out, segment.LogRecord{
					Tenant:             tenant,
					Service:            service,
					Partition:          partition,
					Intent:             intent,
					Asset:              asset,
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
		service, partition, intent, asset := telemetryIdentity(resourceAttrs)
		for _, scopeMetrics := range resourceMetrics.GetScopeMetrics() {
			for _, metric := range scopeMetrics.GetMetrics() {
				base := metricBase(metric, tenant, service, partition, intent, asset, resourceAttrs, engine)
				switch typed := metric.Data.(type) {
				case *metricspb.Metric_Gauge:
					for _, point := range typed.Gauge.GetDataPoints() {
						out = append(out, normalizeMetricRecord(numberPointRecord(base, "gauge", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), "", point.GetAttributes(), point)))
					}
				case *metricspb.Metric_Sum:
					temporality := typed.Sum.GetAggregationTemporality().String()
					for _, point := range typed.Sum.GetDataPoints() {
						out = append(out, normalizeMetricRecord(numberPointRecord(base, "sum", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, point.GetAttributes(), point)))
					}
				case *metricspb.Metric_Histogram:
					temporality := typed.Histogram.GetAggregationTemporality().String()
					for _, point := range typed.Histogram.GetDataPoints() {
						out = append(out, normalizeMetricRecords(histogramPointRecords(base, temporality, point))...)
					}
				case *metricspb.Metric_ExponentialHistogram:
					temporality := typed.ExponentialHistogram.GetAggregationTemporality().String()
					for _, point := range typed.ExponentialHistogram.GetDataPoints() {
						out = append(out, normalizeMetricRecords(exponentialHistogramPointRecords(base, temporality, point))...)
					}
				case *metricspb.Metric_Summary:
					for _, point := range typed.Summary.GetDataPoints() {
						out = append(out, normalizeMetricRecords(summaryPointRecords(base, point))...)
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
	partition          string
	intent             string
	asset              string
	name               string
	description        string
	unit               string
	resourceAttributes map[string]string
	engine             *privacy.Engine
}

func metricBase(metric *metricspb.Metric, tenant, service, partition, intent, asset string, resourceAttrs map[string]string, engine *privacy.Engine) metricBaseRecord {
	return metricBaseRecord{
		tenant:             tenant,
		service:            service,
		partition:          partition,
		intent:             intent,
		asset:              asset,
		name:               metric.GetName(),
		description:        metric.GetDescription(),
		unit:               metric.GetUnit(),
		resourceAttributes: resourceAttrs,
		engine:             engine,
	}
}

func telemetryIdentity(resourceAttrs map[string]string) (service, partition, intent, asset string) {
	service = firstNonEmpty(
		resourceAttrs["service.name"],
		resourceAttrs["app.kubernetes.io/name"],
		resourceAttrs["k8s.deployment.name"],
		resourceAttrs["k8s.statefulset.name"],
		resourceAttrs["k8s.daemonset.name"],
		resourceAttrs["k8s.job.name"],
		resourceAttrs["k8s.cronjob.name"],
		resourceAttrs["k8s.pod.name"],
		resourceAttrs["service.instance.id"],
	)
	if service == "" {
		service = "unknown"
	}
	partition = firstNonEmpty(
		resourceAttrs["guardian.partition"],
		resourceAttrs["partition"],
		resourceAttrs["k8s.namespace.name"],
		resourceAttrs["namespace"],
	)
	intent = firstNonEmpty(
		resourceAttrs["guardian.intent"],
		resourceAttrs["intent"],
		resourceAttrs["app.kubernetes.io/component"],
	)
	asset = firstNonEmpty(
		resourceAttrs["guardian.asset"],
		resourceAttrs["asset"],
		resourceAttrs["service.instance.id"],
		resourceAttrs["k8s.pod.name"],
		resourceAttrs["container.name"],
		resourceAttrs["app.kubernetes.io/instance"],
	)
	return service, partition, intent, asset
}

// canonicalizeResourceIdentity applies the canonical producer identity to logs
// and traces at ingest. Only explicit Guardian coordinates or an already
// canonical service name are promoted; a short application name without
// Guardian metadata is left alone (an external or not-yet-instrumented
// producer) so Doctor never invents an identity from a pod name.
func canonicalizeResourceIdentity(service, partition, intent, asset string, attrs map[string]string) (string, string, string, string) {
	explicitPartition := firstNonEmpty(attrs["guardian.partition"], attrs["guardian_partition"])
	explicitIntent := firstNonEmpty(attrs["guardian.intent"], attrs["guardian_intent"])
	explicitAsset := firstNonEmpty(attrs["guardian.asset"], attrs["guardian_asset"])
	if explicitPartition != "" && explicitIntent != "" && explicitAsset != "" {
		canonical := identity.CanonicalServiceName(explicitPartition, explicitIntent, explicitAsset)
		if canonical != "" {
			if service != "" && service != canonical && attrs["service.alias"] == "" {
				attrs["service.alias"] = service
			}
			return canonical, explicitPartition, explicitIntent, explicitAsset
		}
	}
	if sp, si, sa, ok := identity.SplitServiceName(service); ok {
		return service, sp, si, sa
	}
	return service, partition, intent, asset
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// normalizeMetricRecords applies the canonical identity to a batch of metric
// points. Prometheus-scraped metrics carry guardian_* labels on the data point
// instead of the resource, so storage must promote them to typed identity just
// like OTLP-native metrics.
func normalizeMetricRecords(records []segment.MetricPointRecord) []segment.MetricPointRecord {
	for index := range records {
		normalizeMetricRecordInPlace(&records[index])
	}
	return records
}

func normalizeMetricRecord(record segment.MetricPointRecord) segment.MetricPointRecord {
	normalizeMetricRecordInPlace(&record)
	return record
}

// normalizeMetricRecordInPlace resolves the two identities carried by a metric
// record:
//
//   - the producer identity on the OTLP resource, and
//   - the subject identity on the data point, if the metric is about another
//     component (probe, asset observation, control-plane task, flow metrics).
//
// Resource identity always wins. Point labels may only become the producer
// identity when the resource has none (the Prometheus scrape path). This keeps
// a control-plane metric from being renamed to the component it operates on.
func normalizeMetricRecordInPlace(record *segment.MetricPointRecord) {
	if record == nil {
		return
	}
	resource := record.ResourceAttributes
	point := record.Attributes

	resService := firstNonEmpty(resource["service.name"], record.Service)
	resPartition := firstNonEmpty(resource["guardian.partition"], record.Partition)
	resIntent := firstNonEmpty(resource["guardian.intent"], record.Intent)
	resAsset := firstNonEmpty(resource["guardian.asset"], record.Asset)
	resCanonical, resPartition, resIntent, resAsset := producerIdentity(
		resService, resPartition, resIntent, resAsset,
	)

	pointService := firstNonEmpty(point["service"], point["service.name"])
	subjectService := firstNonEmpty(
		point["guardian.target.service"],
		point["guardian_target_service"],
		pointService,
	)
	pointCanonical, pointPartition, pointIntent, pointAsset := pointIdentity(point, pointService)
	producerPointCanonical, producerPointPartition, producerPointIntent, producerPointAsset := legacyPointIdentity(point)

	// Subject metrics: an explicit point service different from a known
	// producer. Asset observation is the main example: guardiand produces the
	// series, but the series describes agent.secrets.deepseek-api-key.
	// A bare guardian.partition/intent/asset label set is not enough: k8s-top
	// attaches those labels to pod metrics to describe the pod, not to rename
	// the k8s-top producer.
	if resCanonical != "" && pointCanonical != "" && pointCanonical != resCanonical &&
		identity.IsCanonicalServiceName(subjectService) {
		applySubjectIdentity(record, resCanonical, pointCanonical, pointPartition, pointIntent, pointAsset)
		return
	}

	// Producer metrics: the resource is authoritative. Preserve a different
	// point identity as target labels instead of consuming it.
	if resCanonical != "" {
		if record.Service != "" && record.Service != resCanonical {
			setRecordAttr(record, "service.alias", record.Service)
		}
		record.Service = resCanonical
		record.Partition = resPartition
		record.Intent = resIntent
		record.Asset = resAsset
		relabelPointIdentityAsTarget(record, resource, point)
		return
	}

	// Prometheus scrapes carry no resource identity. The scrape relabeling puts
	// the producer identity on guardian_partition/guardian_intent/guardian_asset;
	// an explicit point service or guardian_target_* labels describe the subject.
	if pointCanonical != "" && identity.IsCanonicalServiceName(pointService) {
		applySubjectIdentity(record, producerPointCanonical, pointCanonical, pointPartition, pointIntent, pointAsset)
		return
	}
	if producerPointCanonical != "" {
		if record.Service != "" && record.Service != producerPointCanonical {
			setRecordAttr(record, "service.alias", record.Service)
		}
		record.Service = producerPointCanonical
		record.Partition = producerPointPartition
		record.Intent = producerPointIntent
		record.Asset = producerPointAsset
		return
	}
	if pointCanonical != "" {
		if record.Service != "" && record.Service != pointCanonical {
			setRecordAttr(record, "service.alias", record.Service)
		}
		record.Service = pointCanonical
		record.Partition = pointPartition
		record.Intent = pointIntent
		record.Asset = pointAsset
	}
}

func legacyPointIdentity(point map[string]string) (canonical, p, i, a string) {
	p = firstNonEmpty(point["guardian.partition"], point["guardian_partition"], point["partition"])
	i = firstNonEmpty(point["guardian.intent"], point["guardian_intent"], point["intent"])
	a = firstNonEmpty(point["guardian.asset"], point["guardian_asset"], point["asset"])
	if p != "" && i != "" && a != "" {
		return identity.CanonicalServiceName(p, i, a), p, i, a
	}
	return "", p, i, a
}

func producerIdentity(service, partition, intent, asset string) (canonical, p, i, a string) {
	if sp, si, sa, ok := identity.SplitServiceName(service); ok {
		return service, sp, si, sa
	}
	if partition != "" && intent != "" && asset != "" {
		return identity.CanonicalServiceName(partition, intent, asset), partition, intent, asset
	}
	return "", partition, intent, asset
}

func pointIdentity(point map[string]string, service string) (canonical, p, i, a string) {
	p = firstNonEmpty(
		point["guardian.target.partition"],
		point["guardian_target_partition"],
		point["guardian.partition"],
		point["guardian_partition"],
		point["partition"],
	)
	i = firstNonEmpty(
		point["guardian.target.intent"],
		point["guardian_target_intent"],
		point["guardian.intent"],
		point["guardian_intent"],
		point["intent"],
	)
	a = firstNonEmpty(
		point["guardian.target.asset"],
		point["guardian_target_asset"],
		point["guardian.asset"],
		point["guardian_asset"],
		point["asset"],
	)
	if sp, si, sa, ok := identity.SplitServiceName(service); ok {
		if p == "" {
			p = sp
		}
		if i == "" {
			i = si
		}
		if a == "" {
			a = sa
		}
		return service, p, i, a
	}
	if p != "" && i != "" && a != "" {
		return identity.CanonicalServiceName(p, i, a), p, i, a
	}
	return "", p, i, a
}

func applySubjectIdentity(record *segment.MetricPointRecord, producerService, subjectService, partition, intent, asset string) {
	if producerService != "" {
		setRecordAttr(record, "guardian.producer.service", producerService)
	}
	if alias := record.ResourceAttributes["service.alias"]; alias != "" {
		setRecordAttr(record, "guardian.producer.alias", alias)
		delete(record.ResourceAttributes, "service.alias")
	}
	if subjectService == "" {
		return
	}
	record.Service = subjectService
	if partition != "" {
		record.Partition = partition
	}
	if intent != "" {
		record.Intent = intent
	}
	if asset != "" {
		record.Asset = asset
	}
	// Make the subject explicit for query projections even when the producer
	// used short legacy label names.
	setPointAttr(record, "guardian.target.partition", partition)
	setPointAttr(record, "guardian.target.intent", intent)
	setPointAttr(record, "guardian.target.asset", asset)
	setPointAttr(record, "guardian.target.service", subjectService)
}

// relabelPointIdentityAsTarget turns point-level guardian.* identity labels
// into guardian.target.* when the resource already identifies the producer.
// It also removes the legacy keys so a later query projection cannot use them
// to overwrite service.name.
func relabelPointIdentityAsTarget(record *segment.MetricPointRecord, resource, point map[string]string) {
	producerPartition := firstNonEmpty(resource["guardian.partition"], record.Partition)
	producerIntent := firstNonEmpty(resource["guardian.intent"], record.Intent)
	producerAsset := firstNonEmpty(resource["guardian.asset"], record.Asset)

	targets := []struct {
		legacyKeys []string
		targetKey  string
		producer   string
	}{
		{[]string{"guardian.partition", "guardian_partition", "partition"}, "guardian.target.partition", producerPartition},
		{[]string{"guardian.intent", "guardian_intent", "intent"}, "guardian.target.intent", producerIntent},
		{[]string{"guardian.asset", "guardian_asset", "asset"}, "guardian.target.asset", producerAsset},
		{[]string{"guardian.asset.type", "guardian_asset_type"}, "guardian.target.asset.type", ""},
	}
	for _, target := range targets {
		value := firstNonEmpty(pointValues(point, target.legacyKeys)...)
		if value == "" {
			continue
		}
		if target.producer == "" || value != target.producer {
			setPointAttr(record, target.targetKey, value)
		}
		for _, key := range target.legacyKeys {
			delete(record.Attributes, key)
		}
	}
}

func pointValues(point map[string]string, keys []string) []string {
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, point[key])
	}
	return values
}

func setRecordAttr(record *segment.MetricPointRecord, key, value string) {
	if value == "" {
		return
	}
	if record.Attributes == nil {
		record.Attributes = make(map[string]string)
	}
	if _, exists := record.Attributes[key]; !exists {
		record.Attributes[key] = value
	}
}

func setPointAttr(record *segment.MetricPointRecord, key, value string) {
	if value == "" {
		return
	}
	if record.Attributes == nil {
		record.Attributes = make(map[string]string)
	}
	record.Attributes[key] = value
}

func mergeStringMaps(maps ...map[string]string) map[string]string {
	out := make(map[string]string)
	for _, values := range maps {
		for key, value := range values {
			if value != "" {
				out[key] = value
			}
		}
	}
	return out
}

func numberPointRecord(base metricBaseRecord, metricType string, ts, start uint64, temporality string, attrs []*commonpb.KeyValue, point *metricspb.NumberDataPoint) segment.MetricPointRecord {
	value, hasValue := numberDataPointValue(point)
	traceID, spanID := exemplarTraceSpan(point.GetExemplars())
	return segment.MetricPointRecord{
		Tenant:             base.tenant,
		Service:            base.service,
		Partition:          base.partition,
		Intent:             base.intent,
		Asset:              base.asset,
		TraceID:            traceID,
		SpanID:             spanID,
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
		Partition:          base.partition,
		Intent:             base.intent,
		Asset:              base.asset,
		TraceID:            "",
		SpanID:             "",
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
	traceID, spanID := exemplarTraceSpan(point.GetExemplars())
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
			traceID,
			spanID,
			float64(cumulativeCount),
		))
	}

	records = append(records,
		floatPointRecord(base, base.name+"_count", "histogram_count", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, attrs, traceID, spanID, float64(point.GetCount())),
		floatPointRecord(base, base.name+"_sum", "histogram_sum", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, attrs, traceID, spanID, point.GetSum()),
	)
	return records
}

func exponentialHistogramPointRecords(base metricBaseRecord, temporality string, point *metricspb.ExponentialHistogramDataPoint) []segment.MetricPointRecord {
	attrs := attributesMap(point.GetAttributes(), base.engine)
	traceID, spanID := exemplarTraceSpan(point.GetExemplars())
	return []segment.MetricPointRecord{
		floatPointRecord(base, base.name+"_count", "exp_histogram_count", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, attrs, traceID, spanID, float64(point.GetCount())),
		floatPointRecord(base, base.name+"_sum", "exp_histogram_sum", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), temporality, attrs, traceID, spanID, point.GetSum()),
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
			"",
			"",
			quantileValue.GetValue(),
		))
	}
	records = append(records,
		floatPointRecord(base, base.name+"_count", "summary_count", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), "", attrs, "", "", float64(point.GetCount())),
		floatPointRecord(base, base.name+"_sum", "summary_sum", point.GetTimeUnixNano(), point.GetStartTimeUnixNano(), "", attrs, "", "", point.GetSum()),
	)
	return records
}

func floatPointRecord(base metricBaseRecord, metricName, metricType string, ts, start uint64, temporality string, attrs map[string]string, traceID, spanID string, value float64) segment.MetricPointRecord {
	return segment.MetricPointRecord{
		Tenant:             base.tenant,
		Service:            base.service,
		Partition:          base.partition,
		Intent:             base.intent,
		Asset:              base.asset,
		TraceID:            traceID,
		SpanID:             spanID,
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

func exemplarTraceSpan(exemplars []*metricspb.Exemplar) (string, string) {
	for _, exemplar := range exemplars {
		if exemplar == nil {
			continue
		}
		traceID := hex.EncodeToString(exemplar.GetTraceId())
		spanID := hex.EncodeToString(exemplar.GetSpanId())
		if traceID != "" || spanID != "" {
			return traceID, spanID
		}
	}
	return "", ""
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
