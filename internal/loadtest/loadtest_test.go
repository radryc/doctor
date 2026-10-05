package loadtest

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/proxy"
	"github.com/rydzu/ainfra/doctor/internal/ruler"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// TestProxyEndpointsLoad exercises the fan-out router with thousands of
// registered endpoints. Enable with DOCTOR_LOADTEST=1.
func TestProxyEndpointsLoad(t *testing.T) {
	if os.Getenv("DOCTOR_LOADTEST") != "1" {
		t.Skip("set DOCTOR_LOADTEST=1 to run the load test")
	}
	const (
		endpointCount = 2000
		concurrency   = 100
	)
	router := proxy.NewRouter(proxy.RouterConfig{
		MaxInflight:    64,
		BackendTimeout: 5 * time.Second,
	}, proxy.NewCompositeRegistry())
	for i := 0; i < endpointCount; i++ {
		name := fmt.Sprintf("prom-%04d", i)
		backend := proxy.Backend{
			Name:    name,
			Kind:    proxy.KindPrometheus,
			BaseURL: "http://" + name,
			Signals: []proxy.Signal{proxy.SignalMetrics},
			Policy:  proxy.PolicyAlways,
		}
		if err := router.Register(backend, proxy.Upstreams{Metrics: &fakeMetrics{}}); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := router.MetricsInstant(context.Background(), "default", "up", time.Now()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("query failed: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("proxy load: %d endpoints x %d concurrent queries in %s (%.2f ms/query)",
		endpointCount, concurrency, elapsed, float64(elapsed.Milliseconds())/float64(concurrency))
}

// TestRulesLoad exercises rule storage and evaluation at tens of thousands of
// rules. Enable with DOCTOR_LOADTEST=1.
func TestRulesLoad(t *testing.T) {
	if os.Getenv("DOCTOR_LOADTEST") != "1" {
		t.Skip("set DOCTOR_LOADTEST=1 to run the load test")
	}
	const (
		groupCount = 400
		rulesPer   = 50
	)
	memory := ruler.NewMemoryStore()
	ruleStore := ruler.NewStore(memory, "token", "")
	ctx := context.Background()
	for g := 0; g < groupCount; g++ {
		rules := ""
		for r := 0; r < rulesPer; r++ {
			rules += fmt.Sprintf("      - record: doctor:load_%d_%d\n        expr: sum(rate(http_requests_total[5m]))\n", g, r)
		}
		doc := fmt.Sprintf("version: 1\ngroups:\n  - name: group-%d\n    tenant: default\n    interval: 30s\n    rules:\n%s", g, rules)
		file, err := ruler.ParseFile([]byte(doc))
		if err != nil {
			t.Fatalf("ParseFile: %v", err)
		}
		if err := ruleStore.Put(ctx, "default", fmt.Sprintf("group-%d", g), file, true); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	start := time.Now()
	refs, err := ruleStore.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	loadElapsed := time.Since(start)

	writer := &fakeWriter{}
	evaluator := &ruler.Evaluator{
		Querier: &fakeQuerier{},
		Writer:  writer,
	}
	start = time.Now()
	total := 0
	for _, ref := range refs {
		if ref.File == nil || len(ref.File.Groups) == 0 {
			continue
		}
		results := evaluator.EvaluateGroup(ctx, ref.File.Groups[0])
		total += len(results)
	}
	evalElapsed := time.Since(start)
	t.Logf("rules load: %d groups / %d rules listed in %s; evaluated in %s (%.1f rules/s, %d records)",
		len(refs), total, loadElapsed, evalElapsed, float64(total)/evalElapsed.Seconds(), len(writer.records))
}

type fakeMetrics struct{}

func (f *fakeMetrics) Name() string { return "fake" }

func (f *fakeMetrics) PromInstant(context.Context, string, string, time.Time) (map[string]any, error) {
	return map[string]any{
		"resultType": "vector",
		"result": []any{map[string]any{
			"metric": map[string]any{"__name__": "up", "job": "api"},
			"value":  []any{float64(time.Now().Unix()), "1"},
		}},
	}, nil
}

func (f *fakeMetrics) PromRange(context.Context, string, string, time.Time, time.Time, time.Duration) (map[string]any, error) {
	return f.PromInstant(context.Background(), "", "", time.Now())
}

func (f *fakeMetrics) PromSeries(context.Context, string, []string, time.Time, time.Time) ([]map[string]string, error) {
	return nil, nil
}

func (f *fakeMetrics) PromLabels(context.Context, string, []string, time.Time, time.Time) ([]string, error) {
	return nil, nil
}

func (f *fakeMetrics) PromLabelValues(context.Context, string, string, []string, time.Time, time.Time) ([]string, error) {
	return nil, nil
}

type fakeQuerier struct{}

func (f *fakeQuerier) PromInstant(context.Context, string, time.Time) (map[string]any, bool, error) {
	return map[string]any{
		"resultType": "vector",
		"result": []any{map[string]any{
			"metric": map[string]any{"job": "api"},
			"value":  []any{float64(time.Now().Unix()), "1"},
		}},
	}, false, nil
}

type fakeWriter struct {
	records []segment.MetricPointRecord
}

func (f *fakeWriter) WriteMetrics(_ context.Context, records []segment.MetricPointRecord) error {
	f.records = append(f.records, records...)
	return nil
}
