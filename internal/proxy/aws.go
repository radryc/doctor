package proxy

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
	"github.com/rydzu/ainfra/doctor/internal/tracecontext"
)

// AWS partition metadata labels, shared with native telemetry.
const (
	AttrAWSRegion     = "aws_region"
	AttrAWSAccount    = "aws_account"
	AWSPartitionLabel = "guardian_partition"
)

const (
	awsQueryNone = iota
	awsQueryOnly
	awsQueryMixed
)

// AWSQueryNone/AWSQueryOnly/AWSQueryMixed are exported for query handlers that
// route between local storage and the AWS federation.
const (
	AWSQueryNone  = awsQueryNone
	AWSQueryOnly  = awsQueryOnly
	AWSQueryMixed = awsQueryMixed
)

var (
	awsSourceExactRe  = regexp.MustCompile(`\bsource\s*=\s*"aws"`)
	awsSourceRegexRe  = regexp.MustCompile(`\bsource\s*=~\s*"([^"]*)"`)
	selectorLeadComma = regexp.MustCompile(`\{\s*,`)
	selectorTailComma = regexp.MustCompile(`,\s*\}`)
	selectorLeadSpace = regexp.MustCompile(`\{\s+`)
	selectorTailSpace = regexp.MustCompile(`\s+\}`)

	lokiSourceAWSPattern = regexp.MustCompile(`source\s*=~?\s*"(aws|aws\|[^"]*)"`)
	lokiLogGroupPattern  = regexp.MustCompile(`aws_log_group\s*(=~?)\s*"([^"]+)"`)
)

// AWSQueryMode classifies a query and returns it with the source matcher
// removed (CloudWatch has no `source` label).
func AWSQueryMode(query string) (string, int) {
	if awsSourceExactRe.MatchString(query) {
		return StripAWSSourceMatcher(query), AWSQueryOnly
	}
	if match := awsSourceRegexRe.FindStringSubmatch(query); len(match) == 2 {
		value := match[1]
		if strings.Contains(value, "aws") {
			mode := AWSQueryMixed
			if value == "aws" {
				mode = AWSQueryOnly
			}
			return StripAWSSourceMatcher(query), mode
		}
	}
	return query, AWSQueryNone
}

// StripAWSSourceMatcher removes a `source="aws"` (or regex) matcher and cleans
// up the selector punctuation it leaves behind.
func StripAWSSourceMatcher(query string) string {
	out := awsSourceExactRe.ReplaceAllString(query, "")
	out = awsSourceRegexRe.ReplaceAllString(out, "")
	out = selectorLeadComma.ReplaceAllString(out, "{")
	out = selectorTailComma.ReplaceAllString(out, "}")
	out = strings.ReplaceAll(out, ",,", ",")
	out = selectorLeadSpace.ReplaceAllString(out, "{")
	out = selectorTailSpace.ReplaceAllString(out, "}")
	// A metric selector that becomes empty should collapse to the bare metric
	// name (CloudWatch accepts `Metric` but not `Metric{}`).
	out = strings.ReplaceAll(out, "{}", "")
	return out
}

// AWSMatches splits match[] selectors into whether any targets AWS and the
// AWS-forwardable (source-stripped) selectors.
func AWSMatches(matches []string) (bool, []string) {
	hasAWS := false
	forwarded := make([]string, 0, len(matches))
	for _, match := range matches {
		stripped, mode := AWSQueryMode(match)
		if mode != AWSQueryNone {
			hasAWS = true
			forwarded = append(forwarded, stripped)
		} else {
			forwarded = append(forwarded, match)
		}
	}
	return hasAWS, forwarded
}

// AWSPartition derives a partition identifier from an AWS region and account so
// federated records can be grouped in the same partition filters as native
// telemetry. Region is preferred because it is always present.
func AWSPartition(region, account string) string {
	switch {
	case region != "":
		return region
	case account != "":
		return account
	default:
		return SourceAWS
	}
}

