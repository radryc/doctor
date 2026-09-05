package monofs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/rydzu/ainfra/doctor/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const defaultClientHeartbeatInterval = 30 * time.Second

type routerClient interface {
	UpsertGuardianPaths(ctx context.Context, in *pb.UpsertGuardianPathsRequest, opts ...grpc.CallOption) (*pb.UpsertGuardianPathsResponse, error)
	DeleteGuardianPaths(ctx context.Context, in *pb.DeleteGuardianPathsRequest, opts ...grpc.CallOption) (*pb.DeleteGuardianPathsResponse, error)
	ListGuardianVersions(ctx context.Context, in *pb.ListGuardianVersionsRequest, opts ...grpc.CallOption) (*pb.ListGuardianVersionsResponse, error)
	GetGuardianVersion(ctx context.Context, in *pb.GetGuardianVersionRequest, opts ...grpc.CallOption) (*pb.GetGuardianVersionResponse, error)
	RegisterClient(ctx context.Context, in *pb.RegisterClientRequest, opts ...grpc.CallOption) (*pb.RegisterClientResponse, error)
	UnregisterClient(ctx context.Context, in *pb.UnregisterClientRequest, opts ...grpc.CallOption) (*pb.UnregisterClientResponse, error)
	GetClusterInfo(ctx context.Context, in *pb.ClusterInfoRequest, opts ...grpc.CallOption) (*pb.ClusterInfoResponse, error)
	ClientHeartbeat(ctx context.Context, in *pb.ClientHeartbeatRequest, opts ...grpc.CallOption) (*pb.ClientHeartbeatResponse, error)
}

type ClientConfig struct {
	RouterAddr           string
	ClientID             string
	Token                string
	PrincipalID          string
	Role                 string
	BaseURL              string
	Version              string
	Hostname             string
	MountPoint           string
	UseExternalAddresses bool
	ExternalHostAlias    string
	RPCTimeout           time.Duration
	Writable             bool
}

type GRPCClient struct {
	routerAddr           string
	clientID             string
	token                string
	principalID          string
	role                 string
	baseURL              string
	version              string
	hostname             string
	mountPoint           string
	useExternalAddresses bool
	externalHostAlias    string
	rpcTimeout           time.Duration
	writable             bool

	mu sync.Mutex

	routerConn  *grpc.ClientConn
	router      routerClient
	nodeConns   map[string]*grpc.ClientConn
	nodeClients map[string]pb.MonoFSClient

	stopHeartbeat chan struct{}
	stopOnce      sync.Once
	heartbeatWG   sync.WaitGroup
}

type nodeTarget struct {
	id     string
	client pb.MonoFSClient
}

func NewGRPCClient(ctx context.Context, cfg ClientConfig) (*GRPCClient, error) {
	if cfg.RouterAddr == "" {
		return nil, fmt.Errorf("monofs router address is required")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("monofs client id is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("monofs guardian token is required")
	}
	if cfg.PrincipalID == "" {
		cfg.PrincipalID = cfg.ClientID
	}
	if cfg.Role == "" {
		cfg.Role = "doctor"
	}
	if cfg.Version == "" {
		cfg.Version = "doctor"
	}
	if cfg.RPCTimeout <= 0 {
		cfg.RPCTimeout = 10 * time.Second
	}
	if cfg.Hostname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("resolve hostname: %w", err)
		}
		cfg.Hostname = hostname
	}

	conn, err := grpc.NewClient(
		cfg.RouterAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256*1024*1024),
			grpc.MaxCallSendMsgSize(256*1024*1024),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to monofs router: %w", err)
	}

	client := &GRPCClient{
		routerAddr:           cfg.RouterAddr,
		clientID:             cfg.ClientID,
		token:                cfg.Token,
		principalID:          cfg.PrincipalID,
		role:                 cfg.Role,
		baseURL:              strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		version:              cfg.Version,
		hostname:             cfg.Hostname,
		mountPoint:           cfg.MountPoint,
		useExternalAddresses: cfg.UseExternalAddresses,
		externalHostAlias:    strings.TrimSpace(cfg.ExternalHostAlias),
		rpcTimeout:           cfg.RPCTimeout,
		writable:             cfg.Writable,
		routerConn:           conn,
		router:               pb.NewMonoFSRouterClient(conn),
		nodeConns:            map[string]*grpc.ClientConn{},
		nodeClients:          map[string]pb.MonoFSClient{},
		stopHeartbeat:        make(chan struct{}),
	}

	if err := client.refreshNodes(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	heartbeatInterval, err := client.register(ctx)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	client.startHeartbeatLoop(heartbeatInterval)
	return client, nil
}

