package query

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/radryc/monofs/pkg/authz/policy"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
	"github.com/rydzu/ainfra/doctor/internal/tracecontext"
)

const (
	// tracePageSpanBatch bounds each storage read. A page is assembled from one
	// or more batches, never from an unbounded scan of the requested window.
	tracePageSpanBatch = 5000
	// tracePageMaxBatches bounds the work spent filling one page when a
	// selective partition filter matches only a small fraction of spans.
	tracePageMaxBatches = 8
	// awsTracePageLimit bounds the federated X-Ray read per page. It is larger
	// than the usual page size so cursor filtering still leaves enough rows.
	awsTracePageLimit = 200
)

// traceSummary is the trace-list projection shared by the local and federated
// backends. MinTime is the trace start and is also the pagination key: sorting
// by it makes a trace complete as soon as the scan watermark drops below it.
type traceSummary struct {
	TraceID   string    `json:"trace_id"`
	Service   string    `json:"service"`
	Partition string    `json:"partition,omitempty"`
	Intent    string    `json:"intent,omitempty"`
	Asset     string    `json:"asset,omitempty"`
	MinTime   time.Time `json:"min_time"`
	MaxTime   time.Time `json:"max_time"`
	SpanCount int       `json:"span_count"`
}

// tracePageCursor is an opaque page token. TraceID breaks ties when several
// traces start at the same instant.
type tracePageCursor struct {
	MinTime time.Time `json:"min_time"`
	TraceID string    `json:"trace_id"`
}

func encodeTraceCursor(cursor *tracePageCursor) string {
	if cursor == nil || cursor.MinTime.IsZero() {
		return ""
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeTraceCursor(raw string) (*tracePageCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid trace cursor: %w", err)
	}
	var cursor tracePageCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return nil, fmt.Errorf("invalid trace cursor: %w", err)
	}
	if cursor.MinTime.IsZero() {
		return nil, fmt.Errorf("invalid trace cursor")
	}
	return &cursor, nil
}

// traceBeforeCursor reports whether summary sorts after the cursor in the
// newest-first trace list. The cursor is exclusive. IDs are compared in
// canonical form so a W3C local rendering and an X-Ray federated rendering of
// the same trace cannot land on different pages.
func traceBeforeCursor(summary *traceSummary, cursor *tracePageCursor) bool {
	if cursor == nil {
		return true
	}
	if summary.MinTime.Before(cursor.MinTime) {
		return true
	}
	return summary.MinTime.Equal(cursor.MinTime) &&
		traceMergeKey(summary.TraceID) < traceMergeKey(cursor.TraceID)
}

func sortTraceSummaries(summaries []*traceSummary) {
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].MinTime.Equal(summaries[j].MinTime) {
			return traceMergeKey(summaries[i].TraceID) > traceMergeKey(summaries[j].TraceID)
		}
		return summaries[i].MinTime.After(summaries[j].MinTime)
	})
}

func copyTraceSummarySlice(summaries []*traceSummary, limit int) []traceSummary {
	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	out := make([]traceSummary, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, *summary)
	}
	return out
}

