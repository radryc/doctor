package federation

import (
	"context"
	"time"

	proxy "github.com/radryc/doctor/internal/proxy"
	awsstore "github.com/radryc/doctor/internal/store/aws"
	gcpstore "github.com/radryc/doctor/internal/store/gcp"
)

// gcpAsAWS adapts the GCP read client to proxy.AWSClient so the existing
// federation manager (planner, fan-out, circuit breakers, merge) can serve GCP
// targets without a second implementation.
type gcpAsAWS struct {
	client *gcpstore.Client
}

func newGCPAsAWS(client *gcpstore.Client) proxy.AWSClient {
	return &gcpAsAWS{client: client}
}

func (c *gcpAsAWS) Enabled() bool { return c.client != nil && c.client.Enabled() }

func (c *gcpAsAWS) ListMetrics(ctx context.Context, namespace, metric string) ([]awsstore.MetricDescriptor, error) {
	descriptors, err := c.client.ListMetrics(ctx, namespace, metric)
	if err != nil {
		return nil, err
	}
	out := make([]awsstore.MetricDescriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		out = append(out, awsstore.MetricDescriptor{
			Namespace:  descriptor.Namespace,
			MetricName: descriptor.MetricName,
			Dimensions: descriptor.Dimensions,
			Region:     descriptor.Region,
			Account:    descriptor.Project,
		})
	}
	return out, nil
}

func (c *gcpAsAWS) ListServices(ctx context.Context) ([]awsstore.ServiceInfo, error) {
	services, err := c.client.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]awsstore.ServiceInfo, 0, len(services))
	for _, service := range services {
		out = append(out, awsstore.ServiceInfo{Name: service.Name, Region: service.Region, Account: service.Project})
	}
	return out, nil
}

func (c *gcpAsAWS) ListLogGroups(ctx context.Context) ([]string, error) {
	return c.client.ListLogNames(ctx)
}

func (c *gcpAsAWS) QueryLogs(ctx context.Context, q awsstore.LogQuery) ([]awsstore.LogRecord, error) {
	logName := ""
	if len(q.LogGroups) > 0 {
		logName = q.LogGroups[0]
	}
	records, err := c.client.QueryLogs(ctx, gcpstore.LogQuery{
		LogName: logName,
		Query:   q.Query,
		From:    q.From,
		To:      q.To,
		Limit:   q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]awsstore.LogRecord, 0, len(records))
	for _, record := range records {
		out = append(out, awsstore.LogRecord{
			Timestamp: record.Timestamp,
			LogGroup:  record.LogGroup,
			LogStream: record.LogStream,
			Message:   record.Message,
			Fields:    record.Fields,
			Region:    record.Region,
			Account:   record.Project,
		})
	}
	return out, nil
}

func (c *gcpAsAWS) QueryTraces(ctx context.Context, q awsstore.TraceQuery) ([]awsstore.TraceSummary, error) {
	summaries, err := c.client.QueryTraces(ctx, gcpstore.TraceQuery{
		Service: q.Service,
		From:    q.From,
		To:      q.To,
		Limit:   q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]awsstore.TraceSummary, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, awsstore.TraceSummary{
			TraceID:    summary.TraceID,
			Service:    summary.Service,
			StartTime:  summary.StartTime,
			EndTime:    summary.EndTime,
			Duration:   summary.Duration,
			Status:     summary.Status,
			SpanCount:  summary.SpanCount,
			Attributes: summary.Attributes,
			Region:     summary.Region,
			Account:    summary.Project,
		})
	}
	return out, nil
}

func (c *gcpAsAWS) GetTrace(ctx context.Context, traceID string) ([]awsstore.Span, error) {
	spans, err := c.client.GetTrace(ctx, traceID)
	if err != nil {
		return nil, err
	}
	out := make([]awsstore.Span, 0, len(spans))
	for _, span := range spans {
		out = append(out, awsstore.Span{
			TraceID:      span.TraceID,
			SpanID:       span.SpanID,
			ParentSpanID: span.ParentSpanID,
			Name:         span.Name,
			Service:      span.Service,
			StartTime:    span.StartTime,
			EndTime:      span.EndTime,
			StatusCode:   span.StatusCode,
			Attributes:   span.Attributes,
			Region:       span.Region,
			Account:      span.Project,
		})
	}
	return out, nil
}

func (c *gcpAsAWS) PrometheusQuery(ctx context.Context, query string, at time.Time) (map[string]any, error) {
	return c.client.PrometheusQuery(ctx, query, at)
}

func (c *gcpAsAWS) PrometheusQueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	return c.client.PrometheusQueryRange(ctx, query, start, end, step)
}

func (c *gcpAsAWS) PrometheusSeries(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	return c.client.PrometheusSeries(ctx, match, start, end)
}

func (c *gcpAsAWS) PrometheusLabels(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	return c.client.PrometheusLabels(ctx, match, start, end)
}

func (c *gcpAsAWS) PrometheusLabelValues(ctx context.Context, label string, match []string, start, end time.Time) (map[string]any, error) {
	return c.client.PrometheusLabelValues(ctx, label, match, start, end)
}

// gcpTargetTokenSource adapts a federation TokenResolver to gcpstore.TokenSource.
type gcpTargetTokenSource struct {
	resolver TokenResolver
	target   Target
}

func (s gcpTargetTokenSource) Token(ctx context.Context) (string, error) {
	return s.resolver.Resolve(ctx, s.target)
}

