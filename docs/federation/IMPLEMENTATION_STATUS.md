# Multi-Account Federation — Implementation Status

Date: 2026-10-22  
Design: [`multi-account-federation-50k-design.md`](./multi-account-federation-50k-design.md)  
Plan: [`multi-account-federation-50k.md`](./multi-account-federation-50k.md)

## Session handoff — 2026-10-22

Status saved for branch testing tomorrow.

### Doctor branches (stacked)

```text
efd341c feat/federation-catalog-builder   build catalog from partition metadata
48c5fcc feat/federation-monofs-catalog    persist catalog in MonoFS
bffa647 feat/multi-account-federation     multi-account AWS federation
58d5c3d feat/aws-observability-fixes      AWS dashboards + service filters
123643b access-policy                     unchanged base
```

### Guardian / stratatools

- `guardian` branch `aws-dashboard-panels` — `d638181`
- `stratatools` branch `aws-dashboard-panels` — `20766ed`
- `guardian/bin/guardianctl` and `stratatools` release-version bumps remain unstaged local artifacts.

### Live kind cluster

- Running image: multi-account federation branch.
- 3 targets: `div-a-orders`, `div-a-billing`, `div-b-inventory`.
- Metrics/logs verified for all three accounts.
- `usera`/`userb` isolation verified.
- Broad-query 422 verified.
- Grafana multi-account metric queries verified.
- X-Ray live data not verified because the demo AWS stacks had been deleted.

### Next stage — OTel relay

**Clarification:** the relay is for **OTLP telemetry in general**, not only
distributed tracing. The same pusher receiver can forward traces, metrics, and
logs. Distributed tracing is the primary use case because spans need a central
trace store and a `trace_id -> account` index.

Open decisions for tomorrow:

1. Relay scope: traces only first, or traces + metrics + logs.
2. Gateway topology: pusher → regional OTel gateway → central store.
3. Trace store: current MonoFS logengine vs external Tempo/ClickHouse.
4. Trace index: `trace_id -> account/region` built at ingest.
5. Whether relayed OTel should be tagged `source="doctor"` and queried only
   through the central store (recommended), never through the pusher.

## Done

### E1 — Catalog

- [x] `Target`, `Snapshot`, `Predicates`, `Plan` domain types.
- [x] Static YAML/JSON catalog loader.
- [x] `token_env` resolution (tokens never serialized).
- [x] Indexes: account, region, partition, log group, service, metric, trace.
- [x] Target validation and duplicate-ID detection.
- [x] Tests: `internal/federation/catalog_test.go`.
- [x] O(n) index construction (PR1): no per-append uniqueness scan; measured
  `NewCatalog` 50k = 186ms, 150k = ~1.0s.
- [x] Cached, sorted `Targets()` list; no per-call catalog sort.
- [x] Tests/benchmarks: `internal/federation/catalog_scale_test.go`.

Files:

- `doctor/internal/federation/types.go`
- `doctor/internal/federation/catalog.go`
- `doctor/internal/federation/catalog_scale_test.go`

### E2 — Query planner

- [x] PromQL predicate extraction (metric, account, region, partition, service, log group aliases).
- [x] LogQL/TraceQL predicate extraction.
- [x] Account/partition/log-group/service/metric resolution.
- [x] Guardian log-group parsing fallback (`guardian/<account>/<region>/<partition>/<intent>`).
- [x] Max fan-out enforcement.
- [x] 422 scope errors for unscoped/too-broad live AWS queries.
- [x] Tests: `internal/federation/planner_test.go`.

Files:

- `doctor/internal/federation/planner.go`

### E3 — Federated manager

- [x] Implements `proxy.AWSClient`.
- [x] Lazy per-target client pool.
- [x] Bounded fan-out with per-request timeouts.
- [x] Multi-account metric merge (Prometheus vector/matrix/series/labels/label values).
- [x] Multi-account log merge with account/region/partition stamping.
- [x] Multi-account X-Ray trace summary merge.
- [x] Trace-by-ID target resolution (small-catalog fallback).
- [x] Metadata union (`ListMetrics`, `ListServices`, `ListLogGroups`).
- [x] Regex log-group resolution via catalog.
- [x] Strip Doctor-only target labels before forwarding PromQL to CloudWatch.
- [x] Tests: `internal/federation/manager_test.go`.

