package federation

import (
	"context"
	"sort"
	"strings"
	"time"
)

// TargetPage is the paginated target detail view used by the federation admin
// endpoint. It never contains bearer tokens.
type TargetPage struct {
	Targets    []map[string]any `json:"targets"`
	Total      int              `json:"total"`
	Matched    int              `json:"matched"`
	NextCursor string           `json:"nextCursor,omitempty"`
	HasMore    bool             `json:"hasMore"`
}

// TargetsPage returns a filtered, paginated target view. The cursor is the last
// target ID returned.
func (m *Manager) TargetsPage(_ context.Context, account, region, partition, query, cursor string, limit int) TargetPage {
	state := m.current()
	if state == nil || state.catalog == nil {
		return TargetPage{Targets: []map[string]any{}}
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	account = strings.ToLower(strings.TrimSpace(account))
	region = strings.ToLower(strings.TrimSpace(region))
	partition = strings.ToLower(strings.TrimSpace(partition))
	query = strings.ToLower(strings.TrimSpace(query))

	targets := state.catalog.Targets()
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	matched := make([]Target, 0, len(targets))
	for _, target := range targets {
		if account != "" && !strings.EqualFold(target.Account, account) {
			continue
		}
		if region != "" && !strings.EqualFold(target.Region, region) {
			continue
		}
		if partition != "" && !containsFold(target.Partitions, partition) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(target.ID), query) {
			continue
		}
		matched = append(matched, target)
	}

	start := 0
	if cursor != "" {
		start = sort.Search(len(matched), func(i int) bool { return matched[i].ID >= cursor })
		for start < len(matched) && matched[start].ID == cursor {
			start++
		}
	}
	end := start + limit
	if end > len(matched) {
		end = len(matched)
	}
	page := TargetPage{Total: state.catalog.Len(), Matched: len(matched)}
	if end < len(matched) {
		page.HasMore = true
		page.NextCursor = matched[end-1].ID
	}
	now := time.Now().UTC()
	for _, target := range matched[start:end] {
		breaker := m.breakerFor(target.ID).snapshot(target.ID, now)
		item := map[string]any{
			"id":           target.ID,
			"account":      target.Account,
			"region":       target.Region,
			"provider":     target.Provider,
			"partitions":   target.Partitions,
			"endpoint":     target.Endpoint,
			"version":      target.Version,
			"capabilities": target.Capabilities,
			"labels":       target.Labels,
			"healthy":      !breaker.Open,
			"circuit":      breaker,
		}
		page.Targets = append(page.Targets, item)
	}
	if page.Targets == nil {
		page.Targets = []map[string]any{}
	}
	return page
}

func containsFold(values []string, needle string) bool {
	for _, value := range values {
		if strings.EqualFold(value, needle) {
			return true
		}
	}
	return false
}
