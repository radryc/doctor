package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/store"
)

// ErrSnapshotNotFound reports that no catalog manifest exists yet.
var ErrSnapshotNotFound = errors.New("federation catalog snapshot not found")

// SnapshotStore persists and loads a catalog snapshot. Implementations are
// responsible for atomic publish and version preconditions.
type SnapshotStore interface {
	Load(ctx context.Context) (Snapshot, error)
	Save(ctx context.Context, snapshot Snapshot, expectedVersion string) (string, error)
}

// ShardRef points at one immutable shard in a MonoFS catalog snapshot.
type ShardRef struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Targets int    `json:"targets"`
	SHA256  string `json:"sha256"`
}

// Manifest is the commit point of a sharded catalog snapshot.
type Manifest struct {
	Version     string     `json:"version"`
	GeneratedAt time.Time  `json:"generated_at"`
	Shards      []ShardRef `json:"shards"`
}

// CatalogFileStore is the subset of the Doctor file store needed by the MonoFS
// snapshot store. ruler.FileStore and ruler.MemoryStore satisfy it.
type CatalogFileStore interface {
	ReadFile(ctx context.Context, logicalPath string) ([]byte, error)
	UpsertPaths(ctx context.Context, token string, writes []store.PathWrite, mutation store.MutationContext) (store.BatchRevision, error)
	ListVersions(ctx context.Context, token, logicalPath string) ([]store.FileVersion, error)
}

// FileSnapshotStore is the static/demo implementation. It stores the whole
// snapshot in one local JSON/YAML file.
type FileSnapshotStore struct {
	Path string
}

func (s *FileSnapshotStore) Load(context.Context) (Snapshot, error) {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return Snapshot{}, ErrSnapshotNotFound
	}
	snapshot, err := LoadSnapshotFile(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, ErrSnapshotNotFound
		}
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *FileSnapshotStore) Save(_ context.Context, snapshot Snapshot, expectedVersion string) (string, error) {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return "", errors.New("federation snapshot path is required")
	}
	if expectedVersion != "" && expectedVersion != "absent" {
		current, err := LoadSnapshotFile(s.Path)
		if err == nil && current.Version != expectedVersion {
			return "", fmt.Errorf("federation snapshot version conflict: current=%s expected=%s", current.Version, expectedVersion)
		}
	}
	if snapshot.Version == "" {
		snapshot.Version = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return "", err
	}
	return snapshot.Version, nil
}

// MemorySnapshotStore is for tests and local development.
type MemorySnapshotStore struct {
	mu       sync.Mutex
	snapshot Snapshot
	exists   bool
}

func (s *MemorySnapshotStore) Load(context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.exists {
		return Snapshot{}, ErrSnapshotNotFound
	}
	return cloneSnapshot(s.snapshot), nil
}

