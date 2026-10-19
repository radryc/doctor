package query

import (
	"context"
	"fmt"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"github.com/rydzu/ainfra/doctor/internal/access"
	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// policyBackend decorates the logengine backend with access-policy filtering.
// Metrics pass through untouched; logs and traces are filtered per partition
// using the identity attached to the request context, and AWS-federated
// records are canonicalized to their bound partition first. It is installed
// by Service.SetPolicyGate and is a transparent passthrough elsewhere.
type policyBackend struct {
	inner LogEngineBackend
	gate  *access.PolicyGate
}

func identityFromContext(ctx context.Context) authz.Identity {
	id, _ := authz.IdentityFromContext(ctx)
	return id
}

// QueryLogs filters log records by the caller's log partitions.
func (b *policyBackend) QueryLogs(ctx context.Context, tenant, query, service string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	records, err := b.inner.QueryLogs(ctx, tenant, query, service, from, to, limit)
	if err != nil {
		return records, err
	}
	return b.gate.FilterLogs(identityFromContext(ctx), records), nil
}

// QueryLogsRaw executes a full LogQL expression and filters the result. The
// proxy's local logs upstream prefers this method when available; without it,
// the LogQL selector is passed as a text filter to QueryLogs and matches
// nothing.
func (b *policyBackend) QueryLogsRaw(ctx context.Context, tenant, query string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	raw, ok := b.inner.(rawLogQueryBackend)
	if !ok {
		return nil, fmt.Errorf("log backend does not support raw queries")
	}
	records, err := raw.QueryLogsRaw(ctx, tenant, query, from, to, limit)
	if err != nil {
		return records, err
	}
	return b.gate.FilterLogs(identityFromContext(ctx), records), nil
}

// QueryMetrics is never filtered: metrics are visible to everyone.
func (b *policyBackend) QueryMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time) ([]segment.MetricPointRecord, error) {
	return b.inner.QueryMetrics(ctx, tenant, query, from, to)
}

// StreamMetrics is never filtered.
func (b *policyBackend) StreamMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricPointRecord) error) error {
	return b.inner.StreamMetrics(ctx, tenant, query, from, to, yield)
}

// StreamMetricSeries is never filtered.
func (b *policyBackend) StreamMetricSeries(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricSeriesMeta) error) error {
	return b.inner.StreamMetricSeries(ctx, tenant, query, from, to, yield)
}

// QueryTraces filters spans by the caller's trace partitions (span-level: a
// trace remains visible while at least one span survives).
func (b *policyBackend) QueryTraces(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int) ([]segment.TraceRecord, error) {
	spans, err := b.inner.QueryTraces(ctx, tenant, traceID, service, from, to, limit)
	if err != nil {
		return spans, err
	}
	return b.gate.FilterSpans(identityFromContext(ctx), spans), nil
}

// AWSUpstream adapts the AWS client with the same access-policy filters as
// native telemetry, so federated CloudWatch/X-Ray records are canonicalized to
// their bound partition and filtered per caller.
func (s *Service) AWSUpstream() *proxy.AWSUpstream {
	up := &proxy.AWSUpstream{Client: s.aws}
	gate := s.policyGate
	if gate == nil || s.aws == nil {
		return up
	}
	up.FilterLogs = func(ctx context.Context, records []segment.LogRecord) []segment.LogRecord {
		return gate.FilterLogs(identityFromContext(ctx), records)
	}
	up.FilterSpans = func(ctx context.Context, spans []segment.TraceRecord) []segment.TraceRecord {
		return gate.FilterSpans(identityFromContext(ctx), spans)
	}
	up.FilterSummaries = func(ctx context.Context, summaries []proxy.TraceSummary) []proxy.TraceSummary {
		id := identityFromContext(ctx)
		kept := make([]proxy.TraceSummary, 0, len(summaries))
		for _, summary := range summaries {
			partition := gate.CanonicalPartition(summary.Account, summary.Region, summary.Region)
			if gate.PartitionVisible(id, partition, policy.SignalTraces) {
				kept = append(kept, summary)
			}
		}
		return kept
	}
	return up
}

// policyPartitionAllowed canonicalizes an AWS record partition and reports
// whether the caller may see it.
func (s *Service) policyPartitionAllowed(ctx context.Context, account, region, current string, signal policy.Signal) (string, bool) {
	if s.policyGate == nil {
		return current, true
	}
	canonical := s.policyGate.CanonicalPartition(account, region, current)
	return canonical, s.policyGate.PartitionVisible(identityFromContext(ctx), canonical, signal)
}
