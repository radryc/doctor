package monofs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// LogEngineClient wraps the MonoFSRouter gRPC client specifically for
// Doctor telemetry ingest and query operations (logs, metrics, traces).
// It talks directly to the monofs router which forwards to the best healthy node.
type LogEngineClient struct {
	conn   *grpc.ClientConn
	router pb.MonoFSRouterClient
}

// NewLogEngineClient dials the monofs router and returns a client.
func NewLogEngineClient(ctx context.Context, routerAddr string) (*LogEngineClient, error) {
	if routerAddr == "" {
		return nil, fmt.Errorf("monofs router address is required")
	}
	conn, err := grpc.NewClient(
		routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256*1024*1024),
			grpc.MaxCallSendMsgSize(256*1024*1024),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to monofs router: %w", err)
	}
	return &LogEngineClient{
		conn:   conn,
		router: pb.NewMonoFSRouterClient(conn),
	}, nil
}

// Close tears down the gRPC connection.
func (c *LogEngineClient) Close() error {
	return c.conn.Close()
}

// IngestLogs sends log records to the monofs logengine.
func (c *LogEngineClient) IngestLogs(ctx context.Context, chunkID string, records []segment.LogRecord) error {
	entries := make([]*pb.LogEntry, 0, len(records))
	for _, r := range records {
		entries = append(entries, &pb.LogEntry{
			TimestampUnixNano: r.Timestamp.UnixNano(),
			Level:             r.SeverityText,
			Service:           r.Service,
			TraceId:           r.TraceID,
			RawMessage:        r.Body,
		})
	}
	_, err := c.router.IngestLogs(ctx, &pb.IngestLogsRequest{
		ChunkId: chunkID,
		Logs:    entries,
	})
	return err
}

// IngestMetrics sends metric records to the monofs logengine.
func (c *LogEngineClient) IngestMetrics(ctx context.Context, chunkID string, records []segment.MetricPointRecord) error {
	entries := make([]*pb.MetricEntry, 0, len(records))
	for _, r := range records {
		entries = append(entries, &pb.MetricEntry{
			TimestampUnixNano: r.Timestamp.UnixNano(),
			Service:           r.Service,
			MetricName:        r.MetricName,
			Value:             r.Value,
			Labels:            mergeMetricLabels(r.ResourceAttributes, r.Attributes, r.TraceID, r.SpanID),
		})
	}
	_, err := c.router.IngestMetrics(ctx, &pb.IngestMetricsRequest{
		ChunkId: chunkID,
		Metrics: entries,
	})
	return err
}

// IngestTraces sends trace spans to the monofs logengine.
func (c *LogEngineClient) IngestTraces(ctx context.Context, chunkID string, records []segment.TraceRecord) error {
	entries := make([]*pb.SpanEntry, 0, len(records))
	for _, r := range records {
		entries = append(entries, &pb.SpanEntry{
			StartUnixNano: r.StartTime.UnixNano(),
			EndUnixNano:   r.EndTime.UnixNano(),
			TraceId:       r.TraceID,
			SpanId:        r.SpanID,
			ParentSpanId:  r.ParentSpanID,
			Service:       r.Service,
			Name:          r.Name,
			StatusCode:    r.StatusCode,
			Attributes:    r.Attributes,
		})
	}
	_, err := c.router.IngestTraces(ctx, &pb.IngestTracesRequest{
		ChunkId: chunkID,
		Spans:   entries,
	})
	return err
}

// QueryLogs executes a LogQL query and returns matching log records.
// The tenant parameter is ignored by monofs (it uses internal partitioning).
func (c *LogEngineClient) QueryLogs(ctx context.Context, tenant, query, service string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	resp, err := c.router.QueryLogs(ctx, &pb.QueryLogsRequest{
		Query:        buildLogQL(service, query),
		Service:      service,
		FromUnixNano: timeToNano(from),
		ToUnixNano:   timeToNano(to),
		Limit:        int32(limit),
	})
	if err != nil {
		return nil, err
	}
	var records []segment.LogRecord
	if err := json.Unmarshal(resp.ResultsJson, &records); err != nil {
		return nil, fmt.Errorf("unmarshal log results: %w", err)
	}
	return records, nil
}

// QueryLogsRaw executes a full LogQL expression and returns matching log records.
func (c *LogEngineClient) QueryLogsRaw(ctx context.Context, tenant, query string, from, to time.Time, limit int) ([]segment.LogRecord, error) {
	resp, err := c.router.QueryLogs(ctx, &pb.QueryLogsRequest{
		Query:        normalizeRawLogQL(query),
		FromUnixNano: timeToNano(from),
		ToUnixNano:   timeToNano(to),
		Limit:        int32(limit),
	})
	if err != nil {
		return nil, err
	}
	var records []segment.LogRecord
	if err := json.Unmarshal(resp.ResultsJson, &records); err != nil {
		return nil, fmt.Errorf("unmarshal log results: %w", err)
	}
	return records, nil
}

