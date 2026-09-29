package query

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

// awsSourceLabel marks federated CloudWatch/X-Ray records.
const awsSourceLabel = "aws"

const (
	// attrAWSRegion / attrAWSAccount expose the AWS partition an X-Ray/CloudWatch
	// record came from. awsPartitionLabel is the canonical, filterable partition
	// shared with native telemetry.
	attrAWSRegion     = "aws_region"
	attrAWSAccount    = "aws_account"
	awsPartitionLabel = labelPartition
)

// awsReady reports whether the AWS federation backend is configured and enabled.
func (s *Service) awsReady() bool {
	return s.aws != nil && s.aws.Enabled()
}

// awsPartition derives a partition identifier from an AWS region and account so
// federated records can be grouped in the same partition filters as native
// telemetry. Region is preferred because it is always present.
func awsPartition(region, account string) string {
	switch {
	case region != "":
		return region
	case account != "":
		return account
	default:
		return awsSourceLabel
	}
}

// applyAWSPartition adds the AWS partition and region/account labels to attrs.
func applyAWSPartition(attrs map[string]string, region, account string) {
	if region != "" {
		attrs[attrAWSRegion] = region
	}
	if account != "" {
		attrs[attrAWSAccount] = account
	}
	if partition := awsPartition(region, account); partition != "" {
		attrs[awsPartitionLabel] = partition
	}
}

// awsBackend is the read surface Doctor federates from guardian-pusher-aws.
type awsBackend interface {
	Enabled() bool
	ListMetrics(ctx context.Context, namespace, metric string) ([]awsstore.MetricDescriptor, error)
	ListServices(ctx context.Context) ([]awsstore.ServiceInfo, error)
	QueryLogs(ctx context.Context, q awsstore.LogQuery) ([]awsstore.LogRecord, error)
	QueryTraces(ctx context.Context, q awsstore.TraceQuery) ([]awsstore.TraceSummary, error)
	GetTrace(ctx context.Context, traceID string) ([]awsstore.Span, error)
	PrometheusQuery(ctx context.Context, query string, at time.Time) (map[string]any, error)
	PrometheusQueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string]any, error)
	PrometheusSeries(ctx context.Context, match []string, start, end time.Time) (map[string]any, error)
	PrometheusLabels(ctx context.Context, match []string, start, end time.Time) (map[string]any, error)
	PrometheusLabelValues(ctx context.Context, label string, match []string, start, end time.Time) (map[string]any, error)
}

var (
	lokiSourceAWSPattern = regexp.MustCompile(`source\s*=~?\s*"(aws|aws\|[^"]*)"`)
	lokiLogGroupPattern  = regexp.MustCompile(`aws_log_group\s*=~?\s*"([^"]+)"`)
)

// queryAWSLogsForLoki executes a parsed LogQL plan against CloudWatch Logs when
// it targets the AWS federated source. Errors are swallowed so native logs still
// return.
func (s *Service) queryAWSLogsForLoki(ctx context.Context, tenant string, plan lokiPlan, from, to time.Time, limit int) []segment.LogRecord {
	if s.aws == nil || !s.aws.Enabled() {
		return nil
	}
	if !lokiSourceAWSPattern.MatchString(plan.selector) {
		return nil
	}

	var logGroups []string
	if match := lokiLogGroupPattern.FindStringSubmatch(plan.selector); len(match) == 2 {
		for _, group := range strings.Split(match[1], "|") {
			if trimmed := strings.TrimSpace(group); trimmed != "" {
				logGroups = append(logGroups, trimmed)
			}
		}
	}
	if len(logGroups) == 0 {
		return nil
	}

	insights := "fields @timestamp, @message, @logStream, @log"
	conditions := make([]string, 0, len(plan.lineFilters))
	for _, filter := range plan.lineFilters {
		switch filter.op {
		case "|=":
			conditions = append(conditions, "@message like /"+regexp.QuoteMeta(filter.value)+"/")
		case "|~":
			conditions = append(conditions, "@message like /"+filter.value+"/")
		case "!=":
			conditions = append(conditions, "@message not like /"+regexp.QuoteMeta(filter.value)+"/")
		case "!~":
			conditions = append(conditions, "@message not like /"+filter.value+"/")
		}
	}
	if len(conditions) > 0 {
		insights += " | filter " + strings.Join(conditions, " and ")
	}
	insights += " | sort @timestamp desc"

	records, err := s.aws.QueryLogs(ctx, awsstore.LogQuery{
		LogGroups: logGroups,
		Query:     insights,
		From:      from,
		To:        to,
		Limit:     limit,
	})
	if err != nil {
		return nil
	}

	out := make([]segment.LogRecord, 0, len(records))
	for _, record := range records {
		attributes := map[string]string{
			labelSource:     awsSourceLabel,
			"aws_log_group": record.LogGroup,
		}
		if record.LogStream != "" {
			attributes["aws_log_stream"] = record.LogStream
		}
		for key, value := range record.Fields {
			attributes[key] = value
		}
		applyAWSPartition(attributes, record.Region, record.Account)
		out = append(out, segment.LogRecord{
			Tenant:     tenant,
			Service:    firstNonEmpty(record.Fields["service"], record.Fields["service_name"], awsSourceLabel),
			Partition:  record.Region,
			Timestamp:  record.Timestamp,
			Body:       record.Message,
			Attributes: attributes,
		})
	}
	return out
}

