# Federation 50k Production Readiness — Gap Closure Plan

- **Status:** Draft for implementation
- **Date:** 2026-10-23
- **Scope:** Doctor query-plane federation for 50k pushers / 150k account-region
  targets.
- **Related:**
  - [Design](./multi-account-federation-50k-design.md)
  - [Plan](./multi-account-federation-50k.md)
  - [Implementation status](./IMPLEMENTATION_STATUS.md)

---

## 1. Verdict

The earlier finding is correct:

> **3-account live demo: yes. 50k production: no.**

The architecture is 50k-oriented, but the current code is a 3-account MVP. A
number of issues are not just “missing polish”; they will fail at 50k.

One additional blocker was found while validating the catalog:

- `NewCatalog` builds its indexes with `appendUnique`, which is O(n²) for
  high-fanout index keys. Measured:
  - 50k targets: **7.3s**
  - 150k targets: **70.4s**
  - The 50k builder CLI itself is fast (**1.3s**), but that only builds JSON.

At 150k targets, a 70s startup/reload is unacceptable. This is fixable and
should be the first PR.

---

## 2. Readiness matrix

| Area | Current | Gap to production | Priority |
| --- | --- | --- | --- |
| Catalog indexing | O(n²) index build; repeated `Targets()` sorts | Linear index build; cached sorted target list; 150k budget test | **P0** |
| Token handling | `token_env`; `token_ref` exists but is not resolved | Secret resolver, cache, 401 refresh, no per-target envs | **P0** |
| Catalog refresh | Load once at startup; static ConfigMap in demo | Versioned reload loop; atomic swap; stale status | **P0** |
| Catalog lifecycle | Builder CLI with hand-made input; no controller | Guardian + pusher metadata ingest; publish loop; leader election | **P0** |
| Fan-out runtime | Semaphores in logs/traces; metrics sequential; unbounded client map | Global/per-target limits, retries, circuit breaker, LRU pool | **P0** |
| Observability | `/v1/federation/status` aggregate; `recordLatency` is a no-op | OTel metrics, per-target health, partial headers, status caps | **P0** |
| 50k test | 50k builder test only | 50k/150k catalog + query + chaos + reload tests | **P0** |
| X-Ray E2E | Unit/merge tests; live AWS blocked | Fake-pusher E2E now; live AWS after stacks return | **P1** |
| Access policy | `PartitionForAWS` linear over `AWSBindings`; 3-account test | Index by account/region; 50k binding benchmark; selector grants | **P1** |
| Metadata APIs | Cached unions still scan all targets; status returns all partitions | Scoped/paginated endpoints; result caps | **P1** |
| Broad queries | 422 for unscoped/too-broad | Central rollups/store if fleet dashboards are required | **P1/P2** |
| “Universal” | AWS only | Provider interface if GCP/Azure is a real requirement | **P2** |

**P0 means “must not call it 50k production without this”.**

---

## 3. P0 workstreams

### 3.1 Catalog scale and memory

**Problem**

- `NewCatalog` uses `appendUnique` for every index entry:
  - `byRegion` for 150k targets in one region is ~11B comparisons.
  - `byMetric` for a metric present on 150k targets is the same.
- `Catalog.Targets()` sorts a 150k slice on every call; manager/status call it
  frequently.
- `Status()` returns the full partition map; at 150k partitions this is a huge
  JSON response.
- `MonoFSSnapshotStore.Load` reads shards sequentially; 300 shards means 300
  round trips.
- `Save` writes shards sequentially and calls `NewCatalog` for validation.

**Implementation**

- In `internal/federation/catalog.go`:
  - Build indexes with plain `append`; target values are already deduped by
    `normalizeTarget`.
  - After the loop, sort and dedupe each index list once.
  - Cache a sorted `targetList []Target` in `NewCatalog`; `Targets()` returns a
    shallow copy of that slice.
- In `internal/federation/manager.go`:
  - `Status()` returns counts, not full `partitions` / `regions` maps:
    ```json
    {
      "enabled": true,
      "targets": 150000,
      "partitions": 150000,
      "regions": 12,
      "max_fanout": 32,
      "version": "...",
      "source": "monofs:...",
      "loaded_at": "...",
      "stale": false
    }
    ```
  - Add paginated target search separately; do not serialize 150k rows.
- In `internal/federation/snapshot.go`:
  - Load shards with a worker pool (8–16), preserve order, validate SHA-256.
  - Save shards with a worker pool; manifest remains last.
  - Validate once, not once per shard.
