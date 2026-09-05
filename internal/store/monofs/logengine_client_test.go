package monofs

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/rydzu/ainfra/doctor/internal/segment"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type logEngineRouterStreamStub struct {
	pb.UnimplementedMonoFSRouterServer

	metricItems [][]byte
	traceItems  [][]byte
}

func (s *logEngineRouterStreamStub) StreamQueryMetrics(_ *pb.QueryMetricsRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	for _, item := range s.metricItems {
		if err := stream.Send(&pb.QueryResultItem{ItemJson: append([]byte(nil), item...)}); err != nil {
			return err
		}
	}
	return nil
}

func (s *logEngineRouterStreamStub) StreamQueryTraces(_ *pb.QueryTracesRequest, stream grpc.ServerStreamingServer[pb.QueryResultItem]) error {
	for _, item := range s.traceItems {
		if err := stream.Send(&pb.QueryResultItem{ItemJson: append([]byte(nil), item...)}); err != nil {
			return err
		}
	}
	return nil
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
