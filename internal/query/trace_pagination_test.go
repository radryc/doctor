package query

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

type recentTracePageResponse struct {
	Returned   int    `json:"returned"`
	HasMore    bool   `json:"has_more"`
	PageSize   int    `json:"page_size"`
	NextCursor string `json:"next_cursor"`
	Traces     []struct {
		TraceID string `json:"trace_id"`
	} `json:"traces"`
}

func fetchRecentTracePage(t *testing.T, svc *Service, from, to time.Time, limit int, cursor string) recentTracePageResponse {
	t.Helper()
	query := url.Values{}
	query.Set("tenant", "default")
	query.Set("limit", fmt.Sprintf("%d", limit))
	query.Set("from", from.Format(time.RFC3339Nano))
	query.Set("to", to.Format(time.RFC3339Nano))
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/traces/recent?"+query.Encode(), nil)
	rec := httptest.NewRecorder()
	svc.handleRecentTraces(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp recentTracePageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestRecentTracesPaginatesWithoutOverlap(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	from := base.Add(-1 * time.Hour)
	to := base.Add(time.Minute)

	var spans []segment.TraceRecord
	for i := 0; i < 25; i++ {
		traceID := fmt.Sprintf("trace-%02d", i)
		start := base.Add(-time.Duration(i) * time.Minute)
		for j := 0; j < 3; j++ {
			spans = append(spans, segment.TraceRecord{
				TraceID:   traceID,
				SpanID:    fmt.Sprintf("span-%02d-%d", i, j),
				Service:   "pager",
				Partition: "pager",
				StartTime: start.Add(time.Duration(j) * time.Second),
				EndTime:   start.Add(time.Duration(j)*time.Second + 500*time.Millisecond),
			})
		}
	}

	backend := &fakeLogEngineBackend{traces: spans}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	first := fetchRecentTracePage(t, svc, from, to, 10, "")
	if first.Returned != 10 || len(first.Traces) != 10 {
		t.Fatalf("page 1 returned %d traces, want 10", first.Returned)
	}
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("page 1 has_more=%v next_cursor=%q, want more and a cursor", first.HasMore, first.NextCursor)
	}
	for i, trace := range first.Traces {
		want := fmt.Sprintf("trace-%02d", i)
		if trace.TraceID != want {
			t.Fatalf("page 1 trace[%d] = %q, want %q", i, trace.TraceID, want)
		}
	}
	if backend.lastLimit > tracePageSpanBatch {
		t.Fatalf("page 1 storage limit = %d, want <= %d", backend.lastLimit, tracePageSpanBatch)
	}

	backend.traceQueries = nil
	second := fetchRecentTracePage(t, svc, from, to, 10, first.NextCursor)
	if second.Returned != 10 {
		t.Fatalf("page 2 returned %d traces, want 10", second.Returned)
	}
	if len(backend.traceQueries) == 0 {
		t.Fatal("page 2 did not query storage")
	}
	cursor, err := decodeTraceCursor(first.NextCursor)
	if err != nil {
		t.Fatalf("decode page 1 cursor: %v", err)
	}
	if !backend.traceQueries[0].to.Equal(cursor.MinTime) {
		t.Fatalf("page 2 first storage query to = %s, want cursor boundary %s", backend.traceQueries[0].to, cursor.MinTime)
	}
	if !second.HasMore || second.NextCursor == "" {
		t.Fatalf("page 2 has_more=%v next_cursor=%q, want more and a cursor", second.HasMore, second.NextCursor)
	}
	for i, trace := range second.Traces {
		want := fmt.Sprintf("trace-%02d", i+10)
		if trace.TraceID != want {
			t.Fatalf("page 2 trace[%d] = %q, want %q", i, trace.TraceID, want)
		}
	}

	third := fetchRecentTracePage(t, svc, from, to, 10, second.NextCursor)
	if third.Returned != 5 {
		t.Fatalf("page 3 returned %d traces, want 5", third.Returned)
	}
	if third.HasMore || third.NextCursor != "" {
		t.Fatalf("page 3 has_more=%v next_cursor=%q, want final page", third.HasMore, third.NextCursor)
	}
	seen := map[string]bool{}
	for _, page := range []recentTracePageResponse{first, second, third} {
		for _, trace := range page.Traces {
			if seen[trace.TraceID] {
				t.Fatalf("trace %q appeared on multiple pages", trace.TraceID)
			}
			seen[trace.TraceID] = true
		}
	}
	if len(seen) != 25 {
		t.Fatalf("saw %d unique traces across pages, want 25", len(seen))
	}
}

func TestRecentTracesPaginationFillsPageAfterPartitionFilter(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	from := base.Add(-1 * time.Hour)
	to := base.Add(time.Minute)

	var spans []segment.TraceRecord
	for i := 0; i < 20; i++ {
		partition := "div-a"
		if i%2 == 1 {
			partition = "div-b"
		}
		spans = append(spans, segment.TraceRecord{
			TraceID:   fmt.Sprintf("trace-%02d", i),
			SpanID:    fmt.Sprintf("span-%02d", i),
			Service:   partition + ".app",
			Partition: partition,
			StartTime: base.Add(-time.Duration(i) * time.Second),
			EndTime:   base.Add(-time.Duration(i)*time.Second + 100*time.Millisecond),
		})
	}

	backend := &fakeLogEngineBackend{traces: spans}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	query := url.Values{}
	query.Set("tenant", "default")
	query.Set("partition", "div-a")
	query.Set("limit", "4")
	query.Set("from", from.Format(time.RFC3339Nano))
	query.Set("to", to.Format(time.RFC3339Nano))

	cursor := ""
	var seen []string
	for pageNumber := 1; pageNumber <= 3; pageNumber++ {
		query.Set("cursor", cursor)
		req := httptest.NewRequest(http.MethodGet, "/v1/traces/recent?"+query.Encode(), nil)
		rec := httptest.NewRecorder()
		svc.handleRecentTraces(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d, body = %s", pageNumber, rec.Code, rec.Body.String())
		}
		var resp recentTracePageResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("page %d decode: %v", pageNumber, err)
		}
		if pageNumber < 3 && len(resp.Traces) != 4 {
			t.Fatalf("page %d returned %d traces, want 4 (partition filter must not shrink pages)", pageNumber, len(resp.Traces))
		}
		for _, trace := range resp.Traces {
			if trace.TraceID == "" {
				t.Fatal("empty trace id")
			}
			seen = append(seen, trace.TraceID)
		}
		if !resp.HasMore {
			break
		}
		cursor = resp.NextCursor
		if cursor == "" {
			t.Fatalf("page %d has_more but no cursor", pageNumber)
		}
	}
	if len(seen) != 10 {
		t.Fatalf("saw %d div-a traces, want 10: %v", len(seen), seen)
	}
	unique := make(map[string]bool, len(seen))
	for _, traceID := range seen {
		if unique[traceID] {
			t.Fatalf("duplicate div-a trace across pages: %v", seen)
		}
		unique[traceID] = true
	}
}

