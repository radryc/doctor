package monofs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/doctor/internal/segment"
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
			SpanId:            r.SpanID,
			RawMessage:        r.Body,
			Labels: mergeLabels(
				r.ResourceAttributes,
				identityLabels(r.Partition, r.Intent, r.Asset),
				r.Attributes,
				correlationLabels(r.TraceID, r.SpanID),
			),
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
			Labels: filterUnboundedMetricLabels(mergeLabels(
				r.ResourceAttributes,
				identityLabels(r.Partition, r.Intent, r.Asset),
				r.Attributes,
			)),
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
			Attributes: mergeLabels(
				r.ResourceAttributes,
				identityLabels(r.Partition, r.Intent, r.Asset),
				r.Attributes,
			),
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
	records := make([]segment.MetricPointRecord, 0)
	err := c.StreamMetrics(ctx, tenant, query, from, to, func(record segment.MetricPointRecord) error {
		records = append(records, record)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// StreamMetrics streams metric data points for the given query and time range,
// invoking yield for every record as it arrives. Unlike QueryMetrics it never
// buffers the full result set, so callers that only need per-record metadata
// (label/series discovery) stay bounded regardless of the number of samples.
// The tenant parameter is ignored by monofs (it uses internal partitioning).
func (c *LogEngineClient) StreamMetrics(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricPointRecord) error) error {
	stream, err := c.router.StreamQueryMetrics(ctx, &pb.QueryMetricsRequest{
		MetricName:    query.MetricName,
		FromUnixNano:  timeToNano(from),
		ToUnixNano:    timeToNano(to),
		Service:       query.Service,
		LabelMatchers: protoMetricMatchers(query.LabelMatchers),
		BatchSize:     queryResultBatchSize,
	})
	if err != nil {
		return err
	}
	for {
		item, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if item == nil {
			continue
		}
		for _, raw := range queryResultRecords(item) {
			var record metricStreamRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("unmarshal metric results: %w", err)
			}
			if err := yield(metricPointFromStreamRecord(tenant, record)); err != nil {
				return err
			}
		}
	}
}

// queryResultBatchSize is the number of records requested per streamed message.
// Batching cuts the per-record gRPC overhead that otherwise dominates wide
// metric scans.
const queryResultBatchSize = 512

// StreamMetricSeries streams per-series metadata (identity plus first/last
// sample time) from the storage chunk series index without scanning samples.
func (c *LogEngineClient) StreamMetricSeries(ctx context.Context, tenant string, query segment.MetricQuery, from, to time.Time, yield func(segment.MetricSeriesMeta) error) error {
	stream, err := c.router.StreamMetricSeries(ctx, &pb.QueryMetricsRequest{
		MetricName:    query.MetricName,
		FromUnixNano:  timeToNano(from),
		ToUnixNano:    timeToNano(to),
		Service:       query.Service,
		LabelMatchers: protoMetricMatchers(query.LabelMatchers),
	})
	if err != nil {
		return err
	}
	for {
		item, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if item == nil {
			continue
		}
		if err := yield(metricSeriesMetaFromProto(tenant, item)); err != nil {
			return err
		}
	}
}

func metricSeriesMetaFromProto(tenant string, item *pb.MetricSeriesItem) segment.MetricSeriesMeta {
	labels := item.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	partition, intent, asset := labelsFromIdentity(labels)
	return segment.MetricSeriesMeta{
		Tenant:     tenant,
		Service:    item.GetService(),
		Partition:  partition,
		Intent:     intent,
		Asset:      asset,
		MetricName: item.GetMetricName(),
		Labels:     labels,
		MinTime:    time.Unix(0, item.GetMinTimeUnixNano()).UTC(),
		MaxTime:    time.Unix(0, item.GetMaxTimeUnixNano()).UTC(),
	}
}

func metricPointFromStreamRecord(tenant string, record metricStreamRecord) segment.MetricPointRecord {
	labels := record.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	partition, intent, asset := labelsFromIdentity(labels)
	return segment.MetricPointRecord{
		Tenant:     tenant,
		Service:    record.Service,
		Partition:  partition,
		Intent:     intent,
		Asset:      asset,
		MetricName: record.MetricName,
		Timestamp:  record.Timestamp.UTC(),
		TraceID:    labels["trace_id"],
		SpanID:     labels["span_id"],
		Attributes: labels,
		HasValue:   true,
		Value:      record.Value,
	}
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

// QueryTraceDiscovery returns bounded trace discovery metadata (services,
// tag names, or values for one tag) from MonoFS chunk manifests.
func (c *LogEngineClient) QueryTraceDiscovery(ctx context.Context, tenant, discovery, tag string, from, to time.Time) ([]string, error) {
	_ = tenant
	stream, err := c.router.StreamQueryTraces(ctx, &pb.QueryTracesRequest{
		FromUnixNano: timeToNano(from),
		ToUnixNano:   timeToNano(to),
		Discovery:    discovery,
		DiscoveryTag: tag,
	})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	for {
		item, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return nil, recvErr
		}
		if item == nil {
			continue
		}
		for _, raw := range queryResultRecords(item) {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, fmt.Errorf("decode trace discovery value: %w", err)
			}
			if value != "" {
				seen[value] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
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
		BatchSize:    queryResultBatchSize,
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
		if item == nil {
			continue
		}
		for _, raw := range queryResultRecords(item) {
			var decoded T
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return nil, err
			}
			items = append(items, decoded)
		}
	}
}

// queryResultRecords unwraps a stream item into its record payloads, accepting
// both batched (items_json) and legacy single-record (item_json) messages.
func queryResultRecords(item *pb.QueryResultItem) [][]byte {
	if records := item.GetItemsJson(); len(records) > 0 {
		return records
	}
	if raw := item.GetItemJson(); len(raw) > 0 {
		return [][]byte{raw}
	}
	return nil
}

func timeToNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// mergeLabels flattens several label maps into one, later maps winning and
// empty values skipped.
func mergeLabels(maps ...map[string]string) map[string]string {
	merged := make(map[string]string)
	for _, m := range maps {
		for key, value := range m {
			if value == "" {
				continue
			}
			merged[key] = value
		}
	}
	return merged
}

// filterUnboundedMetricLabels drops per-record correlation identifiers so they
// never become metric series labels (which would explode cardinality).
func filterUnboundedMetricLabels(labels map[string]string) map[string]string {
	for key := range labels {
		if segment.IsUnboundedMetricLabel(key) {
			delete(labels, key)
		}
	}
	return labels
}

// identityLabels exposes the resolved Guardian identity as canonical label keys.
func identityLabels(partition, intent, asset string) map[string]string {
	out := make(map[string]string, 3)
	if partition != "" {
		out["guardian.partition"] = partition
	}
	if intent != "" {
		out["guardian.intent"] = intent
	}
	if asset != "" {
		out["guardian.asset"] = asset
	}
	return out
}

// correlationLabels exposes trace/span identifiers as canonical label keys.
func correlationLabels(traceID, spanID string) map[string]string {
	out := make(map[string]string, 2)
	if traceID != "" {
		out["trace_id"] = traceID
	}
	if spanID != "" {
		out["span_id"] = spanID
	}
	return out
}

// labelsFromIdentity extracts the resolved identity back out of a label map.
func labelsFromIdentity(labels map[string]string) (partition, intent, asset string) {
	partition = labels["guardian.partition"]
	intent = labels["guardian.intent"]
	asset = labels["guardian.asset"]
	return partition, intent, asset
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
