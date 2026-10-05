package ruler

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type countingEvaluator struct {
	calls atomic.Int64
	done  chan struct{}
}

func (c *countingEvaluator) EvaluateGroup(context.Context, Group) []EvalResult {
	if c.calls.Add(1) == 1 && c.done != nil {
		close(c.done)
	}
	return nil
}

func TestSchedulerEvaluatesDueGroups(t *testing.T) {
	file, err := ParseFile([]byte(validGroupYAML))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	evaluator := &countingEvaluator{done: make(chan struct{})}
	scheduler := &Scheduler{
		Evaluator: evaluator,
		Load: func(context.Context) ([]GroupRef, error) {
			return []GroupRef{{Tenant: "default", Group: "shard-health", File: file}}, nil
		},
		Workers:      2,
		PollInterval: time.Minute,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)

	select {
	case <-evaluator.done:
	case <-time.After(3 * time.Second):
		t.Fatalf("scheduler did not evaluate the group; stats=%#v", scheduler.Stats())
	}
	if stats := scheduler.Stats(); stats.Groups != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestSchedulerReloadRemovesGroups(t *testing.T) {
	file, _ := ParseFile([]byte(validGroupYAML))
	var include atomic.Bool
	include.Store(true)
	scheduler := &Scheduler{
		Evaluator: &countingEvaluator{},
		Load: func(context.Context) ([]GroupRef, error) {
			if !include.Load() {
				return nil, nil
			}
			return []GroupRef{{Tenant: "default", Group: "g", File: file}}, nil
		},
		Workers:      1,
		PollInterval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go scheduler.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if scheduler.Stats().Groups == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	include.Store(false)
	for time.Now().Before(deadline) {
		if scheduler.Stats().Groups == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("group was not removed; stats=%#v", scheduler.Stats())
}

func BenchmarkRuleStoreLoad10k(b *testing.B) {
	memory := NewMemoryStore()
	ruleStore := NewStore(memory, "token", "")
	ctx := context.Background()

	// 200 groups x 50 rules = 10k rules.
	for g := 0; g < 200; g++ {
		rules := make([]string, 0, 50)
		for r := 0; r < 50; r++ {
			rules = append(rules, fmt.Sprintf("      - record: doctor:metric_%d_%d\n        expr: sum(rate(http_requests_total[5m]))\n", g, r))
		}
		doc := "version: 1\ngroups:\n  - name: group-%d\n    tenant: default\n    interval: 30s\n    rules:\n%s"
		file, err := ParseFile([]byte(fmt.Sprintf(doc, g, joinLines(rules))))
		if err != nil {
			b.Fatalf("ParseFile: %v", err)
		}
		if err := ruleStore.Put(ctx, "default", fmt.Sprintf("group-%d", g), file, true); err != nil {
			b.Fatalf("Put: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		refs, err := ruleStore.List(ctx)
		if err != nil || len(refs) != 200 {
			b.Fatalf("List: %v refs=%d", err, len(refs))
		}
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, line := range lines {
		out += line
	}
	return out
}

func BenchmarkEvaluatorMerge10k(b *testing.B) {
	const sampleCount = 10_000
	samples := make([]map[string]any, 0, sampleCount)
	for i := 0; i < sampleCount; i++ {
		shard := fmt.Sprintf("shard-%d", i%16)
		samples = append(samples, sample(map[string]string{"job": "api", "shard": shard, "source": "prometheus"}, "1"))
	}
	group := testGroup(Rule{Record: "doctor:up:sum", Expr: "sum(up) by (job)"})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writer := &fakeWriter{}
		evaluator := &Evaluator{Querier: &fakeQuerier{data: vector(samples...)}, Writer: writer}
		results := evaluator.EvaluateGroup(context.Background(), group)
		if results[0].Samples != 1 {
			b.Fatalf("samples = %d, want 1", results[0].Samples)
		}
	}
}