func TestRecentTracesFiltersByIdentity(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	from := base.Add(-time.Hour)
	to := base.Add(time.Minute)

	backend := &fakeLogEngineBackend{traces: []segment.TraceRecord{
		{
			TraceID:   "trace-a",
			SpanID:    "span-a",
			Service:   "monofs.monofs-storage.node-a",
			Partition: "monofs",
			Intent:    "monofs-storage",
			Asset:     "node-a",
			StartTime: base,
			EndTime:   base.Add(time.Second),
		},
		{
			TraceID:   "trace-b",
			SpanID:    "span-b",
			Service:   "monofs.monofs-storage.node-b",
			Partition: "monofs",
			Intent:    "monofs-storage",
			Asset:     "node-b",
			StartTime: base.Add(-time.Minute),
			EndTime:   base.Add(-time.Minute + time.Second),
		},
		{
			TraceID:   "trace-c",
			SpanID:    "span-c",
			Service:   "doctor.query.query",
			Partition: "doctor",
			Intent:    "query",
			Asset:     "query",
			StartTime: base.Add(-2 * time.Minute),
			EndTime:   base.Add(-2*time.Minute + time.Second),
		},
	}}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	query := url.Values{}
	query.Set("tenant", "default")
	query.Set("partition", "monofs")
	query.Set("intent", "monofs-storage")
	query.Set("asset", "node-a")
	query.Set("from", from.Format(time.RFC3339Nano))
	query.Set("to", to.Format(time.RFC3339Nano))
	req := httptest.NewRequest(http.MethodGet, "/v1/traces/recent?"+query.Encode(), nil)
	rec := httptest.NewRecorder()
	svc.handleRecentTraces(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp recentTracePageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Traces) != 1 || resp.Traces[0].TraceID != "trace-a" {
		t.Fatalf("traces = %+v, want only trace-a", resp.Traces)
	}
}

func TestMergeTracePagesDeduplicatesW3CAndXRayIDs(t *testing.T) {
	at := time.Date(2026, 10, 26, 13, 30, 0, 0, time.UTC)
	local := []traceSummary{{
		TraceID:   "6adf53471aed2f5b4632305cda5b230d",
		Service:   "div-a-orders.app.service",
		MinTime:   at,
		MaxTime:   at.Add(time.Second),
		SpanCount: 3,
	}}
	aws := []traceSummary{{
		TraceID:   "1-6adf5347-1aed2f5b4632305cda5b230d",
		Service:   "xray-api",
		MinTime:   at,
		MaxTime:   at.Add(2 * time.Second),
		SpanCount: 4,
	}}
	merged, hasMore := mergeTracePages(local, aws, 10, nil)
	if hasMore {
		t.Fatal("single deduplicated trace should not report more pages")
	}
	if len(merged) != 1 {
		t.Fatalf("merged traces = %d, want 1: %+v", len(merged), merged)
	}
	if merged[0].TraceID != local[0].TraceID {
		t.Fatalf("merged trace ID = %q, want local canonical ID %q", merged[0].TraceID, local[0].TraceID)
	}
	if merged[0].SpanCount != 4 {
		t.Fatalf("merged span count = %d, want 4", merged[0].SpanCount)
	}
}

