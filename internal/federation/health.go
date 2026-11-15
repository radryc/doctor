package federation

import (
	"context"
	"sync"
	"time"
)

// HealthSummary is the aggregate state of one federation provider (aws/gcp).
// The query UI uses it to avoid presenting a provider as healthy when every
// target is circuit-open.
type HealthSummary struct {
	Provider string `json:"provider"`
	Total    int    `json:"total"`
	Open     int    `json:"open"`
	Healthy  int    `json:"healthy"`
}

// HealthSummary snapshots the provider's target circuits.
func (m *Manager) HealthSummary() HealthSummary {
	if m == nil {
		return HealthSummary{}
	}
	state := m.current()
	if state == nil || state.catalog == nil {
		return HealthSummary{}
	}
	now := time.Now().UTC()
	summary := HealthSummary{}
	for _, target := range state.catalog.Targets() {
		if summary.Provider == "" {
			summary.Provider = normalizeProvider(target.Provider)
		}
		summary.Total++
		snapshot := m.breakerFor(target.ID).snapshot(target.ID, now)
		if snapshot.Open {
			summary.Open++
		} else {
			summary.Healthy++
		}
	}
	if summary.Provider == "" {
		summary.Provider = "federation"
	}
	return summary
}

// probeFailedTargets runs in the manager Run loop. It uses a cheap read-API
// call to re-open a circuit when a previously failed pusher becomes healthy,
// without waiting for a user query to trigger the half-open probe.
func (m *Manager) probeFailedTargets(ctx context.Context) {
	if m == nil {
		return
	}
	state := m.current()
	if state == nil || state.catalog == nil {
		return
	}
	now := time.Now().UTC()
	targets := make([]Target, 0)
	for _, target := range state.catalog.Targets() {
		if m.breakerFor(target.ID).needsProbe(now) {
			targets = append(targets, target)
		}
	}
	if len(targets) == 0 {
		return
	}
	concurrency := m.maxConcurrentTargets
	if concurrency <= 0 {
		concurrency = 4
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			client, err := m.clientFor(target)
			if err != nil {
				m.noteTargetFailure(target.ID, err)
				return
			}
			callCtx, cancel := context.WithTimeout(ctx, m.timeout)
			_, err = client.ListServices(callCtx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				m.noteTargetFailure(target.ID, err)
				return
			}
			m.noteTargetSuccess(target.ID)
		}()
	}
	wg.Wait()
}
