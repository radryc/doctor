package ruler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/radryc/doctor/internal/store"
	"sigs.k8s.io/yaml"
)

// ErrNotFound reports a missing rule group.
var ErrNotFound = errors.New("ruler: rule group not found")

// FileStore is the file-catalog surface the ruler needs. It is implemented by
// internal/store/monofs.GRPCClient and by MemoryStore for tests.
type FileStore interface {
	ReadFile(ctx context.Context, logicalPath string) ([]byte, error)
	ListDir(ctx context.Context, logicalDir string) ([]store.DirEntry, error)
	UpsertPaths(ctx context.Context, token string, writes []store.PathWrite, mutation store.MutationContext) (store.BatchRevision, error)
	DeletePaths(ctx context.Context, token string, deletes []store.PathDelete, mutation store.MutationContext) (store.BatchRevision, error)
	ListVersions(ctx context.Context, token, logicalPath string) ([]store.FileVersion, error)
}

// GroupRef identifies one stored group.
type GroupRef struct {
	Tenant string
	Group  string
	File   *File
}

// Store persists rule group files.
type Store struct {
	Files     FileStore
	Token     string
	Root      string
	Principal string
}

// NewStore builds a rule store rooted at root (default /doctor/v1/rules).
func NewStore(files FileStore, token, root string) *Store {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" {
		root = "doctor/v1/rules"
	}
	return &Store{Files: files, Token: token, Root: "/" + root, Principal: "doctor-rules"}
}

func (s *Store) groupPath(tenant, group string) string {
	return fmt.Sprintf("%s/%s/%s.yaml", s.Root, tenant, group)
}

// Get loads one group file.
func (s *Store) Get(ctx context.Context, tenant, group string) (*File, error) {
	data, err := s.Files.ReadFile(ctx, s.groupPath(tenant, group))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	file, err := ParseFile(data)
	if err != nil {
		return nil, err
	}
	for i := range file.Groups {
		if strings.TrimSpace(file.Groups[i].Tenant) == "" {
			file.Groups[i].Tenant = tenant
		}
	}
	return file, nil
}

