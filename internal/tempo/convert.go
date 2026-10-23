package tempo

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// TraceFromRecords converts internal span records into the OTLP-JSON trace
// shape. Spans are grouped into one batch per service, preserving input order
// within a service.
func TraceFromRecords(records []segment.TraceRecord) Trace {
	if len(records) == 0 {
		return Trace{Batches: []Batch{}}
	}
	order := make([]string, 0)
	batches := make(map[string]*Batch)
	for _, record := range records {
		service := record.Service
		batch := batches[service]
		if batch == nil {
			batch = &Batch{Resource: Resource{Attributes: resourceAttributes(record)}}
			batches[service] = batch
			order = append(order, service)
		}
		if len(batch.ScopeSpans) == 0 {
			batch.ScopeSpans = []ScopeSpans{{}}
		}
		batch.ScopeSpans[0].Spans = append(batch.ScopeSpans[0].Spans, spanFromRecord(record))
	}
	trace := Trace{Batches: make([]Batch, 0, len(order))}
	for _, service := range order {
		trace.Batches = append(trace.Batches, *batches[service])
	}
	return trace
}

func resourceAttributes(record segment.TraceRecord) []KeyValue {
	attributes := make(map[string]string, len(record.ResourceAttributes)+1)
	if record.Service != "" {
		attributes["service.name"] = record.Service
	}
	for key, value := range record.ResourceAttributes {
		if value == "" {
			continue
		}
		attributes[key] = value
	}
	return sortedKeyValues(attributes)
}

func spanFromRecord(record segment.TraceRecord) Span {
	span := Span{
		TraceID:           record.TraceID,
		SpanID:            record.SpanID,
		ParentSpanID:      record.ParentSpanID,
		Name:              record.Name,
		Kind:              record.Kind,
		StartTimeUnixNano: strconv.FormatInt(record.StartTime.UTC().UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(record.EndTime.UTC().UnixNano(), 10),
	}
	attributes := make(map[string]string, len(record.Attributes)+3)
	for key, value := range record.Attributes {
		if value == "" {
			continue
		}
		attributes[key] = value
	}
	if record.Partition != "" {
		attributes["guardian_partition"] = record.Partition
	}
	if record.Intent != "" {
		attributes["guardian_intent"] = record.Intent
	}
	if record.Asset != "" {
		attributes["guardian_asset"] = record.Asset
	}
	span.Attributes = sortedKeyValues(attributes)
	if record.StatusCode != "" || record.StatusMessage != "" {
		span.Status = &Status{Code: record.StatusCode, Message: record.StatusMessage}
	}
	for _, event := range record.Events {
		span.Events = append(span.Events, Event{
			Name:         event.Name,
			TimeUnixNano: strconv.FormatInt(event.Timestamp.UTC().UnixNano(), 10),
			Attributes:   sortedKeyValues(event.Attributes),
		})
	}
	return span
}

func sortedKeyValues(attributes map[string]string) []KeyValue {
	if len(attributes) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]KeyValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, KeyValue{Key: key, Value: Value{StringValue: attributes[key]}})
	}
	return out
}

