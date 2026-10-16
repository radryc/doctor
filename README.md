# Doctor

> Part of the **Strata** platform.

Doctor is a privacy-first telemetry backend for OpenTelemetry Collector. It
accepts OTLP traffic, anonymizes sensitive data before persistence, and stores
telemetry in the MonoFS logengine. The query service exposes trace lookup, log
search, metric range queries, and Guardian deployment health APIs, with a
built-in browser UI, drop-in Prometheus/Loki/Tempo compatibility APIs for
Grafana, multi-backend federation, and recording rules.

See [`docs/architecture.md`](docs/architecture.md) for the full architecture.

## Binaries

| Binary | Purpose | Default port |
| --- | --- | --- |
| `doctor-ingest` | OTLP receiver → privacy → write to MonoFS logengine | gRPC `:4317`, HTTP `:4318` |
| `doctor-query` | HTTP query API + browser UI + federation proxy | `:18080` |
| `doctor-rules` | Recording-rule evaluator with lease-based HA | `:18081` |

## Repository layout

```text
cmd/
  doctor-ingest/        OTLP + Guardian event receiver
  doctor-query/         query API, UI, proxy, access control
  doctor-rules/         rule CRUD + evaluator
frontend/               React/TypeScript UI (Vite), embedded into the query binary
internal/
  ingest/ privacy/ otlp/ wal/     receive → anonymize → buffer → write
  store/                          MonoFS logengine + guardian-path clients, AWS client
  query/ proxy/ tempo/ access/ auth/ ruler/
deploy/guardian/partitions/doctor-telemetry/   Guardian intents for the stack
scripts/                contract + federation verification
```

## Prerequisites

Doctor depends on MonoFS logengine for all storage. Both repos must be checked
out as siblings:

```text
replace github.com/radryc/monofs => ../monofs
```

Go 1.26.4 and Node 22 (for the frontend) are required.

## Running locally

Both services need `DOCTOR_MONOFS_LOGENGINE_ADDR` pointing at a running MonoFS
router.

```bash
export DOCTOR_PRIVACY_HMAC_KEY=dev-secret
export DOCTOR_MONOFS_LOGENGINE_ADDR=localhost:9090
go run ./cmd/doctor-ingest
```

That starts:

- OTLP gRPC on `:4317` and OTLP HTTP-protobuf on `:4318`
- Guardian event ingest on `POST /v1/guardian/events`
- WAL-backed write buffers under `.doctor/`

Run the query API in a second terminal:

```bash
export DOCTOR_MONOFS_LOGENGINE_ADDR=localhost:9090
go run ./cmd/doctor-query
```

Open `http://localhost:18080/` for the UI. Recording rules run as a third
service (it needs a MonoFS token for rule files and the leader lease):

```bash
export DOCTOR_MONOFS_LOGENGINE_ADDR=localhost:9090
export DOCTOR_MONOFS_TOKEN=...
export DOCTOR_RULES_QUERY_URL=http://localhost:18080
go run ./cmd/doctor-rules
```

For tail-based trace sampling, put an OpenTelemetry Collector in front of
`doctor-ingest` and apply the example config in
`deploy/guardian/partitions/doctor-telemetry/examples/otel-collector.yaml`.
Doctor stores whatever traces it receives; sampling decisions belong in the
upstream collector pipeline.

## Environment variables

### Storage (required)

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_MONOFS_LOGENGINE_ADDR` | MonoFS router address for logengine RPCs | **required** |
| `DOCTOR_MONOFS_TOKEN` | Guardian token used for rule/OWNERS files and leases | unset |

### Ingest

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_GRPC_ADDR` | OTLP gRPC listen address | `:4317` |
| `DOCTOR_HTTP_ADDR` | OTLP HTTP-protobuf listen address | `:4318` |
| `DOCTOR_DEBUG_ADDR` | pprof listen address | `:6060` |
| `DOCTOR_TENANT_HEADER` | Metadata key/header for tenant routing | `X-Doctor-Tenant` |
| `DOCTOR_DEFAULT_TENANT` | Fallback tenant when none is set | `default` |
| `DOCTOR_PRIVACY_HMAC_KEY` | HMAC key for deterministic tokenization | **required** |

### Write buffers (ingest)

