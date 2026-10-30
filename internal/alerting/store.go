package alerting

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	doctorstore "github.com/radryc/doctor/internal/store"
	"sigs.k8s.io/yaml"
)

// ErrNotFound reports a missing alert, silence, or config.
var ErrNotFound = errors.New("alerting: not found")

// FileStore is the CAS-capable file surface Sentinel needs. It is implemented
// by internal/store/monofs.GRPCClient and by MemoryStore for tests.
type FileStore interface {
	ReadFile(ctx context.Context, logicalPath string) ([]byte, error)
	ListDir(ctx context.Context, logicalDir string) ([]doctorstore.DirEntry, error)
	UpsertPaths(ctx context.Context, token string, writes []doctorstore.PathWrite, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error)
	DeletePaths(ctx context.Context, token string, deletes []doctorstore.PathDelete, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error)
	ListVersions(ctx context.Context, token, logicalPath string) ([]doctorstore.FileVersion, error)
}

// Store persists alerts, silences, outbox records, and receiver config.
type Store struct {
	Files     FileStore
	Token     string
	Root      string
	Principal string
}

// NewStore builds a store rooted at root (default /doctor/v1/alerts).
func NewStore(files FileStore, token, root string) *Store {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" {
		root = "doctor/v1/alerts"
	}
	return &Store{Files: files, Token: token, Root: "/" + root, Principal: "doctor-sentinel"}
}

func (s *Store) statePath(tenant, partition, fingerprint string) string {
	return fmt.Sprintf("%s/state/%s/%s/%s.json", s.Root, tenant, partition, fingerprint)
}

func (s *Store) silencePath(tenant, partition, id string) string {
	return fmt.Sprintf("%s/silences/%s/%s/%s.yaml", s.Root, tenant, partition, id)
}

func (s *Store) outboxPath(tenant, partition, id string) string {
	return fmt.Sprintf("%s/outbox/%s/%s/%s.json", s.Root, tenant, partition, id)
}

func (s *Store) receiverPath() string {
	return s.Root + "/config/receivers.yaml"
}

func (s *Store) policyPath() string {
	return s.Root + "/config/sources/aws.yaml"
}

// GetAlert loads one alert state.
func (s *Store) GetAlert(ctx context.Context, tenant, partition, fingerprint string) (*Alert, error) {
	data, err := s.Files.ReadFile(ctx, s.statePath(tenant, partition, fingerprint))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var alert Alert
	if err := yaml.Unmarshal(data, &alert); err != nil {
		return nil, fmt.Errorf("alerting: decode alert: %w", err)
	}
	return &alert, nil
}

// ListAlerts loads every alert for a tenant/partition.
func (s *Store) ListAlerts(ctx context.Context, tenant, partition string) ([]Alert, error) {
	dir := fmt.Sprintf("%s/state/%s/%s", s.Root, tenant, partition)
	entries, err := s.Files.ListDir(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Alert, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		fingerprint := strings.TrimSuffix(entry.Name, ".json")
		alert, err := s.GetAlert(ctx, tenant, partition, fingerprint)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, *alert)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].Fingerprint < out[j].Fingerprint
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// SaveAlert writes alert state, optionally with outbox records in the same CAS
// batch.
func (s *Store) SaveAlert(ctx context.Context, alert Alert, outbox []OutboxRecord) error {
	alert.UpdatedAt = time.Now().UTC()
	data, err := yaml.Marshal(alert)
	if err != nil {
		return fmt.Errorf("alerting: encode alert: %w", err)
	}
	writes := []doctorstore.PathWrite{{
		LogicalPath:       s.statePath(alert.Tenant, alert.Partition, alert.Fingerprint),
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, s.statePath(alert.Tenant, alert.Partition, alert.Fingerprint)),
	}}
	for i := range outbox {
		record := &outbox[i]
		if record.ID == "" {
			record.ID = fmt.Sprintf("%s-%d", alert.Fingerprint, time.Now().UTC().UnixNano())
		}
		if record.CreatedAt.IsZero() {
			record.CreatedAt = time.Now().UTC()
		}
		record.UpdatedAt = time.Now().UTC()
		payload, err := yaml.Marshal(record)
		if err != nil {
			return fmt.Errorf("alerting: encode outbox: %w", err)
		}
		writes = append(writes, doctorstore.PathWrite{
			LogicalPath:       s.outboxPath(record.Tenant, record.Partition, record.ID),
			Content:           payload,
			ExpectedVersionID: s.latestVersion(ctx, s.outboxPath(record.Tenant, record.Partition, record.ID)),
		})
	}
	_, err = s.Files.UpsertPaths(ctx, s.Token, writes, doctorstore.MutationContext{
		PrincipalID: s.Principal,
		Reason:      "alert state transition",
	})
	return err
}

