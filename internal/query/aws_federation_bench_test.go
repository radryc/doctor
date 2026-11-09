package query

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/proxy"
	awsstore "github.com/radryc/doctor/internal/store/aws"
)

// benchAWSClient implements only the discovery methods used by these
// benchmarks; the embedded interface satisfies the rest of proxy.AWSClient.
type benchAWSClient struct {
	proxy.AWSClient
	services atomic.Int64
	metrics  atomic.Int64
	latency  time.Duration
}

func (c *benchAWSClient) Enabled() bool { return true }

func (c *benchAWSClient) ListServices(context.Context) ([]awsstore.ServiceInfo, error) {
	c.services.Add(1)
	if c.latency > 0 {
		time.Sleep(c.latency)
	}
	return []awsstore.ServiceInfo{{Name: "api", Region: "us-east-1", Account: "111111111111"}}, nil
}

func (c *benchAWSClient) ListMetrics(context.Context, string, string) ([]awsstore.MetricDescriptor, error) {
	c.metrics.Add(1)
	if c.latency > 0 {
		time.Sleep(c.latency)
	}
	return []awsstore.MetricDescriptor{{MetricName: "Invocations"}}, nil
}

func benchAWSClientWithService(tb testing.TB) (*Service, *benchAWSClient) {
	tb.Setenv("DOCTOR_QUERY_CACHE_TTL", "15s")
	client := &benchAWSClient{latency: 200 * time.Microsecond}
	return &Service{aws: client}, client
}

func BenchmarkAWSServiceInfos(b *testing.B) {
	svc, client := benchAWSClientWithService(b)
	ctx := context.Background()
	// Warm the cache once so the benchmark measures repeated discovery reads.
	_ = svc.awsServiceInfos(ctx)
	client.services.Store(0)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := svc.awsServiceInfos(ctx); len(got) != 1 {
			b.Fatalf("awsServiceInfos() returned %d services", len(got))
		}
	}
	b.ReportMetric(float64(client.services.Load())/float64(b.N), "backend-calls/op")
}

func BenchmarkAWSMetricNames(b *testing.B) {
	svc, client := benchAWSClientWithService(b)
	ctx := context.Background()
	_ = svc.awsMetricNames(ctx)
	client.metrics.Store(0)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := svc.awsMetricNames(ctx); len(got) != 1 {
			b.Fatalf("awsMetricNames() returned %d names", len(got))
		}
	}
	b.ReportMetric(float64(client.metrics.Load())/float64(b.N), "backend-calls/op")
}
