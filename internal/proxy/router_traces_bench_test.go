package proxy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/segment"
)

type benchTraceUpstream struct {
	name    string
	latency time.Duration
}

func (u *benchTraceUpstream) Name() string { return u.name }

func (u *benchTraceUpstream) ListServices(context.Context, string) ([]string, error) {
	if u.latency > 0 {
		time.Sleep(u.latency)
	}
	return []string{u.name + "-service"}, nil
}

func (u *benchTraceUpstream) SearchTraces(context.Context, string, TraceSearch) ([]TraceSummary, error) {
	return nil, nil
}

func (u *benchTraceUpstream) GetTrace(context.Context, string, string) ([]segment.TraceRecord, bool, error) {
	return nil, false, nil
}

// BenchmarkListServicesFanout measures trace service discovery across remote
// backends. Each fake upstream adds a fixed latency, so a serial implementation
// costs N*latency while the bounded parallel implementation costs ~latency.
func BenchmarkListServicesFanout(b *testing.B) {
	for _, backendCount := range []int{4, 8, 16} {
		b.Run(fmt.Sprintf("backends=%d", backendCount), func(b *testing.B) {
			router := NewRouter(RouterConfig{
				MaxInflight:       32,
				MaxFanoutBackends: 32,
				BackendTimeout:    time.Second,
			}, NewSliceRegistry(nil))
			for i := 0; i < backendCount; i++ {
				name := fmt.Sprintf("tempo-%02d", i)
				backend := Backend{
					Name:     name,
					Kind:     KindTempo,
					BaseURL:  "http://example.invalid",
					Signals:  []Signal{SignalTraces},
					Tenant:   "default",
					Priority: i,
				}
				upstream := &benchTraceUpstream{name: name, latency: 500 * time.Microsecond}
				if err := router.Register(backend, Upstreams{Traces: upstream}); err != nil {
					b.Fatal(err)
				}
			}

			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				services, warnings, err := router.ListServices(ctx, "default")
				if err != nil {
					b.Fatal(err)
				}
				if len(warnings) != 0 || len(services) != backendCount {
					b.Fatalf("services=%d warnings=%d", len(services), len(warnings))
				}
			}
		})
	}
}
