package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/federation"
	"github.com/radryc/doctor/internal/proxy"
	"github.com/radryc/doctor/internal/ruler"
)

func loadtestEnabled(t *testing.T) bool {
	t.Helper()
	if os.Getenv("DOCTOR_LOADTEST") != "1" {
		t.Skip("set DOCTOR_LOADTEST=1 to run federation load/chaos tests")
		return false
	}
	return true
}

func federationTargets(count int) []federation.Target {
	targets := make([]federation.Target, 0, count)
	for i := 0; i < count; i++ {
		account := fmt.Sprintf("%012d", i)
		partition := fmt.Sprintf("part-%06d", i)
		targets = append(targets, federation.Target{
			ID:           "aws-" + account + "-us-east-1",
			Account:      account,
			Region:       "us-east-1",
			Partitions:   []string{partition},
			Endpoint:     fmt.Sprintf("http://pusher-%d:19090", i),
			Token:        "loadtest-token",
			MetricNames:  []string{"Invocations", fmt.Sprintf("Custom%06d", i)},
			LogGroups:    []string{fmt.Sprintf("guardian/%s/us-east-1/%s/app", account, partition)},
			XrayServices: []string{"shared-service"},
		})
	}
	return targets
}

func TestFederationPlanner150kBudget(t *testing.T) {
	if !loadtestEnabled(t) {
		return
	}
	const count = 150000
	start := time.Now()
	catalog, err := federation.NewCatalog(federationTargets(count), "loadtest-150k")
	buildElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if buildElapsed > 5*time.Second {
		t.Fatalf("NewCatalog(150k) = %s, budget is 5s", buildElapsed)
	}
	planner := federation.NewPlanner(catalog, 32)

	scoped := []federation.Query{
		{Signal: federation.SignalMetrics, Account: "000000000042"},
		{Signal: federation.SignalMetrics, Partition: "part-000042"},
		{Signal: federation.SignalMetrics, Expr: `Invocations{account="000000000042"}`},
		{Signal: federation.SignalLogs, LogGroups: []string{"guardian/000000000042/us-east-1/part-000042/app"}},
	}
	for _, query := range scoped {
		plan, planErr := planner.Plan(context.Background(), query)
		if planErr != nil {
			t.Fatalf("scoped Plan(%#v): %v", query, planErr)
		}
		if plan.Mode != federation.PlanPull || len(plan.Targets) == 0 || len(plan.Targets) > 32 {
			t.Fatalf("scoped Plan(%#v) = mode=%s targets=%d", query, plan.Mode, len(plan.Targets))
		}
	}

	rejected := []federation.Query{
		{Signal: federation.SignalMetrics, Expr: `Invocations`},
		{Signal: federation.SignalMetrics, Region: "us-east-1"},
		{Signal: federation.SignalMetrics, Expr: `Invocations{region="us-east-1"}`},
	}
	for _, query := range rejected {
		plan, planErr := planner.Plan(context.Background(), query)
		if planErr != nil {
			t.Fatalf("rejected Plan(%#v): %v", query, planErr)
		}
		if plan.Mode != federation.PlanReject {
			t.Fatalf("Plan(%#v) = mode=%s, want reject", query, plan.Mode)
		}
	}

	const iterations = 1000
	durations := make([]time.Duration, 0, iterations)
	for i := 0; i < iterations; i++ {
		query := federation.Query{
			Signal:    federation.SignalMetrics,
			Account:   fmt.Sprintf("%012d", i+1000),
			Partition: fmt.Sprintf("part-%06d", i+1000),
		}
		start := time.Now()
		plan, planErr := planner.Plan(context.Background(), query)
		durations = append(durations, time.Since(start))
		if planErr != nil || plan.Mode != federation.PlanPull || len(plan.Targets) > 32 {
			t.Fatalf("hot-loop Plan = %#v, %v", plan, planErr)
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p99 := durations[int(float64(len(durations))*0.99)]
	if p99 > 5*time.Millisecond {
		t.Fatalf("planner p99 = %s, budget is 5ms", p99)
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	t.Logf("150k catalog build=%s planner_p99=%s heap=%dMB targets=%d",
		buildElapsed.Round(time.Millisecond), p99.Round(time.Microsecond), stats.HeapAlloc>>20, catalog.Len())
	if stats.HeapAlloc > 2<<30 {
		t.Fatalf("heap = %d MB, budget is 2GB", stats.HeapAlloc>>20)
	}
}

type chaosMode int

const (
	chaosSuccess chaosMode = iota
	chaosError
	chaosSlow
	chaos429
)

func TestFederationFanoutChaos(t *testing.T) {
	if !loadtestEnabled(t) {
		return
	}
	const targetCount = 32
	const queryCount = 100
	modes := make([]chaosMode, targetCount)
	for i := range modes {
		switch {
		case i%10 < 3:
			modes[i] = chaosError
		case i%10 == 3:
			modes[i] = chaosSlow
		case i%20 == 4:
			modes[i] = chaos429
		default:
			modes[i] = chaosSuccess
		}
	}

	var active, maxActive atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := strings.TrimPrefix(r.URL.Path, "/t/")
		indexPart := strings.SplitN(prefix, "/", 2)[0]
		index, _ := strconv.Atoi(indexPart)
		mode := chaosError
		if index >= 0 && index < len(modes) {
			mode = modes[index]
		}
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maxActive.Load()
			if current <= previous || maxActive.CompareAndSwap(previous, current) {
				break
			}
		}
		switch mode {
		case chaosError:
			http.Error(w, "chaos failure", http.StatusInternalServerError)
		case chaos429:
			http.Error(w, "chaos throttle", http.StatusTooManyRequests)
		case chaosSlow:
			select {
			case <-time.After(250 * time.Millisecond):
				writeChaosMetrics(w, index)
			case <-r.Context().Done():
			}
		default:
			writeChaosMetrics(w, index)
		}
	}))
	defer server.Close()

	targets := make([]federation.Target, 0, targetCount)
	for i := 0; i < targetCount; i++ {
		account := fmt.Sprintf("%012d", i)
		targets = append(targets, federation.Target{
			ID:           "aws-" + account + "-us-east-1",
			Account:      account,
			Region:       "us-east-1",
			Partitions:   []string{fmt.Sprintf("chaos-%02d", i)},
			Endpoint:     fmt.Sprintf("%s/t/%d", server.URL, i),
			Token:        "loadtest-token",
			MetricNames:  []string{"Invocations"},
			Capabilities: []federation.Signal{federation.SignalMetrics},
		})
	}
	catalog, err := federation.NewCatalog(targets, "chaos")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	manager := federation.NewManager(catalog, federation.Options{
		MaxFanout:            32,
		MaxInflight:          64,
		MaxConcurrentTargets: 8,
		MaxAttempts:          2,
		Timeout:              100 * time.Millisecond,
		ClientPoolMax:        100,
		BreakerThreshold:     1000,
		Logger:               log.New(io.Discard, "", 0),
	})
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, queryCount)
	for i := 0; i < queryCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx, collector := proxy.WithWarningCollector(ctx)
			data, err := manager.PrometheusQuery(ctx, `Invocations{region="us-east-1"}`, time.Now())
			if err != nil {
				errs <- err
				return
			}
			if got := len(proxy.PrometheusResultSlice(proxy.ExtractPrometheusData(data))); got < 10 {
				errs <- fmt.Errorf("successful series = %d, want at least 10", got)
				return
			}
			if collector.Total() == 0 {
				errs <- fmt.Errorf("expected partial warnings")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("chaos query: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 15*time.Second {
		t.Fatalf("chaos queries took %s, budget is 15s", elapsed)
	}
	if got := maxActive.Load(); got > 64 {
		t.Fatalf("max concurrent requests = %d, global limit is 64", got)
	}
	t.Logf("fanout chaos: %d queries x %d targets in %s; max active=%d", queryCount, targetCount, elapsed.Round(time.Millisecond), maxActive.Load())
}