// queryLocalTracePage assembles one page from bounded span reads. It walks the
// window backwards until every returned trace is complete: a trace is eligible
// only when the scan watermark has moved below its earliest span start.
func (s *Service) queryLocalTracePage(
	ctx context.Context,
	tenant, service, partition string,
	from, to time.Time,
	limit int,
	cursor *tracePageCursor,
) ([]traceSummary, *tracePageCursor, bool, error) {
	if limit <= 0 {
		return nil, nil, false, nil
	}

	traceMap := make(map[string]*traceSummary)
	scanTo := to
	if cursor != nil && cursor.MinTime.Before(scanTo) {
		// Start at the page boundary. Starting at the original upper bound
		// would rescan every newer page before reaching this page's rows.
		scanTo = cursor.MinTime
	}
	watermark := time.Time{}
	truncated := false

	for batch := 0; batch < tracePageMaxBatches; batch++ {
		spans, err := s.logEngine.QueryTraces(ctx, tenant, "", service, from, scanTo, tracePageSpanBatch)
		if err != nil {
			return nil, nil, false, err
		}
		if len(spans) == 0 {
			truncated = false
			break
		}
		truncated = len(spans) >= tracePageSpanBatch

		batchWatermark := spans[len(spans)-1].StartTime
		for _, span := range spans {
			if span.StartTime.Before(batchWatermark) {
				batchWatermark = span.StartTime
			}
		}
		if watermark.IsZero() || batchWatermark.Before(watermark) {
			watermark = batchWatermark
		}

		for _, span := range spans {
			if span.TraceID == "" {
				continue
			}
			spanPartition, intent, asset := traceIdentityLabels(span)
			if partition != "" && spanPartition != partition {
				continue
			}
			summary := traceMap[span.TraceID]
			if summary == nil {
				summary = &traceSummary{
					TraceID:   span.TraceID,
					Service:   span.Service,
					Partition: spanPartition,
					Intent:    intent,
					Asset:     asset,
					MinTime:   span.StartTime,
					MaxTime:   span.EndTime,
				}
				traceMap[span.TraceID] = summary
			}
			summary.SpanCount++
			if span.StartTime.Before(summary.MinTime) {
				summary.MinTime = span.StartTime
			}
			if span.EndTime.After(summary.MaxTime) {
				summary.MaxTime = span.EndTime
			}
		}

		// Cursor-eligible traces only ever get older. Drop traces that can no
		// longer appear on this page so the map stays proportional to one page.
		for traceID, summary := range traceMap {
			if !traceBeforeCursor(summary, cursor) {
				delete(traceMap, traceID)
			}
		}

		candidates := make([]*traceSummary, 0, len(traceMap))
		for _, summary := range traceMap {
			if summary.MinTime.Before(watermark) {
				continue // an older span may still arrive
			}
			candidates = append(candidates, summary)
		}
		sortTraceSummaries(candidates)

		if len(candidates) >= limit || !truncated {
			out := copyTraceSummarySlice(candidates, limit)
			hasMore := len(candidates) > limit || truncated
			var next *tracePageCursor
			if len(out) > 0 && hasMore {
				next = &tracePageCursor{MinTime: out[len(out)-1].MinTime, TraceID: out[len(out)-1].TraceID}
			}
			return out, next, hasMore, nil
		}

		nextScanTo := watermark.Add(-time.Nanosecond)
		if nextScanTo.Before(from) || !nextScanTo.Before(scanTo) {
			// Reached the window start (or made no progress): there is no
			// older batch left to fetch for this page.
			truncated = false
			break
		}
		scanTo = nextScanTo
	}

	candidates := make([]*traceSummary, 0, len(traceMap))
	for _, summary := range traceMap {
		if summary.MinTime.Before(watermark) || !traceBeforeCursor(summary, cursor) {
			continue
		}
		candidates = append(candidates, summary)
	}
	sortTraceSummaries(candidates)
	out := copyTraceSummarySlice(candidates, limit)
	hasMore := truncated || len(candidates) > limit
	var next *tracePageCursor
	if len(out) > 0 && hasMore {
		next = &tracePageCursor{MinTime: out[len(out)-1].MinTime, TraceID: out[len(out)-1].TraceID}
	} else if len(out) == 0 && hasMore && !watermark.IsZero() {
		// The page was empty only because the batch cap was reached. Let the
		// next request continue strictly below the scanned watermark.
		next = &tracePageCursor{MinTime: watermark, TraceID: "\uffff"}
	}
	return out, next, hasMore, nil
}