// awsMetricNames returns discovered CloudWatch metric names. It uses the REST
// ListMetrics endpoint because CloudWatch's PromQL API does not implement the
// Prometheus label-values route used by Grafana's metric browser.
func (s *Service) awsMetricNames(ctx context.Context) []string {
	if !s.awsReady() {
		return nil
	}
	metrics, err := s.aws.ListMetrics(ctx, "", "")
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

// awsServiceInfos returns X-Ray services with their AWS partition metadata.
func (s *Service) awsServiceInfos(ctx context.Context) []awsstore.ServiceInfo {
	if !s.awsReady() {
		return nil
	}
	services, err := s.aws.ListServices(ctx)
	if err != nil {
		return nil
	}
	return services
}

// awsServices returns X-Ray service names.
func (s *Service) awsServices(ctx context.Context) []string {
	seen := make(map[string]struct{})
	for _, service := range s.awsServiceInfos(ctx) {
		if service.Name != "" {
			seen[service.Name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// awsZipkinTraces converts X-Ray trace summaries into single-span trace groups.
func (s *Service) awsZipkinTraces(ctx context.Context, service string, from, to time.Time, limit int) [][]zipkinSpan {
	if s.aws == nil || !s.aws.Enabled() {
		return nil
	}
	summaries, err := s.aws.QueryTraces(ctx, awsstore.TraceQuery{
		Service: service,
		From:    from,
		To:      to,
		Limit:   limit,
	})
	if err != nil {
		return nil
	}
	out := make([][]zipkinSpan, 0, len(summaries))
	for _, summary := range summaries {
		serviceName := firstNonEmpty(summary.Service, awsSourceLabel)
		duration := summary.Duration.Microseconds()
		if duration < 0 {
			duration = 0
		}
		tags := map[string]string{
			labelSource: awsSourceLabel,
			"status":    summary.Status,
		}
		applyAWSPartition(tags, summary.Region, summary.Account)
		out = append(out, []zipkinSpan{{
			TraceID:       summary.TraceID,
			ID:            summary.TraceID,
			Name:          firstNonEmpty(summary.Service, "xray"),
			Kind:          "SERVER",
			Timestamp:     summary.StartTime.UTC().UnixMicro(),
			Duration:      duration,
			LocalEndpoint: zipkinEndpoint{ServiceName: serviceName},
			Tags:          tags,
		}})
	}
	return out
}

// awsZipkinTraceByID fetches an X-Ray trace and converts it to Zipkin spans.
func (s *Service) awsZipkinTraceByID(ctx context.Context, traceID string) ([]zipkinSpan, bool) {
	if s.aws == nil || !s.aws.Enabled() {
		return nil, false
	}
	spans, err := s.aws.GetTrace(ctx, traceID)
	if err != nil || len(spans) == 0 {
		return nil, false
	}
	out := make([]zipkinSpan, 0, len(spans))
	for _, span := range spans {
		duration := span.EndTime.Sub(span.StartTime).Microseconds()
		if duration < 0 {
			duration = 0
		}
		tags := make(map[string]string, len(span.Attributes)+1)
		for key, value := range span.Attributes {
			if value == "" {
				continue
			}
			tags[key] = value
		}
		tags[labelSource] = awsSourceLabel
		applyAWSPartition(tags, span.Region, span.Account)
		out = append(out, zipkinSpan{
			TraceID:       span.TraceID,
			ID:            span.SpanID,
			ParentID:      span.ParentSpanID,
			Name:          span.Name,
			Timestamp:     span.StartTime.UTC().UnixMicro(),
			Duration:      duration,
			LocalEndpoint: zipkinEndpoint{ServiceName: firstNonEmpty(span.Service, awsSourceLabel)},
			Tags:          tags,
		})
	}
	return out, true
}
