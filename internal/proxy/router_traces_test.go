package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/segment"
)

type fakeTracesUpstream struct {
	name       string
	services   []string
	summaries  []TraceSummary
	records    []segment.TraceRecord
	tags       []string
	tagValues  []string
	err        error
	traceql    []TraceSummary
	searchSeen []TraceSearch
}

func (f *fakeTracesUpstream) Name() string { return f.name }

func (f *fakeTracesUpstream) ListServices(context.Context, string) ([]string, error) {
	return f.services, f.err
}

func (f *fakeTracesUpstream) SearchTraces(_ context.Context, _ string, search TraceSearch) ([]TraceSummary, error) {
	f.searchSeen = append(f.searchSeen, search)
	return f.summaries, f.err
}

func (f *fakeTracesUpstream) GetTrace(context.Context, string, string) ([]segment.TraceRecord, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	return f.records, len(f.records) > 0, nil
}

func (f *fakeTracesUpstream) TraceTags(context.Context, string) ([]string, error) {
	return f.tags, f.err
}

func (f *fakeTracesUpstream) TraceTagValues(context.Context, string, string) ([]string, error) {
	return f.tagValues, f.err
}

type fakeTraceQLUpstream struct {
	fakeTracesUpstream
	traceql []TraceSummary
	queries []string
}

func (f *fakeTraceQLUpstream) SearchTraceQL(_ context.Context, _, query string, _, _ time.Time, _ int) ([]TraceSummary, error) {
	f.queries = append(f.queries, query)
	return f.traceql, f.err
}

func newTraceRouter(t *testing.T, upstreams map[string]TracesUpstream, kinds map[string]string) *Router {
	t.Helper()
	router := NewRouter(RouterConfig{MaxInflight: 8, BackendTimeout: time.Second}, NewCompositeRegistry())
	for name, upstream := range upstreams {
		kind := kinds[name]
		backend := Backend{Name: name, Kind: kind, Signals: []Signal{SignalTraces}, Policy: PolicyAlways}
		if kind == KindTempo {
			backend.BaseURL = "http://" + name
		}
		if err := router.Register(backend, Upstreams{Traces: upstream}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	return router
}

func TestRouterTraceSearchMergesAndDedupes(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tempoA := &fakeTracesUpstream{name: "tempo-a", summaries: []TraceSummary{
		{TraceID: "trace-1", RootServiceName: "api", StartTime: base.Add(time.Second), Duration: time.Second, SpanCount: 1},
		{TraceID: "trace-2", RootServiceName: "worker", StartTime: base, Duration: 500 * time.Millisecond, SpanCount: 1},
	}}
	tempoB := &fakeTracesUpstream{name: "tempo-b", summaries: []TraceSummary{
		{TraceID: "trace-1", StartTime: base, Duration: 2 * time.Second, SpanCount: 2},
	}}
	router := newTraceRouter(t,
		map[string]TracesUpstream{"tempo-a": tempoA, "tempo-b": tempoB},
		map[string]string{"tempo-a": KindTempo, "tempo-b": KindTempo},
	)

	summaries, warnings, err := router.SearchTraces(context.Background(), "default", TraceSearch{Service: "api"})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("SearchTraces: warnings=%#v err=%v", warnings, err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %#v", summaries)
	}
	merged := summaries[0]
	if merged.TraceID != "trace-1" || !merged.StartTime.Equal(base) || merged.Duration != 2*time.Second || merged.SpanCount != 3 {
		t.Fatalf("merged summary = %#v", merged)
	}
	if merged.RootServiceName != "api" {
		t.Fatalf("root service = %q, want api", merged.RootServiceName)
	}
}

func TestRouterGetTraceShardsOverlap(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tempoA := &fakeTracesUpstream{name: "tempo-a", records: []segment.TraceRecord{traceSpan("trace-1", "span-1", "api", base)}}
	tempoB := &fakeTracesUpstream{name: "tempo-b", records: []segment.TraceRecord{traceSpan("trace-1", "span-1", "api", base)}}
	router := newTraceRouter(t,
		map[string]TracesUpstream{"tempo-a": tempoA, "tempo-b": tempoB},
		map[string]string{"tempo-a": KindTempo, "tempo-b": KindTempo},
	)

	records, warnings, err := router.GetTrace(context.Background(), "default", "trace-1")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("GetTrace: warnings=%#v err=%v", warnings, err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Attributes[LabelShard] != "tempo-a" || records[1].Attributes[LabelShard] != "tempo-b" {
		t.Fatalf("shards = %q, %q", records[0].Attributes[LabelShard], records[1].Attributes[LabelShard])
	}
}

func TestRouterTraceQLRoutesOnlyToCapableBackends(t *testing.T) {
	tempo := &fakeTraceQLUpstream{
		fakeTracesUpstream: fakeTracesUpstream{name: "tempo-a"},
		traceql:            []TraceSummary{{TraceID: "trace-1"}},
	}
	plain := &fakeTracesUpstream{name: "tempo-b", summaries: []TraceSummary{{TraceID: "ignored"}}}
	router := newTraceRouter(t,
		map[string]TracesUpstream{"tempo-a": tempo, "tempo-b": plain},
		map[string]string{"tempo-a": KindTempo, "tempo-b": KindTempo},
	)

	summaries, _, err := router.SearchTraces(context.Background(), "default", TraceSearch{TraceQL: `{ .name = "x" }`})
	if err != nil {
		t.Fatalf("SearchTraces: %v", err)
	}
	if len(summaries) != 1 || summaries[0].TraceID != "trace-1" {
		t.Fatalf("summaries = %#v", summaries)
	}
	if len(tempo.queries) != 1 {
		t.Fatalf("traceql queries = %#v", tempo.queries)
	}
	if len(plain.searchSeen) != 0 {
		t.Fatalf("plain backend received a TraceQL search: %#v", plain.searchSeen)
	}
}

func TestRouterTraceTagsUnion(t *testing.T) {
	tempoA := &fakeTracesUpstream{name: "tempo-a", tags: []string{"service.name", "http.method"}}
	tempoB := &fakeTracesUpstream{name: "tempo-b", tags: []string{"service.name", "k8s.pod.name"}}
	router := newTraceRouter(t,
		map[string]TracesUpstream{"tempo-a": tempoA, "tempo-b": tempoB},
		map[string]string{"tempo-a": KindTempo, "tempo-b": KindTempo},
	)

	tags, _, err := router.TraceTags(context.Background(), "default")
	if err != nil {
		t.Fatalf("TraceTags: %v", err)
	}
	if len(tags) != 3 {
		t.Fatalf("tags = %#v", tags)
	}
	values, _, err := router.TraceTagValues(context.Background(), "default", "service.name")
	if err != nil {
		t.Fatalf("TraceTagValues: %v", err)
	}
	_ = values
}

func TestRouterTraceRemoteFailureWarns(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	local := &fakeTracesUpstream{name: "doctor", records: []segment.TraceRecord{traceSpan("trace-1", "span-1", "api", base)}}
	remote := &fakeTracesUpstream{name: "tempo-a", err: errors.New("boom")}
	router := newTraceRouter(t,
		map[string]TracesUpstream{"doctor": local, "tempo-a": remote},
		map[string]string{"doctor": KindMonoFS, "tempo-a": KindTempo},
	)

	records, warnings, err := router.GetTrace(context.Background(), "default", "trace-1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(records) != 1 || len(warnings) != 1 || warnings[0].Backend != "tempo-a" {
		t.Fatalf("records=%#v warnings=%#v", records, warnings)
	}
}