- Add benchmarks:
  - `NewCatalog` 50k / 150k with 1 region, 2 metrics, 1 log group per target.
  - `Targets()` repeated.
  - 300-shard load/save with a fake file store.

**Acceptance**

- 150k `NewCatalog` < 5s on the dev machine (target < 2s).
- No O(n²) behavior with a single hot region/metric.
- 300-shard load < 10s with 16 workers.
- `/v1/federation/status` response < 100 KB at 150k targets.

---

### 3.2 Token resolver

**Problem**

- `Target.TokenRef` is validated but never resolved.
- `token_env` at 50k means 50k environment variables in every Doctor replica.
- `Manager` creates `awsstore.NewClient(endpoint, target.Token)`; `TokenRef`
  targets get an empty token and fail with 401.
- No rotation or 401 retry.

**Implementation**

- Add `internal/federation/token.go`:
  ```go
  type TokenResolver interface {
      Resolve(ctx context.Context, target Target) (string, error)
      Invalidate(target Target)
  }
  ```
- Implement:
  - `EnvTokenResolver` (dev; reads `target.TokenEnv`).
  - `GuardianTokenResolver` (**primary**): lazy
    `GET /api/m2m/pushers/{name}` using the internal Guardian token; per-target
    cache, singleflight, 401 invalidation.
  - `BundleTokenResolver` (**fallback**): sharded MonoFS bundles, not one
    monolithic file. Default 256 shards; one file per shard; lazy shard read.
  - `ChainTokenResolver`: env → Guardian → MonoFS bundle.
- Sharded bundle format:
  ```json
  {
    "version": "2026-10-23T...",
    "tokens": {"aws-780603797542-us-east-1": "..."}
  }
  ```
  Stored under
  `/doctor/v1/federation/tokens/<tenant>/<shard>.json`; shard =
  `fnv1a(target.ID) % shard_count`. A rotation rewrites one shard, not all 50k
  tokens. The catalog stores only `token_ref` such as
  `guardian-pusher://<pusher>` or `monofs-token://<tenant>`.
- Add to `Manager.Options`: `TokenResolver`.
- Resolve lazily on first request through `awsstore.TokenSource`; cache token
  and shard with TTL (default 5m).
- On HTTP 401:
  - Invalidate the cached token/shard.
  - Re-resolve once.
  - Retry the request once.
  - If still 401, mark the target unhealthy; do not log the token.
- Add a typed `awsstore.HTTPError{StatusCode, Body}` so retry logic does not
  parse error strings.
- Keep `token_env` for the 3-account demo only; log a warning when it is used
  with more than e.g. 1000 targets.

**Acceptance**

- 50k-target catalog with only `token_ref`; zero token env vars.
- Token rotation without restart: old token invalidated, new token used.
- A token rotation writes only one 20–60 KB shard, not a multi-MB bundle.
- 401 storm test: one refresh per target, no token leakage in logs.
- Unit tests for Guardian cache/singleflight, sharded bundle parsing, chain
  fallback, and 401 retry.

---

### 3.3 Catalog reload / hot swap

**Problem**

- `cmd/doctor-query/main.go` loads the catalog once at startup.
- A new pusher/partition/metadata requires a pod restart.
- `Manager` holds `catalog` and `planner` fields directly; no atomic swap.
- No stale-catalog signal.

**Implementation**

- Add an atomic runtime state in `Manager`:
  ```go
  type managerState struct {
      catalog  *Catalog
      planner  *Planner
      source   string
      loadedAt time.Time
  }
  type Manager struct {
      state atomic.Pointer[managerState]
      ...
  }
  ```
- Every query takes one state snapshot at the start and uses that
  catalog/planner for the whole request.
- `Manager.Reload(catalog *Catalog, source string) error` validates and swaps.
- Add `internal/federation/reload.go`:
  - `RunCatalogReload(ctx, cfg, store, manager, interval)`.
  - Load manifest; if version equals current, skip.
  - Load shards; build `Catalog`; swap.
  - On failure, keep the current catalog and increment an error metric.
  - Static file mode: poll mtime/version.
- Config:
  - `DOCTOR_FEDERATION_RELOAD_INTERVAL` (default 30s; demo 60s).
  - `DOCTOR_FEDERATION_STALE_AFTER` (default 5m).
- Add `Status()` fields: `source`, `loaded_at`, `stale`, `reload_errors`,
  `last_reload_error`.
- Add `POST /v1/federation/refresh` (admin) to trigger a reload.

**Acceptance**

- Publish a new snapshot; Doctor sees it within one reload interval without a
  restart.
