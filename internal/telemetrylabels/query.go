package telemetrylabels

import (
	"regexp"
	"strings"
)

// PartitionMatcher returns the label name and PromQL matcher that should be
// pushed down for a partition filter. The label name is returned separately so
// callers can skip injection when the query already constrains that label.
func PartitionMatcher(provider Provider, partition string) (label, matcher string, ok bool) {
	partition = strings.TrimSpace(partition)
	if partition == "" {
		return "", "", false
	}
	switch provider {
	case ProviderDoctor:
		return GuardianPartition, quoteMatcher(GuardianPartition, partition), true
	case ProviderAWS:
		return AWSPartitionTag, quoteMatcher(AWSPartitionTag, partition), true
	case ProviderGCP:
		// GCP partitions can be a region, a project, or a Guardian application
		// partition (for example diva-shared). Only regions map cleanly to a
		// native Cloud Monitoring label; everything else is left to the merged
		// result filter, which understands gcp_partition/gcp_project and the
		// guardian_partition derived from the Cloud Run service name.
		if looksLikeGCPRegion(partition) {
			return GCPLocation, quoteMatcher(GCPLocation, partition), true
		}
		return "", "", false
	default:
		return "", "", false
	}
}

// ApplyPartition rewrites query so it filters on the provider-specific
// partition label. Queries that already constrain the label, and expressions
// without a metric selector the injector understands, are returned unchanged.
func ApplyPartition(query string, provider Provider, partition string) string {
	label, matcher, ok := PartitionMatcher(provider, partition)
	if !ok || HasLabelMatcher(query, label) {
		return query
	}
	return injectMatcher(query, matcher)
}

// HasLabelMatcher reports whether query already contains a matcher for label.
// Both bare and quoted UTF-8 label names are recognized.
func HasLabelMatcher(query, label string) bool {
	escaped := regexp.QuoteMeta(label)
	pattern := regexp.MustCompile(`(?:^|[,{(\s])(?:"` + escaped + `"|` + escaped + `)\s*=~?`)
	return pattern.MatchString(query)
}

// quoteMatcher renders a label matcher, quoting labels that are not valid
// PromQL identifiers (for example AWS tag labels containing '.' and '/').
func quoteMatcher(label, value string) string {
	name := label
	if !validLabelNamePattern.MatchString(label) {
		name = `"` + label + `"`
	}
	return name + `="` + escapePromQLString(value) + `"`
}

var (
	validLabelNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	bareMetricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	gcpRegionPattern      = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)+[0-9]+$`)
)

func looksLikeGCPRegion(value string) bool {
	return gcpRegionPattern.MatchString(value)
}

func escapePromQLString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}

// injectMatcher inserts matcher into the first metric selector, or appends a
// selector to a bare metric name.
func injectMatcher(query, matcher string) string {
	if idx := strings.Index(query, "{"); idx >= 0 {
		rest := strings.TrimSpace(query[idx+1:])
		if rest == "" || strings.HasPrefix(rest, "}") {
			return query[:idx+1] + matcher + query[idx+1:]
		}
		return query[:idx+1] + matcher + ", " + query[idx+1:]
	}
	if bareMetricNamePattern.MatchString(query) {
		return query + "{" + matcher + "}"
	}
	return query
}
