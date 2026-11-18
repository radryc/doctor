package monofs

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

type logEngineRouterStreamStub struct {
	pb.UnimplementedMonoFSRouterServer

	mu          sync.Mutex
	metricReq   *pb.QueryMetricsRequest
	metricItems [][]byte
	traceItems  [][]byte
}

func (s *logEngineRouterStreamStub) StreamQueryMetrics(req *pb.QueryMetricsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	s.mu.Lock()
	s.metricReq = req
	s.mu.Unlock()
	for _, item := range s.metricItems {
		if err := stream.Send(&pb.QueryResultItem{ItemJson: append([]byte(nil), item...)}); err != nil {
			return err
		}
	}
	return nil
}

func (s *logEngineRouterStreamStub) lastMetricRequest() *pb.QueryMetricsRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metricReq
}

func (s *logEngineRouterStreamStub) StreamQueryTraces(_ *pb.QueryTracesRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	for _, item := range s.traceItems {
		if err := stream.Send(&pb.QueryResultItem{ItemJson: append([]byte(nil), item...)}); err != nil {
			return err
		}
	}
	return nil
}

type authCaptureRouter struct {
	pb.UnimplementedMonoFSRouterServer
	authz chan string
}

func (s *authCaptureRouter) IngestLogs(ctx context.Context, _ *pb.IngestLogsRequest) (*pb.IngestLogsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) > 0 {
		s.authz <- values[0]
	} else {
		s.authz <- ""
	}
	return &pb.IngestLogsResponse{Ok: true}, nil
}