Files:

- `doctor/internal/federation/manager.go`
- `doctor/internal/proxy/aws.go` (`AWSLogRecords`, `LogGroupResolver` support)

### Token resolution (P0/PR2)

- [x] `TokenResolver` interface (`Resolve`, `Invalidate`).
- [x] Lazy Guardian resolver via `GET /api/m2m/pushers/{name}`; per-target
  cache, singleflight, 401 invalidation.
- [x] Sharded MonoFS token-bundle fallback (256 shards by default, lazy shard
  reads, per-target O(1) lookup).
- [x] `ChainTokenResolver`: env (dev) → Guardian → MonoFS bundle.
- [x] `awsstore.TokenSource` + `HTTPError`; one refresh/retry on 401.
- [x] Config: `DOCTOR_FEDERATION_TOKEN_RESOLVER`,
  `DOCTOR_FEDERATION_TOKEN_CACHE_TTL`,
  `DOCTOR_FEDERATION_TOKEN_BUNDLE_ROOT`,
  `DOCTOR_FEDERATION_TOKEN_SHARD_COUNT`.
- [x] Tests: `internal/federation/token_test.go`,
  `internal/store/aws/client_test.go`,
  `internal/federation/manager_test.go`.
- [x] Guardian M2M pusher endpoint deployed and verified:
  unauthorized `403`; authorized `/api/m2m/pushers/{name}` returns the pusher
  token; paginated list returns `total`/`hasMore`/`nextCursor`.
- [x] Live token_ref catalog: Doctor loaded
  `monofs:1792749549329844260` with 3 targets and used the Guardian resolver
  (no `DIV_PUSHER_TOKEN_*` dependency in the catalog).
- [x] Live AWS metrics via Guardian-resolved targets: 3 accounts
  (`677296948949`, `755370915520`, `780603797542`).
- [x] Live logs/traces RBAC: `usera` sees DivisionA only; `userb` sees
  DivisionB only; admin sees all. `demo.sh verify` passes.
- [x] PR1 budget tests pass: 150k catalog ~0.8s; 300-shard save/load
  ~0.75s/0.12s.
- [ ] X-Ray end-to-end still has no trace data: Lambda is deployed with
  `TracingConfig=Active` and X-Ray IAM permissions, but
  `GetTraceSummaries` returns 0. This is an AWS-demo/account issue, not the
  resolver path.

Files:

- `doctor/internal/federation/token.go`
- `doctor/internal/store/aws/client.go`
- `doctor/cmd/doctor-query/main.go`

### Catalog hot reload and self-observability (P0/PR3)

- [x] Manager catalog/planner state is atomically swapped; queries take one
  consistent state snapshot.
- [x] `CatalogReloader` checks the snapshot version, loads shards only when the
  version changes, and keeps the old catalog on failure.
- [x] Fast `SnapshotVersioner` path for MonoFS, file, and memory stores.
- [x] `DOCTOR_FEDERATION_RELOAD_INTERVAL` (default 30s) and
  `DOCTOR_FEDERATION_STALE_AFTER`.
- [x] Status now includes `source`, `loaded_at`, `stale`, and capped counts.
- [x] Live hot reload verified without a pod restart:
  `reload-1792750451 -> reload-1792750497-b`, same pod.
- [x] Catalog metrics:
  `doctor_federation_catalog_targets`,
  `doctor_federation_catalog_partitions`,
  `doctor_federation_catalog_targets_by_region`,
  `doctor_federation_catalog_stale`,
  `doctor_federation_catalog_loaded_timestamp_seconds`,
  `doctor_federation_catalog_info`,
  `doctor_federation_catalog_reloads`,
  `doctor_federation_catalog_reload_duration_seconds`.
- [x] Doctor Self-Observability preset updated with catalog target/partition
  counts, stale/age, per-region targets, reload rate, reload p95, reload logs,
  and a catalog status table.
- [x] Live dashboard API returns the new catalog panels; live Prometheus
  queries return catalog targets=3, partitions=3, stale=0, and per-region
  `us-east-1`=3. Reload counters are populated after a publish.