func (s gcpTargetTokenSource) Invalidate() {
	s.resolver.Invalidate(s.target)
}

// gcpClientAdapter adapts the federation manager (proxy.AWSClient) to
// proxy.GCPClient so the router's GCP backend can use the catalog manager.
type gcpClientAdapter struct {
	client proxy.AWSClient
}

// NewGCPClientAdapter exposes a federation manager as a proxy.GCPClient.
func NewGCPClientAdapter(client proxy.AWSClient) proxy.GCPClient {
	return &gcpClientAdapter{client: client}
}

func (a *gcpClientAdapter) Enabled() bool { return a.client != nil && a.client.Enabled() }

func (a *gcpClientAdapter) ListMetrics(ctx context.Context, namespace, metric string) ([]gcpstore.MetricDescriptor, error) {
	descriptors, err := a.client.ListMetrics(ctx, namespace, metric)
	if err != nil {
		return nil, err
	}
	out := make([]gcpstore.MetricDescriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		out = append(out, gcpstore.MetricDescriptor{
			Namespace:  descriptor.Namespace,
			MetricName: descriptor.MetricName,
			Dimensions: descriptor.Dimensions,
			Region:     descriptor.Region,
			Project:    descriptor.Account,
		})
	}
	return out, nil
}

func (a *gcpClientAdapter) QueryMetric(context.Context, gcpstore.MetricQuery) ([]gcpstore.MetricSeries, error) {
	// The GCP upstream uses PromQL through PrometheusQuery/Range; the REST
	// metric query is only used by the single-pusher client.
	return nil, nil
}

func (a *gcpClientAdapter) ListServices(ctx context.Context) ([]gcpstore.ServiceInfo, error) {
	services, err := a.client.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]gcpstore.ServiceInfo, 0, len(services))
	for _, service := range services {
		out = append(out, gcpstore.ServiceInfo{Name: service.Name, Region: service.Region, Project: service.Account})
	}
	return out, nil
}

func (a *gcpClientAdapter) ListLogNames(ctx context.Context) ([]string, error) {
	return a.client.ListLogGroups(ctx)
}

func (a *gcpClientAdapter) QueryLogs(ctx context.Context, q gcpstore.LogQuery) ([]gcpstore.LogRecord, error) {
	var groups []string
	if q.LogName != "" {
		groups = []string{q.LogName}
	}
	records, err := a.client.QueryLogs(ctx, awsstore.LogQuery{
		LogGroups: groups,
		Query:     q.Query,
		From:      q.From,
		To:        q.To,
		Limit:     q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]gcpstore.LogRecord, 0, len(records))
	for _, record := range records {
		out = append(out, gcpstore.LogRecord{
			Timestamp: record.Timestamp,
			LogGroup:  record.LogGroup,
			LogStream: record.LogStream,
			Message:   record.Message,
			Fields:    record.Fields,
			Region:    record.Region,
			Project:   record.Account,
		})
	}
	return out, nil
}

func (a *gcpClientAdapter) QueryTraces(ctx context.Context, q gcpstore.TraceQuery) ([]gcpstore.TraceSummary, error) {
	summaries, err := a.client.QueryTraces(ctx, awsstore.TraceQuery{
		Service: q.Service,
		From:    q.From,
		To:      q.To,
		Limit:   q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]gcpstore.TraceSummary, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, gcpstore.TraceSummary{
			TraceID:    summary.TraceID,
			Service:    summary.Service,
			StartTime:  summary.StartTime,
			EndTime:    summary.EndTime,
			Duration:   summary.Duration,
			Status:     summary.Status,
			SpanCount:  summary.SpanCount,
			Attributes: summary.Attributes,
			Region:     summary.Region,
			Project:    summary.Account,
		})
	}
	return out, nil
}

func (a *gcpClientAdapter) GetTrace(ctx context.Context, traceID string) ([]gcpstore.Span, error) {
	spans, err := a.client.GetTrace(ctx, traceID)
	if err != nil {
		return nil, err
	}
	out := make([]gcpstore.Span, 0, len(spans))
	for _, span := range spans {
		out = append(out, gcpstore.Span{
			TraceID:      span.TraceID,
			SpanID:       span.SpanID,
			ParentSpanID: span.ParentSpanID,
			Name:         span.Name,
			Service:      span.Service,
			StartTime:    span.StartTime,
			EndTime:      span.EndTime,
			StatusCode:   span.StatusCode,
			Attributes:   span.Attributes,
			Region:       span.Region,
			Project:      span.Account,
		})
	}
	return out, nil
}

func (a *gcpClientAdapter) PrometheusQuery(ctx context.Context, query string, at time.Time) (map[string]any, error) {
	return a.client.PrometheusQuery(ctx, query, at)
}

func (a *gcpClientAdapter) PrometheusQueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (map[string]any, error) {
	return a.client.PrometheusQueryRange(ctx, query, start, end, step)
}

func (a *gcpClientAdapter) PrometheusSeries(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	return a.client.PrometheusSeries(ctx, match, start, end)
}

func (a *gcpClientAdapter) PrometheusLabels(ctx context.Context, match []string, start, end time.Time) (map[string]any, error) {
	return a.client.PrometheusLabels(ctx, match, start, end)
}

func (a *gcpClientAdapter) PrometheusLabelValues(ctx context.Context, label string, match []string, start, end time.Time) (map[string]any, error) {
	return a.client.PrometheusLabelValues(ctx, label, match, start, end)
}