- A corrupt/missing shard keeps the old catalog and reports `stale=true`.
- Reload under concurrent queries passes `go test -race`.
- No request sees a mix of old and new planners.

---

### 3.4 Catalog lifecycle controller

**Problem**

- `doctor-federation-build` only transforms a hand-written `BuildInput`.
- The demo builds a 3-target ConfigMap in `step3_onboard.sh`.
- No code fetches Guardian partitions/pushers or 50k `/v1/aws/meta` endpoints.
- No periodic rebuild/publish loop.

**Implementation**

- Add `cmd/doctor-federation-controller`:
  - Flags/env:
    - Guardian API URL + read token
    - MonoFS router + token
    - snapshot root/tenant
    - interval, metadata concurrency, metadata timeout, metadata TTL
    - `--once`, `--dry-run`
  - Loop:
    1. Fetch Guardian pusher registry (paginated; `updated_since` when
       available).
    2. Fetch normalized partition metadata (bulk endpoint; see below).
    3. Refresh `/v1/aws/meta` for healthy targets whose cache is stale or whose
       pusher version changed.
    4. Build `BuildInput`; call `BuildCatalog`.
    5. Publish only changed token shards (if the bundle fallback is used).
    6. Publish catalog snapshot with shards first, manifest last, CAS.
    7. Sleep with jitter; keep last-known-good on failure.
- Metadata fetch:
  - Worker pool (32–64), per-request timeout (5s), 2 retries for 429/5xx.
  - Global rate limit (e.g. 50–200 req/s).
  - 30–60m TTL, jittered; only a fraction of targets refreshed per cycle.
  - Keep last-known metadata on failure; record warnings.
  - Cap metric names/log groups per target; cap global index size.
- Add a Guardian bulk feed:
  - Either extend `GET /api/partitions` with pagination and
    `account`, `region`, `provider`, `labels`, `updatedAt`;
  - or add `GET /api/federation/partitions?cursor=&limit=`.
  - Derive account/region from the partition intent `spec.target`; labels from
    partition metadata. 50k individual `GET /api/partitions/{name}` calls are
    not acceptable for a rebuild.
- Controller placement:
  - One replica in the `doctor` namespace, or a sidecar with a leader lease.
  - Use a Kubernetes Lease or a MonoFS CAS lock to avoid duplicate publishers.
- Token source:
  - Guardian `/api/pushers` already returns the token for each pusher.
  - The controller writes the token bundle; it never puts tokens in the
    catalog snapshot.

**Acceptance**

- 50k fake Guardian partitions + 50k fake pushers: one rebuild completes within
  the configured interval.
- 30% metadata fetch failures: build succeeds with warnings; last-known
  metadata is retained.
- Controller restart resumes from the last snapshot; no full metadata refetch
  unless TTL expired.
- Two controllers: only one publishes; the loser backs off.

---

### 3.5 Fan-out runtime: limits, retries, circuit breaker, client pool

**Problem**

- Metrics fan out sequentially; 32 targets × timeout can exceed any useful
  request deadline.
- Logs/traces use a hardcoded semaphore of 8.
- No retry for 429/5xx/timeouts.
- No circuit breaker; a dead target is retried on every query.
- `clients map[string]proxy.AWSClient` grows without bound.
- `AWSLogsForLoki` and `AWSServices` swallow errors, so partial failures are
  invisible.

**Implementation**

- Add a shared fan-out helper:
  - global `MaxInflight` (default 64)
  - `MaxConcurrentTargets` (default 8–16)
  - per-target timeout (existing `FederationTimeout`)
  - context cancellation when the caller disconnects
- Retries:
  - 2 attempts for 429/5xx/timeout; jittered backoff.
  - Do not retry 4xx other than 429.
- Circuit breaker per target:
  - 5 consecutive failures → open for 30s.
  - One half-open probe; success closes, failure re-opens.
  - 401 is a token-refresh path, not a circuit failure.
- Client pool:
  - Lazy create; LRU max (e.g. 10k).
  - Idle eviction after 15m.
  - Shared, tuned `http.Transport` (`MaxIdleConns`, `MaxIdleConnsPerHost`,
    `IdleConnTimeout`).
- Warning propagation:
  - Add `proxy.WithWarningCollector(ctx)` / `WarningsFromContext(ctx)`.
  - Manager records per-target failures; query handlers set
    `X-Doctor-Partial` and per-backend warnings.
  - Change `AWSLogsForLoki` and `AWSServices` to return errors/warnings instead
    of silently returning nil.
- `Status()` exposes open circuits and recent errors; target-level detail is
  paginated.

**Acceptance**

