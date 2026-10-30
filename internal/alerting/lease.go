package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log"
	"os"
	"sync"
	"time"

	doctorstore "github.com/radryc/doctor/internal/store"
)

// ErrLeaseConflict reports a lost lease race.
var ErrLeaseConflict = errors.New("alerting: lease conflict")

// Lease is a persisted shard leadership record.
type Lease struct {
	Owner      string    `json:"owner"`
	Shard      int       `json:"shard"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LeaseManager elects one owner per shard.
type LeaseManager struct {
	Files  FileStore
	Token  string
	Path   string
	Owner  string
	Shard  int
	TTL    time.Duration
	Renew  time.Duration
	Logger *log.Logger
	Now    func() time.Time

	mu     sync.RWMutex
	leader bool
}

// NewLeaseManager builds a shard lease manager.
func NewLeaseManager(files FileStore, token, path, owner string, shard int, ttl, renew time.Duration, logger *log.Logger) *LeaseManager {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if renew <= 0 {
		renew = ttl / 3
	}
	return &LeaseManager{
		Files: files, Token: token, Path: path, Owner: owner, Shard: shard,
		TTL: ttl, Renew: renew, Logger: logger,
		Now: func() time.Time { return time.Now().UTC() },
	}
}

// Leader reports whether this manager currently holds the lease.
func (m *LeaseManager) Leader() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.leader
}

// Run acquires the lease and invokes onLeader while holding it.
func (m *LeaseManager) Run(ctx context.Context, onLeader func(context.Context)) {
	for {
		if ctx.Err() != nil {
			return
		}
		if m.TryAcquire(ctx) {
			m.setLeader(true)
			m.runLeader(ctx, onLeader)
			m.setLeader(false)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.Renew):
		}
	}
}

// TryAcquire attempts to take or renew the shard lease.
func (m *LeaseManager) TryAcquire(ctx context.Context) bool {
	now := m.now()
	content, err := json.Marshal(Lease{Owner: m.Owner, Shard: m.Shard, AcquiredAt: now, ExpiresAt: now.Add(m.TTL)})
	if err != nil {
		return false
	}
	data, version, err := m.read(ctx, m.Path)
	if err != nil {
		m.logf("lease read failed: %v", err)
		return false
	}
	if data == nil {
		if err := m.write(ctx, m.Path, content, "absent"); err != nil {
			return false
		}
		m.logf("acquired shard %d lease", m.Shard)
		return true
	}
	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		m.logf("lease malformed: %v", err)
		return false
	}
	if now.Before(lease.ExpiresAt) && lease.Owner != m.Owner {
		return false
	}
	if err := m.write(ctx, m.Path, content, version); err != nil {
		return false
	}
	if lease.Owner != m.Owner {
		m.logf("took over shard %d lease from %s", m.Shard, lease.Owner)
	}
	return true
}

func (m *LeaseManager) runLeader(ctx context.Context, onLeader func(context.Context)) {
	leaderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		onLeader(leaderCtx)
	}()
	ticker := time.NewTicker(m.Renew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			<-done
			return
		case <-ticker.C:
			if !m.TryAcquire(ctx) {
				m.logf("lost shard %d lease; stepping down", m.Shard)
				cancel()
				<-done
				return
			}
		case <-done:
			return
		}
	}
}

func (m *LeaseManager) read(ctx context.Context, path string) ([]byte, string, error) {
	data, err := m.Files.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", nil
		}
		return nil, "", err
	}
	versions, err := m.Files.ListVersions(ctx, m.Token, path)
	if err != nil || len(versions) == 0 {
		return data, "", nil
	}
	return data, versions[0].VersionID, nil
}

func (m *LeaseManager) write(ctx context.Context, path string, content []byte, expected string) error {
	_, err := m.Files.UpsertPaths(ctx, m.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           content,
		ExpectedVersionID: expected,
	}}, doctorstore.MutationContext{PrincipalID: "doctor-sentinel", Reason: "sentinel shard lease"})
	if err != nil {
		return ErrLeaseConflict
	}
	return nil
}

// ShardFor maps a rule group to an evaluation shard.
func ShardFor(tenant, partition, group string, count int) int {
	if count <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(tenant + "/" + partition + "/" + group))
	return int(h.Sum32() % uint32(count))
}

func (m *LeaseManager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *LeaseManager) setLeader(leader bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leader = leader
}

func (m *LeaseManager) logf(format string, args ...any) {
	if m.Logger != nil {
		m.Logger.Printf("sentinel lease: "+format, args...)
	}
}
