package quota

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/radryc/doctor/internal/store"
	monofsstore "github.com/radryc/doctor/internal/store/monofs"
)

// monoFSCASStore adapts the MonoFS guardian file client to CASStore.
type monoFSCASStore struct {
	client *monofsstore.GRPCClient
	token  string
}

func (m *monoFSCASStore) ReadVersion(ctx context.Context, logicalPath string) ([]byte, string, error) {
	versions, err := m.client.ListVersions(ctx, m.token, logicalPath)
	if err != nil {
		return nil, "", err
	}
	if len(versions) == 0 {
		return nil, "", os.ErrNotExist
	}
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].CommittedAt.After(versions[j].CommittedAt)
	})
	file, err := m.client.GetVersion(ctx, m.token, logicalPath, versions[0].VersionID)
	if err != nil {
		return nil, "", err
	}
	return file.Content, file.Version.VersionID, nil
}

func (m *monoFSCASStore) WriteCAS(ctx context.Context, logicalPath string, content []byte, expectedVersion string) error {
	_, err := m.client.UpsertPaths(ctx, m.token, []store.PathWrite{{
		LogicalPath:       logicalPath,
		Content:           content,
		ExpectedVersionID: expectedVersion,
	}}, store.MutationContext{
		PrincipalID: "doctor-ingest",
		Reason:      "quota-state",
	})
	return err
}

func (m *monoFSCASStore) ListDir(ctx context.Context, logicalDir string) ([]store.DirEntry, error) {
	return m.client.ListDir(ctx, logicalDir)
}

// NewMonoFSStorageTracker builds a shared storage tracker backed by MonoFS
// CAS files. The returned tracker owns the MonoFS client and closes it on
// Close/Run shutdown.
func NewMonoFSStorageTracker(ctx context.Context, routerAddr, token, clientID, root string, reserveBytes int64, failMode string) (*StorageTracker, error) {
	routerAddr = strings.TrimSpace(routerAddr)
	token = strings.TrimSpace(token)
	if routerAddr == "" {
		return nil, fmt.Errorf("quota: DOCTOR_MONOFS_ROUTER_ADDR is required for shared state")
	}
	if token == "" {
		return nil, fmt.Errorf("quota: DOCTOR_MONOFS_TOKEN is required for shared state")
	}
	if strings.TrimSpace(clientID) == "" {
		clientID = "doctor-ingest"
	}
	client, err := monofsstore.NewGRPCClient(ctx, monofsstore.ClientConfig{
		RouterAddr: routerAddr,
		ClientID:   clientID,
		Token:      token,
		Role:       "doctor",
		Writable:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("quota: connect MonoFS file store: %w", err)
	}
	state := newSharedState(&monoFSCASStore{client: client, token: token}, root, clientID, reserveBytes, failMode)
	state.closer = client
	tracker, err := NewStorageTrackerWithState(ctx, state)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return tracker, nil
}