- [x] Native table widgets honor their configured endpoint. The catalog status
  table now calls `/v1/federation/status` instead of repeating the proxy
  endpoint table; initial status includes `source=monofs:<version>`.
- [x] Tests: `internal/federation/reload_test.go`,
  `internal/dashboards/presets_test.go`.

### E4 — Query integration

- [x] `Service.aws` accepts the `proxy.AWSClient` interface.
- [x] One logical `aws-federated` backend registered in `proxy.Router`.
- [x] `Federated` backend flag bypasses router-level account/region pruning.
- [x] `Service.awsViaRouter` prevents double AWS queries.
- [x] Broad live-AWS metric queries return HTTP 422.
- [x] Broad live-AWS log queries return HTTP 422.
- [x] Scope/fan-out rejections are client errors and no longer count as AWS
  backend failures, so repeated 422s cannot open the router circuit breaker or
  mark the `aws` endpoint unhealthy.
- [x] The AWS federation preset keeps its default metric queries scoped by
  account and its log queries scoped by guardian log group; broad queries are
  still rejected at fleet scale.
- [x] Legacy `DOCTOR_AWS_BACKEND_URL` single-backend mode preserved.
- [x] `/v1/federation/status` endpoint.
- [x] Status partition map capped at 512 entries; full `partition_count` and
  `partitions_truncated` fields added for 150k-scale responses.
- [x] Tests: full `go test ./...` passes.

Files:

- `doctor/cmd/doctor-query/main.go`
- `doctor/internal/config/config.go`
- `doctor/internal/query/service.go`
- `doctor/internal/query/federation_scope.go`
- `doctor/internal/query/observability_compat.go`
- `doctor/internal/query/proxy_handlers.go`
- `doctor/internal/proxy/backend.go`
- `doctor/internal/proxy/router.go`

### MonoFS catalog snapshot

- [x] `SnapshotStore` interface (`Load`, `Save`).
- [x] `FileSnapshotStore` for static/local catalogs.
- [x] `MemorySnapshotStore` for tests.
- [x] `MonoFSSnapshotStore` with sharded immutable shards and manifest-last publish.
- [x] SHA-256 validation on load.
- [x] CAS manifest version precondition.
- [x] `doctor-federation-publish` CLI to seed/publish a static catalog to MonoFS.
- [x] Bounded parallel shard load/save (16 workers by default); manifest last.
- [x] Tests: `internal/federation/snapshot_test.go`,
  `internal/federation/snapshot_scale_test.go` (1000 targets / 100 shards).
- [x] In-cluster publish/load verification — published 3 targets (version
  `1792743800844127340`, shard size 500) and verified the snapshot from an
  in-cluster pod. Host-side `--verify` still hits per-node MonoFS DNS
  resolution; run verification from a pod in the cluster.
- [x] Doctor query loads the MonoFS snapshot: startup log
  `multi-account aws federation enabled with 3 target(s) from
  monofs:1792743800844127340`.
- [x] 50k synthetic catalog build: 50,000 targets produced in ~1.3s by
  `doctor-federation-build` (builder only; publish sharding covered by tests).

Files:

- `doctor/internal/federation/snapshot.go`
- `doctor/internal/federation/snapshot_test.go`
- `doctor/cmd/doctor-federation-publish/main.go`
- `doctor/cmd/doctor-query/main.go`
- `doctor/internal/config/config.go`

### Catalog builder from partition information

- [x] `PartitionInfo`, `PusherInfo`, `TargetMeta`, `BuildInput`.
- [x] Join partition ownership with account/region pusher endpoints.
- [x] One target per account/region with all served partitions.
- [x] Prefer healthy pushers, then priority/name.
- [x] Merge labels, log groups, metric names, X-Ray services.
- [x] Guardian log-group convention fallback.
- [x] Skip non-AWS partitions and missing pushers with warnings.
- [x] `doctor-federation-build` CLI (YAML/JSON input → snapshot JSON).
- [x] Tests: `internal/federation/builder_test.go`.

Files:

- `doctor/internal/federation/builder.go`
- `doctor/internal/federation/builder_test.go`
- `doctor/cmd/doctor-federation-build/main.go`

### Demo catalog wiring