Each signal has an independent WAL-backed buffer that absorbs bursts and
flushes on size or time boundaries.

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_TRACE_BUFFER_ENABLED` / `_WAL_DIR` / `_FLUSH_INTERVAL` / `_MAX_RECORDS` / `_MAX_BYTES` | Trace buffer | `true`, `.doctor/trace-buffer`, `5s`, `4096`, `4194304` |
| `DOCTOR_LOG_BUFFER_ENABLED` / `_WAL_DIR` / `_FLUSH_INTERVAL` / `_MAX_BYTES` | Log buffer | `true`, `.doctor/log-buffer`, `5s`, `4194304` |
| `DOCTOR_METRIC_BUFFER_ENABLED` / `_WAL_DIR` / `_FLUSH_INTERVAL` / `_MAX_BYTES` | Metric buffer | `true`, `.doctor/metric-buffer`, `5s`, `4194304` |

### Ingest pipeline

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_PIPELINE_ENABLED` | Async write pipeline with backpressure | `true` |
| `DOCTOR_PIPELINE_QUEUE_SIZE` | In-flight segment queue depth | `1000` |
| `DOCTOR_PIPELINE_WORKERS` | Writer goroutine count | `4` |

> `DOCTOR_COST_*` limits are defined in config but are **not enforced** by the
> current `doctor-ingest` build.

### Query

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_QUERY_HTTP_ADDR` | Query HTTP listen address | `:18080` |
| `DOCTOR_DEFAULT_TENANT` | Default tenant for queries without one | `default` |
| `DOCTOR_TENANT_HEADER` | Header used when no `tenant` query parameter is set | `X-Doctor-Tenant` |
| `DOCTOR_DEFAULT_PARTITION` | Default Guardian partition in the UI | unset |
| `DOCTOR_GUARDIAN_UI_URL` / `DOCTOR_GUARDIAN_API_URL` | Guardian deep links / live topology data | unset |
| `DOCTOR_AWS_ENABLED` | Federate CloudWatch/Logs Insights/X-Ray via pusher-aws | `true` |
| `DOCTOR_AWS_BACKEND_URL` / `DOCTOR_AWS_BACKEND_TOKEN` | pusher-aws read API base URL + bearer | unset |
| `DOCTOR_PROXY_ENABLED` | Route queries through the multi-backend proxy | `true` |
| `DOCTOR_PROXY_BACKENDS_FILE` | YAML/JSON backend registry (hot-reloaded) | unset |
| `DOCTOR_PROXY_MAX_INFLIGHT` / `DOCTOR_PROXY_BACKEND_TIMEOUT` | Fan-out bound / per-backend timeout | `32`, `15s` |
| `DOCTOR_GUARDIAN_DISCOVERY_ENABLED` / `_INTERVAL` / `DOCTOR_GUARDIAN_TOKEN` | Backend discovery from Guardian | `false`, `30s`, unset |
| `DOCTOR_AUTH_REQUIRE` | Reject anonymous callers outside exempt paths | `false` |
| `DOCTOR_AUTH_EXEMPT_PATHS` | Comma-separated unauthenticated paths | health/ready/metrics/Prometheus |
| `DOCTOR_OIDC_ISSUER` / `DOCTOR_OIDC_AUDIENCE` | OIDC verification | unset |
| `DOCTOR_OIDC_CLIENT_ID` / `_CLIENT_SECRET` / `_REDIRECT_URL` | Browser login client | unset |
| `DOCTOR_BREAK_GLASS_TOKEN` | Static admin bearer token | unset |
| `DOCTOR_SESSION_DIR` | Persistent browser session directory | `.doctor/sessions` |
| `DOCTOR_ACCESS_RESOLVER` | `owners`, `grants`, `static`, `chain`, or `allow-all` | `owners` |
| `DOCTOR_ACCESS_MODE` | `observe` (log only) or `enforce` (filter logs/traces) | `observe` |
| `DOCTOR_ACCESS_ENFORCE_LOGS` / `_TRACES` | Per-signal enforcement | `false` |
| `DOCTOR_ACCESS_TEAM_MAPPING` | OWNERS `@team` → IdP group mapping (file or inline YAML) | unset |
| `DOCTOR_ACCESS_GRANTS_PATH` | Grant-store JSON path for the `grants` resolver | unset |
| `DOCTOR_ACCESS_OWNERS_ROOT` | Default OWNERS path root | `/doctor/v1/endpoints` |
| `DOCTOR_ACCESS_CACHE_TTL` | OWNERS decision cache TTL | `60s` |
| `DOCTOR_ACCESS_TRUST_TENANT_HEADER` | Treat the tenant header/parameter as machine identity `tenant:<name>` (required for header-authenticated integrations under enforcement; only enable where all callers are trusted) | `false` |
| `DOCTOR_PROM_MAX_SAMPLES` / `_MAX_SERIES` / `_GLOBAL_MAX_SAMPLES` | PromQL budgets (HTTP 422 beyond) | `4000000`, `100000`, `8000000` |

### Recording rules

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_RULES_ENABLED` | Master switch | `true` |
| `DOCTOR_RULES_HTTP_ADDR` | CRUD + health | `:18081` |
| `DOCTOR_RULES_QUERY_URL` / `DOCTOR_RULES_QUERY_TOKEN` | doctor-query base URL + bearer | unset |
| `DOCTOR_RULES_WORKERS` / `DOCTOR_RULES_EVAL_TIMEOUT` | Evaluation workers / per-rule timeout | `8`, `30s` |
| `DOCTOR_RULES_LEASE_TTL` / `_RENEW` / `DOCTOR_RULES_RULER_ID` | Leader lease timing and identity | `30s`, `10s`, hostname |
| `DOCTOR_RULES_STORE_ROOT` / `DOCTOR_RULES_POLL_INTERVAL` | MonoFS rule root / reload interval | `/doctor/v1/rules`, `30s` |

