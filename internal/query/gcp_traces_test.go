package query

import (
	"context"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/proxy"
	gcpstore "github.com/radryc/doctor/internal/store/gcp"
)

type fakeGCPTraceClient struct {
	proxy.GCPClient
	traces []gcpstore.TraceSummary
	logs   []gcpstore.LogRecord
}

func (f *fakeGCPTraceClient) Enabled() bool { return true }

func (f *fakeGCPTraceClient) QueryTraces(context.Context, gcpstore.TraceQuery) ([]gcpstore.TraceSummary, error) {
	return f.traces, nil
}

func (f *fakeGCPTraceClient) QueryLogs(context.Context, gcpstore.LogQuery) ([]gcpstore.LogRecord, error) {
	return f.logs, nil
}

func TestQueryGCPTracePageUsesLogPartitionMap(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	traceID := "003d7f5064a0f777e45f37c2d4039907"
	svc := &Service{gcp: &fakeGCPTraceClient{
		traces: []gcpstore.TraceSummary{{
			TraceID:   traceID,
			Service:   "/health",
			StartTime: base,
			EndTime:   base.Add(25 * time.Millisecond),
			SpanCount: 1,
			Region:    "europe-west1",
			Project:   "strata-gcp-dev",
		}},
		logs: []gcpstore.LogRecord{{
			Timestamp: base,
			LogGroup:  "run.googleapis.com/stdout",
			LogStream: "guardian-diva-shared-gcp-app-00002-876",
			Fields: map[string]string{
				"traceId":            traceID,
				"guardian_partition": "diva-shared",
			},
			Region:  "europe-west1",
			Project: "strata-gcp-dev",
		}},
	}}

	out, _ := svc.queryGCPTracePage(context.Background(), "", signalIdentity{Partition: "diva-shared"}, base.Add(-time.Hour), base.Add(time.Hour), 10, nil)
	if len(out) != 1 {
		t.Fatalf("traces = %#v, want one diva-shared trace", out)
	}
	if out[0].Partition != "diva-shared" {
		t.Fatalf("partition = %q, want diva-shared", out[0].Partition)
	}
}

func TestQueryGCPTracePageFallsBackToRegion(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	traceID := "003d7f5064a0f777e45f37c2d4039907"
	svc := &Service{gcp: &fakeGCPTraceClient{
		traces: []gcpstore.TraceSummary{{
			TraceID:   traceID,
			Service:   "/health",
			StartTime: base,
			EndTime:   base.Add(25 * time.Millisecond),
			SpanCount: 1,
			Region:    "europe-west1",
			Project:   "strata-gcp-dev",
		}},
	}}
	out, _ := svc.queryGCPTracePage(context.Background(), "", signalIdentity{Partition: "europe-west1"}, base.Add(-time.Hour), base.Add(time.Hour), 10, nil)
	if len(out) != 1 {
		t.Fatalf("traces = %#v, want one region trace", out)
	}
	if out[0].Partition != "europe-west1" {
		t.Fatalf("partition = %q, want europe-west1", out[0].Partition)
	}
	if got, _ := svc.queryGCPTracePage(context.Background(), "", signalIdentity{Partition: "diva-shared"}, base.Add(-time.Hour), base.Add(time.Hour), 10, nil); len(got) != 0 {
		t.Fatalf("diva-shared traces = %#v, want none without a log mapping", got)
	}
}
