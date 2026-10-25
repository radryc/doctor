package federation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	awsstore "github.com/rydzu/ainfra/doctor/internal/store/aws"
)

type fakeAWSClient struct {
	mu sync.Mutex

	metricData map[string]any
	logs       []awsstore.LogRecord
	traces     []awsstore.TraceSummary
	spans      []awsstore.Span
	err        error
	calls      []string
	logGroups  []string
	services   []awsstore.ServiceInfo
}

func (f *fakeAWSClient) Enabled() bool { return true }

func (f *fakeAWSClient) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeAWSClient) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAWSClient) ListMetrics(context.Context, string, string) ([]awsstore.MetricDescriptor, error) {
	f.record("ListMetrics")
	if f.err != nil {
		return nil, f.err
	}
	return []awsstore.MetricDescriptor{{MetricName: "Invocations", Region: "us-east-1"}}, nil
}

func (f *fakeAWSClient) ListServices(context.Context) ([]awsstore.ServiceInfo, error) {
	f.record("ListServices")
	if f.err != nil {
		return nil, f.err
	}
	if f.services != nil {
		return f.services, nil
	}
	return []awsstore.ServiceInfo{{Name: "svc", Region: "us-east-1", Account: "111111111111"}}, nil
}

func (f *fakeAWSClient) ListLogGroups(context.Context) ([]string, error) {
	f.record("ListLogGroups")
	if f.err != nil {
		return nil, f.err
	}
	return []string{"guardian/111111111111/us-east-1/div-a/app"}, nil
}

func (f *fakeAWSClient) QueryLogs(_ context.Context, q awsstore.LogQuery) ([]awsstore.LogRecord, error) {
	f.record("QueryLogs")
	f.mu.Lock()
	f.logGroups = append([]string(nil), q.LogGroups...)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.logs, nil
}

func (f *fakeAWSClient) QueryTraces(context.Context, awsstore.TraceQuery) ([]awsstore.TraceSummary, error) {
	f.record("QueryTraces")
	if f.err != nil {
		return nil, f.err
	}
	return f.traces, nil
}

func (f *fakeAWSClient) GetTrace(context.Context, string) ([]awsstore.Span, error) {
	f.record("GetTrace")
	if f.err != nil {
		return nil, f.err
	}
	return f.spans, nil
}

func (f *fakeAWSClient) PrometheusQuery(context.Context, string, time.Time) (map[string]any, error) {
	f.record("PrometheusQuery")
	if f.err != nil {
		return nil, f.err
	}
	if f.metricData != nil {
		return f.metricData, nil
	}
	return vectorResponse("Invocations"), nil
}

func (f *fakeAWSClient) PrometheusQueryRange(context.Context, string, time.Time, time.Time, time.Duration) (map[string]any, error) {
	f.record("PrometheusQueryRange")
	if f.err != nil {
		return nil, f.err
	}
	if f.metricData != nil {
		return f.metricData, nil
	}
	return matrixResponse("Invocations"), nil
}

func (f *fakeAWSClient) PrometheusSeries(context.Context, []string, time.Time, time.Time) (map[string]any, error) {
	f.record("PrometheusSeries")
	return map[string]any{"status": "success", "data": []any{}}, nil
}

func (f *fakeAWSClient) PrometheusLabels(context.Context, []string, time.Time, time.Time) (map[string]any, error) {
	f.record("PrometheusLabels")
	return map[string]any{"status": "success", "data": []string{}}, nil
}

func (f *fakeAWSClient) PrometheusLabelValues(context.Context, string, []string, time.Time, time.Time) (map[string]any, error) {
	f.record("PrometheusLabelValues")
	return map[string]any{"status": "success", "data": []string{}}, nil
}

func vectorResponse(metric string) map[string]any {
	return map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{
				map[string]any{"metric": map[string]any{"__name__": metric}, "value": []any{float64(1), "1"}},
			},
		},
	}
}

func matrixResponse(metric string) map[string]any {
	return map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "matrix",
			"result": []any{
				map[string]any{"metric": map[string]any{"__name__": metric}, "values": []any{[]any{float64(1), "1"}}},
			},
		},
	}
}

func managerTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := NewCatalog([]Target{
		{ID: "aws-a", Account: "111111111111", Region: "us-east-1", Partitions: []string{"div-a"}, Endpoint: "http://a", Token: "a", LogGroups: []string{"guardian/111111111111/us-east-1/div-a/app"}},
		{ID: "aws-b", Account: "222222222222", Region: "us-east-1", Partitions: []string{"div-b"}, Endpoint: "http://b", Token: "b"},
	}, "test")
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func TestTargetPartitionForService(t *testing.T) {
	target := Target{
		Partitions: []string{"checkout", "billing"},
		ServicePartitions: map[string]string{
			"checkout-api": "checkout",
		},
	}
	if got := targetPartitionForService(target, "checkout-api"); got != "checkout" {
		t.Fatalf("service partition = %q, want checkout", got)
	}
	if got := targetPartitionForService(target, "unknown"); got != "" {
		t.Fatalf("unknown partition = %q, want empty", got)
	}
	single := Target{Partitions: []string{"checkout"}}
	if got := targetPartitionForService(single, "anything"); got != "checkout" {
		t.Fatalf("single partition = %q, want checkout", got)
	}
}

func TestManagerRoutesAccountQueryToSingleTarget(t *testing.T) {
	fakes := map[string]*fakeAWSClient{"aws-a": {}, "aws-b": {}}
	manager := NewManager(managerTestCatalog(t), Options{
		ClientFactory: func(target Target) (proxy.AWSClient, error) { return fakes[target.ID], nil },
	})

	resp, err := manager.PrometheusQueryRange(context.Background(), `Invocations{account="111111111111"}`, time.Now().Add(-time.Hour), time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("PrometheusQueryRange() error = %v", err)
	}
	if fakes["aws-a"].CallCount() != 1 || fakes["aws-b"].CallCount() != 0 {
		t.Fatalf("calls a=%d b=%d, want a=1 b=0", fakes["aws-a"].CallCount(), fakes["aws-b"].CallCount())
	}
	data := proxy.ExtractPrometheusData(resp)
	if len(proxy.PrometheusResultSlice(data)) != 1 {
		t.Fatalf("result = %#v", data)
	}
}

func TestManagerRejectsUnscopedQuery(t *testing.T) {
	manager := NewManager(managerTestCatalog(t), Options{
		MaxFanout: 1,
		ClientFactory: func(target Target) (proxy.AWSClient, error) {
			return &fakeAWSClient{}, nil
		},
	})
	_, err := manager.PrometheusQuery(context.Background(), `sum(UnknownMetric)`, time.Now())
	if err == nil {
		t.Fatal("expected scope error")
	}
	if !errors.Is(err, ErrScopeRequired) && !errors.Is(err, ErrTooBroad) {
		t.Fatalf("error = %v, want scope error", err)
	}
}

