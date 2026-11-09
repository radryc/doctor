package proxy

import (
	"regexp"
	"strings"
	"sync"

	"github.com/radryc/doctor/internal/telemetrylabels"
)

// gcpResourceTypesPattern extracts the possible resource types from Cloud
// Monitoring's "must specify a label matcher on the 'monitored_resource'
// label" error, e.g.:
//
//	... multiple monitored resource types [cloud_run_revision cloud_run_job]
//	    are possible for metric type "..."
var gcpResourceTypesPattern = regexp.MustCompile(`multiple monitored resource types \[([^\]]+)\] are possible`)

const gcpMonitoredResourceLabel = "monitored_resource"

// GCPMonitoredResourceTypes returns the resource types Cloud Monitoring lists
// when a metric can belong to more than one monitored-resource type.
func GCPMonitoredResourceTypes(err error) []string {
	if err == nil {
		return nil
	}
	match := gcpResourceTypesPattern.FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return nil
	}
	fields := strings.Fields(match[1])
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

// WithGCPMonitoredResource adds a monitored_resource matcher to the query's
// metric selector. Doctor-generated selectors look like
// `{__name__="run.googleapis.com/..."}`; bare metric names are accepted too.
// Complex expressions without any selector are returned unchanged, in which
// case callers should surface the original backend error.
func WithGCPMonitoredResource(query, resourceType string) string {
	resourceType = strings.TrimSpace(resourceType)
	if resourceType == "" || strings.Contains(query, gcpMonitoredResourceLabel) {
		return query
	}
	matcher := gcpMonitoredResourceLabel + `="` + escapePromQLString(resourceType) + `"`
	if idx := strings.Index(query, "{"); idx >= 0 {
		rest := strings.TrimSpace(query[idx+1:])
		if rest == "" || strings.HasPrefix(rest, "}") {
			return query[:idx+1] + matcher + query[idx+1:]
		}
		return query[:idx+1] + matcher + ", " + query[idx+1:]
	}
	if isBareMetricName(query) {
		return query + "{" + matcher + "}"
	}
	return query
}

var bareMetricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

func isBareMetricName(query string) bool {
	return bareMetricNamePattern.MatchString(query)
}

func escapePromQLString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}

// gcpPrometheusDataForResourceTypes runs the query once per monitored-resource
// type and merges the Prometheus results. This makes Cloud Monitoring metrics
// with several possible resource types (for example Cloud Run) usable from the
// metric catalog without hand-writing the matcher.
func (u *GCPUpstream) gcpPrometheusDataForResourceTypes(
	query string,
	resourceTypes []string,
	originalErr error,
	run func(query string) (map[string]any, error),
) (map[string]any, error) {
	type result struct {
		data map[string]any
		err  error
	}
	results := make([]result, len(resourceTypes))
	var wg sync.WaitGroup
	for i, resourceType := range resourceTypes {
		wg.Add(1)
		go func(i int, resourceType string) {
			defer wg.Done()
			data, err := run(WithGCPMonitoredResource(query, resourceType))
			results[i] = result{data: data, err: err}
		}(i, resourceType)
	}
	wg.Wait()

	parts := make([]map[string]any, 0, len(results))
	var lastErr error
	for _, res := range results {
		if res.err != nil {
			lastErr = res.err
			continue
		}
		if res.data != nil {
			parts = append(parts, res.data)
		}
	}
	if len(parts) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, originalErr
	}
	return mergeGCPPrometheusData(parts), nil
}

// mergeGCPPrometheusData concatenates result arrays from per-resource-type
// queries. The monitored_resource label keeps the series distinct.
func mergeGCPPrometheusData(parts []map[string]any) map[string]any {
	if len(parts) == 0 {
		return nil
	}
	resultType := "matrix"
	merged := make([]any, 0)
	for _, part := range parts {
		if part == nil {
			continue
		}
		if current, ok := part["resultType"].(string); ok && current != "" {
			resultType = current
		}
		switch result := part["result"].(type) {
		case []any:
			merged = append(merged, result...)
		case []map[string]any:
			for _, item := range result {
				merged = append(merged, item)
			}
		}
	}
	return map[string]any{"resultType": resultType, "result": merged}
}

