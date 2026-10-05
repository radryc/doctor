# Unified proxy and doctor-rules — load test report

Date: 2026-10-05
Hardware: Intel Core i9-13900KF (32 threads), Linux
Commit: P5 (`19c9396`) + P6 load test harness
Command:

```bash
DOCTOR_LOADTEST=1 go test ./internal/loadtest/... -v -count=1
```

## Proxy fan-out: 2,000 endpoints

- 2,000 registered backends, each an in-memory fake Prometheus upstream.
- 100 concurrent `MetricsInstant("up")` queries through `proxy.Router`
  (`MaxInflight=64`, 5s backend timeout).
- Result: **1.156 s total, 11.55 ms/query**.
- Per query the router fans out to 2,000 upstreams, tags `source`, and runs
  cross-backend `shard` collision detection over the merged series.
- Caveat: fake upstreams return instantly; real deployments are dominated by
  network latency and are bounded by `DOCTOR_PROXY_MAX_INFLIGHT`, not by merge
  cost. The number above measures the router's own overhead plus the fake
  backends' call path.

## Recording rules: 20,000 rules

- 400 groups × 50 rules in `ruler.MemoryStore` (same CAS store logic as MonoFS).
- `Store.List` of all groups: **73.8 ms** for 400 files / 20k rules.
- Full evaluation of all 20k rules with an in-memory querier and writer:
  **32.5 ms** (≈ 615k rules/s, 20k derived records).
- Caveat: the fake querier returns instantly. In production each rule issues
  one instant PromQL query to `doctor-query`; throughput is network-bound and
  controlled by `DOCTOR_RULES_WORKERS` and `DOCTOR_RULES_EVAL_TIMEOUT`. The
  numbers demonstrate that scheduling, merge, label handling, and write-back
  preparation are not the bottleneck.

## Component benchmarks

| Benchmark | Result |
| --- | --- |
| `BenchmarkMergePrometheusSeriesOverlap` (16 shards × 10k series) | ≈ 104 ms |
| `BenchmarkRouterFanout256Backends` | ≈ 0.62 ms |
| `BenchmarkRuleStoreLoad10k` (200 groups / 10k rules) | ≈ 39 ms |
| `BenchmarkEvaluatorMerge10k` (10k samples) | ≈ 3.2 ms |
| `BenchmarkOwnersResolve10kEndpoints` (all 10k warm) | ≈ 1.9 ms |
| `BenchmarkMergeLogsNoOverlap` (10k records) | ≈ 10 ms |

## Observations

- The `shard` collision pass is the dominant cost of metric merges with full
  overlap; without overlap it is skipped entirely.
- Rule scheduling and evaluation overhead scales with groups, not rules; 400
  groups is far below the 1 s scheduler tick budget.
- OWNERS decisions are ~190 ns/endpoint warm; the first decision per path pays
  a MonoFS read.
- Follow-ups if higher scale is required: hash-based series identity instead of
  string keys in the merge pass, and a rule-level worker queue rather than
  group-sequential evaluation.
