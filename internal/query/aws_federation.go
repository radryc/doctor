package query

import (
	"context"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

// awsSourceLabel marks federated CloudWatch/X-Ray records.
const awsSourceLabel = proxy.SourceAWS

const (
	// attrAWSRegion / attrAWSAccount expose the AWS partition an X-Ray/CloudWatch
	// record came from. awsPartitionLabel is the canonical, filterable partition
	// shared with native telemetry.
	attrAWSRegion     = proxy.AttrAWSRegion
	attrAWSAccount    = proxy.AttrAWSAccount
	awsPartitionLabel = proxy.AWSPartitionLabel
)

// awsReady reports whether the AWS federation backend is configured and enabled.
func (s *Service) awsReady() bool {
	return s.aws != nil && s.aws.Enabled()
}

// awsPartition derives a partition identifier from an AWS region and account so
// federated records can be grouped in the same partition filters as native
// telemetry. Region is preferred because it is always present.
func awsPartition(region, account string) string {
	return proxy.AWSPartition(region, account)
}

// queryAWSLogsForLoki executes a parsed LogQL plan against CloudWatch Logs when
// it targets the AWS federated source. Errors are swallowed so native logs still
// return.
func (s *Service) queryAWSLogsForLoki(ctx context.Context, tenant string, plan lokiPlan, from, to time.Time, limit int) []segment.LogRecord {
	return proxy.AWSLogsForLoki(ctx, s.aws, tenant, logPushdown(plan), from, to, limit)
}

// logPushdown reduces a parsed LogQL plan to the selector plus line filters
// understood by non-native backends.
func logPushdown(plan lokiPlan) proxy.LogPushdown {
	filters := make([]proxy.LogLineFilter, 0, len(plan.lineFilters))
	for _, filter := range plan.lineFilters {
		filters = append(filters, proxy.LogLineFilter{Op: filter.op, Value: filter.value})
	}
	return proxy.LogPushdown{Selector: plan.selector, LineFilters: filters}
}

// awsMetricNames returns discovered CloudWatch metric names. It uses the REST
// ListMetrics endpoint because CloudWatch's PromQL API does not implement the
// Prometheus label-values route used by Grafana's metric browser.
func (s *Service) awsMetricNames(ctx context.Context) []string {
	return proxy.AWSMetricNames(ctx, s.aws)
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
	return proxy.AWSServices(ctx, s.aws)
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
		proxy.ApplyAWSPartition(tags, summary.Region, summary.Account)
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
		proxy.ApplyAWSPartition(tags, span.Region, span.Account)
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