func TestManagerLogsFilterGroupsAndStampAccount(t *testing.T) {
	fakeA := &fakeAWSClient{logs: []awsstore.LogRecord{{Timestamp: time.Now(), LogGroup: "guardian/111111111111/us-east-1/div-a/app", Message: "hello"}}}
	fakeB := &fakeAWSClient{}
	manager := NewManager(managerTestCatalog(t), Options{
		ClientFactory: func(target Target) (proxy.AWSClient, error) {
			if target.ID == "aws-a" {
				return fakeA, nil
			}
			return fakeB, nil
		},
	})

	records, err := manager.QueryLogs(context.Background(), awsstore.LogQuery{
		LogGroups: []string{"guardian/111111111111/us-east-1/div-a/app"},
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	if fakeB.CallCount() != 0 {
		t.Fatalf("target B was queried %d times", fakeB.CallCount())
	}
	if len(fakeA.logGroups) != 1 || fakeA.logGroups[0] != "guardian/111111111111/us-east-1/div-a/app" {
		t.Fatalf("target A groups = %#v", fakeA.logGroups)
	}
	if records[0].Fields["aws_account"] != "111111111111" {
		t.Fatalf("aws_account = %q", records[0].Fields["aws_account"])
	}
}

func TestManagerQueryTracesRoutesServiceAndStampsAccount(t *testing.T) {
	fakeA := &fakeAWSClient{traces: []awsstore.TraceSummary{{TraceID: "t1", Service: "svc-a", SpanCount: 2}}}
	fakeB := &fakeAWSClient{traces: []awsstore.TraceSummary{{TraceID: "t2", Service: "svc-b", SpanCount: 1}}}
	catalog, err := NewCatalog([]Target{
		{ID: "aws-a", Account: "111111111111", Region: "us-east-1", Partitions: []string{"div-a"}, Endpoint: "http://a", Token: "a", XrayServices: []string{"svc-a"}},
		{ID: "aws-b", Account: "222222222222", Region: "us-east-1", Partitions: []string{"div-b"}, Endpoint: "http://b", Token: "b", XrayServices: []string{"svc-b"}},
	}, "test")
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	manager := NewManager(catalog, Options{
		ClientFactory: func(target Target) (proxy.AWSClient, error) {
			if target.ID == "aws-a" {
				return fakeA, nil
			}
			return fakeB, nil
		},
	})
	traces, err := manager.QueryTraces(context.Background(), awsstore.TraceQuery{Service: "svc-a", Limit: 10})
	if err != nil {
		t.Fatalf("QueryTraces() error = %v", err)
	}
	if len(traces) != 1 || traces[0].TraceID != "t1" || traces[0].Account != "111111111111" {
		t.Fatalf("traces = %#v", traces)
	}
	if fakeB.CallCount() != 0 {
		t.Fatalf("target B was queried %d times", fakeB.CallCount())
	}
}

func TestStripTargetLabels(t *testing.T) {
	cases := []struct{ in, want string }{
		{`Invocations{account="1"}`, `Invocations`},
		{`Invocations{account="1",region="us-east-1"}`, `Invocations`},
		{`Invocations{account="1",foo="bar"}`, `Invocations{foo="bar"}`},
		{`Invocations{foo="bar",account="1"}`, `Invocations{foo="bar"}`},
		{`sum(Invocations{guardian_partition="div-a"})`, `sum(Invocations)`},
		{`Invocations{"@aws.account"="1"}`, `Invocations{"@aws.account"="1"}`},
	}
	for _, tc := range cases {
		if got := stripTargetLabels(tc.in); got != tc.want {
			t.Fatalf("stripTargetLabels(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestManagerPartialFailureReturnsRemainingData(t *testing.T) {
	fakeA := &fakeAWSClient{}
	fakeB := &fakeAWSClient{err: errors.New("boom")}
	manager := NewManager(managerTestCatalog(t), Options{
		MaxFanout: 2,
		ClientFactory: func(target Target) (proxy.AWSClient, error) {
			if target.ID == "aws-a" {
				return fakeA, nil
			}
			return fakeB, nil
		},
	})
	resp, err := manager.PrometheusQueryRange(context.Background(), `Invocations{region="us-east-1"}`, time.Now().Add(-time.Hour), time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("PrometheusQueryRange() error = %v", err)
	}
	if got := len(proxy.PrometheusResultSlice(proxy.ExtractPrometheusData(resp))); got != 1 {
		t.Fatalf("result length = %d, want 1", got)
	}
}

type staticTokenResolver struct {
	token string
}

func (s staticTokenResolver) Resolve(context.Context, Target) (string, error) { return s.token, nil }
func (s staticTokenResolver) Invalidate(Target)                               {}

func TestManagerResolvesTokenRefThroughClientTokenSource(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer server.Close()

	catalog, err := NewCatalog([]Target{{
		ID:           "aws-111111111111-us-east-1",
		Account:      "111111111111",
		Region:       "us-east-1",
		Endpoint:     server.URL,
		TokenRef:     "guardian-pusher://aws-pusher",
		Capabilities: []Signal{SignalMetrics},
	}}, "v1")
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	manager := NewManager(catalog, Options{
		TokenResolver: staticTokenResolver{token: "resolved-token"},
	})
	if _, err := manager.PrometheusQuery(context.Background(), `up{account="111111111111"}`, time.Now()); err != nil {
		t.Fatalf("PrometheusQuery() error = %v", err)
	}
	if gotAuth != "Bearer resolved-token" {
		t.Fatalf("Authorization = %q, want resolved token", gotAuth)
	}
}