### Self-telemetry (all services)

All telemetry is disabled when `DOCTOR_OTEL_ENDPOINT` is unset.

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_OTEL_ENDPOINT` | OTLP gRPC endpoint for Doctor's own telemetry | unset (disabled) |
| `DOCTOR_OTEL_SERVICE_NAME` / `DOCTOR_OTEL_INSECURE` / `DOCTOR_OTEL_METRIC_INTERVAL` | Service name, plaintext toggle, metric interval | `doctor`, `true`, `15s` |

## HTTP endpoints

### Ingest (`doctor-ingest`)

| Endpoint | Purpose |
| --- | --- |
| `POST /v1/traces`, `/v1/logs`, `/v1/metrics` | OTLP HTTP ingest (protobuf only, 10 MB cap, `202`) |
| `POST /v1/guardian/events` | Guardian deployment events (JSON array; accepted, not persisted — Guardian history is fetched live at query time) |
| `GET /healthz` | Liveness |
| `GET /api/v1/status/buildinfo` | Build metadata |

### Query (`doctor-query`)

Native APIs: `/v1/spans/search`, `/v1/traces/recent`, `/v1/traces/{id}`,
`/v1/logs/services`, `/v1/logs/search`, `/v1/metrics/range`,
`/v1/services/discover`, `/v1/services/suggest`, `/v1/proxy/endpoints`,
`/v1/guardian/{topology,events,health,overview}`, `/v1/ui/config`, `/healthz`,
`/ready`, and the UI at `/`.

Grafana compatibility surfaces (read-only):

| Prefix | Purpose |
| --- | --- |
| `/api/v1/*` | Prometheus API (`query`, `query_range`, `labels`, `label/*`, `series`, `metadata`, `query_exemplars`, `rules`) |
| `/loki/api/v1/*` | Loki API (`query`, `query_range`, `labels`, `series`, `index/*`, `patterns`, `detected_fields`, `detected_labels`, `status/buildinfo`) |
| `/api/*`, `/api/v2/*` | Tempo API (trace by ID, TraceQL search, tag discovery, TraceQL metrics) |

Tempo/Loki trace-by-ID endpoints serve OTLP protobuf when the client sends
`Accept: application/protobuf` (as Grafana does) and OTLP JSON otherwise.
`latest*` magic trace IDs let panels pin the newest trace. Run
`scripts/verify-grafana-contract.sh` (with `DOCTOR_QUERY_URL`) to check every
endpoint and query shape the Grafana datasources use.

### Rules (`doctor-rules`)

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/rules` | List groups |
| GET/PUT/DELETE | `/v1/rules/{tenant}/{group}` | Fetch/replace/delete a group (YAML) |
| POST | `/v1/rules/evaluate?tenant=&group=` | Evaluate once |
| GET | `/healthz` | Liveness |

## Grafana unified dataspace

The Prometheus, Loki, and Tempo APIs project every record through one shared
label vocabulary:

| Label | Source |
| --- | --- |
| `service` | OTLP service name |
| `tenant` | tenant from header/parameter |
| `guardian_partition`, `guardian_intent`, `guardian_asset` | resolved identity |
| `k8s_namespace_name`, `k8s_pod_name`, `k8s_container_name`, `k8s_node_name` | resource attributes |
| `trace_id`, `span_id` | OTLP correlation IDs (metrics via exemplars) |
| `source` | `doctor` for native telemetry, `aws` for federated data |
| `aws_region`, `aws_account` | federated AWS partition |

Loki exposes a curated subset as stream labels to keep cardinality bounded;
metrics and traces carry the full attribute set. Metric samples with a
`trace_id` come back as Prometheus exemplars, so provisioned datasources can
link metrics → traces → logs.

### Under access enforcement

`DOCTOR_ACCESS_MODE=observe` (default) only logs decisions, so Grafana's
provisioned datasources keep working. With `enforce`, logs and traces are
actually filtered (metrics never are). To keep datasources working:

1. Configure Grafana datasources with `X-Doctor-Tenant` and set
   `DOCTOR_ACCESS_TRUST_TENANT_HEADER=true` on doctor-query; each request then
   carries the machine identity `tenant:<tenant>` in group `doctor:tenant`.
2. Allow that principal with `DOCTOR_ACCESS_RESOLVER=chain` +
   `DOCTOR_ACCESS_STATIC_ALLOW_GROUPS=doctor:tenant`, or by listing
   `tenant:<tenant>` as a viewer in the endpoint's OWNERS file.

Only enable header trust where every caller reaching doctor-query is trusted.

## Multi-backend proxy

`doctor-query` federates PromQL, Loki, Tempo, Elasticsearch, and BigQuery
sources into the same three dataspaces. Backends come from
`DOCTOR_PROXY_BACKENDS_FILE` and/or Guardian discovery:

```yaml
version: 1
backends:
  - name: prom-edge
    kind: prometheus
    baseUrl: http://prom-edge:9090
    signals: [metrics]
    ownersPath: /doctor/v1/endpoints/prom-edge
  - name: loki-shard-1
    kind: loki
    baseUrl: http://loki-1:3100
    signals: [logs]
  - name: es-logs
    kind: elasticsearch
    baseUrl: http://elasticsearch:9200
    index: logs-*.otel-*
    signals: [logs]
```

Queries fan out to healthy backends and merge into one result set. Records are
tagged `source=<kind>`; when two backends of the same kind return the same
series/stream/span, a `shard=<backend-name>` label is added so nothing is
dropped. Remote failures surface as `X-Doctor-Partial`. `GET
/v1/proxy/endpoints` lists backends and health. AWS stays registered as a
`fallback` backend: it is queried only when local results are empty or the
query selects `source="aws"`.

## AWS federation

CloudWatch metrics are federated through CloudWatch's own Prometheus-compatible
PromQL API (proxied and SigV4-signed by `guardian-pusher-aws`), not a selector
translation. Add `source="aws"` to a query to route it there; Doctor strips
`source` and forwards the rest verbatim:

```promql
sum(rate({CPUUtilization, "@instrumentation.@name"="cloudwatch.aws/ec2", source="aws"}[5m]))
```

Metric/log/trace discovery always unions the AWS federation when
`DOCTOR_AWS_ENABLED=true`, so CloudWatch metrics and X-Ray services are
browsable without knowing `source="aws"` first. Federated records carry
`aws_region`/`aws_account` with `guardian_partition` set to the region.

Verify with `./scripts/verify-aws-federation.sh`.

## Recording rules

`doctor-rules` evaluates PromQL recording rules per shard, merges results by
the declared strategy (`sum|max|min|last|keep`; `keep` preserves the `shard`
label), and writes derived metrics back into the logengine where the normal
query APIs read them. Rules live in MonoFS at
`/doctor/v1/rules/<tenant>/<group>.yaml`. Partial sums are skipped rather than
written incorrectly. One replica holds a MonoFS CAS lease at
`<store>/.lease/leader.json`; followers take over after the lease expires.

```yaml
version: 1
groups:
  - name: shard-health
    tenant: default
    interval: 30s
    rules:
      - record: doctor:shard_up:sum
        expr: sum(up) by (job)
        labels: { tier: edge }
        sources: [prom-a, prom-b]
        merge: sum
```

## Correlation verification

`scripts/verify-correlation.sh` confirms one trace carries the same
correlation IDs across traces, logs, and metric exemplars:

```bash
DOCTOR_QUERY_URL=http://localhost:18080 DOCTOR_TENANT=default ./scripts/verify-correlation.sh
```

## Building containers

The Dockerfile needs the MonoFS checkout because of the go.mod replace
directive. Pass it as a named build context:

```bash
make docker-build SERVICE=doctor-ingest MONOFS_DIR=../monofs
make docker-build SERVICE=doctor-query  MONOFS_DIR=../monofs
make image-all
```

Pass `BUILD_VERSION`, `BUILD_COMMIT`, and `BUILD_TIME` to embed version
metadata reported by `/api/v1/status/buildinfo`.

## Guardian deployment

Guardian manifests live under `deploy/guardian/partitions/doctor-telemetry/`:
ingest gateway (Compute + LoadBalancer), query plane (proxy + owners resolver,
observe mode), rules plane, and a join-only lifecycle intent. They assume a
running MonoFS logengine and a Guardian token for auth; an example Collector
pipeline is in `examples/otel-collector.yaml`.

## Current limitations

- `DOCTOR_COST_*` cost limits are not enforced yet.
- OTLP over HTTP is protobuf-only (no JSON).
- No retention or sampling inside Doctor — both are delegated to MonoFS
  logengine / the upstream collector.
- Guardian events are accepted but not persisted; Guardian history is read
  live from the Guardian API at query time.