func TestRecentTracesPartitionPageWalksBoundedBatches(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	from := base.Add(-3 * time.Hour)
	to := base.Add(time.Minute)

	var spans []segment.TraceRecord
	// 6,000 spans from an unrelated partition fill more than one storage batch.
	for i := 0; i < 6000; i++ {
		spans = append(spans, segment.TraceRecord{
			TraceID:   fmt.Sprintf("noise-%04d", i),
			SpanID:    fmt.Sprintf("noise-span-%04d", i),
			Service:   "noise.app",
			Partition: "noise",
			StartTime: base.Add(-time.Duration(i) * time.Second),
			EndTime:   base.Add(-time.Duration(i)*time.Second + 100*time.Millisecond),
		})
	}
	// Ten matching traces sit below the first batch, so the page can only be
	// filled by issuing a second bounded query at the watermark.
	for i := 0; i < 10; i++ {
		spans = append(spans, segment.TraceRecord{
			TraceID:   fmt.Sprintf("match-%02d", i),
			SpanID:    fmt.Sprintf("match-span-%02d", i),
			Service:   "match.app",
			Partition: "match",
			StartTime: base.Add(-time.Duration(7000+i) * time.Second),
			EndTime:   base.Add(-time.Duration(7000+i)*time.Second + 100*time.Millisecond),
		})
	}

	backend := &fakeLogEngineBackend{traces: spans}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	query := url.Values{}
	query.Set("tenant", "default")
	query.Set("partition", "match")
	query.Set("limit", "200")
	query.Set("from", from.Format(time.RFC3339Nano))
	query.Set("to", to.Format(time.RFC3339Nano))
	req := httptest.NewRequest(http.MethodGet, "/v1/traces/recent?"+query.Encode(), nil)
	rec := httptest.NewRecorder()
	svc.handleRecentTraces(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp recentTracePageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Traces) != 10 {
		t.Fatalf("returned %d matching traces, want 10: %+v", len(resp.Traces), resp.Traces)
	}
	if len(backend.traceQueries) < 2 {
		t.Fatalf("storage queries = %d, want at least 2 bounded batches", len(backend.traceQueries))
	}
	for _, traceQuery := range backend.traceQueries {
		if traceQuery.limit > tracePageSpanBatch {
			t.Fatalf("storage query limit = %d, want <= %d", traceQuery.limit, tracePageSpanBatch)
		}
	}
}

func TestRecentTracesRejectsInvalidCursor(t *testing.T) {
	svc := &Service{defaultTenant: "default", logEngine: &fakeLogEngineBackend{}}
	req := httptest.NewRequest(http.MethodGet, "/v1/traces/recent?tenant=default&cursor=not-base64", nil)
	rec := httptest.NewRecorder()
	svc.handleRecentTraces(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestTraceBeforeCursorUsesCanonicalTraceIDs(t *testing.T) {
	at := time.Date(2026, 10, 26, 13, 30, 0, 0, time.UTC)
	cursor := &tracePageCursor{MinTime: at, TraceID: "6adf53471aed2f5b4632305cda5b230d"}
	awsRendering := &traceSummary{MinTime: at, TraceID: "1-6adf5347-1aed2f5b4632305cda5b230d"}
	if traceBeforeCursor(awsRendering, cursor) {
		t.Fatal("X-Ray and W3C rendering of the same trace must not cross the cursor")
	}
	older := &traceSummary{MinTime: at, TraceID: "5adf53471aed2f5b4632305cda5b230d"}
	if !traceBeforeCursor(older, cursor) {
		t.Fatal("lower canonical trace ID at the same instant should be before the cursor")
	}
}

func TestTracePageCursorRoundTrip(t *testing.T) {
	want := &tracePageCursor{MinTime: time.Date(2026, 10, 26, 13, 30, 0, 0, time.UTC), TraceID: "6adf53471aed2f5b4632305cda5b230d"}
	raw := encodeTraceCursor(want)
	if raw == "" {
		t.Fatal("encoded cursor is empty")
	}
	got, err := decodeTraceCursor(raw)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if !got.MinTime.Equal(want.MinTime) || got.TraceID != want.TraceID {
		t.Fatalf("cursor = %+v, want %+v", got, want)
	}
}
