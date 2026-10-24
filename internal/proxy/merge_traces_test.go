package proxy

import (
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
	"github.com/rydzu/ainfra/doctor/internal/tempo"
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

func TestMergeTraceRecordsMergesCrossSourceDuplicate(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	local := traceSpan("trace-1", "span-1", "api", base)
	local.Attributes = map[string]string{"http.method": "GET"}
	aws := traceSpan("trace-1", "span-1", "api", base)
	aws.Attributes = map[string]string{"http.method": "GET", "aws.region": "us-east-1"}
	merged := MergeTraceRecords([]segment.TraceRecord{local}, TraceRemote{
		Source:  SourceAWS,
		Backend: "aws-federated",
		Records: []segment.TraceRecord{aws},
	})
	if len(merged) != 1 {
		t.Fatalf("merged = %d records, want 1", len(merged))
	}
	if got := merged[0].Attributes[LabelSources]; got != "aws,doctor" {
		t.Fatalf("sources = %q, want aws,doctor", got)
	}
	if got := merged[0].Attributes[LabelBackends]; got != "aws-federated,doctor" {
		t.Fatalf("backends = %q, want aws-federated,doctor", got)
	}
	if got := merged[0].Attributes["aws.region"]; got != "us-east-1" {
		t.Fatalf("merged aws.region = %q", got)
	}
}

func TestMergeSpanSetsCombinesDistinctSpans(t *testing.T) {
	dst := map[string]tempo.SpanSet{
		"trace-1": {Spans: []tempo.SearchSpan{{SpanID: "a"}}, Matched: 1},
	}
	src := map[string]tempo.SpanSet{
		"trace-1": {Spans: []tempo.SearchSpan{{SpanID: "b"}, {SpanID: "a"}}, Matched: 2},
	}
	mergeSpanSets(dst, src)
	if got := len(dst["trace-1"].Spans); got != 2 {
		t.Fatalf("spans = %d, want 2", got)
	}
	if dst["trace-1"].Matched != 2 {
		t.Fatalf("matched = %d, want 2", dst["trace-1"].Matched)
	}
}
