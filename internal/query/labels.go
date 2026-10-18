package query

import (
	promlabels "github.com/prometheus/prometheus/model/labels"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// This file defines the canonical label vocabulary shared by the metrics
// (Prometheus), logs (Loki) and traces (Tempo) compatibility APIs. Grafana
// correlates signals by matching these labels, so they must be projected
// identically regardless of the source record type.

const (
	labelService     = "service"
	labelServiceFull = "service_full"
	labelTenant      = "tenant"
	labelSource      = "source"
	labelPartition   = "guardian_partition"
	labelIntent      = "guardian_intent"
	labelAsset       = "guardian_asset"

	sourceDoctor = "doctor"

	attrGuardianPartition = "guardian.partition"
	attrGuardianIntent    = "guardian.intent"
	attrGuardianAsset     = "guardian.asset"
	attrGuardianFullName  = "guardian.service.full_name"

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
		attrs["partition"],
		attrs[attrK8sNamespace],
		attrs["namespace"],
	)
	intent = firstNonEmpty(
		attrs[attrGuardianIntent],
		attrs[labelIntent],
		attrs["intent"],
		attrs[attrAppComponent],
	)
	asset = firstNonEmpty(
		attrs[attrGuardianAsset],
		attrs[labelAsset],
		attrs["asset"],
		attrs[attrK8sPod],
		attrs[attrK8sContainer],
		attrs[attrAppInstance],
	)
	return partition, intent, asset
}

// lokiLabelAllowlist is the curated set of low-cardinality labels exposed as
// Loki stream labels. Point attributes outside this set stay queryable via the
// log body / line filters but do not fragment streams.
var lokiLabelAllowlist = map[string]struct{}{
	labelService:         {},
	labelServiceFull:     {},
	labelTenant:          {},
	labelSource:          {},
	labelPartition:       {},
	labelIntent:          {},
	labelAsset:           {},
	"guardian_cluster":   {},
	"k8s_cluster_name":   {},
	"level":              {},
	"trace_id":           {},
	"span_id":            {},
	"k8s_namespace_name": {},
	"k8s_pod_name":       {},
	"k8s_container_name": {},
	"k8s_node_name":      {},
	"guardian_component": {},
	"aws_log_group":      {},
	"aws_log_stream":     {},
	"aws_region":         {},
	"aws_account":        {},
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
	attrs := mergeStringMaps(span.ResourceAttributes, span.Attributes)
	attrPartition, attrIntent, attrAsset := identityFromAttributes(attrs)
	return firstNonEmpty(span.Partition, attrPartition),
		firstNonEmpty(span.Intent, attrIntent),
		firstNonEmpty(span.Asset, attrAsset)
}

// canonicalLabels projects a record's identity and attributes into the shared
// Grafana label vocabulary. Explicit typed identity wins over attribute-derived
// identity; reserved keys are never overwritten by raw attributes.
func canonicalLabels(service, tenant, partition, intent, asset string, attrs map[string]string) map[string]string {
	labels := make(map[string]string, len(attrs)+6)
	if service != "" {
		labels[labelService] = service
	}
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
	if fullName == "" && partition != "" && intent != "" && asset != "" {
		fullName = partition + "." + intent + "." + asset
	}
	if fullName != "" {
		labels[labelServiceFull] = fullName
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
		case promlabels.MetricName, labelService, labelServiceFull, labelTenant, labelSource, labelPartition, labelIntent, labelAsset:
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
