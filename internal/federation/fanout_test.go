package federation

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/proxy"
	awsstore "github.com/radryc/doctor/internal/store/aws"
)

type sequenceAWSClient struct {
	*fakeAWSClient

	mu     sync.Mutex
	errors []error
	calls  int
}

func newSequenceClient(errors ...error) *sequenceAWSClient {
	return &sequenceAWSClient{fakeAWSClient: &fakeAWSClient{}, errors: errors}
}

func (f *sequenceAWSClient) PrometheusQuery(context.Context, string, time.Time) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.errors) == 0 {
		return vectorResponse("Invocations"), nil
	}
	err := f.errors[0]
	if len(f.errors) > 1 {
		f.errors = f.errors[1:]
	}
	if err != nil {
		return nil, err
	}
	return vectorResponse("Invocations"), nil
}

func (f *sequenceAWSClient) PrometheusQueryRange(context.Context, string, time.Time, time.Time, time.Duration) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.errors) == 0 {
		return matrixResponse("Invocations"), nil
	}
	err := f.errors[0]
	if len(f.errors) > 1 {
		f.errors = f.errors[1:]
	}
	if err != nil {
		return nil, err
	}
	return matrixResponse("Invocations"), nil
}

func (f *sequenceAWSClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestFanoutRetriesTransientHTTPFailures(t *testing.T) {
	flaky := newSequenceClient(&awsstore.HTTPError{StatusCode: http.StatusInternalServerError, Body: "boom"}, nil)
	manager := NewManager(managerTestCatalog(t), Options{
		MaxAttempts: 2,
		ClientFactory: func(target Target) (proxy.AWSClient, error) {
			if target.ID == "aws-a" {
				return flaky, nil
			}
			return &fakeAWSClient{}, nil
		},
	})
	resp, err := manager.PrometheusQuery(context.Background(), `Invocations{account="111111111111"}`, time.Now())
	if err != nil {
		t.Fatalf("PrometheusQuery: %v", err)
	}
	if got := len(proxy.PrometheusResultSlice(proxy.ExtractPrometheusData(resp))); got != 1 {
		t.Fatalf("result length = %d, want 1", got)
	}
	if flaky.callCount() != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", flaky.callCount())
	}
}

func TestFanoutCircuitOpensAndHalfOpenProbeCloses(t *testing.T) {
	failing := newSequenceClient(&awsstore.HTTPError{StatusCode: http.StatusServiceUnavailable, Body: "down"})
	manager := NewManager(managerTestCatalog(t), Options{
		MaxAttempts:      1,
		BreakerThreshold: 5,
		BreakerCooldown:  20 * time.Millisecond,
		ClientFactory: func(target Target) (proxy.AWSClient, error) {
			return failing, nil
		},
	})
	for i := 0; i < 5; i++ {
		if _, err := manager.PrometheusQuery(context.Background(), `Invocations{account="111111111111"}`, time.Now()); err == nil {
			t.Fatal("expected failing query")
		}
	}
	if failing.callCount() != 5 {
		t.Fatalf("calls = %d, want 5", failing.callCount())
	}
	if status := manager.Status(); status["open_circuits"] != 1 {
		t.Fatalf("open_circuits = %v, want 1", status["open_circuits"])
	}
	if _, err := manager.PrometheusQuery(context.Background(), `Invocations{account="111111111111"}`, time.Now()); err == nil {
		t.Fatal("expected open-circuit error")
	}
	if failing.callCount() != 5 {
		t.Fatalf("open circuit called target: calls = %d", failing.callCount())
	}

	// Half-open probe succeeds and closes the circuit.
	failing.mu.Lock()
	failing.errors = nil
	failing.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	if _, err := manager.PrometheusQuery(context.Background(), `Invocations{account="111111111111"}`, time.Now()); err != nil {
		t.Fatalf("half-open probe: %v", err)
	}
	if failing.callCount() != 6 {
		t.Fatalf("probe calls = %d, want 6", failing.callCount())
	}
	if status := manager.Status(); status["open_circuits"] != 0 {
		t.Fatalf("open_circuits after success = %v, want 0", status["open_circuits"])
	}
}

func TestClientPoolEvictsLeastRecentlyUsed(t *testing.T) {
	catalog, err := NewCatalog([]Target{
		{ID: "aws-a", Account: "1", Region: "us-east-1", Endpoint: "http://a", Token: "a"},
		{ID: "aws-b", Account: "2", Region: "us-east-1", Endpoint: "http://b", Token: "b"},
		{ID: "aws-c", Account: "3", Region: "us-east-1", Endpoint: "http://c", Token: "c"},
	}, "test")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	manager := NewManager(catalog, Options{
		ClientPoolMax: 2,
		ClientIdleTTL: time.Hour,
		ClientFactory: func(Target) (proxy.AWSClient, error) { return &fakeAWSClient{}, nil },
	})
	for _, target := range catalog.Targets() {
		if _, err := manager.clientFor(target); err != nil {
			t.Fatalf("clientFor %s: %v", target.ID, err)
		}
	}
	if got := manager.pool.len(); got != 2 {
		t.Fatalf("pool size = %d, want 2", got)
	}
}

func TestFanoutWarningsReachRequestCollector(t *testing.T) {
	manager := NewManager(managerTestCatalog(t), Options{
		MaxAttempts: 1,
		ClientFactory: func(target Target) (proxy.AWSClient, error) {
			if target.ID == "aws-a" {
				return newSequenceClient(errors.New("boom")), nil
			}
			return &fakeAWSClient{}, nil
		},
	})
	ctx, collector := proxy.WithWarningCollector(context.Background())
	_, err := manager.PrometheusQueryRange(ctx, `Invocations{region="us-east-1"}`, time.Now().Add(-time.Hour), time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("PrometheusQueryRange: %v", err)
	}
	if collector.Total() == 0 || len(collector.Warnings()) == 0 {
		t.Fatalf("warnings = %#v", collector.Warnings())
	}
	if collector.Warnings()[0].Backend != "aws-a" {
		t.Fatalf("warning backend = %q", collector.Warnings()[0].Backend)
	}
}
