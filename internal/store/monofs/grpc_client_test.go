package monofs

import (
	"context"
	"testing"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
)

func TestHealthyNodesFallsBackToCachedNodesWhenClusterRefreshFails(t *testing.T) {
	t.Parallel()

	router := &fakeRouterClient{clusterInfoErr: context.DeadlineExceeded}
	client := &GRPCClient{
		rpcTimeout: time.Second,
		router:     router,
		nodeClients: map[string]pb.MonoFSClient{
			"node-a": nil,
		},
	}

	nodes, err := client.healthyNodes(context.Background())
	if err != nil {
		t.Fatalf("healthyNodes() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0].id != "node-a" {
		t.Fatalf("healthyNodes() = %+v, want cached node-a", nodes)
	}
}

func TestHealthyNodesPreservesCachedNodesWhenRouterReportsNone(t *testing.T) {
	t.Parallel()

	router := &fakeRouterClient{clusterInfoResp: &pb.ClusterInfoResponse{}}
	client := &GRPCClient{
		rpcTimeout: time.Second,
		router:     router,
		nodeClients: map[string]pb.MonoFSClient{
			"node-a": nil,
		},
	}

	nodes, err := client.healthyNodes(context.Background())
	if err != nil {
		t.Fatalf("healthyNodes() error = %v", err)
	}
	if len(nodes) != 1 || nodes[0].id != "node-a" {
		t.Fatalf("healthyNodes() = %+v, want cached node-a", nodes)
	}
}

func TestRewriteNodeAddressUsesExternalHostAliasForLoopback(t *testing.T) {
	t.Parallel()

	client := &GRPCClient{
		useExternalAddresses: true,
		externalHostAlias:    "host.docker.internal",
	}
	if got := client.rewriteNodeAddress("localhost:9001"); got != "host.docker.internal:9001" {
		t.Fatalf("rewrite localhost = %q", got)
	}
	if got := client.rewriteNodeAddress("127.0.0.1:9002"); got != "host.docker.internal:9002" {
		t.Fatalf("rewrite loopback = %q", got)
	}
	if got := client.rewriteNodeAddress("node5:9005"); got != "node5:9005" {
		t.Fatalf("rewrite non-loopback = %q", got)
	}
}

func TestBuildRegisterRequestIncludesBaseURL(t *testing.T) {
	t.Parallel()

	client := &GRPCClient{
		clientID:    "doctor-query",
		token:       "doctor-token",
		principalID: "doctor",
		role:        "doctor",
		baseURL:     "http://doctor.example/ui",
		version:     "doctor-query",
		hostname:    "doctor-host",
		writable:    true,
	}

	req := client.buildRegisterRequest()
	if req.GetClientId() != "doctor-query" {
		t.Fatalf("client_id = %q", req.GetClientId())
	}
	cfg := req.GetGuardianConfig()
	if cfg == nil {
		t.Fatal("expected guardian_config")
	}
	if cfg.GetBaseUrl() != "http://doctor.example/ui" {
		t.Fatalf("base_url = %q", cfg.GetBaseUrl())
	}
	if cfg.GetAuthToken() != "doctor-token" {
		t.Fatalf("auth_token = %q", cfg.GetAuthToken())
	}
}

type fakeRouterClient struct {
	clusterInfoResp *pb.ClusterInfoResponse
	clusterInfoErr  error
}

func (f *fakeRouterClient) UpsertGuardianPaths(context.Context, *pb.UpsertGuardianPathsRequest, ...grpc.CallOption) (*pb.UpsertGuardianPathsResponse, error) {
	return nil, nil
}

func (f *fakeRouterClient) DeleteGuardianPaths(context.Context, *pb.DeleteGuardianPathsRequest, ...grpc.CallOption) (*pb.DeleteGuardianPathsResponse, error) {
	return nil, nil
}

func (f *fakeRouterClient) ListGuardianVersions(context.Context, *pb.ListGuardianVersionsRequest, ...grpc.CallOption) (*pb.ListGuardianVersionsResponse, error) {
	return nil, nil
}

func (f *fakeRouterClient) GetGuardianVersion(context.Context, *pb.GetGuardianVersionRequest, ...grpc.CallOption) (*pb.GetGuardianVersionResponse, error) {
	return nil, nil
}

func (f *fakeRouterClient) RegisterClient(context.Context, *pb.RegisterClientRequest, ...grpc.CallOption) (*pb.RegisterClientResponse, error) {
	return &pb.RegisterClientResponse{Success: true, HeartbeatIntervalMs: 30000}, nil
}

func (f *fakeRouterClient) UnregisterClient(context.Context, *pb.UnregisterClientRequest, ...grpc.CallOption) (*pb.UnregisterClientResponse, error) {
	return &pb.UnregisterClientResponse{Success: true}, nil
}

func (f *fakeRouterClient) GetClusterInfo(context.Context, *pb.ClusterInfoRequest, ...grpc.CallOption) (*pb.ClusterInfoResponse, error) {
	if f.clusterInfoResp != nil || f.clusterInfoErr != nil {
		return f.clusterInfoResp, f.clusterInfoErr
	}
	return &pb.ClusterInfoResponse{}, nil
}

func (f *fakeRouterClient) ClientHeartbeat(context.Context, *pb.ClientHeartbeatRequest, ...grpc.CallOption) (*pb.ClientHeartbeatResponse, error) {
	return &pb.ClientHeartbeatResponse{Success: true}, nil
}

var _ routerClient = (*fakeRouterClient)(nil)
