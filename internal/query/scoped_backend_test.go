package query

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/monofs/pkg/authz"
)

// scopedRecordingBackend simulates a storage backend that accepts a partition
// scope. It records what was pushed down and, like a backend without native
// scope support, still returns the unfiltered batch so the policy layer's
// second-line filtering is exercised.
type scopedRecordingBackend struct {
	fakeLogEngineBackend
	traceVisible     []string
	traceRestricted  bool
	logVisible       []string
	logRestricted    bool
	rawLogVisible    []string
	rawLogRestricted bool
	pagePartition    string
}

func (b *scopedRecordingBackend) QueryTracesScoped(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.TraceRecord, error) {
	b.traceVisible = append([]string(nil), visiblePartitions...)
	b.traceRestricted = restricted
	return b.fakeLogEngineBackend.QueryTraces(ctx, tenant, traceID, service, from, to, limit)
}

func (b *scopedRecordingBackend) QueryTracesPage(ctx context.Context, tenant, traceID, service string, from, to time.Time, limit int) ([]segment.TraceRecord, bool, time.Time, error) {
	spans, err := b.fakeLogEngineBackend.QueryTraces(ctx, tenant, traceID, service, from, to, limit)
	if err != nil {
		return nil, false, time.Time{}, err
	}
	watermark := oldestTraceStart(spans)
	if b.pagePartition == "" {
		return spans, len(spans) >= limit, watermark, nil
	}
	filtered := make([]segment.TraceRecord, 0, len(spans))
	for _, span := range spans {
		if span.Partition == b.pagePartition {
			filtered = append(filtered, span)
		}
	}
	return filtered, len(spans) >= limit, watermark, nil
}

func (b *scopedRecordingBackend) QueryLogsScoped(ctx context.Context, tenant, query, service string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.LogRecord, error) {
	b.logVisible = append([]string(nil), visiblePartitions...)
	b.logRestricted = restricted
	return b.fakeLogEngineBackend.QueryLogs(ctx, tenant, query, service, from, to, limit)
}

func (b *scopedRecordingBackend) QueryLogsRawScoped(ctx context.Context, tenant, query string, from, to time.Time, limit int, visiblePartitions []string, restricted bool) ([]segment.LogRecord, error) {
	b.rawLogVisible = append([]string(nil), visiblePartitions...)
	b.rawLogRestricted = restricted
	return b.fakeLogEngineBackend.QueryLogsRaw(ctx, tenant, query, from, to, limit)
}

func TestPolicyBackendPushesTraceScope(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &scopedRecordingBackend{fakeLogEngineBackend: fakeLogEngineBackend{traces: []segment.TraceRecord{
		{TraceID: "orders", SpanID: "span-a", Partition: "div-a-orders", StartTime: base, EndTime: base},
		{TraceID: "billing", SpanID: "span-b", Partition: "div-a-billing", StartTime: base, EndTime: base},
		{TraceID: "inventory", SpanID: "span-c", Partition: "div-b-inventory", StartTime: base, EndTime: base},
	}}}
	policyBackend := &policyBackend{inner: backend, gate: newCatalogGate(t)}
	ctx := authz.ContextWithIdentity(context.Background(), catalogIdentity("usera@example.com"))

	spans, err := policyBackend.QueryTraces(ctx, "default", "", "", base.Add(-time.Hour), base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}
	if !backend.traceRestricted {
		t.Fatal("trace scope was not restricted")
	}
	if len(backend.traceVisible) != 2 || !containsString(backend.traceVisible, "div-a-orders") || !containsString(backend.traceVisible, "div-a-billing") {
		t.Fatalf("pushed trace scope = %v, want usera's two partitions", backend.traceVisible)
	}
	if len(spans) != 2 {
		t.Fatalf("returned %d spans, want 2: %+v", len(spans), spans)
	}
	for _, span := range spans {
		if span.Partition == "div-b-inventory" {
			t.Fatalf("inventory span leaked through policy: %+v", span)
		}
	}
}

