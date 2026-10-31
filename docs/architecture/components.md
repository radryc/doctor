# Components and interfaces

This document explains what each package and binary owns. The line-by-line file inventory is in
[internal/code-inventory.md](../internal/code-inventory.md).

## 1. Binaries

### `cmd/doctor-ingest`

**Purpose:** receive OTLP and write normalized records to the MonoFS log-engine.

Startup sequence (`cmd/doctor-ingest/main.go`):

1. set `GOMEMLIMIT` to a hard-coded 858 MiB and start an optional pprof server;
2. load `config.IngestConfig` and require `DOCTOR_PRIVACY_HMAC_KEY` and
   `DOCTOR_INGEST_AUTH_TOKEN`;
3. initialize OTel telemetry and the `doctor.component.up` heartbeat;
4. build `privacy.Engine`, `MonoFSWriter`, optional trace/log/metric buffers, optional pipeline;
5. build `ingest.Service` and register the three OTLP gRPC services plus the HTTP collector;
6. serve gRPC and HTTP concurrently; shut down on SIGINT/SIGTERM.

Notable choices:

- gRPC max receive/send size is 256 MiB (`grpc.MaxRecvMsgSize` / `MaxSendMsgSize`).
- HTTP request bodies are limited to 10 MiB per OTLP request.
- `TraceBuffer`, `LogBuffer`, and `MetricBuffer` are always constructed when enabled, even though
  the default pipeline takes precedence over them.
- `WriteGuardianEvents` on the MonoFS backend is a no-op.

### `cmd/doctor-query`

**Purpose:** serve every query API, host the UI, and optionally proxy/federate other backends.

Startup sequence (`cmd/doctor-query/main.go`):

1. load `config.QueryConfig`, telemetry, heartbeat;
2. require `DOCTOR_MONOFS_LOGENGINE_ADDR`, connect `LogEngineClient`;
3. construct `query.Service` and set cache windows, Guardian URLs, default partition, rules URL;
4. open a shared MonoFS file store for dashboards (falls back to in-memory), install dashboard store;
5. build the repo policy gate (`DOCTOR_ACCESS_POLICY_*`), if configured, and wrap the log backend;
6. build the federation token resolver;
7. if federation is enabled, load the snapshot (inline JSON, MonoFS, or file), build the catalog,
   manager, metrics, and optional reloader; else optionally build a legacy single AWS client;
8. build OIDC/break-glass auth, browser login, and the legacy access resolver/gate;
9. if proxy is enabled, build the registry (static file and/or Guardian discovery), register local
   and AWS backends, and install the router;
10. build the middleware chain and serve HTTP.

Middleware order (outermost to innermost):

```
WebAuthenticator.Handler
  -> TenantTrust.Wrap
    -> auth.Middleware.Wrap
      -> PolicyGate.Middleware
        -> query.Service.Handler (which itself wraps warning headers + Guardian token)
```

### `cmd/doctor-rules`

**Purpose:** schedule recording rules and write derived metrics.

Startup sequence:

1. load `RulesConfig`; require MonoFS log-engine and file token;
2. connect `GRPCClient` (file store) and `LogEngineClient` (metric writes);
3. build `ruler.Store`, `Evaluator` using an HTTP querier to `DOCTOR_RULES_QUERY_URL`, `Scheduler`,
   and `LeaseManager`;
4. serve the rules API and run the lease/scheduler loop.

The rules API requires `DOCTOR_RULES_API_TOKEN`; a static bearer middleware protects every route
except `/healthz` and `/metrics`. `ruler.API.Authorizer` is set to `AuthenticatedAuthorizer`, so
mutations are allowed only after the bearer check succeeds. This is still all-or-nothing: there is no
per-tenant rule ownership.

### Federation CLIs

| Binary | Behavior |
|---|---|
| `doctor-federation-build` | loads YAML/JSON `federation.BuildInput`, calls `BuildCatalog`, prints or writes a snapshot JSON |
| `doctor-federation-publish` | loads a snapshot file, writes it through `MonoFSSnapshotStore.Save`, optionally verifies by reading it back |
| `doctor-federation-controller` | connects to Guardian + MonoFS, acquires a leader lease, builds metadata and catalog, publishes token shards and snapshot on an interval |

The controller and publish CLIs take secrets as command-line flags (`--monofs-token`,
`--guardian-token`) as well as environment fallbacks; flags are visible in the process table.

## 2. Configuration and shared primitives

### `internal/config`

All environment parsing. Three loaders:

- `LoadIngest` validates the privacy HMAC key and returns ingest settings.
- `LoadQuery` returns query/auth/access/proxy/federation settings with defaults.
- `LoadRules` returns rule-engine settings with a hostname-derived ruler ID.

Helper semantics worth knowing:

- `envString`/`envBool`/`envInt64` silently fall back on parse errors.
- `envDuration` accepts a Go duration or a bare integer, which it interprets as **hours**.
- `envCSV` trims values and drops empty entries.

`service` parameters are accepted by all three loaders but unused.

