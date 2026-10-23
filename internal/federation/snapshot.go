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

// SnapshotVersioner is an optional fast path for reload loops. It lets a
// process check the current snapshot version without reading every shard.
type SnapshotVersioner interface {
	Version(ctx context.Context) (string, error)
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

func (s *FileSnapshotStore) Version(ctx context.Context) (string, error) {
	snapshot, err := s.Load(ctx)
	if err != nil {
		return "", err
	}
	return snapshot.Version, nil
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

func (s *MemorySnapshotStore) Version(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.exists {
		return "", ErrSnapshotNotFound
	}
	return s.snapshot.Version, nil
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
	ShardWorkers int
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
		ShardWorkers: 16,
		Principal:    "doctor-federation",
	}
}

func (s *MonoFSSnapshotStore) tenantRoot() string {
	return path.Join(s.Root, s.Tenant)
}

func (s *MonoFSSnapshotStore) manifestPath() string {
	return path.Join(s.tenantRoot(), "manifest.json")
}

func (s *MonoFSSnapshotStore) workerCount(count int) int {
	workers := s.ShardWorkers
	if workers <= 0 {
		workers = 16
	}
	if workers > count {
		workers = count
	}
	if workers < 1 {
		return 1
	}
	return workers
}

func (s *MonoFSSnapshotStore) readManifest(ctx context.Context) (Manifest, error) {
	data, err := s.Files.ReadFile(ctx, s.manifestPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Manifest{}, ErrSnapshotNotFound
		}
		return Manifest{}, fmt.Errorf("read federation manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse federation manifest: %w", err)
	}
	return manifest, nil
}

// Version returns the current manifest version without loading shards.
func (s *MonoFSSnapshotStore) Version(ctx context.Context) (string, error) {
	if s == nil || s.Files == nil {
		return "", errors.New("federation snapshot file store is required")
	}
	manifest, err := s.readManifest(ctx)
	if err != nil {
		return "", err
	}
	return manifest.Version, nil
}

func (s *MonoFSSnapshotStore) loadShard(ctx context.Context, shard ShardRef) ([]Target, error) {
	raw, err := s.Files.ReadFile(ctx, shard.Path)
	if err != nil {
		return nil, fmt.Errorf("read federation shard %s: %w", shard.ID, err)
	}
	if shard.SHA256 != "" {
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != shard.SHA256 {
			return nil, fmt.Errorf("federation shard %s checksum mismatch: got %s want %s", shard.ID, got, shard.SHA256)
		}
	}
	var shardSnapshot Snapshot
	if err := json.Unmarshal(raw, &shardSnapshot); err != nil {
		return nil, fmt.Errorf("parse federation shard %s: %w", shard.ID, err)
	}
	if shard.Targets > 0 && len(shardSnapshot.Targets) != shard.Targets {
		return nil, fmt.Errorf("federation shard %s target count mismatch: got %d want %d", shard.ID, len(shardSnapshot.Targets), shard.Targets)
	}
	return shardSnapshot.Targets, nil
}

func (s *MonoFSSnapshotStore) Load(ctx context.Context) (Snapshot, error) {
	if s == nil || s.Files == nil {
		return Snapshot{}, errors.New("federation snapshot file store is required")
	}
	manifest, err := s.readManifest(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if len(manifest.Shards) == 0 {
		return Snapshot{Version: manifest.Version, GeneratedAt: manifest.GeneratedAt}, nil
	}

	// Shards are immutable and independent; load them with a bounded worker
	// pool instead of one round trip per shard. Results are kept in manifest
	// order so the assembled snapshot is deterministic.
	parts := make([][]Target, len(manifest.Shards))
	errs := make([]error, len(manifest.Shards))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < s.workerCount(len(manifest.Shards)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				targets, loadErr := s.loadShard(ctx, manifest.Shards[index])
				if loadErr != nil {
					errs[index] = loadErr
					continue
				}
				parts[index] = targets
			}
		}()
	}
	for index := range manifest.Shards {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	for index, loadErr := range errs {
		if loadErr != nil {
			return Snapshot{}, fmt.Errorf("load shard %d: %w", index, loadErr)
		}
	}

	total := 0
	for _, targets := range parts {
		total += len(targets)
	}
	targets := make([]Target, 0, total)
	for _, part := range parts {
		targets = append(targets, part...)
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
	shardCount := (len(snapshot.Targets) + shardSize - 1) / shardSize
	shards := make([]ShardRef, 0, shardCount)
	rawShards := make([][]byte, 0, shardCount)
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
		shards = append(shards, ShardRef{
			ID:      fmt.Sprintf("%04d", index),
			Path:    shardPath,
			Targets: end - start,
			SHA256:  hex.EncodeToString(sum[:]),
		})
		rawShards = append(rawShards, raw)
	}

	// Shards are independent immutable writes; the manifest is the only
	// commit point and is written after every shard succeeds.
	errs := make([]error, len(shards))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < s.workerCount(len(shards)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				_, writeErr := s.Files.UpsertPaths(ctx, s.Token, []store.PathWrite{{
					LogicalPath:       shards[index].Path,
					Content:           rawShards[index],
					ExpectedVersionID: "absent",
				}}, store.MutationContext{PrincipalID: s.Principal, Reason: "federation catalog shard publish"})
				if writeErr != nil {
					errs[index] = fmt.Errorf("write federation shard %s: %w", shards[index].Path, writeErr)
				}
			}
		}()
	}
	for index := range shards {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	for index, writeErr := range errs {
		if writeErr != nil {
			return "", fmt.Errorf("shard %d: %w", index, writeErr)
		}
	}

	manifest := Manifest{
		Version:     version,
		GeneratedAt: snapshot.GeneratedAt,
		Shards:      shards,
	}
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = time.Now().UTC()
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}

	expected := strings.TrimSpace(expectedVersion)
	if expected != "" {
		current, loadErr := s.readManifest(ctx)
		switch {
		case errors.Is(loadErr, ErrSnapshotNotFound):
			if expected != "absent" {
				return "", fmt.Errorf("federation snapshot version conflict: current=absent expected=%s", expected)
			}
		case loadErr != nil:
			return "", loadErr
		case expected == "absent" || current.Version != expected:
			return "", fmt.Errorf("federation snapshot version conflict: current=%s expected=%s", current.Version, expected)
		}
		// Re-read after shard writes to narrow the lost-update window before
		// the file-version CAS on the manifest.
		if current, loadErr := s.readManifest(ctx); loadErr == nil && current.Version != expected {
			return "", fmt.Errorf("federation snapshot version conflict: current=%s expected=%s", current.Version, expected)
		}
	}
	expectedFileVersion := s.latestVersion(ctx)
	if expected != "" {
		// If a concurrent publisher changed the manifest after the first
		// checks, fail before the CAS. If it changes after this read, the
		// file-version CAS below rejects the write.
		if current, loadErr := s.readManifest(ctx); loadErr == nil && current.Version != expected {
			return "", fmt.Errorf("federation snapshot version conflict: current=%s expected=%s", current.Version, expected)
		}
	}
	if _, err := s.Files.UpsertPaths(ctx, s.Token, []store.PathWrite{{
		LogicalPath:       s.manifestPath(),
		Content:           raw,
		ExpectedVersionID: expectedFileVersion,
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