- 32 targets, 30% failing/slow: response returns partial data within the
  configured timeout; `X-Doctor-Partial` is set.
- No more than `MaxConcurrentTargets` calls per query.
- A dead target opens after 5 failures and stops consuming fan-out slots.
- `go test -race` on a 50k catalog + 100 concurrent queries.
- No unbounded client growth after 10k distinct targets.

---

### 3.6 Observability

**Problem**

- `recordLatency` is a no-op.
- No per-target health/metrics.
- No catalog reload metrics.
- `/v1/federation/status` is aggregate and can be huge.

**Implementation**

- Add OTel metrics in `internal/federation/metrics.go`:
  - Catalog: targets, shards, load seconds, reload total/errors, stale age.
  - Planner: plans by signal/mode, matched/selected, plan duration.
  - Fan-out: per-target requests, errors, latency, circuit state, partials.
  - Token: resolve total/errors, cache size, refreshes.
  - Controller: build duration, metadata fetch success/failure, publish total.
- Add response headers:
  - `X-Doctor-Federation-Version`
  - `X-Doctor-Fanout-Matched`
  - `X-Doctor-Fanout-Selected`
  - `X-Doctor-Partial`
  - `X-Doctor-Partial-Count`
- Status endpoint:
  - Aggregate counts by default.
  - `GET /v1/federation/targets?account=&region=&partition=&q=&cursor=&limit=`
    for detail.
  - `GET /v1/federation/plan?query=&signal=` for admin debugging.
- Add a Grafana panel/alert set for the above.

**Acceptance**

- Grafana can show catalog version, target count, reload age, fan-out p95,
  failures, partial responses, and open circuits.
- No status response contains 150k rows.
- A partial failure is visible in the response header and UI banner.

---

### 3.7 50k load and chaos harness

**Problem**

- Existing `internal/loadtest` covers proxy endpoints and rules, not federation.
- The 50k builder test does not exercise index construction, token resolution,
  fan-out, reload, or failure handling.

**Implementation**

- Add `internal/loadtest/federation_test.go` or
  `cmd/doctor-federation-sim`:
  - Generate 50k and 150k targets; optional 5M metric names.
  - Build catalog; measure time and heap; assert budget.
  - Planner benchmarks: account, region, partition, log group, service, metric,
    regex, unscoped.
  - Fake HTTP pusher with configurable latency/error/429 behavior; run 100–1000
    concurrent queries.
  - Reload loop while queries run; run with `-race`.
  - Snapshot shard load/save with 100/300 shards.
  - Token resolver with 50k bundle; 401 rotation.
  - Chaos: 30% failures, 10% slow, 5% 429; assert cap, partial, no starvation.
- Set explicit budgets in the test and docs:
  - 150k catalog build <5s.
  - Planner scoped p99 <5ms.
  - No query >32 targets.
  - 300-shard load <10s.
  - Per-replica heap budget (measure first; target <2GB for 150k with capped
    metadata).
  - No goroutine/connection growth after 10k queries.

**Acceptance**

- The suite runs in CI with `DOCTOR_LOADTEST=1` (or a separate nightly job).
- Results are recorded in the repo; regressions fail the build.

---

## 4. P1 / P2 work

### 4.1 X-Ray end-to-end

- Add a fake pusher HTTP fixture implementing:
  - `/v1/aws/xray/services`
  - `/v1/aws/xray/traces`
  - `/v1/aws/xray/traces/{id}`
- Test service/time query, trace-by-ID, merge/dedupe, account/region stamping,
  and partial failures.
- Recreate the demo AWS stacks and run the live X-Ray path before calling
  production-ready.

### 4.2 Access policy at 50k

- `Policy.PartitionForAWS` currently loops over all `AWSBindings` for every
  record. Add indexes:
  - exact `account+region`
  - account-only
  - region-only
- Benchmark 50k bindings × 1M record lookups.
- Replace explicit 50k `aws_bindings` with catalog-backed/selector grants where
  possible.
- Test policy bundle size/compile time with 50k partitions.
- Verify `usera`/`userb` isolation at 50k.

### 4.3 Metadata APIs and status

- Scope/paginate `ListMetrics`, `ListServices`, `ListLogGroups` by tenant,
  account, region, partition, query, and limit.
- Do not return a 5M-entry union to Grafana variables.
- Add search endpoints for log groups, metrics, services.
- Keep `/v1/federation/status` aggregate-only.

### 4.4 Central store for broad queries

- If fleet-wide dashboards are required, implement the central path:
  - MonoFS/ClickHouse/Mimir for metrics/logs/traces.
  - Rollups with 1–5m lag.
  - Planner `PlanCentral`.