### `internal/buildinfo`

Collects version, VCS revision/branch/time, modified flag, and Go version from `-ldflags` and
`runtime/debug`. `StatusFields` matches the Prometheus/Grafana build-info shape.

### `internal/identity`

Canonical telemetry identity:

- `SplitServiceName` requires exactly three non-empty dot-separated parts.
- `CanonicalServiceName` joins non-empty parts, caps at 200 characters, and appends a deterministic
  SHA-256-derived suffix when truncation is needed.
- Used by OTLP ingest, query label projection, and Tempo conversion.

### `internal/segment`

The shared record model used by ingest, storage adapters, query, ruler, and federation:

- `TraceRecord`, `TraceEvent`
- `LogRecord`
- `MetricPointRecord`, `MetricSeriesMeta`, `MetricLabelMatcher`, `MetricQuery`
- `GuardianEvent`, `DeploymentNode`, `DeploymentEdge`, `DeploymentTopology`, `HealthScore`,
  `HealthFeedback`
- `IsUnboundedMetricLabel` excludes per-record correlation IDs from metric series identity.

The `GuardianEvent`/topology/health types are declared here but only partially used by the current
services (the query service has its own Guardian wire structs).

### `internal/tracecontext`

W3C/X-Ray bridge:

- canonical 32-hex trace IDs and 16-hex span IDs;
- conversion to/from `1-<8hex>-<24hex>` X-Ray IDs;
- `IsXRayCompatible` checks the embedded epoch;
- `BridgePropagator` reads W3C first and falls back to X-Ray, and injects both;
- `XRayIDGenerator` wraps the AWS propagator ID generator.

### `internal/telemetry`

Doctor's own OTLP telemetry:

- `LoadConfig` reads endpoint, service name, component, instance ID, partition, insecure flag, metric
  interval; if no endpoint, `Setup` returns a disabled handle.
- `Setup` builds OTLP gRPC trace/metric/log exporters, providers, and sets the global propagator.
- `NewStdLogWriter` forwards Go `log` lines as OTLP log records, classifying severity by substring.
- `RegisterComponentHeartbeat` emits `doctor.component.up=1`.

### `internal/privacy`

Deterministic sanitization at ingest:

- identity keys (`service.name`, `guardian.*`, k8s identity fields, `guardian.target.*`) are never
  redacted;
- keys matching password/secret/token/authorization/cookie/session/api-key/JWT/credential are
  replaced with `[REDACTED]`;
- values that look like bearer tokens, JWTs, or long provider secrets are replaced;
- email/user/account/client/IP keys and IPv4/email substrings are replaced with deterministic
  `tok_<16hex>` HMAC-SHA256 tokens;
- `Tokenize` uses the configured HMAC key.

The identity allowlist is also a bypass: any secret placed in an identity key is preserved.

### `internal/wal`

Three nearly identical batch logs plus one unused generic WAL:

- `TraceBatchLog`, `LogBatchLog`, `MetricBatchLog`: `Append`, `ReadAll`, `Iterate`, `Replace`,
  `Count`, length-prefixed little-endian `uint32` + JSON frames, file `*.log`;
- `WAL`: per-entry `.pending` files with `Stage`, `Commit`, `Pending`, `Count`; not used by any
  service.

There is no frame checksum or format version. Corrupt frames are skipped by the log/metric readers
and returned as an error by the trace reader.

### `internal/store`

- `store.Store` defines a versioned file-store interface plus logical-path normalization, content
  hash, version ID, and batch revision ID helpers.
- `store/fs` is a local disk implementation with `.versions/meta` and `.versions/data`; not used by
  any service.
- `store/monofs` contains the two gRPC clients used in production:
  - `GRPCClient`: file CAS operations (`UpsertPaths`, `DeletePaths`, `ListVersions`,
    `GetVersion`, `ReadFile`, `ListDir`), router registration, node discovery, heartbeat, and
    per-node reads. Uses `insecure.NewCredentials()` and the `x-monofs-internal` metadata token for
    node reads.
  - `LogEngineClient`: log/metric/trace ingest and query through `MonoFSRouterClient`; also streams
    metric series metadata and trace discovery. Uses `insecure.NewCredentials()` and has no token
    field.
- `store/aws`: HTTP client for `guardian-pusher-aws` read API, with token-source abstraction,
  one-shot 401 token refresh, cached `/healthz`, and helpers for CloudWatch metrics/logs, X-Ray
  services/traces, and PromQL proxy endpoints.

## 3. Ingest components

### `internal/otlp`

Flattening and identity normalization:

- `FlattenTraces`: resource attributes → `service`/`partition`/`intent`/`asset`; span attributes,
  events, status, timestamps are sanitized.
- `FlattenLogs`: resource/scope/log records; body is sanitized as text.
- `FlattenMetrics`: all OTLP metric types are expanded into point records:
  - gauge/sum → one point;
  - histogram → `<name>_bucket` per bucket, `_count`, `_sum`;
  - exponential histogram → `_count`, `_sum`;
  - summary → quantile series plus `_count`, `_sum`.
