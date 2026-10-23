package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

type fakeXRayPusher struct {
	server *httptest.Server
	mu     sync.Mutex

	services  []awsstore.ServiceInfo
	summaries []awsstore.TraceSummary
	spans     map[string][]awsstore.Span
	failTrace bool

	lastService string
	lastFrom    time.Time
	lastTo      time.Time
}

func newFakeXRayPusher(t *testing.T, services []awsstore.ServiceInfo, summaries []awsstore.TraceSummary, spans map[string][]awsstore.Span, failTrace bool) *fakeXRayPusher {
	t.Helper()
	pusher := &fakeXRayPusher{services: services, summaries: summaries, spans: spans, failTrace: failTrace}
	pusher.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/aws/xray/services":
			writeTestJSON(w, pusher.services)
		case r.URL.Path == "/v1/aws/xray/traces":
			pusher.mu.Lock()
			pusher.lastService = r.URL.Query().Get("service")
			pusher.lastFrom, _ = time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
			pusher.lastTo, _ = time.Parse(time.RFC3339Nano, r.URL.Query().Get("to"))
			fail := pusher.failTrace
			pusher.mu.Unlock()
			if fail {
				http.Error(w, "xray unavailable", http.StatusInternalServerError)
				return
			}
			writeTestJSON(w, pusher.summaries)
		case strings.HasPrefix(r.URL.Path, "/v1/aws/xray/trace/") || strings.HasPrefix(r.URL.Path, "/v1/aws/xray/traces/"):
			traceID := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1/aws/xray/trace/"), "/v1/aws/xray/traces/")
			pusher.mu.Lock()
			spans := pusher.spans[traceID]
			pusher.mu.Unlock()
			writeTestJSON(w, spans)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(pusher.server.Close)
	return pusher
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestXRayPusherHTTPFixture(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	traceID := "1-abcdef12-1234567890abcdef12345678"
	wantTraceID := "abcdef121234567890abcdef12345678"
	pusher := newFakeXRayPusher(t,
		[]awsstore.ServiceInfo{{Name: "api", Region: "us-east-1", Account: "111111111111"}},
		[]awsstore.TraceSummary{{TraceID: traceID, Service: "api", StartTime: base.Add(-2 * time.Second), EndTime: base, Duration: 2 * time.Second, SpanCount: 2}},
		map[string][]awsstore.Span{traceID: {
			{TraceID: traceID, SpanID: "s1", Name: "GET /api", Service: "api", StartTime: base.Add(-2 * time.Second), EndTime: base, Region: "us-east-1", Account: "111111111111"},
			{TraceID: traceID, SpanID: "s2", ParentSpanID: "s1", Name: "DynamoDB", Service: "api", StartTime: base.Add(-time.Second), EndTime: base, Region: "us-east-1", Account: "111111111111"},
		}},
		false,
	)
	client := awsstore.NewClient(pusher.server.URL, "")
	upstream := &proxy.AWSUpstream{Client: client}

	services, err := upstream.ListServices(context.Background(), "default")
	if err != nil || len(services) != 1 || services[0] != "api" {
		t.Fatalf("services = %#v, %v", services, err)
	}
	summaries, err := upstream.SearchTraces(context.Background(), "default", proxy.TraceSearch{
		Service: "api",
		From:    base.Add(-time.Hour),
		To:      base.Add(time.Minute),
	})
	if err != nil || len(summaries) != 1 || summaries[0].TraceID != wantTraceID {
		t.Fatalf("summaries = %#v, %v", summaries, err)
	}
	pusher.mu.Lock()
	if pusher.lastService != "api" || pusher.lastFrom.IsZero() || pusher.lastTo.IsZero() {
		t.Fatalf("time/service query missing: service=%q from=%s to=%s", pusher.lastService, pusher.lastFrom, pusher.lastTo)
	}
	pusher.mu.Unlock()

	spans, _, err := upstream.GetTrace(context.Background(), "default", traceID)
	if err != nil || len(spans) != 2 {
		t.Fatalf("spans = %#v, %v", spans, err)
	}
	if spans[0].Attributes[proxy.LabelSource] != proxy.SourceAWS {
		t.Fatalf("span source = %q", spans[0].Attributes[proxy.LabelSource])
	}
	if spans[0].Attributes[proxy.AttrAWSAccount] != "111111111111" {
		t.Fatalf("span account = %q", spans[0].Attributes[proxy.AttrAWSAccount])
	}
}

func TestXRayFederationMergesDedupesAndStamps(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	traceA := "1-aaaaaaaa-aaaaaaaaaaaaaaaaaaaaaaaa"
	traceB := "1-bbbbbbbb-bbbbbbbbbbbbbbbbbbbbbbbb"
	pusherA := newFakeXRayPusher(t,
		[]awsstore.ServiceInfo{{Name: "api"}},
		[]awsstore.TraceSummary{{TraceID: traceA, Service: "api", StartTime: base.Add(-2 * time.Second), EndTime: base, Duration: 2 * time.Second, SpanCount: 3}},
		map[string][]awsstore.Span{traceA: {
			{TraceID: traceA, SpanID: "s1", Name: "root", Service: "api", StartTime: base.Add(-2 * time.Second), EndTime: base},
			{TraceID: traceA, SpanID: "s2", ParentSpanID: "s1", Name: "db", Service: "api", StartTime: base.Add(-time.Second), EndTime: base},
		}},
		false,
	)
	pusherB := newFakeXRayPusher(t,
		[]awsstore.ServiceInfo{{Name: "api"}},
		[]awsstore.TraceSummary{
			{TraceID: traceA, Service: "api", StartTime: base.Add(-2 * time.Second), EndTime: base, Duration: 2 * time.Second, SpanCount: 1},
			{TraceID: traceB, Service: "api", StartTime: base.Add(-time.Second), EndTime: base, Duration: time.Second, SpanCount: 2},
		},
		map[string][]awsstore.Span{traceA: {
			{TraceID: traceA, SpanID: "s1", Name: "root", Service: "api", StartTime: base.Add(-2 * time.Second), EndTime: base},
			{TraceID: traceA, SpanID: "s3", ParentSpanID: "s1", Name: "queue", Service: "api", StartTime: base.Add(-time.Second), EndTime: base},
		}},
		false,
	)
	catalog, err := NewCatalog([]Target{
		{ID: "aws-a", Account: "111111111111", Region: "us-east-1", Endpoint: pusherA.server.URL, Token: "a", XrayServices: []string{"api"}},
		{ID: "aws-b", Account: "222222222222", Region: "us-west-2", Endpoint: pusherB.server.URL, Token: "b", XrayServices: []string{"api"}},
	}, "xray")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	manager := NewManager(catalog, Options{MaxFanout: 2, MaxAttempts: 1, Timeout: time.Second})

	traces, err := manager.QueryTraces(context.Background(), awsstore.TraceQuery{
		Service: "api",
		From:    base.Add(-time.Hour),
		To:      base.Add(time.Minute),
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(traces) != 2 {
		t.Fatalf("traces = %#v, want 2", traces)
	}
	if traces[0].TraceID != traceB && traces[1].TraceID != traceB {
		t.Fatalf("traceB missing: %#v", traces)
	}
	for _, trace := range traces {
		if trace.TraceID == traceA && trace.SpanCount != 3 {
			t.Fatalf("duplicate trace kept lower span count: %#v", trace)
		}
	}

	spans, err := manager.GetTrace(context.Background(), traceA)
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if len(spans) != 3 {
		t.Fatalf("spans = %#v, want 3 deduped spans", spans)
	}
	for _, span := range spans {
		if span.TraceID != traceA {
			t.Fatalf("span trace = %q", span.TraceID)
		}
		if span.Account == "" || span.Region == "" {
			t.Fatalf("span not account/region stamped: %#v", span)
		}
	}
}

func TestXRayFederationPartialFailure(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	traceID := "1-cccccccc-cccccccccccccccccccccccc"
	good := newFakeXRayPusher(t,
		[]awsstore.ServiceInfo{{Name: "api"}},
		[]awsstore.TraceSummary{{TraceID: traceID, Service: "api", StartTime: base.Add(-time.Second), EndTime: base, Duration: time.Second, SpanCount: 1}},
		nil,
		false,
	)
	bad := newFakeXRayPusher(t, nil, nil, nil, true)
	catalog, err := NewCatalog([]Target{
		{ID: "aws-good", Account: "111111111111", Region: "us-east-1", Endpoint: good.server.URL, Token: "a", XrayServices: []string{"api"}},
		{ID: "aws-bad", Account: "222222222222", Region: "us-east-1", Endpoint: bad.server.URL, Token: "b", XrayServices: []string{"api"}},
	}, "xray")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	manager := NewManager(catalog, Options{MaxFanout: 2, MaxAttempts: 1, Timeout: time.Second})
	ctx, collector := proxy.WithWarningCollector(context.Background())
	traces, err := manager.QueryTraces(ctx, awsstore.TraceQuery{Service: "api", Limit: 10})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(traces) != 1 || traces[0].TraceID != traceID {
		t.Fatalf("traces = %#v", traces)
	}
	if collector.Total() == 0 {
		t.Fatal("expected partial warning for failed X-Ray target")
	}
	if got := fmt.Sprintf("%v", collector.Warnings()); !strings.Contains(got, "aws-bad") {
		t.Fatalf("warnings = %s", got)
	}
}
