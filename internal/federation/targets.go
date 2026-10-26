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

// TargetPageOptions controls the federation target listing.
type TargetPageOptions struct {
	Account   string
	Region    string
	Partition string
	Query     string
	Cursor    string
	Limit     int

	// VisiblePartition restricts the page to targets that own at least one
	// visible partition. Partitions inside each target are reduced to the
	// visible set as well. A nil predicate means every partition is visible.
	VisiblePartition func(string) bool

	// IncludeMetadata adds the discovered metric names, log groups, and X-Ray
	// services to each target. Callers should use this only for a single target
	// or a small page because the metadata can be large.
	IncludeMetadata bool
}

// TargetsPage returns a filtered, paginated target view. The cursor is the last
// target ID returned.
func (m *Manager) TargetsPage(ctx context.Context, account, region, partition, query, cursor string, limit int) TargetPage {
	return m.TargetsPageFor(ctx, TargetPageOptions{
		Account:   account,
		Region:    region,
		Partition: partition,
		Query:     query,
		Cursor:    cursor,
		Limit:     limit,
	})
}

// TargetsPageFor returns a filtered, paginated target view with optional
// partition visibility and catalog metadata.
func (m *Manager) TargetsPageFor(_ context.Context, opts TargetPageOptions) TargetPage {
	state := m.current()
	if state == nil || state.catalog == nil {
		return TargetPage{Targets: []map[string]any{}}
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	account := strings.ToLower(strings.TrimSpace(opts.Account))
	region := strings.ToLower(strings.TrimSpace(opts.Region))
	partition := strings.ToLower(strings.TrimSpace(opts.Partition))
	query := strings.ToLower(strings.TrimSpace(opts.Query))

	targets := state.catalog.Targets()
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })

	type targetView struct {
		target     Target
		partitions []string
	}

	matched := make([]targetView, 0, len(targets))
	total := 0
	for _, target := range targets {
		partitions := visibleTargetPartitions(target.Partitions, opts.VisiblePartition)
		if opts.VisiblePartition != nil && len(partitions) == 0 {
			continue
		}
		total++
		if account != "" && !strings.EqualFold(target.Account, account) {
			continue
		}
		if region != "" && !strings.EqualFold(target.Region, region) {
			continue
		}
		if partition != "" && !containsFold(partitions, partition) {
			continue
		}
		if query != "" && !targetMatchesQuery(target, query) {
			continue
		}
		matched = append(matched, targetView{target: target, partitions: partitions})
	}

	start := 0
	if opts.Cursor != "" {
		start = sort.Search(len(matched), func(i int) bool { return matched[i].target.ID >= opts.Cursor })
		for start < len(matched) && matched[start].target.ID == opts.Cursor {
			start++
		}
	}
	end := start + limit
	if end > len(matched) {
		end = len(matched)
	}
	page := TargetPage{Total: total, Matched: len(matched)}
	if end < len(matched) {
		page.HasMore = true
		page.NextCursor = matched[end-1].target.ID
	}
	now := time.Now().UTC()
	for _, view := range matched[start:end] {
		target := view.target
		breaker := m.breakerFor(target.ID).snapshot(target.ID, now)
		item := map[string]any{
			"id":                 target.ID,
			"account":            target.Account,
			"region":             target.Region,
			"provider":           target.Provider,
			"partitions":         view.partitions,
			"endpoint":           target.Endpoint,
			"version":            target.Version,
			"capabilities":       target.Capabilities,
			"labels":             target.Labels,
			"healthy":            !breaker.Open,
			"circuit":            breaker,
			"metric_count":       len(target.MetricNames),
			"log_group_count":    len(target.LogGroups),
			"xray_service_count": len(target.XrayServices),
		}
		if !target.MetadataFetchedAt.IsZero() {
			item["metadata_fetched_at"] = target.MetadataFetchedAt
		}
		if opts.IncludeMetadata {
			item["metric_names"] = target.MetricNames
			item["log_groups"] = target.LogGroups
			item["xray_services"] = target.XrayServices
			item["service_partitions"] = target.ServicePartitions
			item["tenant"] = target.Tenant
			item["priority"] = target.Priority
		}
		page.Targets = append(page.Targets, item)
	}
	if page.Targets == nil {
		page.Targets = []map[string]any{}
	}
	return page
}

func visibleTargetPartitions(partitions []string, visible func(string) bool) []string {
	if visible == nil {
		return partitions
	}
	out := make([]string, 0, len(partitions))
	for _, partition := range partitions {
		if visible(partition) {
			out = append(out, partition)
		}
	}
	sort.Strings(out)
	return out
}

func targetMatchesQuery(target Target, query string) bool {
	haystack := strings.ToLower(strings.Join([]string{
		target.ID,
		target.Account,
		target.Region,
		target.Endpoint,
	}, " "))
	if strings.Contains(haystack, query) {
		return true
	}
	for _, partition := range target.Partitions {
		if strings.Contains(strings.ToLower(partition), query) {
			return true
		}
	}
	return false
}

func containsFold(values []string, needle string) bool {
	for _, value := range values {
		if strings.EqualFold(value, needle) {
			return true
		}
	}
	return false
}