- `telemetryIdentity`: fallback chain over `service.name`, Kubernetes names, namespace/component,
  instance/pod.
- `canonicalizeResourceIdentity`: promotes explicit Guardian coordinates to canonical names and
  records the old service as `service.alias`.
- `normalizeMetricRecordInPlace`: resolves producer vs subject identity. Resource identity wins;
  point-level identity becomes `guardian.target.*`; asset-observation metrics may describe a
  different subject than the producer.

### `internal/ingest`

- `Service`: routes OTLP requests to pipeline → buffer → writer; exposes gRPC and HTTP collectors.
  HTTP supports OTLP/protobuf and OTLP/JSON; JSON hex trace/span IDs are rewritten to base64 before
  `protojson` decoding.
- `Pipeline`: bounded queue, worker pool, synchronous result channel, `ErrQueueFull` backpressure.
- `TraceBuffer`, `LogBuffer`, `MetricBuffer`: in-memory batch lists with WAL persistence, replay,
  size/record thresholds, and background flush.
- `MonoFSWriter`: `Flusher` implementation using `LogEngineClient`; chunk IDs are timestamp-based.
- `buffer_metrics.go`: OTel gauges/counters for pending batches/bytes/records and flush count.
- `service_metrics.go`: accepted request/record counters, primed per signal.
- `cost.go`: `CostController` limits; **not instantiated anywhere**.
- `errors.go`: `ErrBufferFull` shared by buffers and pipeline mapping.

Signal routing detail:

```text
ingestTraces/Logs/Metrics
  if pipeline != nil   -> pipeline.Submit*  (default)
  else if buffer != nil -> buffer.Append
  else                  -> writer.Write*
```

### `internal/guardian`

Unused HTTP middleware helpers:

- bearer-token auth with constant-time compare;
- per-remote-address token-bucket rate limiter;
- TTL deduplicator.

Only tests reference them. They are not wired into ingest, query, or rules.

## 4. Query components

### `internal/auth`

- `Config` and `NewVerifier`: OIDC verifier (issuer+audience) followed by a break-glass static token.
- `NewWebAuthenticator`: browser Authorization Code + PKCE flow with persistent session store,
  cookie `doctor_session`, optional require-login, RP-initiated logout.
- `Middleware`: wraps the MonoFS `authz.Authenticator`, attaches identity, and rejects anonymous
  callers outside exempt paths when `require` is set.
- `DefaultExemptPaths`: `/healthz`, `/ready`, `/api/echo`, `/metrics`, `/api/v1`. The `/api/v1`
  entry exempts the entire Prometheus API, not just a health endpoint.
- `TenantTrust`: synthetic machine identity `ClientID=tenant:<name>`, group `doctor:tenant`, from
  the tenant header or query parameter, only when no verified identity exists.

### `internal/access`

Two authorization systems live here:

1. **Policy gate** (`policy.go`): loads a compiled policy bundle from Guardian/file, applies
   request-level and record-level filtering, canonicalizes AWS partitions, and hides catalog
   entries. Metrics always pass. Observe/enforce modes.
2. **Legacy endpoint resolvers** (`resolver.go`, `owners.go`, `grants.go`, `monofs_source.go`):
   `allow-all`, `static`, `grants`, `owners`, or a chain; each decides whether an identity may read
   an endpoint signal. The `Gate` also applies observe/enforce per signal and is used by
   `proxy.Router` for remote backends.

The legacy resolvers read OWNERS files from MonoFS at `<dir>/.guardian/OWNERS` and walk ancestor
directories; team handles (`@team`) are resolved through `authz.TeamMapping`.

### `internal/query`

The largest package. Responsibilities:

| File | Responsibility |
|---|---|
| `service.go` | `Service` struct, route table, tenant/partition parsing, Guardian proxy, service discovery, recent traces, native logs/traces/metrics, federation status/targets/plan, UI config/user |
| `prometheus_compat.go` | custom PromQL storage adapter, discovery, sample/series budgets, broad-selector guard, count-by-name fast path hooks, HTTP handlers |
| `prometheus_count_by_name.go` | series-index fast path for `count`/`count by (__name__)` |
| `prometheus_exemplars.go` | `/api/v1/query_exemplars`, trace/span correlation |
| `prometheus_guard.go` | rejects all-series range queries over 15 minutes |
| `series_index.go` | series metadata discovery from MonoFS chunk indexes |
| `loki_query.go` | LogQL subset parser, storage reduction, local stages/metric aggregation |
| `loki_volume.go` | Loki index stats/volume/volume_range, empty patterns/detected fields |
| `observability_compat.go` | Loki HTTP handlers, stream shaping, label projection |
| `tempo_handlers.go` | Tempo echo/buildinfo/traces/search/tags/tag-values/metrics, latest* magic trace IDs |
| `trace_pagination.go` | watermark/cursor pagination for recent traces, local + AWS merge |
| `traceql_metrics.go` | TraceQL metrics subset (`rate`, `count_over_time`, `quantile_over_time`) |
| `trace_discovery_cache.go` | per-identity TTL caches for trace tags/values/scopes |
| `proxy_handlers.go` | router-backed Prometheus handlers, proxy endpoint listing, span search hydration |
| `proxy_wiring.go` | local metrics/logs/traces upstream adapters and canonical trace labels |
| `aws_metrics.go` / `aws_federation.go` | direct AWS wrappers, partition/intent/asset filters, health/discovery |
| `policy_backend.go` | `LogEngineBackend` decorator applying `PolicyGate` to logs/traces; AWS upstream filters |
| `labels.go` | canonical label vocabulary and projection shared by all signals |
| `identity_filter.go` | partition/intent/asset request filters and LogQL selector rendering |
| `dashboards_handlers.go` | dashboard CRUD, versions, import/export |
| `guardian_probes.go` | Guardian `/api/probes` proxy + pusher summary + partition filtering |
| `warnings_middleware.go` | per-request warning collector and `X-Doctor-*` response headers |
| `ui.go` | `//go:embed ui/dist`, SPA fallback, API-path 404 guard |