func (c *GRPCClient) Close() error {
	c.stopHeartbeatLoop()

	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error
	if c.router != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := c.router.UnregisterClient(ctx, &pb.UnregisterClientRequest{
			ClientId: c.clientID,
			Reason:   "doctor shutdown",
		})
		cancel()
		if err != nil && status.Code(err) != codes.NotFound {
			errs = append(errs, err)
		}
	}
	for nodeID, conn := range c.nodeConns {
		if err := conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close node %s: %w", nodeID, err))
		}
	}
	if c.routerConn != nil {
		if err := c.routerConn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func (c *GRPCClient) UpsertPaths(ctx context.Context, token string, writes []store.PathWrite, mutationCtx store.MutationContext) (store.BatchRevision, error) {
	req := &pb.UpsertGuardianPathsRequest{
		GuardianToken: token,
		Context:       toProtoMutationContext(mutationCtx),
		Writes:        make([]*pb.GuardianPathWrite, 0, len(writes)),
	}
	for _, write := range writes {
		req.Writes = append(req.Writes, &pb.GuardianPathWrite{
			LogicalPath:       write.LogicalPath,
			Content:           write.Content,
			ExpectedVersionId: write.ExpectedVersionID,
		})
	}

	callCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.router.UpsertGuardianPaths(callCtx, req)
	if err != nil {
		return store.BatchRevision{}, mapRPCError(err)
	}
	if resp != nil && !resp.GetSuccess() {
		return store.BatchRevision{}, fmt.Errorf("monofs upsert failed: %s", resp.GetMessage())
	}
	return store.BatchRevision{
		BatchRevisionID: resp.GetBatchRevisionId(),
		Files:           convertFileVersions(resp.GetVersions()),
	}, nil
}

func (c *GRPCClient) DeletePaths(ctx context.Context, token string, deletes []store.PathDelete, mutationCtx store.MutationContext) (store.BatchRevision, error) {
	req := &pb.DeleteGuardianPathsRequest{
		GuardianToken: token,
		Context:       toProtoMutationContext(mutationCtx),
		Deletes:       make([]*pb.GuardianPathDelete, 0, len(deletes)),
	}
	for _, del := range deletes {
		req.Deletes = append(req.Deletes, &pb.GuardianPathDelete{
			LogicalPath:       del.LogicalPath,
			ExpectedVersionId: del.ExpectedVersionID,
		})
	}

	callCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.router.DeleteGuardianPaths(callCtx, req)
	if err != nil {
		return store.BatchRevision{}, mapRPCError(err)
	}
	if resp != nil && !resp.GetSuccess() {
		return store.BatchRevision{}, fmt.Errorf("monofs delete failed: %s", resp.GetMessage())
	}
	return store.BatchRevision{
		BatchRevisionID: resp.GetBatchRevisionId(),
		Files:           convertFileVersions(resp.GetTombstones()),
	}, nil
}

func (c *GRPCClient) ListVersions(ctx context.Context, token, logicalPath string) ([]store.FileVersion, error) {
	var (
		pageToken string
		out       []store.FileVersion
	)
	for {
		callCtx, cancel := c.withTimeout(ctx)
		resp, err := c.router.ListGuardianVersions(callCtx, &pb.ListGuardianVersionsRequest{
			GuardianToken: token,
			LogicalPath:   logicalPath,
			PageSize:      256,
			PageToken:     pageToken,
		})
		cancel()
		if err != nil {
			return nil, mapRPCError(err)
		}
		out = append(out, convertFileVersions(resp.GetVersions())...)
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			return out, nil
		}
	}
}

func (c *GRPCClient) GetVersion(ctx context.Context, token, logicalPath, versionID string) (store.VersionedFile, error) {
	callCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	resp, err := c.router.GetGuardianVersion(callCtx, &pb.GetGuardianVersionRequest{
		GuardianToken: token,
		LogicalPath:   logicalPath,
		VersionId:     versionID,
	})
	if err != nil {
		return store.VersionedFile{}, mapRPCError(err)
	}
	return store.VersionedFile{
		Version: convertFileVersion(resp.GetVersion()),
		Content: append([]byte(nil), resp.GetContent()...),
	}, nil
}

