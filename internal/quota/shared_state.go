package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/radryc/doctor/internal/store"
)

// CASStore is the small file-store surface needed for shared quota state.
// MonoFS implements it through ListVersions/GetVersion/UpsertPaths.
type CASStore interface {
	ReadVersion(ctx context.Context, logicalPath string) (content []byte, version string, err error)
	WriteCAS(ctx context.Context, logicalPath string, content []byte, expectedVersion string) error
	ListDir(ctx context.Context, logicalDir string) ([]store.DirEntry, error)
}

type sharedDoc struct {
	Tenant    string    `json:"tenant"`
	Partition string    `json:"partition"`
	Day       string    `json:"day"`
	Bytes     int64     `json:"bytes"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	ReplicaID string    `json:"replica_id,omitempty"`
}

// sharedState implements stateBackend on a CAS file store. It reserves blocks
// of bytes so a replica rarely performs a network CAS operation: admission
// consumes the local reservation, and a new block is reserved when needed.
//
// The shared total includes every replica's reserved-but-unused block, which
// bounds fleet overshoot to roughly replicas × reserve_bytes.
type sharedState struct {
	mu        sync.Mutex
	client    CASStore
	closer    interface{ Close() error }
	root      string
	replicaID string
	reserve   int64
	failMode  string

	totals   map[stateKey]int64
	reserved map[stateKey]int64
}

func newSharedState(client CASStore, root, replicaID string, reserveBytes int64, failMode string) *sharedState {
	if reserveBytes <= 0 {
		reserveBytes = 1 << 20
	}
	root = "/" + strings.Trim(strings.TrimSpace(root), "/")
	if root == "/" {
		root = "/doctor/v1/quota/state"
	}
	return &sharedState{
		client:    client,
		root:      root,
		replicaID: replicaID,
		reserve:   reserveBytes,
		failMode:  strings.ToLower(strings.TrimSpace(failMode)),
		totals:    map[stateKey]int64{},
		reserved:  map[stateKey]int64{},
	}
}

func (s *sharedState) load(ctx context.Context) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("quota: shared state client is nil")
	}
	if err := s.walk(ctx, s.root, 0); err != nil {
		if s.failMode == "open" {
			return nil
		}
		return err
	}
	return nil
}

func (s *sharedState) walk(ctx context.Context, dir string, depth int) error {
	if depth > 4 {
		return nil
	}
	entries, err := s.client.ListDir(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("quota: list shared state %s: %w", dir, err)
	}
	for _, entry := range entries {
		child := path.Join(dir, entry.Name)
		if entry.IsDir {
			if err := s.walk(ctx, child, depth+1); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		content, _, err := s.client.ReadVersion(ctx, child)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("quota: read shared state %s: %w", child, err)
		}
		var doc sharedDoc
		if err := json.Unmarshal(content, &doc); err != nil {
			return fmt.Errorf("quota: parse shared state %s: %w", child, err)
		}
		if doc.Tenant == "" || doc.Partition == "" || doc.Day == "" {
			continue
		}
		s.totals[stateKey{Tenant: doc.Tenant, Partition: doc.Partition, Day: doc.Day}] += doc.Bytes
	}
	return nil
}

func (s *sharedState) snapshot() map[stateKey]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[stateKey]int64, len(s.totals))
	for key, value := range s.totals {
		out[key] = value
	}
	return out
}

func (s *sharedState) add(ctx context.Context, key stateKey, delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if delta <= 0 {
		return nil
	}
	if s.reserved[key] >= delta {
		s.reserved[key] -= delta
		return nil
	}
	need := delta - s.reserved[key]
	block := s.reserve
	if need > block {
		block = need
	}
	total, err := s.adjust(ctx, key, block)
	if err != nil {
		return err
	}
	s.totals[key] = total
	s.reserved[key] += block
	s.reserved[key] -= delta
	return nil
}

func (s *sharedState) adjust(ctx context.Context, key stateKey, delta int64) (int64, error) {
	logicalPath := s.pathFor(key)
	for attempt := 0; attempt < 5; attempt++ {
		content, version, err := s.client.ReadVersion(ctx, logicalPath)
		var doc sharedDoc
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return 0, fmt.Errorf("quota: read shared state %s: %w", logicalPath, err)
			}
			version = ""
		} else if len(content) > 0 {
			if err := json.Unmarshal(content, &doc); err != nil {
				return 0, fmt.Errorf("quota: parse shared state %s: %w", logicalPath, err)
			}
		}
		doc.Tenant = key.Tenant
		doc.Partition = key.Partition
		doc.Day = key.Day
		doc.Bytes += delta
		if doc.Bytes < 0 {
			doc.Bytes = 0
		}
		doc.UpdatedAt = time.Now().UTC()
		doc.ReplicaID = s.replicaID
		encoded, err := json.Marshal(doc)
		if err != nil {
			return 0, err
		}
		err = s.client.WriteCAS(ctx, logicalPath, encoded, version)
		if err == nil {
			return doc.Bytes, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return 0, fmt.Errorf("quota: write shared state %s: %w", logicalPath, err)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("quota: shared state CAS retry exhausted for %s", logicalPath)
}

func (s *sharedState) flush(context.Context) error { return nil }

func (s *sharedState) close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	keys := make([]stateKey, 0, len(s.reserved))
	amounts := make(map[stateKey]int64, len(s.reserved))
	for key, amount := range s.reserved {
		if amount > 0 {
			keys = append(keys, key)
			amounts[key] = amount
		}
	}
	s.reserved = map[stateKey]int64{}
	s.mu.Unlock()

	var firstErr error
	for _, key := range keys {
		total, err := s.adjust(ctx, key, -amounts[key])
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.totals[key] = total
	}
	if s.closer != nil {
		if err := s.closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *sharedState) describe() string { return "shared:" + s.root }

func (s *sharedState) pathFor(key stateKey) string {
	return path.Join(s.root, safeStateName(key.Tenant), safeStateName(key.Partition), safeStateName(key.Day)+".json")
}
