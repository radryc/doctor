package proxy

import (
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

func traceSpan(traceID, spanID, service string, start time.Time) segment.TraceRecord {
	return segment.TraceRecord{
		TraceID:   traceID,
		SpanID:    spanID,
		Name:      "GET /a",
		Service:   service,
		StartTime: start,
		EndTime:   start.Add(time.Second),
	}
}

func TestMergeTraceRecordsDedupesSameBackend(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	record := traceSpan("trace-1", "span-1", "api", base)
	merged := MergeTraceRecords(nil, TraceRemote{Source: SourceDoctor, Backend: "doctor", Records: []segment.TraceRecord{record, record}})
	if len(merged) != 1 {
		t.Fatalf("merged = %d, want 1", len(merged))
	}
}

func TestMergeTraceRecordsShardsCrossBackend(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	merged := MergeTraceRecords(nil,
		TraceRemote{Source: "tempo", Backend: "tempo-a", Records: []segment.TraceRecord{traceSpan("trace-1", "span-1", "api", base)}},
		TraceRemote{Source: "tempo", Backend: "tempo-b", Records: []segment.TraceRecord{traceSpan("trace-1", "span-1", "api", base)}},
	)
	if len(merged) != 2 {
		t.Fatalf("merged = %d, want 2", len(merged))
	}
	if merged[0].Attributes[LabelShard] != "tempo-a" {
		t.Fatalf("first shard = %q, want tempo-a", merged[0].Attributes[LabelShard])
	}
	if merged[1].Attributes[LabelShard] != "tempo-b" {
		t.Fatalf("second shard = %q, want tempo-b", merged[1].Attributes[LabelShard])
	}
	if merged[0].Attributes[LabelSource] != "tempo" {
		t.Fatalf("source = %q, want tempo", merged[0].Attributes[LabelSource])
	}
}

func TestMergeTraceSummariesMergesDuplicates(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	merged := MergeTraceSummaries(
		[]TraceSummary{{TraceID: "trace-1", RootServiceName: "api", StartTime: base.Add(time.Second), Duration: time.Second, SpanCount: 2}},
		[]TraceSummary{{TraceID: "trace-1", StartTime: base, Duration: 3 * time.Second, SpanCount: 1}},
	)
	if len(merged) != 1 {
		t.Fatalf("merged = %#v", merged)
	}
	if !merged[0].StartTime.Equal(base) || merged[0].Duration != 3*time.Second {
		t.Fatalf("merged summary = %#v", merged[0])
	}
	if merged[0].SpanCount != 3 {
		t.Fatalf("span count = %d, want 3", merged[0].SpanCount)
	}
	if merged[0].RootServiceName != "api" {
		t.Fatalf("root service = %q, want api", merged[0].RootServiceName)
	}
}