### `internal/proxy`

The multi-backend model and router:

- `backend.go`: `Backend` metadata, signal/mode/policy validation, deterministic sort.
- `registry.go`: in-memory, static-file (mtime reload), and composite registries.
- `discovery_guardian.go`: polls Guardian `/api/pushers` (delta after a full reconcile) and
  `/api/m2m/partitions`; builds backends from pusher metadata.
- `router.go`: bounded fan-out, access decisions, endpoint summaries/pages, circuit breakers,
  metrics/logs merge, selector-based target pruning.
- `router_traces.go`: trace service/tag/span-set fan-out, TraceQL capability selection, local trace
  access error.
- `upstream.go`: `MetricsUpstream`, `LogsUpstream`, `TracesUpstream`, optional interfaces for
  trace window, tags, span sets, tag scopes, and TraceQL.
- `upstream_prom.go`, `upstream_loki.go`, `upstream_tempo.go`, `upstream_es.go`,
  `upstream_bigquery.go`: HTTP adapters.
- `aws.go`: AWS source detection, Logs Insights pushdown, AWS record conversion, `AWSUpstream`.
- `merge_logs.go`, `merge_metrics.go`, `merge_traces.go`: cross-backend dedup, provenance
  (`source`), collision sharding (`shard`), multi-source trace annotation.
- `traceql_search.go`: TraceQL subset → `TraceSearch`, record matching, value resolution.
- `warnings.go`: request-scoped warning collector (cap 64).
- `translate/`: LogQL selector parser, TraceQL subset parser, ES|QL and BigQuery SQL generation.

### `internal/dashboards`

Portable dashboard schema (`doctor.dashboard/v1`) and CAS store:

- widget kinds: timeseries, stat, toplist, gauge, table, logs, traces, topology, markdown;
- datasources: prometheus, loki, tempo, native_logs, native_traces;
- variables: label values, custom, text, partition, service;
- six built-in presets: fleet, service RED, synthetics, MonoFS storage, Doctor self, AWS federation;
- stored dashboards are YAML at `/doctor/v1/dashboards/<tenant>/<id>.yaml`; built-ins are read-only
  and win over stored IDs;
- CAS via expected version; `List`/`Get` populate version metadata from `ListVersions`.

### `internal/ruler`

Recording rules:

- `rule.go`: `File`, `Group`, `Rule`, merge strategies (`sum`, `max`, `min`, `last`, `keep`),
  duration parsing, validation.
- `store.go`: rule YAML in MonoFS at `/doctor/v1/rules/<tenant>/<group>.yaml`; `MemoryStore` for
  local/test; CAS.
- `scheduler.go`: poll/reload groups, min-heap of next evaluations, jitter in the first 10% of the
  interval, bounded worker pool.
- `evaluator.go`: PromQL instant query through `Querier`, per-shard merge, skip partial sums, write
  derived `MetricPointRecord`s.
- `querier.go`: HTTP client to doctor-query `/api/v1/query`; honors `X-Doctor-Partial`.
- `writer.go`: writes derived metrics through `LogEngineClient`.
- `lease.go`: CAS leader lease at `/doctor/v1/rules/.lease/leader.json`.
- `api.go`: `GET /v1/rules`, `GET /v1/rules/status`, `POST /v1/rules/evaluate`,
  `GET/PUT/DELETE /v1/rules/{tenant}/{group}`. Mutations use `AuthenticatedAuthorizer`, backed by the
  required `DOCTOR_RULES_API_TOKEN` middleware.

## 5. Federation components

