package proxy

import (
	"context"
	"testing"
	"time"

	gcpstore "github.com/radryc/doctor/internal/store/gcp"
)

type fakeGCPLogClient struct {
	GCPClient
	records []gcpstore.LogRecord
}

func (f *fakeGCPLogClient) Enabled() bool { return true }

func (f *fakeGCPLogClient) QueryLogs(context.Context, gcpstore.LogQuery) ([]gcpstore.LogRecord, error) {
	return f.records, nil
}

func TestGCPUpstreamQueryLogsCarriesTraceIDs(t *testing.T) {
	base := time.Date(2026, 11, 19, 13, 59, 43, 0, time.UTC)
	traceID := "c2996ca6bcca394d097a0a067dff7df3"
	spanID := "00f067aa0ba902b7"
	cases := []struct {
		name      string
		fields    map[string]string
		wantTrace string
		wantSpan  string
	}{
		{
			name:      "json payload traceId",
			fields:    map[string]string{"traceId": traceID, "spanId": spanID},
			wantTrace: traceID,
			wantSpan:  spanID,
		},
		{
			name:      "cloud logging trace resource",
			fields:    map[string]string{"trace": "projects/strata-gcp-dev/traces/" + traceID},
			wantTrace: traceID,
		},
		{
			name:      "w3c traceparent",
			fields:    map[string]string{"trace_id": "00-" + traceID + "-" + spanID + "-01"},
			wantTrace: traceID,
			wantSpan:  spanID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &GCPUpstream{Client: &fakeGCPLogClient{records: []gcpstore.LogRecord{{
				Timestamp: base,
				LogGroup:  "run.googleapis.com/stdout",
				Message:   `{"event":"request"}`,
				Fields:    tc.fields,
				Region:    "europe-west1",
				Project:   "strata-gcp-dev",
			}}}}
			records, err := upstream.QueryLogs(
				context.Background(),
				"default",
				LogPushdown{Selector: `{source="gcp"}`},
				base.Add(-time.Hour),
				base.Add(time.Hour),
				10,
			)
			if err != nil {
				t.Fatalf("QueryLogs: %v", err)
			}
			if len(records) != 1 {
				t.Fatalf("records = %#v", records)
			}
			if records[0].TraceID != tc.wantTrace {
				t.Fatalf("trace_id = %q, want %q", records[0].TraceID, tc.wantTrace)
			}
			if records[0].SpanID != tc.wantSpan {
				t.Fatalf("span_id = %q, want %q", records[0].SpanID, tc.wantSpan)
			}
		})
	}
}

func TestGCPUpstreamQueryLogsUsesGuardianPartition(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	upstream := &GCPUpstream{Client: &fakeGCPLogClient{records: []gcpstore.LogRecord{{
		Timestamp: base,
		LogGroup:  "run.googleapis.com/stdout",
		LogStream: "guardian-diva-shared-gcp-app-00002-876",
		Message:   `{"event":"request","guardian_partition":"diva-shared"}`,
		Fields: map[string]string{
			"guardian_partition": "diva-shared",
			"service":            "diva-shared.app.gcp",
		},
		Region:  "europe-west1",
		Project: "strata-gcp-dev",
	}}}}

	records, err := upstream.QueryLogs(
		context.Background(),
		"default",
		LogPushdown{Selector: `{source="gcp"}`},
		base.Add(-time.Hour),
		base.Add(time.Hour),
		10,
	)
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Partition != "diva-shared" {
		t.Fatalf("partition = %q, want diva-shared", records[0].Partition)
	}
	if records[0].Service != "diva-shared.app.gcp" {
		t.Fatalf("service = %q, want diva-shared.app.gcp", records[0].Service)
	}
	if records[0].ResourceAttributes["guardian_partition"] != "diva-shared" {
		t.Fatalf("resource guardian_partition = %q", records[0].ResourceAttributes["guardian_partition"])
	}
}
