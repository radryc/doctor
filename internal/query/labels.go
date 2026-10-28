package query

import (
	promlabels "github.com/prometheus/prometheus/model/labels"
	"github.com/radryc/doctor/internal/identity"
	"github.com/radryc/doctor/internal/segment"
)

// This file defines the canonical label vocabulary shared by the metrics
// (Prometheus), logs (Loki) and traces (Tempo) compatibility APIs. Grafana
// correlates signals by matching these labels, so they must be projected
// identically regardless of the source record type.

const (
	labelService        = "service"
	labelServiceFull    = "service_full"
	labelServiceAlias   = "service_alias"
	labelServiceName    = "service_name"
	labelTenant         = "tenant"
	labelSource         = "source"
	labelPartition      = "guardian_partition"
	labelIntent         = "guardian_intent"
	labelAsset          = "guardian_asset"
	labelShortPartition = "partition"
	labelShortIntent    = "intent"
	labelShortAsset     = "asset"

	// MaxServiceNameLength mirrors Guardian's canonical identity cap.
	MaxServiceNameLength = identity.MaxServiceNameLength

	sourceDoctor = "doctor"

	attrGuardianPartition = "guardian.partition"
	attrGuardianIntent    = "guardian.intent"
	attrGuardianAsset     = "guardian.asset"
	attrGuardianFullName  = "guardian.service.full_name"
	attrServiceAlias      = "service.alias"

	attrK8sNamespace = "k8s.namespace.name"
	attrK8sPod       = "k8s.pod.name"
	attrK8sContainer = "k8s.container.name"
	attrK8sNode      = "k8s.node.name"
	attrAppComponent = "app.kubernetes.io/component"
	attrAppInstance  = "app.kubernetes.io/instance"
)

