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
	records := proxy.AWSLogsForLoki(ctx, s.aws, tenant, logPushdown(plan), from, to, limit)
	if s.policyGate != nil {
		records = s.policyGate.FilterLogs(identityFromContext(ctx), records)
	}
	return records
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