func TestPolicyBackendPushesLogScope(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	backend := &scopedRecordingBackend{fakeLogEngineBackend: fakeLogEngineBackend{logs: []segment.LogRecord{
		{Partition: "div-a-orders", Service: "orders", Timestamp: base, Body: "orders"},
		{Partition: "div-b-inventory", Service: "inventory", Timestamp: base, Body: "inventory"},
	}}}
	policyBackend := &policyBackend{inner: backend, gate: newCatalogGate(t)}
	ctx := authz.ContextWithIdentity(context.Background(), catalogIdentity("usera@example.com"))

	logs, err := policyBackend.QueryLogs(ctx, "default", "", "", base.Add(-time.Hour), base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if !backend.logRestricted || !containsString(backend.logVisible, "div-a-orders") {
		t.Fatalf("pushed log scope = %v restricted=%v, want div-a-orders", backend.logVisible, backend.logRestricted)
	}
	for _, record := range logs {
		if record.Partition == "div-b-inventory" {
			t.Fatalf("inventory log leaked through policy: %+v", record)
		}
	}

	raw, err := policyBackend.QueryLogsRaw(ctx, "default", `{service="orders"}`, base.Add(-time.Hour), base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("QueryLogsRaw() error = %v", err)
	}
	if !backend.rawLogRestricted || !containsString(backend.rawLogVisible, "div-a-orders") {
		t.Fatalf("pushed raw log scope = %v restricted=%v, want div-a-orders", backend.rawLogVisible, backend.rawLogRestricted)
	}
	for _, record := range raw {
		if record.Partition == "div-b-inventory" {
			t.Fatalf("inventory raw log leaked through policy: %+v", record)
		}
	}
}

func TestPolicyBackendQueryTracesPageReportsRawTruncation(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	spans := []segment.TraceRecord{
		{TraceID: "visible", SpanID: "span-a", Partition: "div-a-orders", StartTime: base, EndTime: base},
		{TraceID: "hidden-1", SpanID: "span-b", Partition: "div-b-inventory", StartTime: base.Add(-time.Second), EndTime: base.Add(-time.Second)},
		{TraceID: "hidden-2", SpanID: "span-c", Partition: "div-b-inventory", StartTime: base.Add(-2 * time.Second), EndTime: base.Add(-2 * time.Second)},
		{TraceID: "hidden-3", SpanID: "span-d", Partition: "div-b-inventory", StartTime: base.Add(-3 * time.Second), EndTime: base.Add(-3 * time.Second)},
	}
	backend := &scopedRecordingBackend{fakeLogEngineBackend: fakeLogEngineBackend{traces: spans}}
	policyBackend := &policyBackend{inner: backend, gate: newCatalogGate(t)}
	ctx := authz.ContextWithIdentity(context.Background(), catalogIdentity("usera@example.com"))

	got, truncated, watermark, err := policyBackend.QueryTracesPage(ctx, "default", "", "", base.Add(-time.Hour), base.Add(time.Hour), len(spans))
	if err != nil {
		t.Fatalf("QueryTracesPage() error = %v", err)
	}
	if !truncated {
		t.Fatal("truncated = false, want the raw storage batch to look full")
	}
	if watermark.IsZero() {
		t.Fatal("raw watermark is zero, want the oldest raw span start")
	}
	if len(got) != 1 || got[0].TraceID != "visible" {
		t.Fatalf("page spans = %+v, want only the visible trace", got)
	}
}

func TestRecentTracesContinuesWhenPolicyFiltersMostOfBatch(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	from := base.Add(-3 * time.Hour)
	to := base.Add(time.Minute)

	var spans []segment.TraceRecord
	// More than one storage batch of unrelated (invisible) spans.
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
	// The visible traces sit below the first batch.
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

	backend := &scopedRecordingBackend{
		fakeLogEngineBackend: fakeLogEngineBackend{traces: spans},
		pagePartition:        "match",
	}
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
		t.Fatalf("returned %d traces, want 10 (policy filter must not stop the batch walk)", len(resp.Traces))
	}
	if len(backend.traceQueries) < 2 {
		t.Fatalf("storage queries = %d, want at least 2 batches", len(backend.traceQueries))
	}
}