func (c *GRPCClient) ReadFile(ctx context.Context, mountPath string) ([]byte, error) {
	nodes, err := c.healthyNodes(ctx)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, node := range nodes {
		callCtx, cancel := c.withTimeout(ctx)
		stream, err := node.client.Read(callCtx, &pb.ReadRequest{
			Path:   mountPath,
			Offset: 0,
			Size:   0,
		})
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		content, readErr := readAll(stream)
		cancel()
		if readErr == nil {
			return content, nil
		}
		lastErr = readErr
	}
	if lastErr == nil || status.Code(lastErr) == codes.NotFound {
		return nil, os.ErrNotExist
	}
	return nil, lastErr
}

func (c *GRPCClient) ListDir(ctx context.Context, mountPath string) ([]store.DirEntry, error) {
	nodes, err := c.healthyNodes(ctx)
	if err != nil {
		return nil, err
	}

	entries := map[string]store.DirEntry{}
	var (
		lastErr error
		success bool
	)
	for _, node := range nodes {
		callCtx, cancel := c.withTimeout(ctx)
		stream, err := node.client.ReadDir(callCtx, &pb.ReadDirRequest{Path: mountPath})
		if err != nil {
			cancel()
			if status.Code(err) != codes.NotFound {
				lastErr = err
			}
			continue
		}
		success = true
		for {
			entry, recvErr := stream.Recv()
			if recvErr == io.EOF {
				break
			}
			if recvErr != nil {
				lastErr = recvErr
				break
			}
			name := entry.GetName()
			if name == "" {
				continue
			}
			current := store.DirEntry{
				Name:  name,
				IsDir: entry.GetMode()&0o040000 != 0,
			}
			existing, ok := entries[name]
			if ok && existing.IsDir {
				continue
			}
			if current.IsDir && ok {
				current.Size = existing.Size
			}
			entries[name] = current
		}
		cancel()
	}

	if len(entries) == 0 {
		if success {
			return nil, nil
		}
		if lastErr == nil || status.Code(lastErr) == codes.NotFound {
			return nil, nil
		}
		return nil, lastErr
	}

	out := make([]store.DirEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *GRPCClient) buildRegisterRequest() *pb.RegisterClientRequest {
	return &pb.RegisterClientRequest{
		ClientId:   c.clientID,
		MountPoint: c.mountPoint,
		Hostname:   c.hostname,
		Writable:   c.writable,
		Version:    c.version,
		GuardianConfig: &pb.GuardianConfig{
			BaseUrl:     c.baseURL,
			AuthToken:   c.token,
			PrincipalId: c.principalID,
			Role:        c.role,
			DisplayName: c.clientID,
		},
	}
}

func registerHeartbeatInterval(resp *pb.RegisterClientResponse) (time.Duration, error) {
	if resp == nil {
		return 0, fmt.Errorf("empty register response")
	}
	if !resp.GetSuccess() {
		message := resp.GetMessage()
		if message == "" {
			message = "doctor client registration rejected"
		}
		return 0, errors.New(message)
	}
	interval := time.Duration(resp.GetHeartbeatIntervalMs()) * time.Millisecond
	if interval <= 0 {
		return defaultClientHeartbeatInterval, nil
	}
	return interval, nil
}

func (c *GRPCClient) register(ctx context.Context) (time.Duration, error) {
	callCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.router.RegisterClient(callCtx, c.buildRegisterRequest())
	if err != nil {
		return 0, fmt.Errorf("register doctor client with monofs: %w", mapRPCError(err))
	}
	heartbeatInterval, err := registerHeartbeatInterval(resp)
	if err != nil {
		return 0, fmt.Errorf("register doctor client with monofs: %w", err)
	}
	return heartbeatInterval, nil
}

func (c *GRPCClient) startHeartbeatLoop(interval time.Duration) {
	if interval <= 0 {
		interval = defaultClientHeartbeatInterval
	}
	c.heartbeatWG.Add(1)
	go func() {
		defer c.heartbeatWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-c.stopHeartbeat:
				return
			case <-ticker.C:
				c.sendHeartbeat()
			}
		}
	}()
}

func (c *GRPCClient) sendHeartbeat() {
	callCtx, cancel := c.withTimeout(context.Background())
	defer cancel()
	resp, err := c.router.ClientHeartbeat(callCtx, &pb.ClientHeartbeatRequest{
		ClientId: c.clientID,
	})
	if err != nil {
		return
	}
	if resp.GetShouldRegister() {
		_, _ = c.register(context.Background())
	}
}

func (c *GRPCClient) stopHeartbeatLoop() {
	c.stopOnce.Do(func() {
		if c.stopHeartbeat != nil {
			close(c.stopHeartbeat)
		}
		c.heartbeatWG.Wait()
	})
}