// DeleteAlert removes alert state.
func (s *Store) DeleteAlert(ctx context.Context, alert Alert) error {
	path := s.statePath(alert.Tenant, alert.Partition, alert.Fingerprint)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return nil
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
		LogicalPath:       path,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "alert resolved"})
	return err
}

// ListSilences returns silences for a partition.
func (s *Store) ListSilences(ctx context.Context, tenant, partition string) ([]Silence, error) {
	dir := fmt.Sprintf("%s/silences/%s/%s", s.Root, tenant, partition)
	entries, err := s.Files.ListDir(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Silence, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".yaml") {
			continue
		}
		data, err := s.Files.ReadFile(ctx, dir+"/"+entry.Name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var silence Silence
		if err := yaml.Unmarshal(data, &silence); err != nil {
			continue
		}
		out = append(out, silence)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// GetSilence loads one silence.
func (s *Store) GetSilence(ctx context.Context, tenant, partition, id string) (*Silence, error) {
	data, err := s.Files.ReadFile(ctx, s.silencePath(tenant, partition, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var silence Silence
	if err := yaml.Unmarshal(data, &silence); err != nil {
		return nil, err
	}
	return &silence, nil
}

// PutSilence creates or updates a silence.
func (s *Store) PutSilence(ctx context.Context, silence Silence) error {
	silence.UpdatedAt = time.Now().UTC()
	if silence.CreatedAt.IsZero() {
		silence.CreatedAt = silence.UpdatedAt
	}
	data, err := yaml.Marshal(silence)
	if err != nil {
		return err
	}
	path := s.silencePath(silence.Tenant, silence.Partition, silence.ID)
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "silence update"})
	return err
}

// DeleteSilence removes a silence.
func (s *Store) DeleteSilence(ctx context.Context, tenant, partition, id string) error {
	path := s.silencePath(tenant, partition, id)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return ErrNotFound
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
		LogicalPath:       path,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "silence delete"})
	return err
}

// GetReceiverConfig loads notification receivers/routes, returning a default
// local config when none is stored.
func (s *Store) GetReceiverConfig(ctx context.Context) (*ReceiverConfig, error) {
	data, err := s.Files.ReadFile(ctx, s.receiverPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultReceiverConfig(), nil
		}
		return nil, err
	}
	var cfg ReceiverConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// PutReceiverConfig stores notification receivers/routes.
func (s *Store) PutReceiverConfig(ctx context.Context, cfg ReceiverConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	path := s.receiverPath()
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "receiver config update"})
	return err
}

// ListOutbox returns pending outbox records for a partition.
func (s *Store) ListOutbox(ctx context.Context, tenant, partition string) ([]OutboxRecord, error) {
	dir := fmt.Sprintf("%s/outbox/%s/%s", s.Root, tenant, partition)
	entries, err := s.Files.ListDir(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]OutboxRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		data, err := s.Files.ReadFile(ctx, dir+"/"+entry.Name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var record OutboxRecord
		if err := yaml.Unmarshal(data, &record); err != nil {
			continue
		}
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ListOutboxAll walks tenants and partitions. It is used by the delivery loop.
func (s *Store) ListOutboxAll(ctx context.Context) ([]OutboxRecord, error) {
	root := s.Root + "/outbox"
	tenants, err := s.Files.ListDir(ctx, root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []OutboxRecord
	for _, tenant := range tenants {
		if !tenant.IsDir {
			continue
		}
		partitions, err := s.Files.ListDir(ctx, root+"/"+tenant.Name)
		if err != nil {
			continue
		}
		for _, partition := range partitions {
			if !partition.IsDir {
				continue
			}
			records, err := s.ListOutbox(ctx, tenant.Name, partition.Name)
			if err == nil {
				out = append(out, records...)
			}
		}
	}
	return out, nil
}

// PutOutbox updates one outbox record.
func (s *Store) PutOutbox(ctx context.Context, record OutboxRecord) error {
	record.UpdatedAt = time.Now().UTC()
	data, err := yaml.Marshal(record)
	if err != nil {
		return err
	}
	path := s.outboxPath(record.Tenant, record.Partition, record.ID)
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "outbox update"})
	return err
}

// DeleteOutbox removes a delivered record.
func (s *Store) DeleteOutbox(ctx context.Context, record OutboxRecord) error {
	path := s.outboxPath(record.Tenant, record.Partition, record.ID)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return nil
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
		LogicalPath:       path,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "outbox delivered"})
	return err
}

// GetAWSPolicy loads the CloudWatch alarm-selection policy.
func (s *Store) GetAWSPolicy(ctx context.Context) (*AWSPolicy, error) {
	data, err := s.Files.ReadFile(ctx, s.policyPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultAWSPolicy(), nil
		}
		return nil, err
	}
	var policy AWSPolicy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return nil, err
	}
	if policy.Default == "" {
		policy.Default = AWSPolicyManagedOnly
	}
	return &policy, nil
}