// QueryMetrics returns metric data points for the given query and time range.
// The tenant parameter is ignored by monofs (it uses internal partitioning).
func (c *LogEngineClient) QueryMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time) ([]segment.MetricPointRecord, error) {
	stream, err := c.router.StreamQueryMetrics(ctx, &pb.QueryMetricsRequest{
		MetricName:    query.MetricName,
		FromUnixNano:  timeToNano(from),
		ToUnixNano:    timeToNano(to),
		Service:       query.Service,
		LabelMatchers: protoMetricMatchers(query.LabelMatchers),
	})
	if err != nil {
		return nil, err
	}
	rawRecords, err := unmarshalQueryItems[metricStreamRecord](stream)
	if err != nil {
		return nil, fmt.Errorf("unmarshal metric results: %w", err)
	}
	records := make([]segment.MetricPointRecord, 0, len(rawRecords))
	for _, record := range rawRecords {
		labels := flattenAttrs(record.Labels)
		records = append(records, segment.MetricPointRecord{
			Tenant:     tenant,
			Service:    record.Service,
			MetricName: record.MetricName,
			Timestamp:  record.Timestamp.UTC(),
			TraceID:    labels["trace_id"],
			SpanID:     labels["span_id"],
			Attributes: labels,
			HasValue:   true,
			Value:      record.Value,
		})
	}
	return records, nil
}

func protoMetricMatchers(matchers []segment.MetricLabelMatcher) []*pb.MetricLabelMatcher {
	if len(matchers) == 0 {
		return nil
	}
	out := make([]*pb.MetricLabelMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		out = append(out, &pb.MetricLabelMatcher{
			Name:  matcher.Name,
			Value: matcher.Value,
			Type:  protoMetricMatcherType(matcher.Type),
		})
	}
	return out
}

func protoMetricMatcherType(matchType segment.MetricMatchType) pb.MetricLabelMatcherType {
	switch matchType {
	case segment.MetricMatchNotEqual:
		return pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_NOT_EQUAL
	case segment.MetricMatchRegexp:
		return pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_REGEXP
	case segment.MetricMatchNotRegexp:
		return pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_NOT_REGEXP
	default:
		return pb.MetricLabelMatcherType_METRIC_LABEL_MATCHER_TYPE_EQUAL
	}
}

// QueryTraces returns trace spans matching traceID and/or service in the time range.
// The tenant parameter is ignored by monofs (it uses internal partitioning).
func (c *LogEngineClient) QueryTraces(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int) ([]segment.TraceRecord, error) {
	stream, err := c.router.StreamQueryTraces(ctx, &pb.QueryTracesRequest{
		TraceId:      traceID,
		Service:      service,
		FromUnixNano: timeToNano(from),
		ToUnixNano:   timeToNano(to),
		Limit:        int32(limit),
	})
	if err != nil {
		return nil, err
	}
	records, err := unmarshalQueryItems[segment.TraceRecord](stream)
	if err != nil {
		return nil, fmt.Errorf("unmarshal trace results: %w", err)
	}
	return records, nil
}

type metricStreamRecord struct {
	Timestamp  time.Time         `json:"Timestamp"`
	Service    string            `json:"Service"`
	MetricName string            `json:"MetricName"`
	Value      float64           `json:"Value"`
	Labels     map[string]string `json:"Labels"`
}

func unmarshalQueryItems[T any](stream grpc.ServerStreamingClient[pb.QueryResultItem]) ([]T, error) {
	items := make([]T, 0)
	for {
		item, err := stream.Recv()
		if err == io.EOF {
			return items, nil
		}
		if err != nil {
			return nil, err
		}
		if item == nil || len(item.GetItemJson()) == 0 {
			continue
		}

		var decoded T
		if err := json.Unmarshal(item.GetItemJson(), &decoded); err != nil {
			return nil, err
		}
		items = append(items, decoded)
	}
}

func timeToNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func flattenAttrs(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func mergeMetricLabels(resourceAttributes, pointAttributes map[string]string, traceID, spanID string) map[string]string {
	size := len(resourceAttributes) + len(pointAttributes)
	if traceID != "" {
		size++
	}
	if spanID != "" {
		size++
	}
	if size == 0 {
		return map[string]string{}
	}
	merged := make(map[string]string, size)
	for key, value := range resourceAttributes {
		if value == "" {
			continue
		}
		merged[key] = value
	}
	for key, value := range pointAttributes {
		if value == "" {
			continue
		}
		merged[key] = value
	}
	if traceID != "" {
		merged["trace_id"] = traceID
	}
	if spanID != "" {
		merged["span_id"] = spanID
	}
	return merged
}

// buildLogQL constructs a valid LogQL expression from optional service and text filter.
// service is embedded in the stream selector; textFilter becomes a line filter.
// When no service is given, {service=~".+"} selects all labelled streams.
func buildLogQL(service, textFilter string) string {
	selector := `{service=~".+"}`
	if service != "" {
		selector = fmt.Sprintf(`{service=%q}`, service)
	}
	if textFilter != "" {
		return fmt.Sprintf(`%s |= %q`, selector, textFilter)
	}
	return selector
}

func normalizeRawLogQL(query string) string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return `{service=~".+"}`
	}
	return trimmed
}
