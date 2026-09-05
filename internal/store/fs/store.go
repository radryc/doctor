package fs

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/store"
)

type Store struct {
	root string
	mu   sync.Mutex
}

type fileMeta struct {
	Current string              `json:"current"`
	History []store.FileVersion `json:"history"`
}

func Open(root string) (*Store, error) {
	if root == "" {
		root = ".doctor/catalog"
	}
	if err := os.MkdirAll(filepath.Join(root, ".versions", "meta"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, ".versions", "data"), 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) ReadFile(ctx context.Context, logicalPath string) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return os.ReadFile(s.logicalToPhysical(logicalPath))
}

func (s *Store) ListDir(ctx context.Context, logicalDir string) ([]store.DirEntry, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	entries, err := os.ReadDir(s.logicalToPhysical(logicalDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]store.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if store.NormalizeLogicalPath(logicalDir) == "/" && entry.Name() == ".versions" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, store.DirEntry{
			Name:  entry.Name(),
			IsDir: entry.IsDir(),
			Size:  info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) Stat(ctx context.Context, logicalPath string) (store.FileInfo, error) {
	select {
	case <-ctx.Done():
		return store.FileInfo{}, ctx.Err()
	default:
	}
	info, err := os.Stat(s.logicalToPhysical(logicalPath))
	if err != nil {
		return store.FileInfo{}, err
	}
	meta, err := s.loadMeta(logicalPath)
	if err != nil {
		return store.FileInfo{}, err
	}
	versionID := ""
	if meta != nil {
		versionID = meta.Current
	}
	return store.FileInfo{
		Path:      store.NormalizeLogicalPath(logicalPath),
		Size:      info.Size(),
		VersionID: versionID,
		ModTime:   info.ModTime().UTC(),
	}, nil
}

func (s *Store) UpsertFiles(ctx context.Context, batch store.MutationBatch) (store.BatchRevision, error) {
	select {
	case <-ctx.Done():
		return store.BatchRevision{}, ctx.Err()
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, write := range batch.Writes {
		meta, err := s.loadMeta(write.LogicalPath)
		if err != nil {
			return store.BatchRevision{}, err
		}
		current := ""
		if meta != nil {
			current = meta.Current
		}
		if err := checkExpected(current, write.ExpectedVersionID); err != nil {
			return store.BatchRevision{}, fmt.Errorf("write %s: %w", write.LogicalPath, err)
		}
	}

	now := time.Now().UTC()
	result := store.BatchRevision{BatchRevisionID: store.NewBatchRevisionID()}
	for _, write := range batch.Writes {
		logicalPath := store.NormalizeLogicalPath(write.LogicalPath)
		version := store.FileVersion{
			LogicalPath:     logicalPath,
			VersionID:       store.NewVersionID(),
			BatchRevisionID: result.BatchRevisionID,
			ContentSHA256:   store.ContentHash(write.Content),
			CommittedAt:     now,
			PrincipalID:     batch.Context.PrincipalID,
			Reason:          batch.Context.Reason,
		}
		if err := os.MkdirAll(filepath.Dir(s.logicalToPhysical(logicalPath)), 0o755); err != nil {
			return store.BatchRevision{}, err
		}
		if err := os.WriteFile(s.logicalToPhysical(logicalPath), write.Content, 0o644); err != nil {
			return store.BatchRevision{}, err
		}
		if err := os.MkdirAll(s.versionDataDir(logicalPath), 0o755); err != nil {
			return store.BatchRevision{}, err
		}
		if err := os.WriteFile(filepath.Join(s.versionDataDir(logicalPath), version.VersionID), write.Content, 0o644); err != nil {
			return store.BatchRevision{}, err
		}
		meta, err := s.loadMeta(logicalPath)
		if err != nil {
			return store.BatchRevision{}, err
		}
		if meta == nil {
			meta = &fileMeta{}
		}
		meta.Current = version.VersionID
		meta.History = append(meta.History, version)
		if err := s.storeMeta(logicalPath, meta); err != nil {
			return store.BatchRevision{}, err
		}
		result.Files = append(result.Files, version)
	}
	return result, nil
}

func (s *Store) DeletePaths(ctx context.Context, batch store.DeleteBatch) (store.BatchRevision, error) {
	select {
	case <-ctx.Done():
		return store.BatchRevision{}, ctx.Err()
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, del := range batch.Deletes {
		meta, err := s.loadMeta(del.LogicalPath)
		if err != nil {
			return store.BatchRevision{}, err
		}
		current := ""
		if meta != nil {
			current = meta.Current
		}
		if err := checkExpected(current, del.ExpectedVersionID); err != nil {
			return store.BatchRevision{}, fmt.Errorf("delete %s: %w", del.LogicalPath, err)
		}
	}

	now := time.Now().UTC()
	result := store.BatchRevision{BatchRevisionID: store.NewBatchRevisionID()}
	for _, del := range batch.Deletes {
		logicalPath := store.NormalizeLogicalPath(del.LogicalPath)
		version := store.FileVersion{
			LogicalPath:     logicalPath,
			VersionID:       store.NewVersionID(),
			BatchRevisionID: result.BatchRevisionID,
			ContentSHA256:   store.ContentHash(nil),
			CommittedAt:     now,
			Tombstone:       true,
			PrincipalID:     batch.Context.PrincipalID,
			Reason:          batch.Context.Reason,
		}
		_ = os.Remove(s.logicalToPhysical(logicalPath))
		meta, err := s.loadMeta(logicalPath)
		if err != nil {
			return store.BatchRevision{}, err
		}
		if meta == nil {
			meta = &fileMeta{}
		}
		meta.Current = version.VersionID
		meta.History = append(meta.History, version)
		if err := s.storeMeta(logicalPath, meta); err != nil {
			return store.BatchRevision{}, err
		}
		result.Files = append(result.Files, version)
	}
	return result, nil
}

func (s *Store) ListVersions(ctx context.Context, logicalPath string) ([]store.FileVersion, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	meta, err := s.loadMeta(logicalPath)
	if err != nil || meta == nil {
		return nil, err
	}
	out := make([]store.FileVersion, 0, len(meta.History))
	for i := len(meta.History) - 1; i >= 0; i-- {
		out = append(out, meta.History[i])
	}
	return out, nil
}

func (s *Store) GetVersion(ctx context.Context, logicalPath, versionID string) (store.VersionedFile, error) {
	select {
	case <-ctx.Done():
		return store.VersionedFile{}, ctx.Err()
	default:
	}
	meta, err := s.loadMeta(logicalPath)
	if err != nil || meta == nil {
		return store.VersionedFile{}, err
	}
	for _, version := range meta.History {
		if version.VersionID != versionID {
			continue
		}
		var content []byte
		if !version.Tombstone {
			content, err = os.ReadFile(filepath.Join(s.versionDataDir(logicalPath), versionID))
			if err != nil {
				return store.VersionedFile{}, err
			}
		}
		return store.VersionedFile{Version: version, Content: content}, nil
	}
	return store.VersionedFile{}, os.ErrNotExist
}

func (s *Store) logicalToPhysical(logicalPath string) string {
	clean := store.NormalizeLogicalPath(logicalPath)
	trimmed := strings.TrimPrefix(clean, "/")
	if trimmed == "" {
		return s.root
	}
	return filepath.Join(s.root, filepath.FromSlash(trimmed))
}

func (s *Store) metaPath(logicalPath string) string {
	return filepath.Join(s.root, ".versions", "meta", hashPath(logicalPath)+".json")
}

func (s *Store) versionDataDir(logicalPath string) string {
	return filepath.Join(s.root, ".versions", "data", hashPath(logicalPath))
}

func hashPath(logicalPath string) string {
	return hex.EncodeToString([]byte(store.NormalizeLogicalPath(logicalPath)))
}

func (s *Store) loadMeta(logicalPath string) (*fileMeta, error) {
	data, err := os.ReadFile(s.metaPath(logicalPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var meta fileMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (s *Store) storeMeta(logicalPath string, meta *fileMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.metaPath(logicalPath), data, 0o644)
}

func checkExpected(current, expected string) error {
	switch expected {
	case "":
		return nil
	case "absent":
		if current != "" {
			return store.ErrConflict
		}
		return nil
	default:
		if current != expected {
			return store.ErrConflict
		}
		return nil
	}
}

var _ store.Store = (*Store)(nil)