| File | Responsibility |
|---|---|
| `types.go` | `Target`, `Snapshot`, `Query`, `Predicates`, `Plan`, `ScopeError` |
| `catalog.go` | immutable-ish catalog with account/region/partition/log-group/service/metric indexes; validation; `byTrace` map is read but never populated |
| `builder.go` | joins Guardian partitions + pushers + discovery metadata into account/region targets |
| `snapshot.go` | file/memory/MonoFS snapshot stores; sharded save with SHA-256 shard refs and manifest CAS |
| `targets.go` | paginated, access-filtered target view |
| `planner.go` | resolves PromQL/LogQL/TraceQL predicates to a bounded target set; rejects unscoped/broad queries |
| `fanout.go` | bounded, retrying, circuit-aware per-target calls; records partial warnings |
| `manager.go` | implements `proxy.AWSClient`; targets, status, metric/service/log-group discovery, queries, PromQL, traces, logs, client pool, breakers |
| `breaker.go` | per-target breaker with half-open probe and error classification |
| `client_pool.go` | LRU pool of per-target AWS clients, keyed by target ID, invalidated on endpoint/token change |
| `token.go` | env/Guardian/bundle token resolvers; lazy Guardian fetch with positive/negative TTL and single-flight; sharded bundle fallback |
| `token_bundle.go` | writes AES-256-GCM encrypted (or legacy plaintext) token shards; rewrites every shard so removed targets are revoked |
| `controller.go` | leader-elected Guardian→metadata→catalog→publish cycle |
| `controller_metadata.go` | pusher `/v1/aws/meta` fetcher, durable metadata cache, TTL jitter |
| `guardian_feed.go` | paginated Guardian M2M pusher and partition feeds |
| `leader.go` | CAS leader lease for the controller |
| `reload.go` | periodic snapshot version check and manager reload |
| `metrics.go` | catalog/plan/fan-out/circuit/token OTel instruments |
| `controller_metrics.go` | controller sync/publish/metadata OTel instruments |
| `request_meta.go` | request-scoped federation version/matched/selected headers |
| `debug.go` | planner debug view |

## 6. Storage and wire formats

### Interfaces

| Interface | Implementations | Used by |
|---|---|---|
| `ingest.Flusher` | `MonoFSWriter`, test fakes | ingest service, buffers, pipeline |
| `query.LogEngineBackend` | `monofs.LogEngineClient`, `policyBackend` | query service |
| `proxy.MetricsUpstream` / `LogsUpstream` / `TracesUpstream` | local adapters, Prom/Loki/Tempo/ES/BigQuery HTTP adapters, AWS upstream | proxy router |
| `proxy.AWSClient` | `store/aws.Client`, `federation.Manager` | query AWS paths, federation |
| `ruler.FileStore` | `monofs.GRPCClient`, `ruler.MemoryStore` | rules, dashboards, federation file stores |
| `federation.CatalogFileStore` | `ruler.FileStore` implementations | snapshots, tokens, metadata, leases |
| `federation.SnapshotStore` | `FileSnapshotStore`, `MemorySnapshotStore`, `MonoFSSnapshotStore` | controller, reloader, query startup |
| `federation.TokenResolver` | `EnvTokenResolver`, `GuardianTokenResolver`, `BundleTokenResolver`, `ChainTokenResolver` | manager client factory |
| `access.Resolver` | `AllowAll`, `Static`, `Grants`, `Owners`, `Chain` | legacy proxy gate |
| `authz.TokenVerifier` | OIDC verifier, break-glass, chain | auth middleware, web auth |
| `guardian.GuardianSource` (federation) | `GuardianFeed` | controller |

### MonoFS file layout

```text
/doctor/v1/federation/<tenant>/manifest.json
/doctor/v1/federation/<tenant>/shards/<publishID>/0000.json
/doctor/v1/federation/<tenant>/controller/leader.json
/doctor/v1/federation/<tenant>/controller/metadata.json
/doctor/v1/federation/tokens/<tenant>/0000.json
/doctor/v1/dashboards/<tenant>/<id>.yaml
/doctor/v1/rules/<tenant>/<group>.yaml
/doctor/v1/rules/.lease/leader.json
/doctor/v1/endpoints/<name>/.guardian/OWNERS
```

- Snapshots are sharded JSON (`targets` per shard) with SHA-256 and target-count checks.
- The manifest is the commit point. Shards are written with expected version `absent` under a unique
  `publishID`; the manifest uses the latest file version as its CAS precondition.
- Token shards are keyed by target ID and chosen by FNV-1a hash modulo the configured shard count.
  With a key they contain AES-256-GCM ciphertext; without one they are legacy plaintext JSON. The
  controller requires a key before publishing a non-empty token map.
- Dashboard and rule files are YAML; every write supplies an expected version (`absent` or a prior
  version ID).

### Network protocols

