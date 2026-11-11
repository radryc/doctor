package proxy

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/segment"
	gcpstore "github.com/radryc/doctor/internal/store/gcp"
	"github.com/radryc/doctor/internal/telemetrylabels"
)

// GCP partition and identity labels, shared with native telemetry.
const (
	AttrGCPProject      = "gcp_project"
	AttrGCPRegion       = "gcp_region"
	AttrGCPResourceType = "gcp_resource_type"
)

var (
	gcpSourceExactRe     = regexp.MustCompile(`\bsource\s*=\s*"gcp"`)
	gcpSourceRegexRe     = regexp.MustCompile(`\bsource\s*=~\s*"([^"]*)"`)
	lokiSourceGCPPattern = regexp.MustCompile(`source\s*=~?\s*"(gcp|gcp\|[^"]*)"`)
	gcpLogNamePattern    = regexp.MustCompile(`(?:gcp_log_name|log_name)\s*(=~?)\s*"([^"]+)"`)
)

// GCPQueryMode classifies a query and returns it with the source matcher
// removed (Cloud Monitoring has no `source` label).
func GCPQueryMode(query string) (string, int) {
	if gcpSourceExactRe.MatchString(query) {
		return StripGCPSourceMatcher(query), AWSQueryOnly
	}
	if match := gcpSourceRegexRe.FindStringSubmatch(query); len(match) == 2 {
		value := match[1]
		if strings.Contains(value, "gcp") {
			mode := AWSQueryMixed
			if value == "gcp" {
				mode = AWSQueryOnly
			}
			return StripGCPSourceMatcher(query), mode
		}
	}
	return query, AWSQueryNone
}

// StripGCPSourceMatcher removes a `source="gcp"` matcher and cleans up the
// punctuation it leaves behind.
func StripGCPSourceMatcher(query string) string {
	out := gcpSourceExactRe.ReplaceAllString(query, "")
	out = gcpSourceRegexRe.ReplaceAllString(out, "")
	out = selectorLeadComma.ReplaceAllString(out, "{")
	out = selectorTailComma.ReplaceAllString(out, "}")
	out = strings.ReplaceAll(out, ",,", ",")
	out = selectorLeadSpace.ReplaceAllString(out, "{")
	out = selectorTailSpace.ReplaceAllString(out, "}")
	out = strings.ReplaceAll(out, "{}", "")
	return out
}

// ProviderSelectorMode reports whether a LogQL selector is scoped to one cloud
// provider, returning the stripped selector and the backend kind.
func ProviderSelectorMode(selector string) (string, string, bool) {
	if lokiSourceAWSPattern.MatchString(selector) {
		return StripAWSSourceMatcher(selector), KindAWS, true
	}
	if lokiSourceGCPPattern.MatchString(selector) {
		return StripGCPSourceMatcher(selector), KindGCP, true
	}
	return selector, "", false
}

// GCPPartition derives a partition identifier from a GCP region and project.
func GCPPartition(region, project string) string {
	switch {
	case region != "":
		return region
	case project != "":
		return project
	default:
		return SourceGCP
	}
}

// ApplyGCPLabels adds the GCP project/region labels and the canonical partition.
func ApplyGCPLabels(attrs map[string]string, region, project string) {
	if region != "" {
		attrs[AttrGCPRegion] = region
	}
	if project != "" {
		attrs[AttrGCPProject] = project
	}
	if partition := GCPPartition(region, project); partition != "" {
		if strings.TrimSpace(attrs[AWSPartitionLabel]) == "" {
			attrs[AWSPartitionLabel] = partition
		}
	}
}

// normalizeGCPSpanID converts a Cloud Trace span ID to the 16-character
// lowercase hexadecimal form used by OTLP and X-Ray.
//
// The Cloud Trace REST API returns 64-bit span IDs as decimal strings, while
// k8s/AWS telemetry arrives with hexadecimal span IDs. Without this conversion
// a parent_span_id from Cloud Run can never match the hexadecimal span_id of
// the k8s or AWS span that called it, so federated traces appear as detached
// subtrees in the trace flow view.
func normalizeGCPSpanID(raw string) string {
	id := strings.TrimSpace(raw)
	if id == "" {
		return ""
	}
	if value, err := strconv.ParseUint(id, 10, 64); err == nil {
		return fmt.Sprintf("%016x", value)
	}
	// Already hexadecimal (or an unknown format); normalise the casing so
	// string comparisons against OTLP span IDs are stable.
	return strings.ToLower(id)
}

