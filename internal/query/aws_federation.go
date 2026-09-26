package query

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

// awsSourceLabel marks federated CloudWatch/X-Ray records.
const awsSourceLabel = "aws"

// awsBackend is the read surface Doctor federates from guardian-pusher-aws.
type awsBackend interface {
	Enabled() bool
	ListServices(ctx context.Context) ([]string, error)
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

	insights := "fields @timestamp, @message, @log_stream, @log_group"
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
		out = append(out, segment.LogRecord{
			Tenant:     tenant,
			Service:    firstNonEmpty(record.Fields["service"], record.Fields["service_name"], awsSourceLabel),
			Timestamp:  record.Timestamp,
			Body:       record.Message,
			Attributes: attributes,
		})
	}
	return out
}

// awsServices returns X-Ray service names.
func (s *Service) awsServices(ctx context.Context) []string {
	if s.aws == nil || !s.aws.Enabled() {
		return nil
	}
	services, err := s.aws.ListServices(ctx)
	if err != nil {
		return nil
	}
	return services
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
		out = append(out, []zipkinSpan{{
			TraceID:       summary.TraceID,
			ID:            summary.TraceID,
			Name:          firstNonEmpty(summary.Service, "xray"),
			Kind:          "SERVER",
			Timestamp:     summary.StartTime.UTC().UnixMicro(),
			Duration:      duration,
			LocalEndpoint: zipkinEndpoint{ServiceName: serviceName},
			Tags: map[string]string{
				labelSource: awsSourceLabel,
				"status":    summary.Status,
			},
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