| Peer | Protocol | Auth | Notes |
|---|---|---|---|
| OTLP clients → ingest | gRPC / HTTP protobuf or JSON | none | tenant from header/metadata; 256 MiB gRPC, 10 MiB HTTP |
| ingest/query/rules → MonoFS log-engine | gRPC `MonoFSRouter` | none on the log-engine client | insecure transport |
| query/rules/controller → MonoFS file store | gRPC router + node streaming | `x-monofs-internal` metadata token for node reads | insecure transport |
| query → Guardian | HTTP JSON | caller bearer token or no token | legacy then M2M paths |
| controller → Guardian | HTTP JSON | `X-Guardian-Internal-Token` | paginated M2M feeds |
| query → AWS pusher | HTTP JSON / PromQL proxy | bearer token from target/resolver | optional 401 refresh |
| query → remote Prom/Loki/Tempo/ES/BigQuery | HTTP | optional bearer / X-Scope-OrgID | backend-configured URLs |
| Grafana → query | Prometheus/Loki/Tempo HTTP | OIDC bearer or public metrics paths | compatibility contract |
| browser → query | HTTP + OIDC cookie | PKCE session | embedded SPA |
| query/rules/controller → OTLP collector | gRPC | optional insecure | Doctor self-telemetry |

## 7. Concurrency and resource management

| Area | Bounds |
|---|---|
| Ingest pipeline | `DOCTOR_PIPELINE_QUEUE_SIZE` (1000), `DOCTOR_PIPELINE_WORKERS` (4) |
| Ingest buffers | per-signal max bytes (4 MiB default), pending bytes (3× max), trace max records (4096) |
| OTLP request | gRPC 256 MiB, HTTP 10 MiB |
| Memory | hard-coded 858 MiB `GOMEMLIMIT` in ingest |
| PromQL | 4M samples and 100k series per query; 8M samples global; 1M engine `MaxSamples`; 30s default, 5m max caller timeout |
| PromQL discovery | 15-minute scan window, 10k records, 200k series |
| Broad range guard | 15 minutes for `{__name__=~".+"}` |
| Loki | 5k records for queries, 20k for metrics/volume, 200 series / 5k buckets volume output |
| Tempo pagination | 5k spans/batch, 8 batches/page, 200 AWS trace limit |
| TraceQL metrics | 20k spans, 500 series, 5k buckets, 500k samples |
| Proxy | 32 global inflight by default, 32 remote backends, 15s backend timeout (or caller deadline), 5-failure/30s breaker |
| Federation | 32 targets, 64 inflight, 8 concurrent target calls, 2 attempts, 5s per call, 10k client pool, 15m idle, 5 failures/30s breaker |
| Guardian topology | one goroutine per partition, **unbounded** |
| Trace discovery caches | TTL 30s default; maps have no global size cap/eviction sweep |
| Dashboards/rules | CAS-protected; no request concurrency limiter |

## 8. Configuration reference

### Ingest (`internal/config.LoadIngest`)

| Variable | Default | Notes |
|---|---|---|
| `DOCTOR_DEFAULT_TENANT` | `default` | fallback tenant and default allowlist |
| `DOCTOR_TENANT_HEADER` | `X-Doctor-Tenant` | HTTP/gRPC tenant header |
| `DOCTOR_INGEST_AUTH_TOKEN` | — | required bearer token for OTLP gRPC/HTTP |
| `DOCTOR_INGEST_ALLOWED_TENANTS` | default tenant | CSV; limits accepted tenant headers |
| `DOCTOR_MEMORY_LIMIT_BYTES` | `899678208` (858 MiB) | soft `GOMEMLIMIT` |
| `DOCTOR_TLS_CERT_FILE` / `DOCTOR_TLS_KEY_FILE` | — | HTTPS for OTLP/HTTP |
| `DOCTOR_GRPC_TLS_CERT_FILE` / `DOCTOR_GRPC_TLS_KEY_FILE` | — | TLS for OTLP gRPC |
| `DOCTOR_GRPC_ADDR` | `:4317` | OTLP gRPC |
| `DOCTOR_HTTP_ADDR` | `:4318` | OTLP HTTP |
| `DOCTOR_DEBUG_ADDR` | `127.0.0.1:6060` | pprof; empty disables |
| `DOCTOR_MONOFS_LOGENGINE_ADDR` | — | required |
| `DOCTOR_TRACE_BUFFER_ENABLED` | `true` | |
| `DOCTOR_TRACE_BUFFER_WAL_DIR` | `.doctor/trace-buffer` | |
| `DOCTOR_TRACE_BUFFER_FLUSH_INTERVAL` | `5s` | |
| `DOCTOR_TRACE_BUFFER_MAX_RECORDS` | `4096` | |
| `DOCTOR_TRACE_BUFFER_MAX_BYTES` | `4194304` | |
| `DOCTOR_TRACE_MAX_PENDING_BYTES` | `0` → 3× max | |
| `DOCTOR_LOG_BUFFER_ENABLED` | `true` | |
| `DOCTOR_LOG_BUFFER_WAL_DIR` | `.doctor/log-buffer` | |
| `DOCTOR_LOG_BUFFER_FLUSH_INTERVAL` | `5s` | |
| `DOCTOR_LOG_BUFFER_MAX_BYTES` | `4194304` | |
| `DOCTOR_LOG_MAX_PENDING_BYTES` | `0` → 3× max | |
| `DOCTOR_METRIC_BUFFER_ENABLED` | `true` | |
| `DOCTOR_METRIC_BUFFER_WAL_DIR` | `.doctor/metric-buffer` | |
| `DOCTOR_METRIC_BUFFER_FLUSH_INTERVAL` | `5s` | |
| `DOCTOR_METRIC_BUFFER_MAX_BYTES` | `4194304` | |
| `DOCTOR_METRIC_MAX_PENDING_BYTES` | `0` → 3× max | |
| `DOCTOR_PRIVACY_HMAC_KEY` | — | required |
| `DOCTOR_WAL_ENABLED` | `false` | read but unused |
| `DOCTOR_WAL_DIR` | `.doctor/wal` | read but unused |
| `DOCTOR_PIPELINE_ENABLED` | `true` | takes precedence over buffers |
| `DOCTOR_PIPELINE_QUEUE_SIZE` | `1000` | |
| `DOCTOR_PIPELINE_WORKERS` | `4` | |
| `DOCTOR_COST_*` | `0` | read but unused |

