package proxy

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	awsstore "github.com/radryc/doctor/internal/store/aws"
)

type fakeAWSClient struct {
	enabled      bool
	err          error
	logs         []awsstore.LogRecord
	lastLogQuery awsstore.LogQuery
	metrics      []awsstore.MetricDescriptor
	services     []awsstore.ServiceInfo
	logGroups    []string
	traces       []awsstore.TraceSummary
}

func (f *fakeAWSClient) Enabled() bool { return f.enabled }

func (f *fakeAWSClient) ListMetrics(context.Context, string, string) ([]awsstore.MetricDescriptor, error) {
	return f.metrics, nil
}

func (f *fakeAWSClient) ListServices(context.Context) ([]awsstore.ServiceInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.services, nil
}

func (f *fakeAWSClient) ListLogGroups(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.logGroups, nil
}

func (f *fakeAWSClient) QueryLogs(_ context.Context, q awsstore.LogQuery) ([]awsstore.LogRecord, error) {
	f.lastLogQuery = q
	if f.err != nil {
		return nil, f.err
	}
	return f.logs, nil
}

func (f *fakeAWSClient) QueryTraces(context.Context, awsstore.TraceQuery) ([]awsstore.TraceSummary, error) {
	return f.traces, nil
}

func (f *fakeAWSClient) GetTrace(context.Context, string) ([]awsstore.Span, error) {
	return nil, nil
}

func (f *fakeAWSClient) PrometheusQuery(context.Context, string, time.Time) (map[string]any, error) {
	return nil, nil
}

func (f *fakeAWSClient) PrometheusQueryRange(context.Context, string, time.Time, time.Time, time.Duration) (map[string]any, error) {
	return nil, nil
}

func (f *fakeAWSClient) PrometheusSeries(context.Context, []string, time.Time, time.Time) (map[string]any, error) {
	return nil, nil
}

func (f *fakeAWSClient) PrometheusLabels(context.Context, []string, time.Time, time.Time) (map[string]any, error) {
	return nil, nil
}

func (f *fakeAWSClient) PrometheusLabelValues(context.Context, string, []string, time.Time, time.Time) (map[string]any, error) {
	return nil, nil
}

func TestAWSQueryModeAndStripping(t *testing.T) {
	cases := []struct {
		in        string
		wantQuery string
		wantMode  int
	}{
		{`CPUUtilization`, `CPUUtilization`, AWSQueryNone},
		{`CPUUtilization{source="aws"}`, `CPUUtilization`, AWSQueryOnly},
		{`sum(rate({CPUUtilization, source="aws"}[5m]))`, `sum(rate({CPUUtilization}[5m]))`, AWSQueryOnly},
		{`X{source="aws", InstanceId="i-1"}`, `X{InstanceId="i-1"}`, AWSQueryOnly},
		{`X{source=~"aws|doctor"}`, `X`, AWSQueryMixed},
	}
	for _, tc := range cases {
		forwarded, mode := AWSQueryMode(tc.in)
		if forwarded != tc.wantQuery || mode != tc.wantMode {
			t.Fatalf("AWSQueryMode(%q) = %q, %d; want %q, %d", tc.in, forwarded, mode, tc.wantQuery, tc.wantMode)
		}
	}
}

func TestAWSMatchesStripsForwardedSelectors(t *testing.T) {
	hasAWS, forwarded := AWSMatches([]string{`X{source="aws", a="b"}`, `Y{a="b"}`})
	if !hasAWS {
		t.Fatal("hasAWS = false, want true")
	}
	want := []string{`X{a="b"}`, `Y{a="b"}`}
	if len(forwarded) != len(want) {
		t.Fatalf("forwarded = %#v, want %#v", forwarded, want)
	}
	for i := range want {
		if forwarded[i] != want[i] {
			t.Fatalf("forwarded[%d] = %q, want %q", i, forwarded[i], want[i])
		}
	}
}

func TestAWSLogsForLokiConvertsRecords(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	client := &fakeAWSClient{
		enabled: true,
		logs: []awsstore.LogRecord{{
			Timestamp: base,
			LogGroup:  "/ecs/api",
			LogStream: "stream-1",
			Message:   "boom",
			Fields:    map[string]string{"service": "api"},
			Region:    "eu-west-1",
			Account:   "123456789012",
		}},
	}

	records, err := AWSLogsForLoki(context.Background(), client, "default", LogPushdown{
		Selector:    `{source="aws",aws_log_group="/ecs/api"}`,
		LineFilters: []LogLineFilter{{Op: "|=", Value: "boom"}},
	}, base.Add(-time.Hour), base, 100)
	if err != nil {
		t.Fatalf("AWSLogsForLoki: %v", err)
	}

	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	record := records[0]
	if record.Service != "api" {
		t.Fatalf("service = %q, want api", record.Service)
	}
	if record.Attributes[LabelSource] != SourceAWS {
		t.Fatalf("source = %q, want aws", record.Attributes[LabelSource])
	}
	if record.Attributes["aws_log_group"] != "/ecs/api" {
		t.Fatalf("aws_log_group = %q", record.Attributes["aws_log_group"])
	}
	if record.Attributes[AttrAWSRegion] != "eu-west-1" {
		t.Fatalf("aws_region = %q, want eu-west-1", record.Attributes[AttrAWSRegion])
	}
	if record.Attributes[AWSPartitionLabel] != "eu-west-1" {
		t.Fatalf("partition = %q, want eu-west-1", record.Attributes[AWSPartitionLabel])
	}
	if client.lastLogQuery.Limit != 100 {
		t.Fatalf("limit = %d, want 100", client.lastLogQuery.Limit)
	}
	if got := client.lastLogQuery.Query; got != `fields @timestamp, @message, @logStream, @log | filter @message like /boom/ | sort @timestamp desc` {
		t.Fatalf("insights query = %q", got)
	}
}

