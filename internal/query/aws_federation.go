package query

import (
	"context"
	"strings"
	"time"

	"github.com/radryc/monofs/pkg/authz/policy"
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
// it targets the AWS federated source. Failures become request warnings so
// native logs still return with a partial-result header.
func (s *Service) queryAWSLogsForLoki(ctx context.Context, tenant string, plan lokiPlan, from, to time.Time, limit int) []segment.LogRecord {
	records, err := proxy.AWSLogsForLoki(ctx, s.aws, tenant, logPushdown(plan), from, to, limit)
	if err != nil {
		proxy.AddWarning(ctx, proxy.Warning{Backend: "aws", Signal: proxy.SignalLogs, Message: err.Error()})
		return nil
	}
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

// awsPartitionVisible reports whether federated AWS metrics belong to the
// selected partition. Local partitions must not inherit every CloudWatch
// metric name in the catalog.
func (s *Service) awsPartitionVisible(ctx context.Context, partition string) bool {
	partition = strings.TrimSpace(partition)
	if partition == "" {
		return true
	}
	for _, svc := range s.awsServiceInfos(ctx) {
		if awsPartition(svc.Region, svc.Account) == partition {
			return true
		}
	}
	return false
}

// awsServiceInfos returns X-Ray services with their AWS partition metadata.
func (s *Service) awsServiceInfos(ctx context.Context) []awsstore.ServiceInfo {
	if !s.awsReady() {
		return nil
	}
	services, err := s.aws.ListServices(ctx)
	if err != nil {
		proxy.AddWarning(ctx, proxy.Warning{Backend: "aws", Signal: proxy.SignalTraces, Message: err.Error()})
		return nil
	}
	return services
}

// filterTraceServiceValues removes service names that belong only to AWS
// partitions the caller cannot see. Names shared by a visible partition are
// kept; this closes the service-picker leak without hiding native services.
func (s *Service) filterTraceServiceValues(ctx context.Context, values map[string]struct{}) {
	if s == nil || s.policyGate == nil || s.aws == nil {
		return
	}
	visible := make(map[string]struct{})
	hidden := make(map[string]struct{})
	for _, svc := range s.awsServiceInfos(ctx) {
		if svc.Name == "" {
			continue
		}
		_, allowed := s.policyPartitionAllowed(ctx, svc.Account, svc.Region,
			awsPartition(svc.Region, svc.Account), policy.SignalTraces)
		if allowed {
			visible[svc.Name] = struct{}{}
		} else {
			hidden[svc.Name] = struct{}{}
		}
	}
	for name := range hidden {
		if _, ok := visible[name]; ok {
			continue
		}
		delete(values, name)
	}
}

// awsServices returns X-Ray service names.
func (s *Service) awsServices(ctx context.Context) []string {
	services, err := proxy.AWSServices(ctx, s.aws)
	if err != nil {
		proxy.AddWarning(ctx, proxy.Warning{Backend: "aws", Signal: proxy.SignalTraces, Message: err.Error()})
		return nil
	}
	return services
}