func writeChaosMetrics(w http.ResponseWriter, index int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{map[string]any{
				"metric": map[string]any{
					"__name__":     "Invocations",
					"@aws.account": fmt.Sprintf("%012d", index),
				},
				"value": []any{float64(time.Now().Unix()), "1"},
			}},
		},
	})
}

func TestFederationReloadUnderLoad(t *testing.T) {
	if !loadtestEnabled(t) {
		return
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeChaosMetrics(w, 0)
	}))
	defer server.Close()

	const count = 200
	targets := federationTargets(count)
	for i := range targets {
		targets[i].Endpoint = server.URL
	}
	catalogV1, err := federation.NewCatalog(targets, "reload-v1")
	if err != nil {
		t.Fatalf("NewCatalog(v1): %v", err)
	}
	manager := federation.NewManager(catalogV1, federation.Options{
		MaxFanout:            32,
		MaxInflight:          64,
		MaxConcurrentTargets: 8,
		MaxAttempts:          1,
		Timeout:              time.Second,
		ClientPoolMax:        20,
		Logger:               log.New(io.Discard, "", 0),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for worker := 0; worker < 20; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				account := fmt.Sprintf("%012d", (worker*7+i)%count)
				if _, err := manager.PrometheusQuery(ctx, fmt.Sprintf(`Invocations{account="%s"}`, account), time.Now()); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		for i := 0; i < 100; i++ {
			version := fmt.Sprintf("reload-v%d", i+2)
			catalog, catalogErr := federation.NewCatalog(targets, version)
			if catalogErr != nil {
				errs <- catalogErr
				return
			}
			if reloadErr := manager.Reload(catalog, "loadtest:"+version); reloadErr != nil {
				errs <- reloadErr
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wg.Wait()
	<-reloadDone
	close(errs)
	for err := range errs {
		t.Fatalf("reload/load: %v", err)
	}
}

func TestFederationTokenBundle50k(t *testing.T) {
	if !loadtestEnabled(t) {
		return
	}
	const count = 50000
	files := ruler.NewMemoryStore()
	writer := federation.NewTokenBundleWriter(files, "", "/doctor/v1/federation/tokens", "default", 256)
	tokens := make(map[string]string, count)
	targets := make([]federation.Target, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("aws-%012d-us-east-1", i)
		tokens[id] = fmt.Sprintf("token-%06d", i)
		targets = append(targets, federation.Target{ID: id, TokenRef: "guardian-pusher://" + id})
	}
	start := time.Now()
	changed, err := writer.Write(context.Background(), tokens)
	if err != nil {
		t.Fatalf("TokenBundleWriter.Write: %v", err)
	}
	writeElapsed := time.Since(start)
	resolver := federation.NewBundleTokenResolver(federation.BundleTokenResolverOptions{
		Files:      files,
		Root:       "/doctor/v1/federation/tokens",
		Tenant:     "default",
		ShardCount: 256,
	})
	random := rand.New(rand.NewSource(42))
	for i := 0; i < 1000; i++ {
		target := targets[random.Intn(len(targets))]
		token, resolveErr := resolver.Resolve(context.Background(), target)
		if resolveErr != nil || token != tokens[target.ID] {
			t.Fatalf("Resolve(%s) = %q, %v", target.ID, token, resolveErr)
		}
	}
	t.Logf("token bundle: 50k tokens / 256 shards written in %s (changed=%d)", writeElapsed.Round(time.Millisecond), changed)
}
