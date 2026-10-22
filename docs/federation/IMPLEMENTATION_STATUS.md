# Multi-Account Federation — Implementation Status

Date: 2026-10-22  
Design: [`multi-account-federation-50k-design.md`](./multi-account-federation-50k-design.md)  
Plan: [`multi-account-federation-50k.md`](./multi-account-federation-50k.md)

## Done

### E1 — Catalog

- [x] `Target`, `Snapshot`, `Predicates`, `Plan` domain types.
- [x] Static YAML/JSON catalog loader.
- [x] `token_env` resolution (tokens never serialized).
- [x] Indexes: account, region, partition, log group, service, metric, trace.
- [x] Target validation and duplicate-ID detection.
- [x] Tests: `internal/federation/catalog_test.go`.

Files:

- `doctor/internal/federation/types.go`
- `doctor/internal/federation/catalog.go`

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

### E4 — Query integration

- [x] `Service.aws` accepts the `proxy.AWSClient` interface.
- [x] One logical `aws-federated` backend registered in `proxy.Router`.
- [x] `Federated` backend flag bypasses router-level account/region pruning.
- [x] `Service.awsViaRouter` prevents double AWS queries.
- [x] Broad live-AWS metric queries return HTTP 422.
- [x] Broad live-AWS log queries return HTTP 422.
- [x] Legacy `DOCTOR_AWS_BACKEND_URL` single-backend mode preserved.
- [x] `/v1/federation/status` endpoint.
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
- [x] Tests: `internal/federation/snapshot_test.go`.
- [ ] In-cluster publish/load verification — host-side read verification is blocked by per-node MonoFS DNS resolution; the store is covered by unit tests and the write path succeeded.

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

## Not started

- [ ] Pusher OTLP relay (`:4317/:4318` receiver + WAL + gateway).
- [ ] Central rollups / trace index for broad queries.
- [ ] Grafana `$account` variable and federation status UI.
- [ ] 50k simulated-target load/chaos harness.

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