func (c *GRPCClient) healthyNodes(ctx context.Context) ([]nodeTarget, error) {
	if err := c.refreshNodes(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		nodes := c.snapshotNodeTargets()
		if len(nodes) > 0 {
			return nodes, nil
		}
		return nil, err
	}
	out := c.snapshotNodeTargets()
	if len(out) == 0 {
		return nil, fmt.Errorf("no healthy monofs nodes available")
	}
	return out, nil
}

func (c *GRPCClient) snapshotNodeTargets() []nodeTarget {
	c.mu.Lock()
	defer c.mu.Unlock()

	ids := make([]string, 0, len(c.nodeClients))
	for id := range c.nodeClients {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]nodeTarget, 0, len(ids))
	for _, id := range ids {
		out = append(out, nodeTarget{id: id, client: c.nodeClients[id]})
	}
	return out
}

func (c *GRPCClient) refreshNodes(ctx context.Context) error {
	callCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	resp, err := c.router.GetClusterInfo(callCtx, &pb.ClusterInfoRequest{
		ClientId:             c.clientID,
		UseExternalAddresses: c.useExternalAddresses,
	})
	if err != nil {
		return fmt.Errorf("fetch monofs cluster info: %w", err)
	}

	healthy := make([]*pb.NodeInfo, 0, len(resp.GetNodes()))
	for _, node := range resp.GetNodes() {
		if node == nil || !node.GetHealthy() {
			continue
		}
		if node.GetNodeId() == "" || node.GetAddress() == "" {
			continue
		}
		healthy = append(healthy, node)
	}
	if len(healthy) == 0 {
		return fmt.Errorf("no healthy monofs nodes available")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]struct{}{}
	for _, node := range healthy {
		nodeID := node.GetNodeId()
		seen[nodeID] = struct{}{}
		if c.nodeConns[nodeID] != nil {
			continue
		}
		address := c.rewriteNodeAddress(node.GetAddress())
		nodeConn, dialErr := grpc.NewClient(
			address,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(256*1024*1024),
				grpc.MaxCallSendMsgSize(256*1024*1024),
			),
		)
		if dialErr != nil {
			return fmt.Errorf("connect to monofs node %s: %w", nodeID, dialErr)
		}
		c.nodeConns[nodeID] = nodeConn
		c.nodeClients[nodeID] = pb.NewMonoFSClient(nodeConn)
	}
	for nodeID, conn := range c.nodeConns {
		if _, ok := seen[nodeID]; ok {
			continue
		}
		_ = conn.Close()
		delete(c.nodeConns, nodeID)
		delete(c.nodeClients, nodeID)
	}
	return nil
}

func (c *GRPCClient) rewriteNodeAddress(address string) string {
	if !c.useExternalAddresses || c.externalHostAlias == "" {
		return address
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return address
	}
	if strings.EqualFold(host, "localhost") {
		return net.JoinHostPort(c.externalHostAlias, port)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return net.JoinHostPort(c.externalHostAlias, port)
	}
	return address
}

func (c *GRPCClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.rpcTimeout)
}

func readAll(stream grpc.ServerStreamingClient[pb.DataChunk]) ([]byte, error) {
	var content []byte
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return content, nil
		}
		if err != nil {
			return nil, err
		}
		content = append(content, chunk.GetData()...)
	}
}

func toProtoMutationContext(ctx store.MutationContext) *pb.GuardianMutationContext {
	return &pb.GuardianMutationContext{
		PrincipalId:   ctx.PrincipalID,
		Reason:        ctx.Reason,
		CorrelationId: ctx.CorrelationID,
	}
}

func convertFileVersions(in []*pb.GuardianFileVersion) []store.FileVersion {
	out := make([]store.FileVersion, 0, len(in))
	for _, version := range in {
		out = append(out, convertFileVersion(version))
	}
	return out
}

func convertFileVersion(in *pb.GuardianFileVersion) store.FileVersion {
	if in == nil {
		return store.FileVersion{}
	}
	return store.FileVersion{
		LogicalPath:     in.GetLogicalPath(),
		VersionID:       in.GetVersionId(),
		BatchRevisionID: in.GetBatchRevisionId(),
		ContentSHA256:   in.GetContentSha256(),
		CommittedAt:     time.Unix(in.GetCommittedAt(), 0).UTC(),
		Tombstone:       in.GetTombstone(),
		PrincipalID:     in.GetPrincipalId(),
		Reason:          in.GetReason(),
	}
}

func mapRPCError(err error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.AlreadyExists, codes.FailedPrecondition:
		return store.ErrConflict
	case codes.NotFound:
		return os.ErrNotExist
	default:
		return err
	}
}
