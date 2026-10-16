package proxy

import (
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy/translate"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// ApplyTraceQL parses the supported TraceQL subset and folds it into the
// search. It returns an error wrapping translate.ErrUnsupported for constructs
// outside the subset.
func (s *TraceSearch) ApplyTraceQL() error {
	if strings.TrimSpace(s.TraceQL) == "" {
		return nil
	}
	translated, err := translate.ParseTraceQL(s.TraceQL)
	if err != nil {
		return err
	}
	s.TraceQL = ""
	if translated.Service != "" {
		s.Service = translated.Service
	}
	for _, tag := range translated.Tags {
		s.Tags = append(s.Tags, TraceTag{Key: tag.Key, Value: tag.Value, Regex: tag.Regex})
	}
	for _, tag := range translated.NotTags {
		s.NotTags = append(s.NotTags, TraceTag{Key: tag.Key, Value: tag.Value, Regex: tag.Regex})
	}
	s.Exists = append(s.Exists, translated.Exists...)
	s.Absent = append(s.Absent, translated.Absent...)
	s.RootOnly = s.RootOnly || translated.RootOnly
	s.MatchNone = s.MatchNone || translated.MatchNone
	if translated.MinSpanDuration > 0 {
		s.MinSpanDuration = translated.MinSpanDuration
	}
	if translated.MaxSpanDuration > 0 {
		s.MaxSpanDuration = translated.MaxSpanDuration
	}
	return nil
}

// MatchTraceRecord reports whether one span record satisfies the search
// filters. TraceQL comparison semantics apply: a negative matcher only matches
// records where the attribute is present.
func MatchTraceRecord(record segment.TraceRecord, search TraceSearch) bool {
	if search.MatchNone {
		return false
	}
	if search.Service != "" && record.Service != search.Service {
		return false
	}
	if search.RootOnly && !isRootSpan(record) {
		return false
	}
	if search.MinSpanDuration > 0 || search.MaxSpanDuration > 0 {
		duration := spanDuration(record)
		if search.MinSpanDuration > 0 && duration < search.MinSpanDuration {
			return false
		}
		if search.MaxSpanDuration > 0 && duration > search.MaxSpanDuration {
			return false
		}
	}
	for _, tag := range search.Tags {
		value, found := TraceRecordValue(record, tag.Key)
		if !found || !tag.Matches(value) {
			return false
		}
	}
	for _, tag := range search.NotTags {
		value, found := TraceRecordValue(record, tag.Key)
		if !found || tag.Matches(value) {
			return false
		}
	}
	for _, key := range search.Exists {
		value, found := TraceRecordValue(record, key)
		if !found || value == "" {
			return false
		}
	}
	for _, key := range search.Absent {
		if value, found := TraceRecordValue(record, key); found && value != "" {
			return false
		}
	}
	return true
}

// TraceRecordValue resolves a filter key against a span record. Keys may be
// span intrinsics, canonical identity fields, scoped attributes
// (resource.<attr>, span.<attr>), or bare attribute names.
func TraceRecordValue(record segment.TraceRecord, key string) (string, bool) {
	if rest, ok := strings.CutPrefix(key, "resource."); ok {
		if value, found := record.ResourceAttributes[rest]; found {
			return value, true
		}
		if value, found := record.Attributes[rest]; found {
			return value, true
		}
		return "", false
	}
	if rest, ok := strings.CutPrefix(key, "span."); ok {
		if value, found := record.Attributes[rest]; found {
			return value, true
		}
		return "", false
	}
	switch key {
	case "service.name", "service":
		return record.Service, record.Service != ""
	case "name":
		return record.Name, record.Name != ""
	case "kind":
		return record.Kind, record.Kind != ""
	case "status", "status.code":
		return record.StatusCode, record.StatusCode != ""
	case "guardian_partition":
		return record.Partition, record.Partition != ""
	case "guardian_intent":
		return record.Intent, record.Intent != ""
	case "guardian_asset":
		return record.Asset, record.Asset != ""
	}
	if value, found := record.Attributes[key]; found {
		return value, true
	}
	if value, found := record.ResourceAttributes[key]; found {
		return value, true
	}
	return "", false
}

func isRootSpan(record segment.TraceRecord) bool {
	parent := strings.TrimSpace(record.ParentSpanID)
	return parent == "" || parent == "0000000000000000"
}

func spanDuration(record segment.TraceRecord) time.Duration {
	if record.StartTime.IsZero() || record.EndTime.IsZero() {
		return 0
	}
	duration := record.EndTime.Sub(record.StartTime)
	if duration < 0 {
		return 0
	}
	return duration
}
