package federation

import (
	"context"
	"strings"
	"time"
)

// DebugPlan evaluates a query without fan-out and returns the planner decision.
func (m *Manager) DebugPlan(ctx context.Context, query string, signal Signal, account, region, partition string) map[string]any {
	state := m.current()
	if state == nil || state.planner == nil || state.catalog == nil {
		return map[string]any{"enabled": false}
	}
	if signal == "" {
		signal = SignalMetrics
	}
	started := time.Now()
	plan, err := state.planner.Plan(ctx, Query{
		Signal:    signal,
		Expr:      strings.TrimSpace(query),
		Account:   strings.TrimSpace(account),
		Region:    strings.TrimSpace(region),
		Partition: strings.TrimSpace(partition),
	})
	selected := make([]string, 0, len(plan.Targets))
	for _, target := range plan.Targets {
		selected = append(selected, target.ID)
	}
	out := map[string]any{
		"enabled":  true,
		"version":  state.catalog.Version(),
		"signal":   string(signal),
		"mode":     string(plan.Mode),
		"matched":  plan.Matched,
		"selected": len(plan.Targets),
		"targets":  selected,
		"reason":   plan.Reason,
		"warnings": plan.Warnings,
		"duration": time.Since(started).Seconds(),
	}
	if err != nil {
		out["error"] = err.Error()
	}
	return out
}