// ApplyAWSPartition adds the AWS partition and region/account labels to attrs.
func ApplyAWSPartition(attrs map[string]string, region, account string) {
	if region != "" {
		attrs[AttrAWSRegion] = region
	}
	if account != "" {
		attrs[AttrAWSAccount] = account
	}
	if partition := AWSPartition(region, account); partition != "" {
		// A federation catalog target may already know the real Guardian
		// partition. Region is only a fallback; never overwrite a more
		// specific value with it.
		if strings.TrimSpace(attrs[AWSPartitionLabel]) == "" {
			attrs[AWSPartitionLabel] = partition
		}
	}
}

// AWSLogsForLoki executes a reduced LogQL pushdown against CloudWatch Logs when
// it targets the AWS federated source. Errors are swallowed so native logs still
// return.
func AWSLogsForLoki(ctx context.Context, client AWSClient, tenant string, pushdown LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	if client == nil || !client.Enabled() {
		return nil, nil
	}
	if !lokiSourceAWSPattern.MatchString(pushdown.Selector) {
		return nil, nil
	}

	var logGroups []string
	if match := lokiLogGroupPattern.FindStringSubmatch(pushdown.Selector); len(match) == 3 {
		op, raw := match[1], match[2]
		if op == "=~" {
			// A log-group regex (the Grafana dashboards use one so they work
			// with and without the account prefix) must be resolved against
			// the pusher catalog before calling Logs Insights; Insights only
			// accepts concrete log-group names.
			if resolver, ok := client.(interface {
				ResolveLogGroups(context.Context, string) ([]string, error)
			}); ok {
				groups, err := resolver.ResolveLogGroups(ctx, raw)
				if err != nil {
					return nil, err
				}
				logGroups = append(logGroups, groups...)
			} else {
				patterns := make([]*regexp.Regexp, 0, 2)
				for _, part := range strings.Split(raw, "|") {
					if trimmed := strings.TrimSpace(part); trimmed != "" {
						if re, err := regexp.Compile(trimmed); err == nil {
							patterns = append(patterns, re)
						}
					}
				}
				if len(patterns) == 0 {
					return nil, nil
				}
				groups, err := client.ListLogGroups(ctx)
				if err != nil {
					return nil, err
				}
				for _, group := range groups {
					for _, re := range patterns {
						if re.MatchString(group) {
							logGroups = append(logGroups, group)
							break
						}
					}
				}
			}
		} else {
			for _, group := range strings.Split(raw, "|") {
				if trimmed := strings.TrimSpace(group); trimmed != "" {
					logGroups = append(logGroups, trimmed)
				}
			}
		}
	}
	if len(logGroups) == 0 {
		return nil, nil
	}

	insights := "fields @timestamp, @message, @logStream, @log"
	conditions := make([]string, 0, len(pushdown.LineFilters))
	for _, filter := range pushdown.LineFilters {
		switch filter.Op {
		case "|=":
			conditions = append(conditions, "@message like /"+regexp.QuoteMeta(filter.Value)+"/")
		case "|~":
			conditions = append(conditions, "@message like /"+filter.Value+"/")
		case "!=":
			conditions = append(conditions, "@message not like /"+regexp.QuoteMeta(filter.Value)+"/")
		case "!~":
			conditions = append(conditions, "@message not like /"+filter.Value+"/")
		}
	}
	if len(conditions) > 0 {
		insights += " | filter " + strings.Join(conditions, " and ")
	}
	insights += " | sort @timestamp desc"

	records, err := client.QueryLogs(ctx, awsstore.LogQuery{
		LogGroups: logGroups,
		Query:     insights,
		From:      from,
		To:        to,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}

	defaultGroup := ""
	if len(logGroups) == 1 {
		defaultGroup = logGroups[0]
	}
	return AWSLogRecords(records, tenant, defaultGroup, "", ""), nil
}

// AWSLogRecords converts awsread log records into the segment shape used by
// Doctor, stamping AWS provenance and the target's account/region when the
// pusher response omits them.
func AWSLogRecords(records []awsstore.LogRecord, tenant, defaultLogGroup, defaultAccount, defaultRegion string) []segment.LogRecord {
	out := make([]segment.LogRecord, 0, len(records))
	for _, record := range records {
		if record.LogGroup == "" {
			record.LogGroup = defaultLogGroup
		}
		if record.Account == "" {
			record.Account = defaultAccount
		}
		if record.Region == "" {
			record.Region = defaultRegion
		}
		attributes := map[string]string{
			LabelSource:     SourceAWS,
			"aws_log_group": record.LogGroup,
		}
		if record.LogStream != "" {
			attributes["aws_log_stream"] = record.LogStream
		}
		for key, value := range record.Fields {
			attributes[key] = value
		}
		ApplyAWSPartition(attributes, record.Region, record.Account)
		// Guardian log groups are named
		// guardian/<account>/<region>/<partition>/<intent>. Prefer the
		// partition segment over the region so federated logs use the same ACL
		// scope as the partition's native telemetry and X-Ray traces.
		partition := awsLogGroupPartition(record.LogGroup)
		if partition == "" {
			partition = record.Region
		}
		if partition != "" {
			attributes[AWSPartitionLabel] = partition
		}
		out = append(out, segment.LogRecord{
			Tenant:     tenant,
			Service:    firstNonEmpty(record.Fields["service"], record.Fields["service_name"], SourceAWS),
			Partition:  partition,
			Timestamp:  record.Timestamp,
			Body:       record.Message,
			Attributes: attributes,
		})
	}
	return out
}

// awsLogGroupPartition extracts the partition from a Guardian CloudWatch log
// group (guardian/<account>/<region>/<partition>/<intent>).
func awsLogGroupPartition(logGroup string) string {
	logGroup = strings.TrimSpace(logGroup)
	// CloudWatch log records from the AWS read API are prefixed with the
	// account id (for example "780603797542:guardian/.../web").
	if idx := strings.Index(logGroup, "guardian/"); idx >= 0 {
		logGroup = logGroup[idx:]
	}
	parts := strings.Split(strings.Trim(logGroup, "/"), "/")
	if len(parts) < 4 || parts[0] != "guardian" {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

// AWSMetricNames returns discovered CloudWatch metric names. It uses the REST
// ListMetrics endpoint because CloudWatch's PromQL API does not implement the
// Prometheus label-values route used by Grafana's metric browser.
func AWSMetricNames(ctx context.Context, client AWSClient) []string {
	if client == nil || !client.Enabled() {
		return nil
	}
	metrics, err := client.ListMetrics(ctx, "", "")
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(metrics))
	for _, metric := range metrics {
		if name := strings.TrimSpace(metric.MetricName); name != "" {
			seen[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// AWSLogGroups returns the CloudWatch log groups available from the pusher.
func AWSLogGroups(ctx context.Context, client AWSClient) []string {
	if client == nil || !client.Enabled() {
		return nil
	}
	groups, err := client.ListLogGroups(ctx)
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(groups))
	out := make([]string, 0, len(groups))
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if _, exists := seen[group]; exists {
			continue
		}
		seen[group] = struct{}{}
		out = append(out, group)
	}
	sort.Strings(out)
	return out
}

// LogGroupPartition extracts the Guardian partition from a CloudWatch log
// group name.
func LogGroupPartition(logGroup string) string {
	return awsLogGroupPartition(logGroup)
}

// AWSServices returns X-Ray service names.
func AWSServices(ctx context.Context, client AWSClient) ([]string, error) {
	if client == nil || !client.Enabled() {
		return nil, nil
	}
	services, err := client.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(services))
	for _, service := range services {
		if service.Name != "" {
			seen[service.Name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// AWSUpstream adapts an AWSClient to the upstream interfaces. The optional
// filters apply the repo-managed access policy to federated records after
// conversion (AWS partition bindings + per-partition visibility).
type AWSUpstream struct {
	Client          AWSClient
	FilterLogs      func(ctx context.Context, records []segment.LogRecord) []segment.LogRecord
	FilterSpans     func(ctx context.Context, records []segment.TraceRecord) []segment.TraceRecord
	FilterSummaries func(ctx context.Context, summaries []TraceSummary) []TraceSummary
}

// Name implements MetricsUpstream/LogsUpstream/TracesUpstream.
func (u *AWSUpstream) Name() string { return SourceAWS }

// PromInstant implements MetricsUpstream.
func (u *AWSUpstream) PromInstant(ctx context.Context, _, query string, at time.Time) (map[string]any, error) {
	resp, err := u.Client.PrometheusQuery(ctx, query, at)
	if err != nil {
		return nil, err
	}
	return ExtractPrometheusData(resp), nil
}

// PromRange implements MetricsUpstream.
func (u *AWSUpstream) PromRange(ctx context.Context, _, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	resp, err := u.Client.PrometheusQueryRange(ctx, query, start, end, step)
	if err != nil {
		return nil, err
	}
	return ExtractPrometheusData(resp), nil
}

// PromSeries implements MetricsUpstream.
func (u *AWSUpstream) PromSeries(ctx context.Context, _ string, match []string, start, end time.Time) ([]map[string]string, error) {
	resp, err := u.Client.PrometheusSeries(ctx, match, start, end)
	if err != nil {
		return nil, err
	}
	return ExtractPrometheusSeries(resp), nil
}

// PromLabels implements MetricsUpstream.
func (u *AWSUpstream) PromLabels(ctx context.Context, _ string, match []string, start, end time.Time) ([]string, error) {
	resp, err := u.Client.PrometheusLabels(ctx, match, start, end)
	if err != nil {
		return nil, err
	}
	return ExtractPrometheusStringList(resp), nil
}

// PromLabelValues implements MetricsUpstream.
func (u *AWSUpstream) PromLabelValues(ctx context.Context, _, label string, match []string, start, end time.Time) ([]string, error) {
	resp, err := u.Client.PrometheusLabelValues(ctx, label, match, start, end)
	if err != nil {
		return nil, err
	}
	return ExtractPrometheusStringList(resp), nil
}

// QueryLogs implements LogsUpstream.
func (u *AWSUpstream) QueryLogs(ctx context.Context, tenant string, pushdown LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	records, err := AWSLogsForLoki(ctx, u.Client, tenant, pushdown, from, to, limit)
	if err != nil {
		return nil, err
	}
	if u.FilterLogs != nil {
		records = u.FilterLogs(ctx, records)
	}
	return records, nil
}

// ListServices implements TracesUpstream.
func (u *AWSUpstream) ListServices(ctx context.Context, _ string) ([]string, error) {
	return AWSServices(ctx, u.Client)
}

// TraceTags implements TraceTagSource so federated X-Ray traces contribute tag
// names to the Tempo discovery endpoints.
func (u *AWSUpstream) TraceTags(_ context.Context, _ string) ([]string, error) {
	return []string{"service.name", "aws.region", "aws.account"}, nil
}

// TraceTagValues implements TraceTagSource. X-Ray only supports service name
// discovery, which is what the Tempo service list asks for.
func (u *AWSUpstream) TraceTagValues(ctx context.Context, tenant, tag string) ([]string, error) {
	switch tag {
	case "service.name", "service":
		return u.ListServices(ctx, tenant)
	default:
		return nil, nil
	}
}

// SearchTraces implements TracesUpstream.
func (u *AWSUpstream) SearchTraces(ctx context.Context, _ string, search TraceSearch) ([]TraceSummary, error) {
	if u.Client == nil || !u.Client.Enabled() {
		return nil, nil
	}
	summaries, err := u.Client.QueryTraces(ctx, awsstore.TraceQuery{
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
		if !awsSummaryMatchesSearch(summary, search) {
			continue
		}
		out = append(out, TraceSummary{
			TraceID:         normalizeXRayTraceID(summary.TraceID),
			RootServiceName: firstNonEmpty(summary.Service, SourceAWS),
			RootTraceName:   firstNonEmpty(summary.Service, "xray"),
			Service:         firstNonEmpty(summary.Service, SourceAWS),
			StartTime:       summary.StartTime.UTC(),
			Duration:        summary.Duration,
			SpanCount:       summary.SpanCount,
			Region:          summary.Region,
			Account:         summary.Account,
			Partition:       summary.Partition,
			Status:          summary.Status,
		})
	}
	if u.FilterSummaries != nil {
		out = u.FilterSummaries(ctx, out)
	}
	return out, nil
}

// awsSummaryMatchesSearch applies the TraceQL tags Doctor can evaluate on an
// X-Ray trace summary. Arbitrary span attributes are not available on a
// summary, so unknown keys are left to the span-level matcher and do not
// exclude the summary.
func awsSummaryMatchesSearch(summary awsstore.TraceSummary, search TraceSearch) bool {
	for _, tag := range search.Tags {
		value, known := awsSummaryTagValue(summary, tag.Key)
		if known && !tag.Matches(value) {
			return false
		}
	}
	for _, tag := range search.NotTags {
		value, known := awsSummaryTagValue(summary, tag.Key)
		if known && tag.Matches(value) {
			return false
		}
	}
	return true
}

func awsSummaryTagValue(summary awsstore.TraceSummary, key string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "source":
		return SourceAWS, true
	case "region", "aws_region", "aws.region":
		return summary.Region, true
	case "account", "aws_account", "aws.account":
		return summary.Account, true
	case "service", "service.name":
		return firstNonEmpty(summary.Service, SourceAWS), true
	default:
		return "", false
	}
}

// GetTrace implements TracesUpstream.
func (u *AWSUpstream) GetTrace(ctx context.Context, tenant, traceID string) ([]segment.TraceRecord, bool, error) {
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

// GetTraceRaw converts an X-Ray trace without applying the caller's partition
// filter. It lets the HTTP layer distinguish "trace does not exist" from
// "trace exists but is not visible to this caller" before falling back to a
// local scan.
func (u *AWSUpstream) GetTraceRaw(ctx context.Context, _ string, traceID string) ([]segment.TraceRecord, bool, error) {
	if u.Client == nil || !u.Client.Enabled() {
		return nil, false, nil
	}
	spans, err := u.Client.GetTrace(ctx, xrayTraceID(traceID))
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
		attrs[LabelSource] = SourceAWS
		if span.Partition != "" {
			attrs[AWSPartitionLabel] = span.Partition
		}
		ApplyAWSPartition(attrs, span.Region, span.Account)
		out = append(out, segment.TraceRecord{
			TraceID:      normalizeXRayTraceID(span.TraceID),
			SpanID:       span.SpanID,
			ParentSpanID: span.ParentSpanID,
			Name:         span.Name,
			Service:      firstNonEmpty(span.Service, SourceAWS),
			Partition:    span.Partition,
			StartTime:    span.StartTime.UTC(),
			EndTime:      span.EndTime.UTC(),
			Attributes:   attrs,
			StatusCode:   span.StatusCode,
		})
	}
	return out, true, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// normalizeXRayTraceID converts an X-Ray trace ID (1-<8hex>-<24hex>) into the
// W3C 32-hex form Grafana's Tempo plugin expects. Other IDs pass through.
func normalizeXRayTraceID(id string) string {
	canonical, err := tracecontext.CanonicalTraceID(id)
	if err != nil {
		return strings.TrimSpace(id)
	}
	return canonical
}

// xrayTraceID converts a W3C 32-hex trace ID back to the X-Ray format the
// read API expects. X-Ray-formatted IDs pass through.
func xrayTraceID(id string) string {
	converted, err := tracecontext.XRayTraceID(id)
	if err != nil {
		return strings.TrimSpace(id)
	}
	return converted
}
