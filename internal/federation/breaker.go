package federation

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/radryc/doctor/internal/proxy"
	awsstore "github.com/radryc/doctor/internal/store/aws"
)

var errTargetCircuitOpen = errors.New("target circuit breaker open")

type targetBreaker struct {
	mu          sync.Mutex
	failures    int
	openUntil   time.Time
	halfOpen    bool
	lastError   string
	lastErrorAt time.Time
}

func (b *targetBreaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if now.Before(b.openUntil) {
		return false
	}
	// Cooldown elapsed. Allow exactly one half-open probe; concurrent callers
	// wait for the probe result.
	if b.halfOpen {
		return false
	}
	b.halfOpen = true
	return true
}

func (b *targetBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.halfOpen = false
	b.lastError = ""
	b.lastErrorAt = time.Time{}
}

func (b *targetBreaker) recordFailure(now time.Time, threshold int, cooldown time.Duration, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.lastError = err.Error()
	b.lastErrorAt = now
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	if b.failures >= threshold || b.halfOpen {
		b.openUntil = now.Add(cooldown)
		b.halfOpen = false
	}
}

func (b *targetBreaker) abortProbe() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.halfOpen = false
}

type breakerSnapshot struct {
	Target      string    `json:"target"`
	Open        bool      `json:"open"`
	HalfOpen    bool      `json:"half_open,omitempty"`
	Failures    int       `json:"failures"`
	OpenUntil   time.Time `json:"open_until,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
}

func (b *targetBreaker) snapshot(id string, now time.Time) breakerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return breakerSnapshot{
		Target:      id,
		Open:        !b.openUntil.IsZero() && now.Before(b.openUntil),
		HalfOpen:    b.halfOpen,
		Failures:    b.failures,
		OpenUntil:   b.openUntil,
		LastError:   b.lastError,
		LastErrorAt: b.lastErrorAt,
	}
}

func (m *Manager) breakerFor(targetID string) *targetBreaker {
	m.breakerMu.Lock()
	defer m.breakerMu.Unlock()
	breaker := m.breakers[targetID]
	if breaker == nil {
		breaker = &targetBreaker{}
		m.breakers[targetID] = breaker
	}
	return breaker
}

func (m *Manager) breakerAllow(targetID string, now time.Time) bool {
	return m.breakerFor(targetID).allow(now)
}

func (m *Manager) noteTargetSuccess(targetID string) {
	m.breakerFor(targetID).recordSuccess()
}

func (m *Manager) noteTargetFailure(targetID string, err error) {
	if err == nil || isClientSideFailure(err) || isUnauthorized(err) {
		m.breakerFor(targetID).abortProbe()
		return
	}
	m.breakerFor(targetID).recordFailure(time.Now().UTC(), m.breakerThreshold, m.breakerCooldown, err)
}

func (m *Manager) noteTargetAbort(targetID string) {
	m.breakerFor(targetID).abortProbe()
}

func (m *Manager) circuitStatus() (int, []breakerSnapshot) {
	now := time.Now().UTC()
	m.breakerMu.Lock()
	ids := make([]string, 0, len(m.breakers))
	for id := range m.breakers {
		ids = append(ids, id)
	}
	m.breakerMu.Unlock()
	sort.Strings(ids)
	open := 0
	snapshots := make([]breakerSnapshot, 0, len(ids))
	for _, id := range ids {
		snapshot := m.breakerFor(id).snapshot(id, now)
		if snapshot.Open {
			open++
		}
		if snapshot.Open || snapshot.Failures > 0 {
			snapshots = append(snapshots, snapshot)
		}
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Open != snapshots[j].Open {
			return snapshots[i].Open
		}
		return snapshots[i].LastErrorAt.After(snapshots[j].LastErrorAt)
	})
	const maxSnapshots = 64
	if len(snapshots) > maxSnapshots {
		snapshots = snapshots[:maxSnapshots]
	}
	return open, snapshots
}

func isClientSideFailure(err error) bool {
	return errors.Is(err, proxy.ErrScopeRequired) ||
		errors.Is(err, proxy.ErrTooBroad) ||
		errors.Is(err, proxy.ErrNoTargets) ||
		errors.Is(err, context.Canceled)
}

func isUnauthorized(err error) bool {
	var httpErr *awsstore.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == 401
}

func isRetryableFanoutError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || isClientSideFailure(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var httpErr *awsstore.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 429 || httpErr.StatusCode >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func fanoutWarning(target Target, signal Signal, err error) proxy.Warning {
	return proxy.Warning{
		Backend: target.ID,
		Signal:  proxy.Signal(signal),
		Message: strings.TrimSpace(err.Error()),
	}
}