- Until then, keep explicit 422 for unscoped/too-broad live AWS queries and
  document the limitation.

### 4.5 Provider abstraction

- If “universal” means GCP/Azure, add:
  - `ProviderClient` interface.
  - Provider registry and capability routing.
  - Catalog `Provider` field usage.
- If the product is AWS-only, remove “universal” from the UI/docs to avoid
  overpromising.

---

## 5. Cross-repo changes

| Repo | Change |
| --- | --- |
| `doctor` | Catalog scale, token resolver, reload, controller, fan-out runtime, metrics, load tests |
| `guardian` | Paginated partition feed with account/region/labels; optional token endpoint; keep `/api/pushers` pagination |
| `monofs` | Index `AWSBindings` in policy; selector grants; 50k policy benchmark |
| `stratatools` | `doctor-federation-controller` deployment; query env (`SNAPSHOT_STORE=monofs`, reload interval); remove demo ConfigMap wiring when controller is live |

---

## 6. Recommended PR sequence

1. **PR1 — Catalog scale** (Doctor)
   - O(n) indexes, cached target list, status caps, parallel shard IO,
     50k/150k benchmarks.
   - Low risk; no behavior change.
2. **PR2 — Token resolver** (Doctor)
   - Interface, env/bundle resolvers, manager integration, 401 refresh.
3. **PR3 — Hot reload** (Doctor)
   - Atomic manager state, reload loop, status, tests.
4. **PR4 — Controller** (Doctor + Guardian feed)
   - Guardian/pusher ingestion, metadata cache, token bundle, publish loop,
     leader election.
5. **PR5 — Fan-out runtime** (Doctor)
   - Limits, retries, circuit breaker, client LRU, warning propagation.
6. **PR6 — Observability** (Doctor)
   - OTel metrics, headers, status/target search.
7. **PR7 — 50k load/chaos** (Doctor)
   - Simulator, budgets, CI/nightly.
8. **PR8 — X-Ray + access policy** (Doctor + MonoFS + live AWS)
   - Fake E2E, policy index, 50k benchmark, live X-Ray.

PR1–PR3 are the minimum to make the current 3-account path operationally sane.
PR4–PR7 are required before a 50k production claim.

---

## 7. Definition of done (50k scoped federation)

- 50k pushers / 150k account-region targets load in <5s per replica.
- A new catalog version is visible within one reload interval without restart.
- No query fans out to more than 32 targets; unscoped queries return 422.
- Tokens are resolved from a secret store; no 50k env vars; rotation works.
- A catalog controller rebuilds and publishes snapshots continuously; last
  known good survives partial failures.
- 30% target failure/slowdown yields partial results and a visible warning, not
  a hung query.
- Per-target health, circuit state, latency, errors, catalog age, and partials
  are observable.
- 50k/150k load and chaos tests pass with recorded budgets.
- X-Ray live E2E passes (or the limitation is explicitly documented).
- Access policy works with 50k partitions and O(1) account binding lookup.
- Broad fleet dashboards either use a central store or return an explicit 422;
  the product behavior is documented.

---

## 8. Open decisions

1. **Token storage:** Guardian pusher API vs sharded MonoFS bundle vs
   Vault/Kubernetes Secret. Recommendation: Guardian lazy resolver as primary,
   sharded bundle as fallback/cache, Vault behind the same interface later;
   never a monolithic bundle or `token_env` at 50k.
2. **Controller placement:** separate single-replica Deployment vs sidecar with
   leader lease. Recommendation: separate Deployment with a Lease.
3. **Metadata TTL:** 30m vs 60m; start at 60m with jitter and on-demand refresh.
4. **Guardian feed:** extend `/api/partitions` vs new
   `/api/federation/partitions`. Recommendation: new paginated endpoint to
   avoid breaking the UI.
5. **Central store:** required for fleet dashboards, not for scoped federation.
   Decide before promising broad queries.
6. **Provider scope:** AWS-only vs true multi-cloud. Decide before investing in
   a provider abstraction.

---

## 9. Bottom line

The current implementation is a good MVP and the architecture is sound. To make
it production-ready at 50k, the minimum work is:

- fix catalog indexing and startup,
- make `token_ref` real,
- add catalog hot reload and a controller,
- add fan-out limits, retries, circuit breaking and observability,
- prove it with 50k/150k load and chaos tests.

That is a **P0 of roughly 4–6 weeks for 2 engineers**, followed by X-Ray,
access-policy scale, metadata pagination, and central rollups as P1.