func TestLogEngineClientWithTokenAttachesBearer(t *testing.T) {
	t.Setenv("DOCTOR_MONOFS_TLS_INSECURE", "true")
	t.Setenv("DOCTOR_MONOFS_TLS_REQUIRE", "false")

	capture := &authCaptureRouter{authz: make(chan string, 1)}
	client := newLogEngineClientWithDialerForTest(t, capture, "secret-token")

	if err := client.IngestLogs(context.Background(), "chunk-1", []segment.LogRecord{{
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Service:   "api",
		Body:      "hello",
	}}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	if got, want := <-capture.authz, "Bearer secret-token"; got != want {
		t.Fatalf("authorization metadata = %q, want %q", got, want)
	}
}

func TestLogEngineClientWithoutTokenOmitsBearer(t *testing.T) {
	t.Setenv("DOCTOR_MONOFS_TLS_INSECURE", "true")
	t.Setenv("DOCTOR_MONOFS_TLS_REQUIRE", "false")

	capture := &authCaptureRouter{authz: make(chan string, 1)}
	client := newLogEngineClientWithDialerForTest(t, capture, "")

	if err := client.IngestLogs(context.Background(), "chunk-1", []segment.LogRecord{{
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Service:   "api",
		Body:      "hello",
	}}); err != nil {
		t.Fatalf("IngestLogs() error = %v", err)
	}
	if got := <-capture.authz; got != "" {
		t.Fatalf("authorization metadata = %q, want empty", got)
	}
}

func newLogEngineClientWithDialerForTest(t *testing.T, server pb.MonoFSRouterServer, token string) *LogEngineClient {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	pb.RegisterMonoFSRouterServer(grpcServer, server)
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	t.Cleanup(grpcServer.Stop)

	dialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
	client, err := newLogEngineClientWithOptions(context.Background(), "passthrough:///bufnet", token, grpc.WithContextDialer(dialer))
	if err != nil {
		t.Fatalf("newLogEngineClientWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client
}

func newLogEngineClientForTest(t *testing.T, server pb.MonoFSRouterServer) *LogEngineClient {
	t.Helper()

	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	pb.RegisterMonoFSRouterServer(grpcServer, server)
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	t.Cleanup(grpcServer.Stop)

	dialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.DialContext() error = %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	return &LogEngineClient{
		conn:   conn,
		router: pb.NewMonoFSRouterClient(conn),
	}
}

func TestLogEngineClientQueryMetricsUsesStreamedRouterResults(t *testing.T) {
	t.Parallel()

	ts := time.Unix(1700000000, 0).UTC()
	first, err := json.Marshal(metricStreamRecord{
		Timestamp:  ts,
		Service:    "doctor",
		MetricName: "requests",
		Value:      1.5,
		Labels:     map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("json.Marshal(first metric) error = %v", err)
	}
	second, err := json.Marshal(metricStreamRecord{
		Timestamp:  ts.Add(time.Second),
		Service:    "doctor",
		MetricName: "requests",
		Value:      2.5,
		Labels:     map[string]string{"env": "prod", "region": "use1"},
	})
	if err != nil {
		t.Fatalf("json.Marshal(second metric) error = %v", err)
	}

	client := newLogEngineClientForTest(t, &logEngineRouterStreamStub{metricItems: [][]byte{first, second}})
	records, err := client.QueryMetrics(context.Background(), "tenant-a", segment.MetricQuery{MetricName: "requests"}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("QueryMetrics() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("len(records) = %d, want 2", len(records))
	}
	if records[0].MetricName != "requests" || records[1].MetricName != "requests" {
		t.Fatalf("metric names = %#v, want requests", records)
	}
	if records[0].Tenant != "tenant-a" || records[1].Tenant != "tenant-a" {
		t.Fatalf("tenants = %#v, want tenant-a", records)
	}
	if records[1].Attributes["region"] != "use1" {
		t.Fatalf("second metric attributes = %#v, want region use1", records[1].Attributes)
	}
}

func TestLogEngineClientQueryTracesUsesStreamedRouterResults(t *testing.T) {
	t.Parallel()

	first, err := json.Marshal(segment.TraceRecord{
		TraceID:   "trace-1",
		SpanID:    "span-a",
		Service:   "doctor",
		Name:      "select_logs",
		StartTime: time.Unix(1700000100, 0).UTC(),
		EndTime:   time.Unix(1700000101, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("json.Marshal(first trace) error = %v", err)
	}
	second, err := json.Marshal(segment.TraceRecord{
		TraceID:   "trace-1",
		SpanID:    "span-b",
		Service:   "doctor",
		Name:      "merge_results",
		StartTime: time.Unix(1700000102, 0).UTC(),
		EndTime:   time.Unix(1700000103, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("json.Marshal(second trace) error = %v", err)
	}

	client := newLogEngineClientForTest(t, &logEngineRouterStreamStub{traceItems: [][]byte{first, second}})
	records, err := client.QueryTraces(context.Background(), "tenant-a", "trace-1", "doctor", time.Time{}, time.Time{}, 10)
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("len(records) = %d, want 2", len(records))
	}
	if records[0].SpanID != "span-a" || records[1].SpanID != "span-b" {
		t.Fatalf("span ids = %#v, want span-a/span-b", records)
	}
}

func TestLogEngineClientStreamMetricsForwardsPartitionScope(t *testing.T) {
	t.Parallel()

	stub := &logEngineRouterStreamStub{}
	client := newLogEngineClientForTest(t, stub)

	err := client.StreamMetrics(
		context.Background(),
		"tenant-a",
		segment.MetricQuery{
			VisiblePartitions:    []string{"memebank"},
			VisiblePartitionsSet: true,
		},
		time.Time{}, time.Time{},
		func(segment.MetricPointRecord) error { return nil },
	)
	if err != nil {
		t.Fatalf("StreamMetrics() error = %v", err)
	}

	req := stub.lastMetricRequest()
	if req == nil {
		t.Fatal("router did not receive a metrics request")
	}
	if !req.GetVisiblePartitionsSet() {
		t.Fatal("VisiblePartitionsSet was not forwarded")
	}
	if got := req.GetVisiblePartitions(); len(got) != 1 || got[0] != "memebank" {
		t.Fatalf("VisiblePartitions = %#v, want [memebank]", got)
	}
}