// List walks the store and returns all groups.
func (s *Store) List(ctx context.Context) ([]GroupRef, error) {
	tenants, err := s.Files.ListDir(ctx, s.Root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	refs := make([]GroupRef, 0)
	for _, tenant := range tenants {
		if !tenant.IsDir {
			continue
		}
		files, err := s.Files.ListDir(ctx, s.Root+"/"+tenant.Name)
		if err != nil {
			continue
		}
		for _, entry := range files {
			if entry.IsDir || !strings.HasSuffix(entry.Name, ".yaml") {
				continue
			}
			group := strings.TrimSuffix(entry.Name, ".yaml")
			file, err := s.Get(ctx, tenant.Name, group)
			if err != nil {
				continue
			}
			refs = append(refs, GroupRef{Tenant: tenant.Name, Group: group, File: file})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Tenant != refs[j].Tenant {
			return refs[i].Tenant < refs[j].Tenant
		}
		return refs[i].Group < refs[j].Group
	})
	return refs, nil
}

// Put writes a group file. createOnly uses an "absent" precondition.
func (s *Store) Put(ctx context.Context, tenant, group string, file *File, createOnly bool) error {
	data, err := yaml.Marshal(file)
	if err != nil {
		return fmt.Errorf("ruler: encode rules: %w", err)
	}
	expected := "absent"
	if !createOnly {
		expected = s.latestVersion(ctx, s.groupPath(tenant, group))
	}
	_, err = s.Files.UpsertPaths(ctx, s.Token, []store.PathWrite{{
		LogicalPath:       s.groupPath(tenant, group),
		Content:           data,
		ExpectedVersionID: expected,
	}}, store.MutationContext{PrincipalID: s.Principal, Reason: "rules update"})
	return err
}

// Delete removes a group file with a version precondition.
func (s *Store) Delete(ctx context.Context, tenant, group string) error {
	expected := s.latestVersion(ctx, s.groupPath(tenant, group))
	if expected == "absent" {
		return ErrNotFound
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []store.PathDelete{{
		LogicalPath:       s.groupPath(tenant, group),
		ExpectedVersionID: expected,
	}}, store.MutationContext{PrincipalID: s.Principal, Reason: "rules delete"})
	return err
}

func (s *Store) latestVersion(ctx context.Context, logicalPath string) string {
	versions, err := s.Files.ListVersions(ctx, s.Token, logicalPath)
	if err != nil || len(versions) == 0 {
		return "absent"
	}
	return versions[0].VersionID
}

// MemoryStore is an in-memory FileStore used by tests and benchmarks. It
// enforces the same CAS preconditions as the MonoFS store.
type MemoryStore struct {
	mu       sync.Mutex
	files    map[string][]byte
	versions map[string][]store.FileVersion
	seq      int
}

// NewMemoryStore builds an empty in-memory file store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{files: map[string][]byte{}, versions: map[string][]store.FileVersion{}}
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
func (m *MemoryStore) ListDir(_ context.Context, logicalDir string) ([]store.DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := strings.TrimRight(logicalDir, "/") + "/"
	seen := map[string]store.DirEntry{}
	for path := range m.files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		if rest == "" {
			continue
		}
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			seen[rest[:idx]] = store.DirEntry{Name: rest[:idx], IsDir: true}
		} else {
			seen[rest] = store.DirEntry{Name: rest}
		}
	}
	out := make([]store.DirEntry, 0, len(seen))
	for _, entry := range seen {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// UpsertPaths implements FileStore.
func (m *MemoryStore) UpsertPaths(_ context.Context, _ string, writes []store.PathWrite, mutation store.MutationContext) (store.BatchRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	revision := store.BatchRevision{BatchRevisionID: store.NewBatchRevisionID()}
	for _, write := range writes {
		if err := m.checkExpected(write.LogicalPath, write.ExpectedVersionID); err != nil {
			return store.BatchRevision{}, err
		}
		m.seq++
		version := store.FileVersion{
			LogicalPath:     write.LogicalPath,
			VersionID:       fmt.Sprintf("ver-%d", m.seq),
			BatchRevisionID: revision.BatchRevisionID,
			ContentSHA256:   store.ContentHash(write.Content),
			PrincipalID:     mutation.PrincipalID,
			Reason:          mutation.Reason,
		}
		m.files[write.LogicalPath] = append([]byte(nil), write.Content...)
		m.versions[write.LogicalPath] = append([]store.FileVersion{version}, m.versions[write.LogicalPath]...)
		revision.Files = append(revision.Files, version)
	}
	return revision, nil
}

// DeletePaths implements FileStore.
func (m *MemoryStore) DeletePaths(_ context.Context, _ string, deletes []store.PathDelete, mutation store.MutationContext) (store.BatchRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	revision := store.BatchRevision{BatchRevisionID: store.NewBatchRevisionID()}
	for _, del := range deletes {
		if err := m.checkExpected(del.LogicalPath, del.ExpectedVersionID); err != nil {
			return store.BatchRevision{}, err
		}
		m.seq++
		version := store.FileVersion{
			LogicalPath:     del.LogicalPath,
			VersionID:       fmt.Sprintf("ver-%d", m.seq),
			BatchRevisionID: revision.BatchRevisionID,
			Tombstone:       true,
			PrincipalID:     mutation.PrincipalID,
			Reason:          mutation.Reason,
		}
		delete(m.files, del.LogicalPath)
		m.versions[del.LogicalPath] = append([]store.FileVersion{version}, m.versions[del.LogicalPath]...)
		revision.Files = append(revision.Files, version)
	}
	return revision, nil
}

// ListVersions implements FileStore.
func (m *MemoryStore) ListVersions(_ context.Context, _, logicalPath string) ([]store.FileVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.FileVersion(nil), m.versions[logicalPath]...), nil
}

func (m *MemoryStore) checkExpected(logicalPath, expected string) error {
	switch expected {
	case "":
		return nil
	case "absent":
		if _, exists := m.files[logicalPath]; exists {
			return store.ErrConflict
		}
		return nil
	default:
		versions := m.versions[logicalPath]
		if len(versions) == 0 || versions[0].VersionID != expected {
			return store.ErrConflict
		}
		return nil
	}
}
