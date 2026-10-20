package ruler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/store"
)

// LeaseStore persists the leader lease with compare-and-swap semantics.
type LeaseStore interface {
	// Read returns the lease content and its version. A missing lease returns
	// (nil, "", nil).
	Read(ctx context.Context, path string) ([]byte, string, error)
	// Create writes a new lease; it fails with ErrLeaseConflict if one exists.
	Create(ctx context.Context, path string, content []byte) error
	// Replace writes over an existing lease with a version precondition.
	Replace(ctx context.Context, path string, content []byte, version string) error
}

// ErrLeaseConflict reports a lost lease race.
var ErrLeaseConflict = errors.New("ruler: lease conflict")

// Lease is the persisted leadership record.
type Lease struct {
	RulerID    string    `json:"ruler_id"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LeaseManager elects one ruler replica and keeps the lease renewed.
type LeaseManager struct {
	Store   LeaseStore
	Path    string
	RulerID string
	TTL     time.Duration
	Renew   time.Duration
	Logger  *log.Logger
	Now     func() time.Time

	mu          sync.RWMutex
	leader      bool
	leaderSince time.Time
}

// LeaseStatus is a point-in-time view of this replica's leadership.
type LeaseStatus struct {
	RulerID string `json:"rulerId"`
	Leader  bool   `json:"leader"`
	TTL     string `json:"ttl"`
	Renew   string `json:"renew"`
	Path    string `json:"path"`
	Since   string `json:"since,omitempty"`
}

// Status returns the current leadership state.
func (m *LeaseManager) Status() LeaseStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := LeaseStatus{
		RulerID: m.RulerID,
		Leader:  m.leader,
		TTL:     m.TTL.String(),
		Renew:   m.Renew.String(),
		Path:    m.Path,
	}
	if !m.leaderSince.IsZero() {
		status.Since = m.leaderSince.UTC().Format(time.RFC3339)
	}
	return status
}

func (m *LeaseManager) setLeader(leader bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if leader && !m.leader {
		m.leaderSince = m.now()
	}
	if !leader {
		m.leaderSince = time.Time{}
	}
	m.leader = leader
}

// NewLeaseManager builds a lease manager with sane defaults.
func NewLeaseManager(store LeaseStore, path, rulerID string, ttl, renew time.Duration, logger *log.Logger) *LeaseManager {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if renew <= 0 {
		renew = ttl / 3
	}
	if rulerID == "" {
		if hostname, err := os.Hostname(); err == nil {
			rulerID = hostname
		} else {
			rulerID = "doctor-rules"
		}
	}
	return &LeaseManager{
		Store:   store,
		Path:    path,
		RulerID: rulerID,
		TTL:     ttl,
		Renew:   renew,
		Logger:  logger,
		Now:     func() time.Time { return time.Now().UTC() },
	}
}

// Run acquires leadership and invokes onLeader while holding it. It returns
// when ctx is cancelled.
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

// TryAcquire attempts to take or renew leadership.
func (m *LeaseManager) TryAcquire(ctx context.Context) bool {
	now := m.now()
	content, err := json.Marshal(Lease{RulerID: m.RulerID, AcquiredAt: now, ExpiresAt: now.Add(m.TTL)})
	if err != nil {
		return false
	}

	data, version, err := m.Store.Read(ctx, m.Path)
	if err != nil {
		m.logf("lease read failed: %v", err)
		return false
	}
	if data == nil {
		if err := m.Store.Create(ctx, m.Path, content); err != nil {
			return false
		}
		m.logf("acquired lease")
		return true
	}

	var lease Lease
	if err := json.Unmarshal(data, &lease); err != nil {
		m.logf("lease malformed; refusing leadership: %v", err)
		return false
	}
	if now.Before(lease.ExpiresAt) && lease.RulerID != m.RulerID {
		return false
	}
	if err := m.Store.Replace(ctx, m.Path, content, version); err != nil {
		return false
	}
	if lease.RulerID != m.RulerID {
		m.logf("took over expired lease from %s", lease.RulerID)
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
				m.logf("lost lease; stepping down")
				cancel()
				<-done
				return
			}
		case <-done:
			return
		}
	}
}

func (m *LeaseManager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *LeaseManager) logf(format string, args ...any) {
	if m.Logger != nil {
		m.Logger.Printf("rules: lease %s: %s", m.RulerID, fmt.Sprintf(format, args...))
	}
}

// FileLeaseStore adapts a ruler Store's FileStore to LeaseStore.
type FileLeaseStore struct {
	Files FileStore
	Token string
}

// Read implements LeaseStore.
func (s *FileLeaseStore) Read(ctx context.Context, path string) ([]byte, string, error) {
	data, err := s.Files.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", nil
		}
		return nil, "", err
	}
	versions, err := s.Files.ListVersions(ctx, s.Token, path)
	if err != nil || len(versions) == 0 {
		return data, "", nil
	}
	return data, versions[0].VersionID, nil
}

// Create implements LeaseStore.
func (s *FileLeaseStore) Create(ctx context.Context, path string, content []byte) error {
	_, err := s.Files.UpsertPaths(ctx, s.Token, []store.PathWrite{{
		LogicalPath:       path,
		Content:           content,
		ExpectedVersionID: "absent",
	}}, store.MutationContext{PrincipalID: "doctor-rules", Reason: "rules lease acquire"})
	if err != nil {
		return ErrLeaseConflict
	}
	return nil
}

// Replace implements LeaseStore.
func (s *FileLeaseStore) Replace(ctx context.Context, path string, content []byte, version string) error {
	_, err := s.Files.UpsertPaths(ctx, s.Token, []store.PathWrite{{
		LogicalPath:       path,
		Content:           content,
		ExpectedVersionID: version,
	}}, store.MutationContext{PrincipalID: "doctor-rules", Reason: "rules lease renew"})
	if err != nil {
		return ErrLeaseConflict
	}
	return nil
}