// gcpRetryWithResourceTypes is a convenience wrapper around
// gcpPrometheusDataForResourceTypes used by the PromQL wrappers.
func (u *GCPUpstream) gcpRetryWithResourceTypes(
	query string,
	originalErr error,
	run func(query string) (map[string]any, error),
) (map[string]any, error) {
	resourceTypes := GCPMonitoredResourceTypes(originalErr)
	if len(resourceTypes) == 0 {
		return nil, originalErr
	}
	return u.gcpPrometheusDataForResourceTypes(query, resourceTypes, originalErr, run)
}

var gcpRunServicePartitionPattern = regexp.MustCompile(`guardian-([a-z0-9-]+?)-gcp-app(?:-|$)`)

// gcpPartitionFromName derives the Guardian partition from the demo Cloud Run
// naming convention: guardian-<partition>-gcp-app[-revision].
func gcpPartitionFromName(name string) string {
	match := gcpRunServicePartitionPattern.FindStringSubmatch(name)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

// gcpIdentityPartition resolves the Guardian partition for a GCP record.
// Application-provided partition fields win; then the Cloud Run service /
// revision name; finally the region/project fallback used for generic metrics.
func gcpIdentityPartition(attrs map[string]string, region, project string) string {
	for _, key := range []string{
		telemetrylabels.GuardianPartition,
		"guardian.partition",
		telemetrylabels.Partition,
		"faas.name",
		"service.name",
		"service_name",
		"configuration_name",
		"revision_name",
		"log_stream",
		"cloud.resource_id",
	} {
		value := strings.TrimSpace(attrs[key])
		if value == "" {
			continue
		}
		switch key {
		case telemetrylabels.GuardianPartition, "guardian.partition", telemetrylabels.Partition:
			return value
		}
		if partition := gcpPartitionFromName(value); partition != "" {
			return partition
		}
	}
	return GCPPartition(region, project)
}

func gcpMetricPartition(metric map[string]any, region, project string) string {
	attrs := make(map[string]string, len(metric))
	for key, value := range metric {
		if text, ok := value.(string); ok {
			attrs[key] = text
		}
	}
	return gcpIdentityPartition(attrs, region, project)
}

// annotateGCPMetricData adds Doctor's canonical identity labels to Cloud
// Monitoring series so the partition filter can match GCP results. The native
// Cloud Monitoring labels (location, project_id) are preserved.
func annotateGCPMetricData(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	result, ok := data["result"].([]any)
	if !ok {
		return data
	}
	for _, item := range result {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch metric := entry["metric"].(type) {
		case map[string]any:
			annotateGCPMetricLabels(metric)
		case map[string]string:
			attrs := make(map[string]any, len(metric)+3)
			for key, value := range metric {
				attrs[key] = value
			}
			annotateGCPMetricLabels(attrs)
			converted := make(map[string]string, len(attrs))
			for key, value := range attrs {
				if text, ok := value.(string); ok {
					converted[key] = text
				}
			}
			entry["metric"] = converted
		}
	}
	return data
}

func annotateGCPMetricLabels(metric map[string]any) {
	region := firstMetricString(metric,
		telemetrylabels.GCPRegion,
		telemetrylabels.GCPLocation,
		"@resource.cloud.region",
		"region",
	)
	project := firstMetricString(metric,
		telemetrylabels.GCPProject,
		telemetrylabels.GCPProjectID,
		"@resource.cloud.project",
	)
	if region != "" {
		metric[telemetrylabels.GCPRegion] = region
	}
	if project != "" {
		metric[telemetrylabels.GCPProject] = project
	}
	if region != "" || project != "" {
		metric[telemetrylabels.GCPPartition] = GCPPartition(region, project)
	}
	if partition := gcpMetricPartition(metric, region, project); partition != "" {
		metric[telemetrylabels.GuardianPartition] = partition
	}
	if _, exists := metric[telemetrylabels.Source]; !exists {
		metric[telemetrylabels.Source] = SourceGCP
	}
}

func firstMetricString(metric map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metric[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}
