package proxy

import (
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

func logRecord(timestamp time.Time, source, body string) segment.LogRecord {
	attributes := map[string]string{}
	if source != "" {
		attributes[LabelSource] = source
	}
	return segment.LogRecord{
		Service:    "api",
		Timestamp:  timestamp,
		Body:       body,
		Attributes: attributes,
	}
}

func TestMergeLogsAppendsDistinctRecords(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	local := []segment.LogRecord{logRecord(base, SourceDoctor, "local")}
	remote := []segment.LogRecord{logRecord(base, SourceAWS, "remote")}

	merged := MergeLogs(local, LogRemote{Source: SourceAWS, Records: remote})
	if len(merged) != 2 {
		t.Fatalf("merged length = %d, want 2", len(merged))
	}
	if merged[0].Body != "local" || merged[1].Body != "remote" {
		t.Fatalf("merged order = %q, %q", merged[0].Body, merged[1].Body)
	}
	if _, hasShard := merged[1].Attributes[LabelShard]; hasShard {
		t.Fatalf("distinct records were sharded: %#v", merged[1].Attributes)
	}
}

func TestMergeLogsTagsSourceWhenMissing(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	merged := MergeLogs(nil, LogRemote{Source: "loki-a", Records: []segment.LogRecord{logRecord(base, "", "x")}})
	if len(merged) != 1 {
		t.Fatalf("merged length = %d, want 1", len(merged))
	}
	if merged[0].Attributes[LabelSource] != "loki-a" {
		t.Fatalf("source = %q, want loki-a", merged[0].Attributes[LabelSource])
	}
}

func TestMergeLogsDedupesSameSourceDuplicates(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	record := logRecord(base, SourceAWS, "dup")
	merged := MergeLogs(nil, LogRemote{Source: SourceAWS, Records: []segment.LogRecord{record, record}})
	if len(merged) != 1 {
		t.Fatalf("merged length = %d, want 1", len(merged))
	}
}

func TestMergeLogsShardsCrossBackendOverlap(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Two shards of the same kind return the same line: both are tagged
	// source=loki, so they collide and get shard labels.
	merged := MergeLogs(nil,
		LogRemote{Source: "loki", Backend: "loki-a", Records: []segment.LogRecord{logRecord(base, "", "same")}},
		LogRemote{Source: "loki", Backend: "loki-b", Records: []segment.LogRecord{logRecord(base, "", "same")}},
	)
	if len(merged) != 2 {
		t.Fatalf("merged length = %d, want 2", len(merged))
	}
	if merged[0].Attributes[LabelSource] != "loki" || merged[1].Attributes[LabelSource] != "loki" {
		t.Fatalf("sources = %q, %q; want loki for both", merged[0].Attributes[LabelSource], merged[1].Attributes[LabelSource])
	}
	if merged[0].Attributes[LabelShard] != "loki-a" {
		t.Fatalf("first shard = %q, want loki-a", merged[0].Attributes[LabelShard])
	}
	if merged[1].Attributes[LabelShard] != "loki-b" {
		t.Fatalf("second shard = %q, want loki-b", merged[1].Attributes[LabelShard])
	}
}

func TestMergeLogsDoesNotMutateCallerRecords(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	remote := logRecord(base, "", "x")
	_ = MergeLogs(nil, LogRemote{Source: "loki-a", Records: []segment.LogRecord{remote}})
	if _, exists := remote.Attributes[LabelSource]; exists {
		t.Fatalf("caller record was mutated: %#v", remote.Attributes)
	}
}