func TestAWSLogsForLokiSkipsNonAWSSelector(t *testing.T) {
	client := &fakeAWSClient{enabled: true}
	records, err := AWSLogsForLoki(context.Background(), client, "default", LogPushdown{
		Selector: `{service="api"}`,
	}, time.Time{}, time.Now(), 10)
	if err != nil {
		t.Fatalf("AWSLogsForLoki: %v", err)
	}
	if records != nil {
		t.Fatalf("records = %#v, want nil", records)
	}
	if client.lastLogQuery.LogGroups != nil {
		t.Fatalf("client was queried for a non-AWS selector")
	}
}

func TestAWSLogsForLokiPropagatesErrors(t *testing.T) {
	client := &fakeAWSClient{enabled: true, err: errors.New("insights unavailable")}
	_, err := AWSLogsForLoki(context.Background(), client, "default", LogPushdown{
		Selector: `{source="aws",aws_log_group="/ecs/api"}`,
	}, time.Now().Add(-time.Hour), time.Now(), 10)
	if err == nil {
		t.Fatal("expected AWS logs error")
	}
}

func TestAWSServicesPropagatesErrors(t *testing.T) {
	client := &fakeAWSClient{enabled: true, err: errors.New("xray unavailable")}
	if _, err := AWSServices(context.Background(), client); err == nil {
		t.Fatal("expected AWS services error")
	}
}

func TestAWSPartitionPrefersRegion(t *testing.T) {
	if got := AWSPartition("eu-west-1", "123"); got != "eu-west-1" {
		t.Fatalf("partition = %q, want eu-west-1", got)
	}
	if got := AWSPartition("", "123"); got != "123" {
		t.Fatalf("partition = %q, want 123", got)
	}
	if got := AWSPartition("", ""); got != SourceAWS {
		t.Fatalf("partition = %q, want aws", got)
	}
}

func TestApplyAWSPartitionPreservesCatalogPartition(t *testing.T) {
	attrs := map[string]string{AWSPartitionLabel: "checkout"}
	ApplyAWSPartition(attrs, "eu-west-1", "123")
	if got := attrs[AWSPartitionLabel]; got != "checkout" {
		t.Fatalf("partition = %q, want checkout", got)
	}
	attrs = map[string]string{}
	ApplyAWSPartition(attrs, "eu-west-1", "123")
	if got := attrs[AWSPartitionLabel]; got != "eu-west-1" {
		t.Fatalf("fallback partition = %q, want eu-west-1", got)
	}
}

func TestAWSUpstreamSearchTracesFiltersRegion(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	client := &fakeAWSClient{
		enabled: true,
		traces: []awsstore.TraceSummary{
			{TraceID: "1-us", Service: "api", Region: "us-east-1", Account: "111", StartTime: base.Add(-time.Minute), EndTime: base, Duration: time.Second},
			{TraceID: "1-eu", Service: "api", Region: "eu-west-1", Account: "222", StartTime: base.Add(-time.Minute), EndTime: base, Duration: time.Second},
		},
	}
	upstream := &AWSUpstream{Client: client}

	out, err := upstream.SearchTraces(context.Background(), "default", TraceSearch{
		Tags: []TraceTag{
			{Key: "source", Value: SourceAWS},
			{Key: "region", Value: "eu-.*", Regex: regexp.MustCompile("eu-.*")},
		},
	})
	if err != nil {
		t.Fatalf("SearchTraces: %v", err)
	}
	if len(out) != 1 || out[0].TraceID != "1-eu" || out[0].Region != "eu-west-1" {
		t.Fatalf("traces = %#v, want only 1-eu in eu-west-1", out)
	}

	out, err = upstream.SearchTraces(context.Background(), "default", TraceSearch{
		Tags: []TraceTag{{Key: "source", Value: SourceDoctor}},
	})
	if err != nil {
		t.Fatalf("SearchTraces(source=doctor): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("traces = %#v, want none for source=doctor", out)
	}
}

func TestAWSUpstreamTraceTagValuesExposeXRayServices(t *testing.T) {
	client := &fakeAWSClient{
		enabled:  true,
		services: []awsstore.ServiceInfo{{Name: "telemetrygen-server"}, {Name: "telemetrygen-client"}},
	}
	upstream := &AWSUpstream{Client: client}

	values, err := upstream.TraceTagValues(context.Background(), "default", "service.name")
	if err != nil {
		t.Fatalf("TraceTagValues: %v", err)
	}
	if len(values) != 2 || values[0] != "telemetrygen-client" || values[1] != "telemetrygen-server" {
		t.Fatalf("values = %#v", values)
	}

	other, err := upstream.TraceTagValues(context.Background(), "default", "http.status_code")
	if err != nil || other != nil {
		t.Fatalf("unexpected values for unknown tag: %#v, err=%v", other, err)
	}
}

func TestAWSMetricNamesSortedAndDeduped(t *testing.T) {
	client := &fakeAWSClient{
		enabled: true,
		metrics: []awsstore.MetricDescriptor{
			{MetricName: "b"},
			{MetricName: "a"},
			{MetricName: "a"},
			{MetricName: " "},
		},
	}
	names := AWSMetricNames(context.Background(), client)
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names = %#v, want [a b]", names)
	}
}