// SummariesFromRecords groups spans by trace and produces search hits. When
// traceIDs is non-empty only those traces are returned.
func SummariesFromRecords(records []segment.TraceRecord, traceIDs map[string]bool) []SearchTrace {
	type aggregate struct {
		traceID   string
		start     time.Time
		end       time.Time
		rootName  string
		rootSvc   string
		hasRoot   bool
		spanCount int
	}
	groups := make(map[string]*aggregate)
	for _, record := range records {
		if record.TraceID == "" {
			continue
		}
		if len(traceIDs) > 0 && !traceIDs[record.TraceID] {
			continue
		}
		group := groups[record.TraceID]
		if group == nil {
			group = &aggregate{traceID: record.TraceID, start: record.StartTime, end: record.EndTime}
			groups[record.TraceID] = group
		}
		group.spanCount++
		if record.StartTime.Before(group.start) || group.start.IsZero() {
			group.start = record.StartTime
		}
		if record.EndTime.After(group.end) {
			group.end = record.EndTime
		}
		isRoot := record.ParentSpanID == ""
		if isRoot && !group.hasRoot {
			group.hasRoot = true
			group.rootName = record.Name
			group.rootSvc = record.Service
		}
	}

	out := make([]SearchTrace, 0, len(groups))
	for _, group := range groups {
		duration := group.end.Sub(group.start)
		if duration < 0 {
			duration = 0
		}
		out = append(out, SearchTrace{
			TraceID:           group.traceID,
			RootServiceName:   group.rootSvc,
			RootTraceName:     group.rootName,
			StartTimeUnixNano: strconv.FormatInt(group.start.UTC().UnixNano(), 10),
			DurationMs:        duration.Milliseconds(),
			SpanCount:         group.spanCount,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartTimeUnixNano > out[j].StartTimeUnixNano
	})
	return out
}

// TagKeysFromRecords collects the searchable tag names from recent spans.
func TagKeysFromRecords(records []segment.TraceRecord) []string {
	keys := map[string]struct{}{
		"service.name": {},
		"name":         {},
	}
	for _, record := range records {
		for key := range record.Attributes {
			keys[key] = struct{}{}
		}
		for key := range record.ResourceAttributes {
			keys[key] = struct{}{}
		}
		if record.Kind != "" {
			keys["kind"] = struct{}{}
		}
		if record.StatusCode != "" {
			keys["status"] = struct{}{}
		}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// TagScopesFromRecords collects tag names from recent spans, grouped into the
// Tempo v2 scopes (intrinsic, resource, span).
func TagScopesFromRecords(records []segment.TraceRecord) []TagScope {
	intrinsic := map[string]struct{}{"name": {}, "status": {}, "kind": {}, "duration": {}}
	resource := map[string]struct{}{"service.name": {}}
	span := map[string]struct{}{}
	for _, record := range records {
		for key := range record.Attributes {
			span[key] = struct{}{}
		}
		for key := range record.ResourceAttributes {
			resource[key] = struct{}{}
		}
	}
	scopes := make([]TagScope, 0, 3)
	for _, scope := range []struct {
		name string
		tags map[string]struct{}
	}{
		{"intrinsic", intrinsic},
		{"resource", resource},
		{"span", span},
	} {
		tags := make([]string, 0, len(scope.tags))
		for tag := range scope.tags {
			tags = append(tags, tag)
		}
		sort.Strings(tags)
		if len(tags) > 0 {
			scopes = append(scopes, TagScope{Name: scope.name, Tags: tags})
		}
	}
	return scopes
}

// TagScopesFromNames classifies discovered tag names into Tempo's intrinsic,
// resource, and span scopes. MonoFS discovery cannot always preserve the
// original resource/span split, so known resource prefixes are used.
func TagScopesFromNames(names []string) []TagScope {
	intrinsic := map[string]struct{}{"name": {}, "status": {}, "kind": {}, "duration": {}}
	resource := map[string]struct{}{"service.name": {}}
	span := map[string]struct{}{}
	for _, name := range names {
		if _, ok := intrinsic[name]; ok {
			continue
		}
		switch {
		case name == "service.name" || strings.HasPrefix(name, "service.") ||
			strings.HasPrefix(name, "k8s.") || strings.HasPrefix(name, "host.") ||
			strings.HasPrefix(name, "container.") || strings.HasPrefix(name, "cloud.") ||
			strings.HasPrefix(name, "guardian.") || strings.HasPrefix(name, "aws."):
			resource[name] = struct{}{}
		default:
			span[name] = struct{}{}
		}
	}
	scopes := make([]TagScope, 0, 3)
	for _, scope := range []struct {
		name string
		tags map[string]struct{}
	}{
		{"intrinsic", intrinsic},
		{"resource", resource},
		{"span", span},
	} {
		tags := make([]string, 0, len(scope.tags))
		for tag := range scope.tags {
			tags = append(tags, tag)
		}
		sort.Strings(tags)
		if len(tags) > 0 {
			scopes = append(scopes, TagScope{Name: scope.name, Tags: tags})
		}
	}
	return scopes
}

// TagValuesFromRecords collects values for one tag from recent spans.
func TagValuesFromRecords(records []segment.TraceRecord, tag string) []string {
	values := map[string]struct{}{}
	add := func(value string) {
		if strings.TrimSpace(value) != "" {
			values[value] = struct{}{}
		}
	}
	for _, record := range records {
		switch tag {
		case "service.name", "service":
			add(record.Service)
		case "name":
			add(record.Name)
		case "kind":
			add(record.Kind)
		case "status", "status.code":
			add(record.StatusCode)
		case "guardian_partition":
			add(record.Partition)
		case "guardian_intent":
			add(record.Intent)
		case "guardian_asset":
			add(record.Asset)
		default:
			if value := record.Attributes[tag]; value != "" {
				add(value)
			}
			if value := record.ResourceAttributes[tag]; value != "" {
				add(value)
			}
		}
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