- [x] Step 3 generates a 3-target catalog from the deployment catalog + pusher `/v1/aws/meta`.
- [x] Creates `doctor-federation-catalog` ConfigMap.
- [x] Creates `doctor-federation-tokens` Secret.
- [x] Patches doctor-query with catalog volume, `DOCTOR_FEDERATION_*` env, and Secret env.
- [x] Re-applies after every Guardian partition push (step3/step4).
- [x] Handles X-Ray service objects from `/v1/aws/meta`.

Files:

- `demo-divisions/lib/step3_onboard.sh`

## Live verification

Environment: kind cluster `strata`, three demo pushers `:19290/:19291/:19292`.

- [x] `GET /v1/federation/status` → `targets: 3`, partitions `div-a-orders`, `div-a-billing`, `div-b-inventory`, `max_fanout: 32`.
- [x] Metrics: `Invocations{source="aws",account="<account>"}` returned 2 series for each of the three accounts.
- [x] Logs: `federation-probe-<partition>` returned for each account's log group.
- [x] Wildcard logs: `{source="aws", aws_log_group=~".*"}` returned all three accounts.
- [x] Grafana Prometheus datasource:
  - All partitions → 3 series.
  - `div-a-orders` → 1 series.
- [x] Policy isolation:
  - `usera` sees only `div-a-orders` and `div-a-billing`.
  - `userb` sees only `div-b-inventory`.
  - `usera` querying a `div-b-inventory` log group returns 0 records.
- [x] Broad query with `DOCTOR_FEDERATION_MAX_FANOUT=1` → HTTP 422 `query matches too many AWS federation targets`.
- [ ] X-Ray trace data end-to-end — blocked because the demo AWS CloudFormation stacks were deleted; routing/merge covered by unit tests.
- [ ] CloudWatch/X-Ray functional queries after the MonoFS catalog switch —
  the catalog loads, but the demo pushers now fail with expired AWS SSO
  credentials (`InvalidGrantException`). Re-authenticate the demo profiles and
  recreate the stacks before testing live AWS data again.

## Not started

- [ ] Pusher OTLP relay (`:4317/:4318` receiver + WAL + gateway).
- [ ] Central rollups / trace index for broad queries.
- [ ] Grafana `$account` variable and federation status UI.
- [ ] 50k simulated-target load/chaos harness (the 150k catalog construction
  budget test exists; query/fanout/reload chaos is still pending).

## Verification commands

```bash
cd strata/doctor
go test ./...
go build ./cmd/doctor-query/

cd ../demo-divisions
./demo.sh telemetry
./demo.sh status

# Federation status
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:18080/v1/federation/status

# Multi-account metric
curl -G -H "Authorization: Bearer $TOKEN" \
  http://localhost:18080/api/v1/query_range \
  --data-urlencode 'query=Invocations{source="aws",account="780603797542"}' \
  --data-urlencode "start=$(date -u -d '-6 hours' +%s)" \
  --data-urlencode "end=$(date -u +%s)" \
  --data-urlencode 'step=300s'

# Multi-account log
curl -G -H "Authorization: Bearer $TOKEN" \
  http://localhost:18080/v1/logs/search \
  --data-urlencode 'logGroup=guardian/780603797542/us-east-1/div-a-orders/app'

# Build a catalog from partition + pusher + /v1/aws/meta input
go run ./cmd/doctor-federation-build \
  --input federation-build-input.json \
  --out federation-catalog.json

# Publish a static catalog to MonoFS (run in-cluster or with reachable node DNS)
go run ./cmd/doctor-federation-publish \
  --file /tmp/federation-catalog.json \
  --monofs-router monofs-external.lb-edge.svc.cluster.local:9090 \
  --monofs-token "$DOCTOR_MONOFS_TOKEN" \
  --root /doctor/v1/federation --tenant default \
  --shard-targets 500 --verify

# Use MonoFS at startup
DOCTOR_FEDERATION_SNAPSHOT_STORE=monofs \
DOCTOR_FEDERATION_SNAPSHOT_ROOT=/doctor/v1/federation \
DOCTOR_FEDERATION_SNAPSHOT_TENANT=default \
DOCTOR_MONOFS_TOKEN=... \
doctor-query
```