// PutAWSPolicy stores the CloudWatch alarm-selection policy.
func (s *Store) PutAWSPolicy(ctx context.Context, policy AWSPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(policy)
	if err != nil {
		return err
	}
	path := s.policyPath()
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "aws alarm selection policy update"})
	return err
}

func (s *Store) latestVersion(ctx context.Context, logicalPath string) string {
	versions, err := s.Files.ListVersions(ctx, s.Token, logicalPath)
	if err != nil || len(versions) == 0 {
		return "absent"
	}
	return versions[0].VersionID
}

// DefaultReceiverConfig returns a no-op config with a local webhook placeholder.
func DefaultReceiverConfig() *ReceiverConfig {
	return &ReceiverConfig{}
}

// MemoryStore is an in-memory FileStore for tests and local development.
type MemoryStore struct {
	mu       sync.Mutex
	files    map[string][]byte
	versions map[string][]doctorstore.FileVersion
	seq      int
}

// NewMemoryStore creates an empty memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{files: map[string][]byte{}, versions: map[string][]doctorstore.FileVersion{}}
}

// ReadFile implements FileStore.
func (m *MemoryStore) ReadFile(_ context.Context, logicalPath string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[logicalPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

// ListDir implements FileStore.
func (m *MemoryStore) ListDir(_ context.Context, logicalDir string) ([]doctorstore.DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := strings.TrimRight(logicalDir, "/") + "/"
	seen := map[string]doctorstore.DirEntry{}
	for path := range m.files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		if rest == "" {
			continue
		}
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			seen[rest[:idx]] = doctorstore.DirEntry{Name: rest[:idx], IsDir: true}
		} else {
			seen[rest] = doctorstore.DirEntry{Name: rest}
		}
	}
	out := make([]doctorstore.DirEntry, 0, len(seen))
	for _, entry := range seen {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 {
		return nil, os.ErrNotExist
	}
	return out, nil
}

// UpsertPaths implements FileStore.
func (m *MemoryStore) UpsertPaths(_ context.Context, _ string, writes []doctorstore.PathWrite, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	revision := doctorstore.BatchRevision{BatchRevisionID: doctorstore.NewBatchRevisionID()}
	for _, write := range writes {
		if err := m.checkExpected(write.LogicalPath, write.ExpectedVersionID); err != nil {
			return doctorstore.BatchRevision{}, err
		}
		m.seq++
		version := doctorstore.FileVersion{
			LogicalPath:     write.LogicalPath,
			VersionID:       fmt.Sprintf("ver-%d", m.seq),
			BatchRevisionID: revision.BatchRevisionID,
			ContentSHA256:   doctorstore.ContentHash(write.Content),
			PrincipalID:     mutation.PrincipalID,
			Reason:          mutation.Reason,
			CommittedAt:     time.Now().UTC(),
		}
		m.files[write.LogicalPath] = append([]byte(nil), write.Content...)
		m.versions[write.LogicalPath] = append([]doctorstore.FileVersion{version}, m.versions[write.LogicalPath]...)
		revision.Files = append(revision.Files, version)
	}
	return revision, nil
}

// DeletePaths implements FileStore.
func (m *MemoryStore) DeletePaths(_ context.Context, _ string, deletes []doctorstore.PathDelete, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	revision := doctorstore.BatchRevision{BatchRevisionID: doctorstore.NewBatchRevisionID()}
	for _, del := range deletes {
		if err := m.checkExpected(del.LogicalPath, del.ExpectedVersionID); err != nil {
			return doctorstore.BatchRevision{}, err
		}
		m.seq++
		version := doctorstore.FileVersion{
			LogicalPath:     del.LogicalPath,
			VersionID:       fmt.Sprintf("ver-%d", m.seq),
			BatchRevisionID: revision.BatchRevisionID,
			Tombstone:       true,
			PrincipalID:     mutation.PrincipalID,
			Reason:          mutation.Reason,
			CommittedAt:     time.Now().UTC(),
		}
		delete(m.files, del.LogicalPath)
		m.versions[del.LogicalPath] = append([]doctorstore.FileVersion{version}, m.versions[del.LogicalPath]...)
		revision.Files = append(revision.Files, version)
	}
	return revision, nil
}

// ListVersions implements FileStore.
func (m *MemoryStore) ListVersions(_ context.Context, _, logicalPath string) ([]doctorstore.FileVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]doctorstore.FileVersion(nil), m.versions[logicalPath]...), nil
}

func (m *MemoryStore) checkExpected(logicalPath, expected string) error {
	switch expected {
	case "":
		return nil
	case "absent":
		if _, exists := m.files[logicalPath]; exists {
			return doctorstore.ErrConflict
		}
		return nil
	default:
		versions := m.versions[logicalPath]
		if len(versions) == 0 || versions[0].VersionID != expected {
			return doctorstore.ErrConflict
		}
		return nil
	}
}
