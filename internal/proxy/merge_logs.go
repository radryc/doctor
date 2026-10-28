package proxy

import (
	"sort"
	"strings"

	"github.com/radryc/doctor/internal/segment"
)

// LogRemote is one backend's log query result. Source is the provenance label
// (backend kind), Backend is the backend name used for shard labeling and
// defaults to Source.
type LogRemote struct {
	Source  string
	Backend string
	Records []segment.LogRecord
}

func (r LogRemote) backend() string {
	if strings.TrimSpace(r.Backend) != "" {
		return r.Backend
	}
	return r.Source
}

// MergeLogs merges remote log records into the local result. Remote records are
// tagged with source=<remote.Source> when they do not carry one, and records
// that collide across backends receive a shard label. Duplicate records from
// the same backend are dropped.
func MergeLogs(local []segment.LogRecord, remotes ...LogRemote) []segment.LogRecord {
	out := make([]segment.LogRecord, 0, len(local))
	out = append(out, local...)

	index := make(map[string]int, len(out))
	backendOf := make(map[string]string, len(out))
	for i := range out {
		key := LogRecordKey(out[i])
		if _, exists := index[key]; !exists {
			index[key] = i
			backendOf[key] = logRecordSource(out[i])
		}
	}

	for _, remote := range remotes {
		backend := remote.backend()
		for _, record := range remote.Records {
			record = cloneLogRecord(record)
			if remote.Source != "" {
				setLogAttribute(&record, LabelSource, remote.Source)
			}
			key := LogRecordKey(record)
			if existing, ok := index[key]; ok {
				if backendOf[key] == backend {
					continue
				}
				// Cross-backend overlap: keep both records, disambiguated.
				out[existing] = withLogShard(out[existing], backendOf[key])
				setLogAttribute(&record, LabelShard, backend)
			}
			index[key] = len(out)
			backendOf[key] = backend
			out = append(out, record)
		}
	}
	return out
}

// LogRecordKey builds the identity used for cross-backend dedup: timestamp,
// service, body, correlation IDs, and the sorted attribute set.
func LogRecordKey(record segment.LogRecord) string {
	var builder strings.Builder
	builder.WriteString(record.Timestamp.UTC().Format("20060102T150405.000000000"))
	builder.WriteByte(0)
	builder.WriteString(record.Service)
	builder.WriteByte(0)
	builder.WriteString(record.TraceID)
	builder.WriteByte(0)
	builder.WriteString(record.SpanID)
	builder.WriteByte(0)
	builder.WriteString(record.Body)
	writeSortedAttributes(&builder, record.Attributes)
	builder.WriteByte(1)
	writeSortedAttributes(&builder, record.ResourceAttributes)
	return builder.String()
}

func writeSortedAttributes(builder *strings.Builder, attributes map[string]string) {
	if len(attributes) == 0 {
		return
	}
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		builder.WriteByte(0)
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(attributes[key])
	}
}

func logRecordSource(record segment.LogRecord) string {
	if source := strings.TrimSpace(record.Attributes[LabelSource]); source != "" {
		return source
	}
	if source := strings.TrimSpace(record.ResourceAttributes[LabelSource]); source != "" {
		return source
	}
	return SourceDoctor
}

func cloneLogRecord(record segment.LogRecord) segment.LogRecord {
	record.Attributes = cloneStringMap(record.Attributes)
	record.ResourceAttributes = cloneStringMap(record.ResourceAttributes)
	return record
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func setLogAttribute(record *segment.LogRecord, key, value string) {
	if record.Attributes == nil {
		record.Attributes = make(map[string]string, 1)
	}
	record.Attributes[key] = value
}

func withLogShard(record segment.LogRecord, shard string) segment.LogRecord {
	record = cloneLogRecord(record)
	if _, exists := record.Attributes[LabelShard]; !exists {
		setLogAttribute(&record, LabelShard, shard)
	}
	return record
}
