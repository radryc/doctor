package query

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/proxy"
	"github.com/radryc/doctor/internal/segment"
)

func traceSpans(traceID string, count int) []segment.TraceRecord {
	base := time.Now().UTC().Add(-time.Hour)
	spans := make([]segment.TraceRecord, 0, count)
	for i := 0; i < count; i++ {
		spans = append(spans, segment.TraceRecord{
			TraceID:   traceID,
			SpanID:    fmt.Sprintf("span-%06d", i),
			Service:   "pager",
			Partition: "pager",
			StartTime: base.Add(time.Duration(i) * time.Millisecond),
			EndTime:   base.Add(time.Duration(i)*time.Millisecond + time.Millisecond),
		})
	}
	return spans
}

func TestTraceDetailMarksTruncatedLegacyResponse(t *testing.T) {
	const legacyTraceSpanLimit = 1000
	backend := &fakeLogEngineBackend{traces: traceSpans("trace-big", legacyTraceSpanLimit)}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/v1/traces/trace-big?tenant=default", nil)
	rec := httptest.NewRecorder()
	svc.handleTraceByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Truncated bool `json:"truncated"`
		SpanLimit int  `json:"span_limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Truncated {
		t.Fatal("truncated = false, want true for a capped trace")
	}
	if resp.SpanLimit != legacyTraceSpanLimit {
		t.Fatalf("span_limit = %d, want %d", resp.SpanLimit, legacyTraceSpanLimit)
	}
}

func TestTraceDetailOmitsTruncationForCompleteTrace(t *testing.T) {
	backend := &fakeLogEngineBackend{traces: traceSpans("trace-small", 3)}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	req := httptest.NewRequest(http.MethodGet, "/v1/traces/trace-small?tenant=default", nil)
	rec := httptest.NewRecorder()
	svc.handleTraceByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := resp["truncated"]; ok {
		t.Fatalf("truncated present for a complete trace: %v", resp["truncated"])
	}
}

func TestTraceUpstreamWarnsWhenSpanLimitReached(t *testing.T) {
	backend := &fakeLogEngineBackend{traces: traceSpans("trace-big", traceGetSpanLimit)}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	upstream, ok := svc.LocalTracesUpstream().(localTracesUpstream)
	if !ok {
		t.Fatalf("local traces upstream has unexpected type %T", svc.LocalTracesUpstream())
	}

	ctx, collector := proxy.WithWarningCollector(context.Background())
	records, found, err := upstream.GetTraceWindow(ctx, "default", "trace-big", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetTraceWindow: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if len(records) != traceGetSpanLimit {
		t.Fatalf("len(records) = %d, want %d", len(records), traceGetSpanLimit)
	}

	warnings := collector.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one truncation warning", warnings)
	}
	warning := warnings[0]
	if warning.Backend != "monofs" || warning.Signal != proxy.SignalTraces {
		t.Fatalf("warning = %+v, want monofs/traces", warning)
	}
	if !strings.Contains(warning.Message, "truncated") {
		t.Fatalf("warning message = %q, want truncation detail", warning.Message)
	}
}

func TestTraceUpstreamNoWarningBelowSpanLimit(t *testing.T) {
	backend := &fakeLogEngineBackend{traces: traceSpans("trace-small", 10)}
	svc := &Service{defaultTenant: "default", logEngine: backend}

	upstream := svc.LocalTracesUpstream().(localTracesUpstream)
	ctx, collector := proxy.WithWarningCollector(context.Background())
	if _, _, err := upstream.GetTraceWindow(ctx, "default", "trace-small", time.Time{}, time.Time{}); err != nil {
		t.Fatalf("GetTraceWindow: %v", err)
	}
	if warnings := collector.Warnings(); len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}
