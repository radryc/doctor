package proxy

import (
	"sort"
	"strings"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// TraceRemote is one backend's trace lookup result.
type TraceRemote struct {
	Source  string
	Backend string
	Records []segment.TraceRecord
}

// MergeTraceRecords merges remote spans into the local result, deduplicating by
// (trace_id, span_id) and preferring the local copy. Cross-backend duplicates
// that differ are kept and tagged with shard=<backend>.
func MergeTraceRecords(local []segment.TraceRecord, remotes ...TraceRemote) []segment.TraceRecord {
	out := make([]segment.TraceRecord, 0, len(local))
	out = append(out, local...)

	index := make(map[string]int, len(out))
	backendOf := make(map[string]string, len(out))
	for i := range out {
		key := traceSpanKey(out[i])
		if key == "" {
			continue
		}
		if _, exists := index[key]; !exists {
			index[key] = i
			backendOf[key] = traceRecordSource(out[i])
		}
	}

	for _, remote := range remotes {
		backend := remote.Backend
		if strings.TrimSpace(backend) == "" {
			backend = remote.Source
		}
		for _, record := range remote.Records {
			record = cloneTraceRecord(record)
			if remote.Source != "" {
				setTraceAttribute(&record, LabelSource, remote.Source)
			}
			key := traceSpanKey(record)
			if key == "" {
				out = append(out, record)
				continue
			}
			if existing, ok := index[key]; ok {
				if backendOf[key] == backend {
					continue
				}
				out[existing] = withTraceShard(out[existing], backendOf[key])
				setTraceAttribute(&record, LabelShard, backend)
			}
			index[key] = len(out)
			backendOf[key] = backend
			out = append(out, record)
		}
	}
	return out
}

// MergeTraceSummaries deduplicates search hits by trace ID, merging timing and
// root metadata. Results are ordered newest first.
func MergeTraceSummaries(groups ...[]TraceSummary) []TraceSummary {
	order := make([]string, 0)
	byID := make(map[string]*TraceSummary)
	for _, group := range groups {
		for _, summary := range group {
			if summary.TraceID == "" {
				continue
			}
			existing := byID[summary.TraceID]
			if existing == nil {
				copied := summary
				byID[summary.TraceID] = &copied
				order = append(order, summary.TraceID)
				continue
			}
			if !summary.StartTime.IsZero() && (existing.StartTime.IsZero() || summary.StartTime.Before(existing.StartTime)) {
				existing.StartTime = summary.StartTime
			}
			if summary.Duration > existing.Duration {
				existing.Duration = summary.Duration
			}
			if existing.RootServiceName == "" {
				existing.RootServiceName = summary.RootServiceName
			}
			if existing.RootTraceName == "" {
				existing.RootTraceName = summary.RootTraceName
			}
			existing.SpanCount += summary.SpanCount
		}
	}
	out := make([]TraceSummary, 0, len(order))
	for _, traceID := range order {
		out = append(out, *byID[traceID])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].StartTime.After(out[j].StartTime)
	})
	return out
}

func traceSpanKey(record segment.TraceRecord) string {
	if record.TraceID == "" || record.SpanID == "" {
		return ""
	}
	return record.TraceID + "/" + record.SpanID
}

func traceRecordSource(record segment.TraceRecord) string {
	if source := strings.TrimSpace(record.Attributes[LabelSource]); source != "" {
		return source
	}
	return SourceDoctor
}

func cloneTraceRecord(record segment.TraceRecord) segment.TraceRecord {
	record.Attributes = cloneStringMap(record.Attributes)
	record.ResourceAttributes = cloneStringMap(record.ResourceAttributes)
	return record
}

func setTraceAttribute(record *segment.TraceRecord, key, value string) {
	if record.Attributes == nil {
		record.Attributes = make(map[string]string, 1)
	}
	record.Attributes[key] = value
}

func withTraceShard(record segment.TraceRecord, shard string) segment.TraceRecord {
	record = cloneTraceRecord(record)
	if _, exists := record.Attributes[LabelShard]; !exists {
		setTraceAttribute(&record, LabelShard, shard)
	}
	return record
}