// queryAWSTracePage reads a bounded X-Ray page and applies the same cursor and
// partition rules as local traces.
func (s *Service) queryAWSTracePage(
	ctx context.Context,
	service, partition string,
	from, to time.Time,
	limit int,
	cursor *tracePageCursor,
) ([]traceSummary, bool) {
	if !s.awsReady() || limit <= 0 {
		return nil, false
	}
	awsLimit := limit
	if awsLimit < awsTracePageLimit {
		awsLimit = awsTracePageLimit
	}
	awsTo := to
	if cursor != nil && cursor.MinTime.Before(awsTo) {
		awsTo = cursor.MinTime
	}
	summaries, err := s.aws.QueryTraces(ctx, awsstore.TraceQuery{
		Service: service,
		From:    from,
		To:      awsTo,
		Limit:   awsLimit,
	})
	if err != nil {
		return nil, false
	}

	out := make([]traceSummary, 0, len(summaries))
	for _, summary := range summaries {
		if summary.TraceID == "" {
			continue
		}
		currentPartition := summary.Partition
		if currentPartition == "" {
			currentPartition = awsPartition(summary.Region, summary.Account)
		}
		summaryPartition, allowed := s.policyPartitionAllowed(ctx, summary.Account, summary.Region,
			currentPartition, policy.SignalTraces)
		if !allowed {
			continue
		}
		if partition != "" && summaryPartition != partition {
			continue
		}
		spanCount := summary.SpanCount
		if spanCount <= 0 {
			spanCount = 1
		}
		trace := traceSummary{
			TraceID:   summary.TraceID,
			Service:   firstNonEmpty(summary.Service, awsSourceLabel),
			Partition: summaryPartition,
			MinTime:   summary.StartTime,
			MaxTime:   summary.EndTime,
			SpanCount: spanCount,
		}
		if !traceBeforeCursor(&trace, cursor) {
			continue
		}
		out = append(out, trace)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MinTime.Equal(out[j].MinTime) {
			return traceMergeKey(out[i].TraceID) > traceMergeKey(out[j].TraceID)
		}
		return out[i].MinTime.After(out[j].MinTime)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, len(summaries) >= awsLimit
}

// traceMergeKey folds W3C and X-Ray renderings of the same trace into one key.
func traceMergeKey(traceID string) string {
	if canonical, err := tracecontext.CanonicalTraceID(traceID); err == nil {
		return canonical
	}
	return traceID
}

// mergeTracePages combines local and federated summaries without duplicates,
// then applies the cursor and page size again so the union is globally ordered.
func mergeTracePages(local []traceSummary, aws []traceSummary, limit int, cursor *tracePageCursor) ([]traceSummary, bool) {
	byID := make(map[string]traceSummary, len(local)+len(aws))
	for _, summary := range local {
		key := traceMergeKey(summary.TraceID)
		if existing, ok := byID[key]; ok {
			if summary.MinTime.Before(existing.MinTime) {
				existing.MinTime = summary.MinTime
			}
			if summary.MaxTime.After(existing.MaxTime) {
				existing.MaxTime = summary.MaxTime
			}
			if summary.SpanCount > existing.SpanCount {
				existing.SpanCount = summary.SpanCount
			}
			byID[key] = existing
			continue
		}
		byID[key] = summary
	}
	for _, summary := range aws {
		key := traceMergeKey(summary.TraceID)
		existing, ok := byID[key]
		if !ok {
			byID[key] = summary
			continue
		}
		if summary.MinTime.Before(existing.MinTime) {
			existing.MinTime = summary.MinTime
		}
		if summary.MaxTime.After(existing.MaxTime) {
			existing.MaxTime = summary.MaxTime
		}
		if summary.SpanCount > existing.SpanCount {
			existing.SpanCount = summary.SpanCount
		}
		byID[key] = existing
	}

	merged := make([]traceSummary, 0, len(byID))
	for _, summary := range byID {
		if !traceBeforeCursor(&summary, cursor) {
			continue
		}
		merged = append(merged, summary)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].MinTime.Equal(merged[j].MinTime) {
			return traceMergeKey(merged[i].TraceID) > traceMergeKey(merged[j].TraceID)
		}
		return merged[i].MinTime.After(merged[j].MinTime)
	})
	hasMore := len(merged) > limit
	if hasMore {
		merged = merged[:limit]
	}
	return merged, hasMore
}

// recentTracePage is the paginated form of the recent-trace list. It keeps the
// per-page scan bounded and returns a next cursor instead of loading the whole
// requested window into memory or shipping it to the browser.
func (s *Service) recentTracePage(
	ctx context.Context,
	tenant, service, partition string,
	from, to time.Time,
	limit int,
	cursor *tracePageCursor,
) ([]traceSummary, *tracePageCursor, bool, error) {
	local, localNext, localHasMore, err := s.queryLocalTracePage(ctx, tenant, service, partition, from, to, limit, cursor)
	if err != nil {
		return nil, nil, false, err
	}
	aws, awsHasMore := s.queryAWSTracePage(ctx, service, partition, from, to, limit, cursor)
	merged, mergedHasMore := mergeTracePages(local, aws, limit, cursor)
	hasMore := localHasMore || awsHasMore || mergedHasMore
	var next *tracePageCursor
	if len(merged) > 0 && hasMore {
		next = &tracePageCursor{MinTime: merged[len(merged)-1].MinTime, TraceID: merged[len(merged)-1].TraceID}
	} else if len(merged) == 0 {
		// Preserve the local cursor when this page only had federated filtering.
		next = localNext
	}
	return merged, next, hasMore, nil
}