// mergeStringMaps overlays b onto a copy of a, ignoring empty values.
func mergeStringMaps(maps ...map[string]string) map[string]string {
	out := make(map[string]string)
	for _, m := range maps {
		for key, value := range m {
			if value == "" {
				continue
			}
			out[key] = value
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// identityFromAttributes resolves the canonical partition/intent/asset from a
// flat attribute map, falling back through Kubernetes and app conventions.
func identityFromAttributes(attrs map[string]string) (partition, intent, asset string) {
	partition = firstNonEmpty(
		attrs[attrGuardianPartition],
		attrs[labelPartition],
		attrs[labelShortPartition],
		attrs[attrK8sNamespace],
		attrs["namespace"],
	)
	intent = firstNonEmpty(
		attrs[attrGuardianIntent],
		attrs[labelIntent],
		attrs[labelShortIntent],
		attrs[attrAppComponent],
	)
	asset = firstNonEmpty(
		attrs[attrGuardianAsset],
		attrs[labelAsset],
		attrs[labelShortAsset],
		attrs[attrK8sPod],
		attrs[attrK8sContainer],
		attrs[attrAppInstance],
	)
	return partition, intent, asset
}

// canonicalServiceName is kept local for readability at call sites; the
// implementation lives in the shared identity package so ingest and query
// produce exactly the same name.
func canonicalServiceName(partition, intent, asset string) string {
	return identity.CanonicalServiceName(partition, intent, asset)
}

// lokiLabelAllowlist is the curated set of low-cardinality labels exposed as
// Loki stream labels. Point attributes outside this set stay queryable via the
// log body / line filters but do not fragment streams.
var lokiLabelAllowlist = map[string]struct{}{
	labelService:                 {},
	labelServiceFull:             {},
	labelServiceAlias:            {},
	labelTenant:                  {},
	labelSource:                  {},
	labelPartition:               {},
	labelIntent:                  {},
	labelAsset:                   {},
	"guardian_cluster":           {},
	"guardian_pusher":            {},
	"guardian_provider":          {},
	"guardian_account":           {},
	"guardian_region":            {},
	"k8s_cluster_name":           {},
	"pusher":                     {},
	"provider":                   {},
	"level":                      {},
	"trace_id":                   {},
	"span_id":                    {},
	"k8s_namespace_name":         {},
	"k8s_pod_name":               {},
	"k8s_container_name":         {},
	"k8s_node_name":              {},
	"guardian_component":         {},
	"guardian_asset_type":        {},
	"guardian_identity_kind":     {},
	"guardian_producer_service":  {},
	"guardian_target_partition":  {},
	"guardian_target_intent":     {},
	"guardian_target_asset":      {},
	"guardian_target_asset_type": {},
	"guardian_target_service":    {},
	"guardian_probe_name":        {},
	"guardian_probe_type":        {},
	"guardian_probe_mode":        {},
	"guardian_probe_target":      {},
	"guardian_probe_path":        {},
	"guardian_probe_id":          {},
	"guardian_probe_status":      {},
	"aws_log_group":              {},
	"aws_log_stream":             {},
	"aws_region":                 {},
	"aws_account":                {},
}

func filterLokiLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		if _, ok := lokiLabelAllowlist[key]; ok {
			out[key] = value
		}
	}
	return out
}

// traceIdentityLabels resolves the identity of a trace span, preferring the
// typed fields and falling back to attributes stored with the span.
func traceIdentityLabels(span segment.TraceRecord) (partition, intent, asset string) {
	// Identity belongs on the resource. Span attributes may describe a target
	// (reconcile task, probe, flow), so they are only a last resort when the
	// resource carries no identity at all.
	resourcePartition, resourceIntent, resourceAsset := identityFromAttributes(span.ResourceAttributes)
	partition = firstNonEmpty(span.Partition, resourcePartition)
	intent = firstNonEmpty(span.Intent, resourceIntent)
	asset = firstNonEmpty(span.Asset, resourceAsset)
	if partition == "" && intent == "" && asset == "" {
		partition, intent, asset = identityFromAttributes(span.Attributes)
	}
	return partition, intent, asset
}

func logIdentity(record segment.LogRecord) (partition, intent, asset string) {
	resourcePartition, resourceIntent, resourceAsset := identityFromAttributes(record.ResourceAttributes)
	partition = firstNonEmpty(record.Partition, resourcePartition)
	intent = firstNonEmpty(record.Intent, resourceIntent)
	asset = firstNonEmpty(record.Asset, resourceAsset)
	if partition == "" && intent == "" && asset == "" {
		partition, intent, asset = identityFromAttributes(record.Attributes)
	}
	return partition, intent, asset
}

func metricIdentity(record segment.MetricPointRecord) (partition, intent, asset string) {
	resourcePartition, resourceIntent, resourceAsset := identityFromAttributes(record.ResourceAttributes)
	partition = firstNonEmpty(record.Partition, resourcePartition)
	intent = firstNonEmpty(record.Intent, resourceIntent)
	asset = firstNonEmpty(record.Asset, resourceAsset)
	if partition == "" && intent == "" && asset == "" {
		partition, intent, asset = identityFromAttributes(record.Attributes)
	}
	return partition, intent, asset
}

// canonicalLabels projects a record's identity and attributes into the shared
// Grafana label vocabulary. Explicit typed identity wins over attribute-derived
// identity; reserved keys are never overwritten by raw attributes.
func canonicalLabels(service, tenant, partition, intent, asset string, attrs map[string]string) map[string]string {
	labels := make(map[string]string, len(attrs)+6)
	if tenant != "" {
		labels[labelTenant] = tenant
	}

	attrPartition, attrIntent, attrAsset := identityFromAttributes(attrs)
	if partition == "" {
		partition = attrPartition
	}
	if intent == "" {
		intent = attrIntent
	}
	if asset == "" {
		asset = attrAsset
	}
	if partition != "" {
		labels[labelPartition] = partition
	}
	if intent != "" {
		labels[labelIntent] = intent
	}
	if asset != "" {
		labels[labelAsset] = asset
	}

	fullName := firstNonEmpty(attrs[attrGuardianFullName], attrs[labelServiceFull])
	canonical := canonicalServiceName(partition, intent, asset)
	if fullName == "" {
		fullName = canonical
	}
	if fullName != "" {
		labels[labelServiceFull] = fullName
	}

	// Canonical identity wins over producer-supplied service names. The
	// producer name is retained as service_alias for dashboards and UI filters.
	alias := firstNonEmpty(attrs[attrServiceAlias])
	if canonical != "" {
		if service != "" && service != canonical && alias == "" {
			alias = service
		}
		service = canonical
	}
	if alias == "" {
		alias = attrs["app.kubernetes.io/name"]
	}
	if service != "" {
		labels[labelService] = service
	}
	if alias != "" && alias != service {
		labels[labelServiceAlias] = alias
	}

	for key, value := range attrs {
		if value == "" {
			continue
		}
		normalized := normalizeMetricLabelName(key)
		if normalized == "" {
			continue
		}
		switch normalized {
		case promlabels.MetricName, labelService, labelServiceFull, labelServiceAlias, labelServiceName, labelTenant, labelSource,
			labelPartition, labelIntent, labelAsset, labelShortPartition, labelShortIntent, labelShortAsset:
			continue
		}
		labels[normalized] = value
	}

	// Federated backends (e.g. AWS) set an explicit source attribute; native
	// telemetry defaults to "doctor".
	source := firstNonEmpty(attrs[labelSource], sourceDoctor)
	labels[labelSource] = source
	return labels
}