// GCPUpstream adapts a GCPClient to the upstream interfaces.
type GCPUpstream struct {
	Client          GCPClient
	FilterLogs      func(ctx context.Context, records []segment.LogRecord) []segment.LogRecord
	FilterSpans     func(ctx context.Context, records []segment.TraceRecord) []segment.TraceRecord
	FilterSummaries func(ctx context.Context, summaries []TraceSummary) []TraceSummary
}

// Name implements MetricsUpstream/LogsUpstream/TracesUpstream.
func (u *GCPUpstream) Name() string { return SourceGCP }

func (u *GCPUpstream) PromInstant(ctx context.Context, _, query string, at time.Time) (map[string]any, error) {
	resp, err := u.Client.PrometheusQuery(ctx, query, at)
	if err != nil {
		data, retryErr := u.gcpRetryWithResourceTypes(query, err, func(query string) (map[string]any, error) {
			resp, queryErr := u.Client.PrometheusQuery(ctx, query, at)
			if queryErr != nil {
				return nil, queryErr
			}
			return ExtractPrometheusData(resp), nil
		})
		if retryErr != nil {
			return nil, retryErr
		}
		return annotateGCPMetricData(data), nil
	}
	return annotateGCPMetricData(ExtractPrometheusData(resp)), nil
}

func (u *GCPUpstream) PromRange(ctx context.Context, _, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	resp, err := u.Client.PrometheusQueryRange(ctx, query, start, end, step)
	if err != nil {
		data, retryErr := u.gcpRetryWithResourceTypes(query, err, func(query string) (map[string]any, error) {
			resp, queryErr := u.Client.PrometheusQueryRange(ctx, query, start, end, step)
			if queryErr != nil {
				return nil, queryErr
			}
			return ExtractPrometheusData(resp), nil
		})
		if retryErr != nil {
			return nil, retryErr
		}
		return annotateGCPMetricData(data), nil
	}
	return annotateGCPMetricData(ExtractPrometheusData(resp)), nil
}

func (u *GCPUpstream) PromSeries(ctx context.Context, _ string, match []string, start, end time.Time) ([]map[string]string, error) {
	resp, err := u.Client.PrometheusSeries(ctx, match, start, end)
	if err != nil {
		return nil, err
	}
	return ExtractPrometheusSeries(resp), nil
}

func (u *GCPUpstream) PromLabels(ctx context.Context, _ string, match []string, start, end time.Time) ([]string, error) {
	resp, err := u.Client.PrometheusLabels(ctx, match, start, end)
	if err != nil {
		return nil, err
	}
	return ExtractPrometheusStringList(resp), nil
}

func (u *GCPUpstream) PromLabelValues(ctx context.Context, _, label string, match []string, start, end time.Time) ([]string, error) {
	resp, err := u.Client.PrometheusLabelValues(ctx, label, match, start, end)
	if err != nil {
		return nil, err
	}
	return ExtractPrometheusStringList(resp), nil
}

// QueryLogs implements LogsUpstream.
func (u *GCPUpstream) QueryLogs(ctx context.Context, tenant string, pushdown LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	if u.Client == nil || !u.Client.Enabled() {
		return nil, nil
	}
	logName, query := gcpLogQuery(pushdown)
	records, err := u.Client.QueryLogs(ctx, gcpstore.LogQuery{
		LogName: logName,
		Query:   query,
		From:    from,
		To:      to,
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]segment.LogRecord, 0, len(records))
	for _, record := range records {
		attrs := map[string]string{LabelSource: SourceGCP}
		for key, value := range record.Fields {
			attrs[key] = value
		}
		if record.LogStream != "" {
			attrs["log_stream"] = record.LogStream
		}
		// Resolve the Guardian partition from application-provided labels before
		// ApplyGCPLabels installs the region/project fallback. Otherwise the
		// fallback wins and the record is attributed to a region.
		partition := gcpIdentityPartition(attrs, record.Region, record.Project)
		ApplyGCPLabels(attrs, record.Region, record.Project)
		if partition != "" {
			attrs[telemetrylabels.GuardianPartition] = partition
		}
		resource := map[string]string{
			"service.name": firstNonEmpty(attrs["service.name"], attrs["service"], record.LogGroup, SourceGCP),
			"log.name":     record.LogGroup,
		}
		if record.Project != "" {
			resource[AttrGCPProject] = record.Project
		}
		if record.Region != "" {
			resource[AttrGCPRegion] = record.Region
		}
		if partition != "" {
			resource[telemetrylabels.GuardianPartition] = partition
		}
		out = append(out, segment.LogRecord{
			Tenant:             tenant,
			Service:            resource["service.name"],
			Partition:          partition,
			Timestamp:          record.Timestamp,
			SeverityText:       record.Severity,
			Body:               record.Message,
			ResourceAttributes: resource,
			Attributes:         attrs,
		})
	}
	if u.FilterLogs != nil {
		out = u.FilterLogs(ctx, out)
	}
	return out, nil
}

