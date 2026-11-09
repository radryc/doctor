// Package telemetrylabels centralizes the provider-specific label aliases used
// to map native, AWS, GCP, and future federated telemetry onto Doctor's
// canonical identity fields (partition, intent, asset). It also converts a
// selected partition into a provider-specific PromQL matcher so partition
// filtering is pushed down to the backend instead of only being applied to the
// returned series.
//
// Add new provider exceptions here rather than scattering label lists through
// the query and proxy packages.
package telemetrylabels

import "strings"

// Provider identifies the source of a Prometheus series.
type Provider string

const (
	ProviderDoctor Provider = "doctor"
	ProviderAWS    Provider = "aws"
	ProviderGCP    Provider = "gcp"
)

// Canonical and provider-specific identity label names.
const (
	// Canonical Doctor labels.
	Partition         = "partition"
	GuardianPartition = "guardian_partition"
	Intent            = "intent"
	GuardianIntent    = "guardian_intent"
	Asset             = "asset"
	GuardianAsset     = "guardian_asset"
	Source            = "source"

	// AWS labels injected by the pusher / OTLP enrichment.
	AWSPartitionTag       = "@aws.tag.guardian.io/partition"
	AWSStrataPartitionTag = "@aws.tag.strata.io/partition"
	AWSIntentTag          = "@aws.tag.guardian.io/intent"
	AWSStrataIntentTag    = "@aws.tag.strata.io/intent"
	AWSAssetTag           = "@aws.tag.guardian.io/asset"
	AWSStrataAssetTag     = "@aws.tag.strata.io/asset"
	AWSAccount            = "@aws.account"
	AWSRegion             = "@aws.region"

	// GCP labels added by internal/proxy.ApplyGCPMetricLabels (or already
	// present on Cloud Monitoring results).
	GCPPartition = "gcp_partition"
	GCPProject   = "gcp_project"
	GCPRegion    = "gcp_region"
	GCPProjectID = "project_id"
	GCPLocation  = "location"
)

// PartitionKeys is the precedence order used to resolve a canonical partition
// from a series' labels. Provider-specific tags come before generic fallbacks.
var PartitionKeys = []string{
	GuardianPartition,
	AWSPartitionTag,
	AWSStrataPartitionTag,
	GCPPartition,
	Partition,
	GCPProject,
	GCPProjectID,
	"@resource.cloud.project",
	GCPRegion,
	GCPLocation,
	"@resource.cloud.region",
}

// IntentKeys is the precedence order used to resolve a canonical intent.
var IntentKeys = []string{
	GuardianIntent,
	AWSIntentTag,
	AWSStrataIntentTag,
	"@gcp.tag.guardian.io/intent",
	"@gcp.tag.strata.io/intent",
	Intent,
}

// AssetKeys is the precedence order used to resolve a canonical asset.
var AssetKeys = []string{
	GuardianAsset,
	AWSAssetTag,
	AWSStrataAssetTag,
	"@gcp.tag.guardian.io/asset",
	"@gcp.tag.strata.io/asset",
	Asset,
}

// First returns the first non-empty value among keys.
func First(attrs map[string]string, keys []string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(attrs[key]); value != "" {
			return value
		}
	}
	return ""
}

// PartitionValue resolves the canonical partition label from a series.
func PartitionValue(attrs map[string]string) string { return First(attrs, PartitionKeys) }

// MatchesPartition reports whether any known partition alias on the series
// equals partition. GCP uses several equivalent labels (region, location,
// project), so equality on just the first alias would miss valid matches.
func MatchesPartition(attrs map[string]string, partition string) bool {
	partition = strings.TrimSpace(partition)
	if partition == "" {
		return true
	}
	for _, key := range PartitionKeys {
		if strings.TrimSpace(attrs[key]) == partition {
			return true
		}
	}
	return false
}

// IntentValue resolves the canonical intent label from a series.
func IntentValue(attrs map[string]string) string { return First(attrs, IntentKeys) }

// AssetValue resolves the canonical asset label from a series.
func AssetValue(attrs map[string]string) string { return First(attrs, AssetKeys) }