func (s *MemorySnapshotStore) Save(_ context.Context, snapshot Snapshot, expectedVersion string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exists && expectedVersion != "" && expectedVersion != "absent" && s.snapshot.Version != expectedVersion {
		return "", fmt.Errorf("federation snapshot version conflict: current=%s expected=%s", s.snapshot.Version, expectedVersion)
	}
	if snapshot.Version == "" {
		snapshot.Version = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	s.snapshot = cloneSnapshot(snapshot)
	s.exists = true
	return snapshot.Version, nil
}

// MonoFSSnapshotStore stores a sharded catalog in the Doctor MonoFS file store.
// Shards are immutable and written before the manifest; the manifest is the
// only object with a CAS precondition.
type MonoFSSnapshotStore struct {
	Files        CatalogFileStore
	Token        string
	Root         string
	Tenant       string
	ShardTargets int
	Principal    string
}

// NewMonoFSSnapshotStore builds a MonoFS snapshot store.
func NewMonoFSSnapshotStore(files CatalogFileStore, token, root, tenant string) *MonoFSSnapshotStore {
	if strings.TrimSpace(root) == "" {
		root = "/doctor/v1/federation"
	}
	if strings.TrimSpace(tenant) == "" {
		tenant = "default"
	}
	return &MonoFSSnapshotStore{
		Files:        files,
		Token:        token,
		Root:         "/" + strings.Trim(strings.TrimSpace(root), "/"),
		Tenant:       tenant,
		ShardTargets: 500,
		Principal:    "doctor-federation",
	}
}

func (s *MonoFSSnapshotStore) tenantRoot() string {
	return path.Join(s.Root, s.Tenant)
}

func (s *MonoFSSnapshotStore) manifestPath() string {
	return path.Join(s.tenantRoot(), "manifest.json")
}

func (s *MonoFSSnapshotStore) Load(ctx context.Context) (Snapshot, error) {
	if s == nil || s.Files == nil {
		return Snapshot{}, errors.New("federation snapshot file store is required")
	}
	data, err := s.Files.ReadFile(ctx, s.manifestPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, ErrSnapshotNotFound
		}
		return Snapshot{}, fmt.Errorf("read federation manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Snapshot{}, fmt.Errorf("parse federation manifest: %w", err)
	}
	if len(manifest.Shards) == 0 {
		return Snapshot{Version: manifest.Version, GeneratedAt: manifest.GeneratedAt}, nil
	}
	targets := make([]Target, 0)
	for _, shard := range manifest.Shards {
		raw, err := s.Files.ReadFile(ctx, shard.Path)
		if err != nil {
			return Snapshot{}, fmt.Errorf("read federation shard %s: %w", shard.ID, err)
		}
		if shard.SHA256 != "" {
			sum := sha256.Sum256(raw)
			if got := hex.EncodeToString(sum[:]); got != shard.SHA256 {
				return Snapshot{}, fmt.Errorf("federation shard %s checksum mismatch: got %s want %s", shard.ID, got, shard.SHA256)
			}
		}
		var shardSnapshot Snapshot
		if err := json.Unmarshal(raw, &shardSnapshot); err != nil {
			return Snapshot{}, fmt.Errorf("parse federation shard %s: %w", shard.ID, err)
		}
		targets = append(targets, shardSnapshot.Targets...)
	}
	return Snapshot{Version: manifest.Version, GeneratedAt: manifest.GeneratedAt, Targets: targets}, nil
}

func (s *MonoFSSnapshotStore) Save(ctx context.Context, snapshot Snapshot, expectedVersion string) (string, error) {
	if s == nil || s.Files == nil {
		return "", errors.New("federation snapshot file store is required")
	}
	if len(snapshot.Targets) == 0 {
		return "", errors.New("federation snapshot contains no targets")
	}
	// Validate before writing anything.
	if _, err := NewCatalog(snapshot.Targets, snapshot.Version); err != nil {
		return "", err
	}

	version := strings.TrimSpace(snapshot.Version)
	publishID := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	if version == "" {
		version = publishID
	}
	shardSize := s.ShardTargets
	if shardSize <= 0 {
		shardSize = 500
	}
	shards := make([]ShardRef, 0, (len(snapshot.Targets)+shardSize-1)/shardSize)
	for index, start := 0, 0; start < len(snapshot.Targets); index, start = index+1, start+shardSize {
		end := start + shardSize
		if end > len(snapshot.Targets) {
			end = len(snapshot.Targets)
		}
		shardSnapshot := Snapshot{
			Version:     version,
			GeneratedAt: snapshot.GeneratedAt,
			Targets:     snapshot.Targets[start:end],
		}
		raw, err := json.Marshal(shardSnapshot)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(raw)
		shardPath := path.Join(s.tenantRoot(), "shards", publishID, fmt.Sprintf("%04d.json", index))
		if _, err := s.Files.UpsertPaths(ctx, s.Token, []store.PathWrite{{
			LogicalPath:       shardPath,
			Content:           raw,
			ExpectedVersionID: "absent",
		}}, store.MutationContext{PrincipalID: s.Principal, Reason: "federation catalog shard publish"}); err != nil {
			return "", fmt.Errorf("write federation shard %s: %w", shardPath, err)
		}
		shards = append(shards, ShardRef{
			ID:      fmt.Sprintf("%04d", index),
			Path:    shardPath,
			Targets: end - start,
			SHA256:  hex.EncodeToString(sum[:]),
		})
	}

	manifest := Manifest{
		Version:     version,
		GeneratedAt: time.Now().UTC(),
		Shards:      shards,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	expected := strings.TrimSpace(expectedVersion)
	if expected != "" && expected != "absent" {
		current, loadErr := s.Load(ctx)
		if loadErr != nil && !errors.Is(loadErr, ErrSnapshotNotFound) {
			return "", loadErr
		}
		if loadErr == nil && current.Version != expected {
			return "", fmt.Errorf("federation snapshot version conflict: current=%s expected=%s", current.Version, expected)
		}
	}
	expected = s.latestVersion(ctx)
	if _, err := s.Files.UpsertPaths(ctx, s.Token, []store.PathWrite{{
		LogicalPath:       s.manifestPath(),
		Content:           raw,
		ExpectedVersionID: expected,
	}}, store.MutationContext{PrincipalID: s.Principal, Reason: "federation catalog manifest publish"}); err != nil {
		return "", fmt.Errorf("write federation manifest: %w", err)
	}
	return version, nil
}

func (s *MonoFSSnapshotStore) latestVersion(ctx context.Context) string {
	if s == nil || s.Files == nil {
		return "absent"
	}
	versions, err := s.Files.ListVersions(ctx, s.Token, s.manifestPath())
	if err != nil || len(versions) == 0 {
		return "absent"
	}
	return versions[0].VersionID
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	out := snapshot
	out.Targets = append([]Target(nil), snapshot.Targets...)
	for index := range out.Targets {
		out.Targets[index].Partitions = append([]string(nil), snapshot.Targets[index].Partitions...)
		out.Targets[index].MetricNames = append([]string(nil), snapshot.Targets[index].MetricNames...)
		out.Targets[index].LogGroups = append([]string(nil), snapshot.Targets[index].LogGroups...)
		out.Targets[index].XrayServices = append([]string(nil), snapshot.Targets[index].XrayServices...)
	}
	return out
}