// gcpLogQuery extracts a log name and a Cloud Logging filter from a LogQL
// pushdown. Unsupported selector labels are left to the caller.
func gcpLogQuery(pushdown LogPushdown) (string, string) {
	logName := ""
	if match := gcpLogNamePattern.FindStringSubmatch(pushdown.Selector); len(match) == 3 {
		logName = match[2]
	}
	parts := make([]string, 0, len(pushdown.LineFilters))
	for _, filter := range pushdown.LineFilters {
		switch filter.Op {
		case "|=":
			parts = append(parts, fmt.Sprintf("%q", filter.Value))
		case "!=":
			parts = append(parts, "NOT "+fmt.Sprintf("%q", filter.Value))
		case "|~":
			parts = append(parts, fmt.Sprintf("textPayload =~ %q", filter.Value))
		case "!~":
			parts = append(parts, fmt.Sprintf("NOT textPayload =~ %q", filter.Value))
		}
	}
	return logName, strings.Join(parts, " AND ")
}

// ListServices implements TracesUpstream.
func (u *GCPUpstream) ListServices(ctx context.Context, _ string) ([]string, error) {
	if u.Client == nil || !u.Client.Enabled() {
		return nil, nil
	}
	services, err := u.Client.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(services))
	for _, service := range services {
		if service.Name == "" {
			continue
		}
		if _, ok := seen[service.Name]; ok {
			continue
		}
		seen[service.Name] = struct{}{}
		out = append(out, service.Name)
	}
	sort.Strings(out)
	return out, nil
}

// TraceTags implements TraceTagSource.
func (u *GCPUpstream) TraceTags(_ context.Context, _ string) ([]string, error) {
	return []string{"service.name", AttrGCPRegion, AttrGCPProject}, nil
}

// TraceTagValues implements TraceTagSource.
func (u *GCPUpstream) TraceTagValues(ctx context.Context, tenant, tag string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(tag)) {
	case "service.name", "service":
		return u.ListServices(ctx, tenant)
	case AttrGCPProject, "project":
		return u.gcpTagValues(ctx, func(info gcpstore.ServiceInfo) string { return info.Project }), nil
	case AttrGCPRegion, "region", "gcp.region":
		return u.gcpTagValues(ctx, func(info gcpstore.ServiceInfo) string { return info.Region }), nil
	default:
		return nil, nil
	}
}

