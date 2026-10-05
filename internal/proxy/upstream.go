package proxy

import (
	"context"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

// MetricsUpstream executes Prometheus-compatible queries against one source.
// Implementations must be safe for concurrent use.
type MetricsUpstream interface {
	// Name returns the backend name used for source/shard labeling.
	Name() string
	// PromInstant executes an instant PromQL query.
	PromInstant(ctx context.Context, tenant, query string, at time.Time) (map[string]any, error)
	// PromRange executes a range PromQL query.
	PromRange(ctx context.Context, tenant, query string, start, end time.Time, step time.Duration) (map[string]any, error)
	// PromSeries returns matching series label sets.
	PromSeries(ctx context.Context, tenant string, match []string, start, end time.Time) ([]map[string]string, error)
	// PromLabels returns label names.
	PromLabels(ctx context.Context, tenant string, match []string, start, end time.Time) ([]string, error)
	// PromLabelValues returns values for one label.
	PromLabelValues(ctx context.Context, tenant, label string, match []string, start, end time.Time) ([]string, error)
}

// LogsUpstream executes a reduced LogQL query (selector plus line filters)
// against one source. Parser stages, label filters, and metric aggregation are
// applied locally by Doctor after merge.
type LogsUpstream interface {
	Name() string
	QueryLogs(ctx context.Context, tenant string, pushdown LogPushdown, from, to time.Time, limit int) ([]segment.LogRecord, error)
}

// TracesUpstream executes trace discovery and lookup against one source.
type TracesUpstream interface {
	Name() string
	ListServices(ctx context.Context, tenant string) ([]string, error)
	SearchTraces(ctx context.Context, tenant string, search TraceSearch) ([]TraceSummary, error)
	GetTrace(ctx context.Context, tenant, traceID string) ([]segment.TraceRecord, bool, error)
}

// TraceTagSource is implemented by upstreams that expose tag discovery.
type TraceTagSource interface {
	TraceTags(ctx context.Context, tenant string) ([]string, error)
	TraceTagValues(ctx context.Context, tenant, tag string) ([]string, error)
}

// TraceQLUpstream is implemented by upstreams that can execute TraceQL natively.
type TraceQLUpstream interface {
	SearchTraceQL(ctx context.Context, tenant, query string, from, to time.Time, limit int) ([]TraceSummary, error)
}

// TraceSearch describes a trace search request.
type TraceSearch struct {
	Service     string
	Tags        []TraceTag
	TraceQL     string
	From        time.Time
	To          time.Time
	Limit       int
	MinDuration time.Duration
	MaxDuration time.Duration
}

// TraceTag is an exact-match tag filter.
type TraceTag struct {
	Key   string
	Value string
}

// TraceSummary is a source-agnostic trace search hit.
type TraceSummary struct {
	TraceID         string
	RootServiceName string
	RootTraceName   string
	Service         string
	StartTime       time.Time
	Duration        time.Duration
	SpanCount       int
	Region          string
	Account         string
	Status          string
}

// LogPushdown is the storage subset of a parsed LogQL query: a stream selector
// plus line filters. Backends that cannot evaluate parser stages receive only
// this reduced query.
type LogPushdown struct {
	Selector    string
	LineFilters []LogLineFilter
}

// LogLineFilter is one LogQL line filter (|=, !=, |~, !~).
type LogLineFilter struct {
	Op    string
	Value string
}

// AWSClient is the read surface Doctor federates from guardian-pusher-aws.
// It is implemented by internal/store/aws.Client and by test fakes.
type AWSClient interface {
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
