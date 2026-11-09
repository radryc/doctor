package monofs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type benchRouterClient struct {
	calls   atomic.Int64
	latency time.Duration
}

func (f *benchRouterClient) UpsertGuardianPaths(context.Context, *pb.UpsertGuardianPathsRequest, ...grpc.CallOption) (*pb.UpsertGuardianPathsResponse, error) {
	return &pb.UpsertGuardianPathsResponse{Success: true}, nil
}

func (f *benchRouterClient) DeleteGuardianPaths(context.Context, *pb.DeleteGuardianPathsRequest, ...grpc.CallOption) (*pb.DeleteGuardianPathsResponse, error) {
	return &pb.DeleteGuardianPathsResponse{Success: true}, nil
}

func (f *benchRouterClient) ListGuardianVersions(context.Context, *pb.ListGuardianVersionsRequest, ...grpc.CallOption) (*pb.ListGuardianVersionsResponse, error) {
	return &pb.ListGuardianVersionsResponse{}, nil
}

func (f *benchRouterClient) GetGuardianVersion(context.Context, *pb.GetGuardianVersionRequest, ...grpc.CallOption) (*pb.GetGuardianVersionResponse, error) {
	return &pb.GetGuardianVersionResponse{}, nil
}

func (f *benchRouterClient) RegisterClient(context.Context, *pb.RegisterClientRequest, ...grpc.CallOption) (*pb.RegisterClientResponse, error) {
	return &pb.RegisterClientResponse{Success: true, HeartbeatIntervalMs: 30000}, nil
}

func (f *benchRouterClient) UnregisterClient(context.Context, *pb.UnregisterClientRequest, ...grpc.CallOption) (*pb.UnregisterClientResponse, error) {
	return &pb.UnregisterClientResponse{Success: true}, nil
}

func (f *benchRouterClient) GetClusterInfo(context.Context, *pb.ClusterInfoRequest, ...grpc.CallOption) (*pb.ClusterInfoResponse, error) {
	f.calls.Add(1)
	if f.latency > 0 {
		time.Sleep(f.latency)
	}
	return &pb.ClusterInfoResponse{Nodes: []*pb.NodeInfo{{
		NodeId:  "node-a",
		Address: "127.0.0.1:1",
		Healthy: true,
	}}}, nil
}

func (f *benchRouterClient) ClientHeartbeat(context.Context, *pb.ClientHeartbeatRequest, ...grpc.CallOption) (*pb.ClientHeartbeatResponse, error) {
	return &pb.ClientHeartbeatResponse{Success: true}, nil
}

var _ routerClient = (*benchRouterClient)(nil)

// BenchmarkHealthyNodes measures repeated node selection, which sits on every
// ReadFile/ListDir. The cached implementation should issue ~1 cluster-info RPC
// for the whole benchmark instead of one per operation.
func BenchmarkHealthyNodes(b *testing.B) {
	router := &benchRouterClient{latency: 100 * time.Microsecond}
	client := &GRPCClient{
		rpcTimeout:  time.Second,
		router:      router,
		creds:       insecure.NewCredentials(),
		nodeConns:   map[string]*grpc.ClientConn{},
		nodeClients: map[string]pb.MonoFSClient{},
	}
	b.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := client.healthyNodes(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(router.calls.Load())/float64(b.N), "clusterinfo/op")
}