### Query (`internal/config.LoadQuery`)

| Variable | Default | Notes |
|---|---|---|
| `DOCTOR_DEFAULT_TENANT` | `default` | |
| `DOCTOR_TENANT_HEADER` | `X-Doctor-Tenant` | |
| `DOCTOR_QUERY_HTTP_ADDR` / `DOCTOR_HTTP_ADDR` | `:18080` | |
| `DOCTOR_TLS_CERT_FILE` / `DOCTOR_TLS_KEY_FILE` | — | HTTPS |
| `DOCTOR_GUARDIAN_UI_URL` | — | UI link and API base fallback |
| `DOCTOR_GUARDIAN_API_URL` | falls back to UI URL | server-side Guardian API |
| `DOCTOR_DEFAULT_PARTITION` | — | UI default |
| `DOCTOR_MONOFS_LOGENGINE_ADDR` | — | required |
| `DOCTOR_RULES_URL` | — | enables UI proxy |
| `DOCTOR_RULES_API_TOKEN` | — | forwarded to the rules API |
| `DOCTOR_AWS_ENABLED` | `true` | |
| `DOCTOR_AWS_BACKEND_URL` / `_TOKEN` | — | legacy single pusher |
| `DOCTOR_FEDERATION_ENABLED` | `false` | |
| `DOCTOR_FEDERATION_CATALOG_FILE` | — | static fallback |
| `DOCTOR_FEDERATION_CATALOG_JSON` | — | inline snapshot (highest priority) |
| `DOCTOR_FEDERATION_SNAPSHOT_STORE` | `file` | `file` or `monofs` |
| `DOCTOR_FEDERATION_SNAPSHOT_ROOT` | `/doctor/v1/federation` | |
| `DOCTOR_FEDERATION_SNAPSHOT_TENANT` | `default` | |
| `DOCTOR_FEDERATION_SNAPSHOT_SHARD_TARGETS` | `500` | |
| `DOCTOR_FEDERATION_TOKEN_RESOLVER` | `auto` | `auto`, `env`, `guardian`, `bundle`, `none` |
| `DOCTOR_FEDERATION_TOKEN_CACHE_TTL` | `5m` | |
| `DOCTOR_FEDERATION_TOKEN_BUNDLE_ROOT` | `/doctor/v1/federation/tokens` | |
| `DOCTOR_FEDERATION_TOKEN_SHARD_COUNT` | `256` | must match publisher |
| `DOCTOR_FEDERATION_TOKEN_KEY` | — | AES-256-GCM key for token shards |
| `DOCTOR_FEDERATION_RELOAD_INTERVAL` | `30s` | `0` disables reload |
| `DOCTOR_FEDERATION_STALE_AFTER` | `0` | disables stale metric |
| `DOCTOR_FEDERATION_MAX_FANOUT` | `32` | |
| `DOCTOR_FEDERATION_MAX_INFLIGHT` | `64` | |
| `DOCTOR_FEDERATION_MAX_CONCURRENT_TARGETS` | `8` | |
| `DOCTOR_FEDERATION_MAX_ATTEMPTS` | `2` | |
| `DOCTOR_FEDERATION_CLIENT_POOL_MAX` | `10000` | |
| `DOCTOR_FEDERATION_CLIENT_IDLE_TTL` | `15m` | |
| `DOCTOR_FEDERATION_BREAKER_THRESHOLD` | `5` | |
| `DOCTOR_FEDERATION_BREAKER_COOLDOWN` | `30s` | |
| `DOCTOR_FEDERATION_METADATA_TTL` | `1h` | |
| `DOCTOR_FEDERATION_TIMEOUT` | `5s` | |
| `DOCTOR_TRACE_DISCOVERY_CACHE_TTL` | `30s` | |
| `DOCTOR_TRACE_DISCOVERY_WINDOW` | `1h` | |
| `DOCTOR_PROXY_ENABLED` | `true` | |
| `DOCTOR_PROXY_BACKENDS_FILE` | — | static registry |
| `DOCTOR_PROXY_REGISTRY_INTERVAL` | `30s` | |
| `DOCTOR_PROXY_MAX_INFLIGHT` | `32` | |
| `DOCTOR_PROXY_BACKEND_TIMEOUT` | `15s` | |
| `DOCTOR_GUARDIAN_DISCOVERY_ENABLED` | `false` | |
| `DOCTOR_GUARDIAN_DISCOVERY_INTERVAL` | `30s` | |
| `DOCTOR_GUARDIAN_TOKEN` | — | discovery/token resolver |
| `DOCTOR_AUTH_REQUIRE` | `false` | |
| `DOCTOR_AUTH_EXEMPT_PATHS` | — | CSV; overrides defaults |
| `DOCTOR_OIDC_ISSUER` / `_AUDIENCE` | — | both needed for OIDC |
| `DOCTOR_OIDC_JWKS_URL` | — | discovery if empty |
| `DOCTOR_OIDC_CLIENT_ID` / `_SECRET` | — | browser login |
| `DOCTOR_OIDC_REDIRECT_URL` | — | browser login |
| `DOCTOR_OIDC_END_SESSION_URL` / `_AUTH_URL` | — | optional |
| `DOCTOR_OIDC_REQUIRE_LOGIN` | `false` | |
| `DOCTOR_OIDC_SCOPES` | — | CSV |
| `DOCTOR_SESSION_DIR` | `.doctor/sessions` | |
| `DOCTOR_BREAK_GLASS_TOKEN` | — | static admin token |
| `DOCTOR_ACCESS_RESOLVER` | `owners` | `owners`, `static`, `grants`, `chain`, `allow-all` |
| `DOCTOR_ACCESS_MODE` | `observe` | `observe` or `enforce` |
| `DOCTOR_ACCESS_ENFORCE_LOGS` / `_TRACES` | `false` | used outside `enforce` |
| `DOCTOR_ACCESS_TEAM_MAPPING` | — | file or inline team map |
| `DOCTOR_ACCESS_GRANTS_PATH` | — | JSON grant store |
| `DOCTOR_ACCESS_OWNERS_ROOT` | `/doctor/v1/endpoints` | |
| `DOCTOR_ACCESS_CACHE_TTL` | `60s` | |
| `DOCTOR_ACCESS_STATIC_ALLOW` / `_ALLOW_GROUPS` / `_DENY` | — | CSV |
| `DOCTOR_ACCESS_TRUST_TENANT_HEADER` | `false` | |
| `DOCTOR_ACCESS_POLICY_URL` / `_FILE` | — | enables repo policy gate |
| `DOCTOR_ACCESS_POLICY_TOKEN` | — | policy fetch bearer |
| `DOCTOR_ACCESS_POLICY_INTERVAL` | `30s` | |
| `DOCTOR_MONOFS_TOKEN` | — | file store; dashboards, owners, federation bundle |
| `DOCTOR_MONOFS_CLIENT_ID` | `doctor-query` | |
| `DOCTOR_DASHBOARDS_STORE_ROOT` | `/doctor/v1/dashboards` | |