func (u *GCPUpstream) gcpTagValues(ctx context.Context, pick func(gcpstore.ServiceInfo) string) []string {
	if u.Client == nil || !u.Client.Enabled() {
		return nil
	}
	services, err := u.Client.ListServices(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, service := range services {
		value := pick(service)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// SearchTraces implements TracesUpstream.
func (u *GCPUpstream) SearchTraces(ctx context.Context, _ string, search TraceSearch) ([]TraceSummary, error) {
	if u.Client == nil || !u.Client.Enabled() {
		return nil, nil
	}
	summaries, err := u.Client.QueryTraces(ctx, gcpstore.TraceQuery{
		Service: search.Service,
		From:    search.From,
		To:      search.To,
		Limit:   search.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]TraceSummary, 0, len(summaries))
	for _, summary := range summaries {
		if !gcpSummaryMatchesSearch(summary, search) {
			continue
		}
		service := firstNonEmpty(summary.Service, SourceGCP)
		partition := gcpIdentityPartition(summary.Attributes, summary.Region, summary.Project)
		out = append(out, TraceSummary{
			TraceID:         summary.TraceID,
			RootServiceName: service,
			RootTraceName:   service,
			Service:         service,
			StartTime:       summary.StartTime.UTC(),
			Duration:        summary.Duration,
			SpanCount:       summary.SpanCount,
			Region:          summary.Region,
			Account:         summary.Project,
			Partition:       partition,
			Status:          summary.Status,
		})
	}
	if u.FilterSummaries != nil {
		out = u.FilterSummaries(ctx, out)
	}
	return out, nil
}

func gcpSummaryMatchesSearch(summary gcpstore.TraceSummary, search TraceSearch) bool {
	for _, tag := range search.Tags {
		value, known := gcpSummaryTagValue(summary, tag.Key)
		if known && !tag.Matches(value) {
			return false
		}
	}
	for _, tag := range search.NotTags {
		value, known := gcpSummaryTagValue(summary, tag.Key)
		if known && tag.Matches(value) {
			return false
		}
	}
	return true
}

func gcpSummaryTagValue(summary gcpstore.TraceSummary, key string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "source":
		return SourceGCP, true
	case "region", "gcp_region", "gcp.region":
		return summary.Region, true
	case "project", "account", "gcp_project", "gcp.project":
		return summary.Project, true
	case "service", "service.name":
		return firstNonEmpty(summary.Service, SourceGCP), true
	default:
		return "", false
	}
}

// GCPTracePartition resolves the Guardian partition for a GCP trace summary.
func GCPTracePartition(summary gcpstore.TraceSummary) string {
	attrs := make(map[string]string, len(summary.Attributes)+1)
	for key, value := range summary.Attributes {
		attrs[key] = value
	}
	if summary.Service != "" {
		attrs["service"] = summary.Service
	}
	return gcpIdentityPartition(attrs, summary.Region, summary.Project)
}

// GetTrace implements TracesUpstream.
func (u *GCPUpstream) GetTrace(ctx context.Context, tenant, traceID string) ([]segment.TraceRecord, bool, error) {
	records, found, err := u.GetTraceRaw(ctx, tenant, traceID)
	if err != nil || !found {
		return records, found, err
	}
	if u.FilterSpans != nil {
		records = u.FilterSpans(ctx, records)
	}
	if len(records) == 0 {
		return nil, false, nil
	}
	return records, true, nil
}

// GetTraceRaw converts GCP spans without applying the caller's policy filter.
func (u *GCPUpstream) GetTraceRaw(ctx context.Context, tenant, traceID string) ([]segment.TraceRecord, bool, error) {
	if u.Client == nil || !u.Client.Enabled() {
		return nil, false, nil
	}
	spans, err := u.Client.GetTrace(ctx, traceID)
	if err != nil {
		return nil, false, err
	}
	if len(spans) == 0 {
		return nil, false, nil
	}
	out := make([]segment.TraceRecord, 0, len(spans))
	for _, span := range spans {
		attrs := make(map[string]string, len(span.Attributes)+1)
		for key, value := range span.Attributes {
			if value == "" {
				continue
			}
			attrs[key] = value
		}
		attrs[LabelSource] = SourceGCP
		// Resolve the Guardian partition before ApplyGCPLabels installs the
		// region/project fallback. Cloud Run infrastructure spans (which have no
		// application labels) must still be attributed to the partition that
		// owns the service, or partition-scoped users lose them and the trace
		// detail loses the parent that links the app span to its caller.
		partition := gcpIdentityPartition(attrs, span.Region, span.Project)
		ApplyGCPLabels(attrs, span.Region, span.Project)
		if partition != "" {
			attrs[telemetrylabels.GuardianPartition] = partition
		}
		resource := map[string]string{AttrGCPProject: span.Project, AttrGCPRegion: span.Region}
		if partition != "" {
			resource[telemetrylabels.GuardianPartition] = partition
		}
		out = append(out, segment.TraceRecord{
			Tenant:             tenant,
			TraceID:            span.TraceID,
			SpanID:             normalizeGCPSpanID(span.SpanID),
			ParentSpanID:       normalizeGCPSpanID(span.ParentSpanID),
			Name:               span.Name,
			Kind:               "SPAN_KIND_UNSPECIFIED",
			Service:            firstNonEmpty(span.Service, span.Name, SourceGCP),
			Partition:          partition,
			StartTime:          span.StartTime.UTC(),
			EndTime:            span.EndTime.UTC(),
			StatusCode:         span.StatusCode,
			ResourceAttributes: resource,
			Attributes:         attrs,
		})
	}
	return out, true, nil
}

// TraceSearchProviderKind reports whether a trace search is explicitly scoped
// to one cloud provider via a `source` tag.
func TraceSearchProviderKind(search TraceSearch) string {
	for _, tag := range search.Tags {
		if !strings.EqualFold(strings.TrimSpace(tag.Key), "source") || tag.Regex != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(tag.Value)) {
		case KindAWS:
			return KindAWS
		case KindGCP:
			return KindGCP
		}
	}
	return ""
}