### Rules (`internal/config.LoadRules`)

| Variable | Default |
|---|---|
| `DOCTOR_RULES_ENABLED` | `true` |
| `DOCTOR_RULES_HTTP_ADDR` | `:18081` |
| `DOCTOR_TLS_CERT_FILE` / `DOCTOR_TLS_KEY_FILE` | — |
| `DOCTOR_RULES_API_TOKEN` | — (required by the binary) |
| `DOCTOR_RULES_QUERY_URL` | — |
| `DOCTOR_RULES_QUERY_TOKEN` | — |
| `DOCTOR_RULES_WORKERS` | `8` |
| `DOCTOR_RULES_EVAL_TIMEOUT` | `30s` |
| `DOCTOR_RULES_LEASE_TTL` | `30s` |
| `DOCTOR_RULES_LEASE_RENEW` | `10s` |
| `DOCTOR_RULES_RULER_ID` | hostname |
| `DOCTOR_RULES_STORE_ROOT` | `/doctor/v1/rules` |
| `DOCTOR_RULES_POLL_INTERVAL` | `30s` |
| `DOCTOR_MONOFS_LOGENGINE_ADDR` | — |
| `DOCTOR_MONOFS_TOKEN` | — |
| `DOCTOR_MONOFS_CLIENT_ID` | `doctor-rules` |

### Doctor telemetry (`internal/telemetry.LoadConfig`)

| Variable | Default |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `DOCTOR_OTEL_ENDPOINT` | disabled when empty |
| `OTEL_SERVICE_NAME` / `DOCTOR_OTEL_SERVICE_NAME` | `doctor.invalid.unconfigured` |
| `OTEL_EXPORTER_OTLP_INSECURE` / `DOCTOR_OTEL_INSECURE` | inferred from endpoint scheme (`https` → false) |
| `DOCTOR_OTEL_METRIC_INTERVAL` | `15s` |
| `HOSTNAME` | instance ID |
| `DOCTOR_PARTITION` | resource partition |
