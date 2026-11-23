# Components and interfaces

This document explains what each package and binary owns.

## 1. Binaries

### `cmd/doctor-ingest`

**Purpose:** receive OTLP and write normalized records to the MonoFS log-engine.

Startup sequence (`cmd/doctor-ingest/main.go`):

1. load `config.IngestConfig`, set `GOMEMLIMIT` from `DOCTOR_MEMORY_LIMIT_BYTES` (default 858 MiB), and start an optional pprof server;
2. initialize OTel telemetry and the `doctor.component.up` heartbeat;
3. build `privacy.Engine`, `MonoFSWriter` (requires `DOCTOR_MONOFS_LOGENGINE_ADDR`), optional trace/log/metric WAL buffers, and optional pipeline (which wraps `BufferedWriter` when buffers are enabled);
4. build `ingest.Service`, optional `quota.Engine` (`DOCTOR_QUOTA_ENABLED`), and bearer/mTLS auth (`DOCTOR_INGEST_TOKENS_FILE` or `DOCTOR_INGEST_AUTH_TOKEN`);
5. register the three OTLP gRPC services + gRPC health check plus the HTTP collector;
6. serve gRPC and HTTP concurrently; shut down on SIGINT/SIGTERM.

Notable choices:

- gRPC max receive/send size is 256 MiB (`grpc.MaxRecvMsgSize` / `MaxSendMsgSize`).
- HTTP request bodies are limited to 10 MiB per OTLP request.
- When both `DOCTOR_PIPELINE_ENABLED` and `DOCTOR_*_BUFFER_ENABLED` are true (the default), the pipeline stages records in the WAL buffers via `BufferedWriter` and their background loops flush to MonoFS.
- `WriteGuardianEvents` on the MonoFS backend returns `ErrGuardianEventsUnsupported` (HTTP 501).

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
   manager, metrics, and optional reloader; else optionally build a legacy single AWS/GCP client;
8. build OIDC/break-glass auth, browser login, and the legacy access resolver/gate;
9. build the proxy router with local, AWS, and GCP backends; `DOCTOR_PROXY_ENABLED` only gates
   external registry discovery (static file and/or Guardian);
10. build the middleware chain and serve HTTP.

Middleware order (outermost to innermost):

```
WebAuthenticator.Handler
  -> TenantTrust.Wrap
    -> TenantGuard.Wrap
      -> auth.Middleware.Wrap
        -> PolicyGate.Middleware
          -> quota.Engine.Middleware
            -> query.Service.Handler (which itself wraps HTTP metrics, security headers,
               body limit, request ID, mutation-origin check, audit logging, warning
               headers, and Guardian token)
```

### `cmd/doctor-sentinel`

**Purpose:** alert evaluation, source polling (Guardian/AWS/GCP), routing, notification delivery, and incident/postmortem management.

Startup sequence (`cmd/doctor-sentinel/main.go`):

1. load `config.SentinelConfig` (`DOCTOR_SENTINEL_ENABLED`);
2. connect MonoFS file store when `DOCTOR_MONOFS_LOGENGINE_ADDR` and `DOCTOR_MONOFS_TOKEN` are set, otherwise fall back to `alerting.NewMemoryStore()`;
3. construct `alerting.Store` and `alerting.Service`, start subspace expiry loop;
4. if `DOCTOR_SENTINEL_QUERY_URL` is set, start `alerting.RuleScheduler` behind a sharded `LeaseManager`;
5. if `DOCTOR_SENTINEL_DELIVERY_ENABLED=true`, start `alerting.Dispatcher` (with `delivery.Registry` and optional FCB backend);
6. if `DOCTOR_SENTINEL_INCIDENTS_ENABLED=true`, construct `incident.Store`, `incident.Service`, `incident.API`, and start the incident reconciliation loop;
7. start enabled source pollers (`AWSSource`, `GCPSource`, `GuardianSource`);
8. serve HTTP on `DOCTOR_SENTINEL_HTTP_ADDR` (default `:18082`).

### `cmd/doctor-loadgen`

**Purpose:** CLI tool for generating OTLP gRPC/HTTP telemetry and query load against Doctor endpoints.

### `cmd/doctor-rules`

**Purpose:** schedule recording rules and write derived metrics.

Startup sequence:

1. load `RulesConfig`; require MonoFS log-engine and file token;
2. connect `GRPCClient` (file store) and `LogEngineClient` (metric writes);
3. build `ruler.Store`, `Evaluator` using an HTTP querier to `DOCTOR_RULES_QUERY_URL`, `Scheduler`,
   and `LeaseManager`;
4. serve the rules API and run the lease/scheduler loop.

The rules API requires a bearer credential; a static bearer middleware protects every route except
`/healthz` and `/metrics`. `DOCTOR_RULES_TOKENS_FILE` binds tokens to partitions and roles
(`admin`/`maintainer`/`viewer`), and `ruler.API.Authorizer` is `ScopedRulesAuthorizer`.
`DOCTOR_RULES_API_TOKEN` remains a legacy admin credential. Proxied requests from doctor-query carry
a signed caller identity (`DOCTOR_INTERNAL_IDENTITY_SECRET`).

### Federation CLIs

| Binary | Behavior |
|---|---|
| `doctor-federation-build` | loads YAML/JSON `federation.BuildInput`, calls `BuildCatalog`, prints or writes a snapshot JSON |
| `doctor-federation-publish` | loads a snapshot file, writes it through `MonoFSSnapshotStore.Save`, optionally verifies by reading it back |
| `doctor-federation-controller` | connects to Guardian + MonoFS, acquires a leader lease, builds metadata and catalog, publishes token shards and snapshot on an interval |

The controller and publish CLIs accept `--monofs-token` and `--guardian-token` flags for
compatibility, but read `DOCTOR_MONOFS_TOKEN` and `DOCTOR_GUARDIAN_TOKEN` by default. The controller
reads the router only from `DOCTOR_MONOFS_ROUTER` (or `--monofs-router`); `doctor-federation-publish`
also accepts `DOCTOR_MONOFS_ROUTER_ADDR`. Prefer the environment variables: flags are visible in the
process table.

## 2. Configuration and shared primitives

### `internal/config`

All environment parsing. Loaders:

- `LoadIngest` validates the privacy HMAC key and returns ingest settings.
- `LoadQuery` returns query/auth/access/proxy/federation settings with defaults.
- `LoadRules` returns rule-engine settings with a hostname-derived ruler ID.
- `LoadSentinel` returns alerting/delivery/incident settings.
- `LoadQuota` returns quota settings shared by ingest, query, and rules.

Helper semantics worth knowing:

- `envString`/`envBool`/`envInt64` silently fall back on parse errors.
- `envDuration` accepts a Go duration or a bare integer, which it interprets as **hours**.
- `envCSV` trims values and drops empty entries.

`service` parameters are accepted by every loader but unused. Some runtime knobs are read in their
own packages rather than `LoadQuery`: display bundle (`internal/display`), audit reader
(`internal/query/audit_clickhouse.go`), PromQL budgets, query cache TTL, and default timeout.

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
- email/user/account/client/IP keys and IPv4/email substrings are replaced with deterministic,
  versioned `tok_v2_<keyid>_<32hex>` HMAC-SHA256 tokens (128-bit tag);
- free text additionally tokenizes Luhn-valid PANs, SSNs, phone numbers, IBANs (mod-97), and ABA
  routing numbers (checksum), and redacts CVV/date-of-birth patterns;
- `Tokenize` mints with the active key (`DOCTOR_PRIVACY_KEY_ID`, default `k1`);
  `DOCTOR_PRIVACY_KEYS_JSON` holds the key id → secret map for rotation and historical
  verification; `DOCTOR_PRIVACY_HMAC_KEY` is the single-key fallback.

The identity allowlist is also a bypass: any secret placed in an identity key is preserved.
There is no secret-manager integration or automatic rotation schedule.

### `internal/wal`

One shared generic WAL (`commitLog[T]`) behind three per-signal wrappers, plus one unused legacy WAL:

- `TraceBatchLog`, `LogBatchLog`, `MetricBatchLog`: `Append`, `ReadAll`, `Iterate`, `Retire`,
  `Replace`, `Count`, length-prefixed little-endian `uint32` + JSON frames, file `*.log`;
- appends are group-committed: writers queue completion channels and one goroutine fsyncs the whole
  pending generation;
- `Retire` advances a durable `<file>.checkpoint` offset instead of rewriting the WAL; when more
  than 64 MiB of flushed bytes accumulate, the live tail is compacted into a fresh file;
- replay starts at the checkpoint, ignores a checkpoint past EOF, skips undecodable JSON frames,
  and stops cleanly on a truncated final frame;
- `Replace` remains the full-rewrite path (temp file + fsync + atomic rename);
- `WAL`: per-entry `.pending` files with `Stage`, `Commit`, `Pending`, `Count`; not used by any
  service.

There is no frame checksum or format version; corruption is handled by skipping bad JSON frames and
treating a short read as end-of-log.

### `internal/store`

- `store.Store` defines a versioned file-store interface plus logical-path normalization, content
  hash, version ID, and batch revision ID helpers.
- `store/monofs` contains the two gRPC clients used in production and shared `tls.go` (`DOCTOR_MONOFS_TLS_*`):
  - `GRPCClient`: file CAS operations (`UpsertPaths`, `DeletePaths`, `ListVersions`,
    `GetVersion`, `ReadFile`, `ListDir`), router registration, node discovery, heartbeat, and
    per-node reads. Uses `TLSConfigFromEnv()`, sends the service token as per-RPC
    `authorization: Bearer` metadata, and keeps the `x-monofs-internal` metadata token for node reads.
  - `LogEngineClient`: log/metric/trace ingest and query through `MonoFSRouterClient`; also streams
    metric series metadata and trace discovery, and executes DuckDB/ClickHouse SQL
    (`QueryDuckDBSQL`). Uses `TLSConfigFromEnv()` and sends the MonoFS token as per-RPC
    `authorization: Bearer` metadata (`NewLogEngineClientWithToken`).
- `store/aws`: HTTP client for `guardian-pusher-aws` read API, with token-source abstraction,
  one-shot 401 token refresh, cached `/healthz`, and helpers for CloudWatch metrics/logs, X-Ray
  services/traces, and PromQL proxy endpoints.
- `store/gcp`: HTTP client for `guardian-pusher-gcp` read API (Cloud Monitoring metrics, Cloud Logging, Cloud Trace, resources).

## 3. Ingest components

### `internal/otlp`

Flattening and identity normalization:

- `FlattenTraces`: resource attributes → `service`/`partition`/`intent`/`asset`; span attributes,
  events, status, timestamps are sanitized.
- `FlattenLogs`: resource/scope/log records; body is sanitized as text.
- `FlattenMetrics`: all OTLP metric types are expanded into point records:
  - gauge/sum → one point;
  - histogram → `<name>_bucket` per bucket, `_count`, `_sum`;
  - exponential histogram → `<name>_bucket` with computed `le` boundaries (capped at 160), `_count`, `_sum`;
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
- `errors.go`: `ErrBufferFull` shared by buffers and pipeline mapping.

Signal routing detail:

```text
ingestTraces/Logs/Metrics
  if pipeline != nil   -> pipeline.Submit*  (default; wraps BufferedWriter when buffers are enabled)
  else if buffer != nil -> buffer.Append
  else                  -> writer.Write*
```

## 4. Query components

### `internal/auth`

- `Config` and `NewVerifier`: OIDC verifier (issuer+audience) followed by a break-glass static token.
- `NewWebAuthenticator`: browser Authorization Code + PKCE flow with persistent session store,
  cookie `doctor_session`, optional require-login, RP-initiated logout.
- `Middleware`: wraps the MonoFS `authz.Authenticator`, attaches identity, and rejects anonymous
  callers outside exempt paths when `require` is set.
- `DefaultExemptPaths`: `/healthz`, `/ready`, `/api/echo`, `/metrics`, `/api/v1/status/buildinfo`. Telemetry APIs (including `/api/v1/*`) require authentication when a verifier is configured.
- `TenantGuard`: restricts query requests to `DOCTOR_QUERY_ALLOWED_TENANTS` or forces
  `DOCTOR_QUERY_SINGLE_TENANT`; with neither configured it fails closed to
  `DOCTOR_DEFAULT_TENANT`.
- `InternalIdentity`: HMAC-signed `X-Doctor-Identity` header forwarded from `doctor-query` to `doctor-rules`.
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
| `gcp_federation.go` | GCP backend status, metric/service/log-name discovery, trace/query wrappers |
| `sql.go` | `POST /v1/sql/query` DuckDB/ClickHouse console with partition scope |
| `policy_backend.go` | `LogEngineBackend` decorator applying `PolicyGate` to logs/traces; AWS upstream filters |
| `labels.go` | canonical label vocabulary and projection shared by all signals |
| `identity_filter.go` | partition/intent/asset request filters and LogQL selector rendering |
| `dashboards_handlers.go` | dashboard CRUD, versions, import/export |
| `audit.go` / `audit_clickhouse.go` | `X-Request-ID` propagation, mutation/read audit logging, `GET /v1/audit/events`, `GET /v1/audit/status`, ClickHouse `audit.events` reader |
| `guardian_probes.go` | Guardian `/api/probes` proxy + pusher summary + partition filtering |
| `warnings_middleware.go` | per-request warning collector and `X-Doctor-*` response headers |
| `ui.go` | `//go:embed ui/dist`, SPA fallback, API-path 404 guard |
| `display.go` | `/v1/display-profiles`, `/v1/display-profiles/effective`, query translation, JSON key mapping for `/v1/`, `/loki/`, `/api/`, `/api/v2/` |

### `internal/display`

Loads the Guardian-compiled display bundle from `DOCTOR_DISPLAY_BUNDLE_URL` or
`DOCTOR_DISPLAY_BUNDLE_FILE` (token from `DOCTOR_DISPLAY_BUNDLE_TOKEN`, falling back to
`DOCTOR_ACCESS_POLICY_TOKEN`; refresh interval `DOCTOR_DISPLAY_REFRESH_INTERVAL`, default `30s`).
It resolves a profile for the caller's subjects (user/email plus groups/teams), applies
`X-Strata-Display-Profile` or `displayProfile`, translates repo-assigned display query keys to
canonical keys, and maps JSON response keys. On bundle fetch failure it keeps the last good bundle.
The mapping is presentation-only: values, query execution, ACLs, and audit rows stay canonical.
`X-Strata-Display-Keep-Canonical: 1` keeps canonical keys alongside display keys, and responses
carry `X-Strata-Display-Profile` / `X-Strata-Display-Version`.

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
  `upstream_bigquery.go`: HTTP adapters. `upstream_prom.go` reads Prometheus read-only metadata
  (`/api/v1/series`, `/labels`, `/label/<name>/values`) with GET, because Prometheus rejects POST on
  the label endpoints with 405.
- `gcp.go`: GCP read-API adapter. Cloud Logging trace references (`projects/<project>/traces/<id>`,
  `traceId`/`trace_id`, W3C `traceparent`, `logging.googleapis.com/trace`) and span references
  (`spanId`/`span_id`, `logging.googleapis.com/spanId`) are normalized to the bare trace/span IDs
  carried on the returned log records.
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
- eight built-in presets: fleet, service RED, synthetics, MonoFS storage, ClickHouse health,
  Doctor self, AWS federation, GCP monitoring/trace federation;
- stored dashboards are YAML at `/doctor/v1/dashboards/<tenant>/<id>.yaml`; built-ins are read-only
  and win over stored IDs;
- CAS via expected version; `List`/`Get` populate version metadata from `ListVersions`.

### `internal/ruler`

Recording rules:

- `rule.go`: `File`, `Group`, `Rule`, merge strategies (`sum`, `max`, `min`, `last`, `keep`),
  duration parsing, validation.
- `store.go`: rule YAML in MonoFS at `/doctor/v1/rules/<partition>/<group>.yaml`; `MemoryStore` for
  local/test; CAS.
- `scheduler.go`: poll/reload groups, min-heap of next evaluations, jitter in the first 10% of the
  interval, bounded worker pool.
- `evaluator.go`: PromQL instant query through `Querier`, per-shard merge, skip partial sums, write
  derived `MetricPointRecord`s.
- `querier.go`: HTTP client to doctor-query `/api/v1/query`; honors `X-Doctor-Partial`.
- `writer.go`: writes derived metrics through `LogEngineClient`.
- `lease.go`: CAS leader lease at `/doctor/v1/rules/.lease/leader.json`.
- `api.go`: `GET /v1/rules`, `GET /v1/rules/status`, `POST /v1/rules/evaluate`,
  `POST /v1/rules/backfill`, `GET/PUT/DELETE /v1/rules/{partition}/{group}`. Mutations use
  `ScopedRulesAuthorizer` and the per-partition token groups.

## 5. Federation components

| File | Responsibility |
|---|---|
| `types.go` | `Target`, `Snapshot`, `Query`, `Predicates`, `Plan`, `ScopeError` |
| `catalog.go` | immutable-ish catalog with account/region/partition/log-group/service/metric indexes; validation. Trace IDs are intentionally unindexed and use a bounded fallback. |
| `builder.go` | joins Guardian partitions + pushers + AWS discovery metadata into provider/account/region targets |
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
/doctor/v1/rules/<partition>/<group>.yaml
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
| OTLP clients → ingest | gRPC / HTTP protobuf or JSON | bearer (`DOCTOR_INGEST_TOKENS_FILE` / `DOCTOR_INGEST_AUTH_TOKEN`), optional mTLS | tenant from header/metadata validated against allowlist; 256 MiB gRPC, 10 MiB HTTP |
| ingest/query/rules → MonoFS log-engine | gRPC `MonoFSRouter` | optional token (`DOCTOR_MONOFS_TOKEN`) | TLS/mTLS via `DOCTOR_MONOFS_TLS_*`; TLS required by default when the token is set, plaintext only without a token/TLS config or with an explicit opt-out |
| query/rules/controller/sentinel → MonoFS file store | gRPC router + node streaming | per-RPC `authorization: Bearer` token plus `x-monofs-internal` metadata for node reads | TLS/mTLS via `DOCTOR_MONOFS_TLS_*`; TLS required by default when the token is set, plaintext only without a token/TLS config or with an explicit opt-out |
| query → Guardian | HTTP JSON | caller bearer token or no token | legacy then M2M paths |
| controller → Guardian | HTTP JSON | `X-Guardian-Internal-Token` | paginated M2M feeds plus `includeTokens=1` |
| query → AWS/GCP pusher | HTTP JSON / PromQL proxy | bearer token from target/resolver | AWS client refreshes once after 401 |
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
| Memory | `DOCTOR_MEMORY_LIMIT_BYTES` (default 858 MiB `GOMEMLIMIT` in ingest) |
| PromQL | 4M samples and 100k series per query; 8M samples global; 1M engine `MaxSamples`; 20s default, 5m max caller timeout |
| PromQL discovery | 15-minute scan window, 10k records, 200k series |
| Broad range guard | 15 minutes for `{__name__=~".+"}` |
| Loki | 500 records per query_range/volume scan, 5k for instant metric evaluation, 200 series / 5k buckets volume output |
| Tempo pagination | 2k spans/batch, 8 batches/page, 200 AWS trace limit |
| TraceQL metrics | 20k spans, 500 series, 5k buckets, 500k samples |
| Proxy | 32 global inflight by default, 32 remote backends, 15s backend timeout (or caller deadline), 2-failure/15s breaker |
| Federation | 32 targets, 64 inflight, 8 concurrent target calls, 2 attempts, 5s per call, 10k client pool, 15m idle, 2 failures/15s breaker by default (`DOCTOR_FEDERATION_BREAKER_*`) |
| Guardian topology | capped at 8 concurrent partition fetches |
| Trace discovery caches | TTL 30s default; 10k-entry cap per map with expired-entry pruning |
| Dashboards/rules | CAS-protected; no request concurrency limiter |

## 8. Configuration reference

### Ingest (`internal/config.LoadIngest`)

| Variable | Default | Notes |
|---|---|---|
| `DOCTOR_DEFAULT_TENANT` | `default` | fallback tenant and default allowlist |
| `DOCTOR_TENANT_HEADER` | `X-Doctor-Tenant` | HTTP/gRPC tenant header |
| `DOCTOR_INGEST_AUTH_TOKEN` | — | legacy shared bearer token; scoped to allowed tenants |
| `DOCTOR_INGEST_TOKENS_FILE` | — | preferred per-collector token file with tenant scopes |
| `DOCTOR_INGEST_ALLOWED_TENANTS` | default tenant | CSV; deployment-wide tenant allowlist |
| `DOCTOR_MEMORY_LIMIT_BYTES` | `899678208` (858 MiB) | soft `GOMEMLIMIT` |
| `DOCTOR_TLS_CERT_FILE` / `DOCTOR_TLS_KEY_FILE` | — | HTTPS for OTLP/HTTP |
| `DOCTOR_GRPC_TLS_CERT_FILE` / `DOCTOR_GRPC_TLS_KEY_FILE` | — | TLS for OTLP gRPC |
| `DOCTOR_INGEST_MTLS_MODE` | `off` | shared collector mTLS mode (`off`, `optional`, `require`) |
| `DOCTOR_INGEST_MTLS_CA_FILE` | — | client CA bundle for optional/require |
| `DOCTOR_GRPC_MTLS_MODE` / `DOCTOR_GRPC_MTLS_CA_FILE` | shared values | gRPC overrides |
| `DOCTOR_HTTP_MTLS_MODE` / `DOCTOR_HTTP_MTLS_CA_FILE` | shared values | HTTP overrides |
| `DOCTOR_GRPC_ADDR` | `:4317` | OTLP gRPC |
| `DOCTOR_HTTP_ADDR` | `:4318` | OTLP HTTP |
| `DOCTOR_DEBUG_ADDR` | `127.0.0.1:6060` | pprof when `DOCTOR_DEBUG_ENABLED=true`; empty disables |
| `DOCTOR_DEBUG_ENABLED` | `false` | starts the pprof listener |
| `DOCTOR_MONOFS_LOGENGINE_ADDR` | — | required |
| `DOCTOR_MONOFS_ROUTER_ADDR` | — | file-store router (shared quota state, rules) |
| `DOCTOR_MONOFS_TOKEN` | — | log-engine bearer token and file-store token (shared quota state) |
| `DOCTOR_MONOFS_CLIENT_ID` | `doctor-ingest` | file-store client id |
| `DOCTOR_QUOTA_STATE_MODE` | `memory` | `memory`, `file`, or `shared` |
| `DOCTOR_QUOTA_STATE_DIR` | `.doctor/quota` | journal/snapshot directory (`file`) |
| `DOCTOR_QUOTA_STATE_REPLICA_ID` | hostname | journal identity |
| `DOCTOR_QUOTA_STATE_FLUSH_INTERVAL` | `10s` | group commit / snapshot cadence |
| `DOCTOR_QUOTA_STATE_SHARED_ROOT` | `/doctor/v1/quota/state` | MonoFS CAS prefix (`shared`) |
| `DOCTOR_QUOTA_STATE_RESERVE_BYTES` | `1048576` | block reservation size (`shared`) |
| `DOCTOR_QUOTA_STATE_FAIL_MODE` | `closed` | startup load behavior: `closed` or `open` |
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
| `DOCTOR_PRIVACY_HMAC_KEY` | — | required single-key fallback / active key |
| `DOCTOR_PRIVACY_KEY_ID` | `k1` | active key id used to mint `tok_v2_<keyid>_<32hex>` tokens |
| `DOCTOR_PRIVACY_KEYS_JSON` | — | JSON map of key id → secret for rotation and historical verification |
| `DOCTOR_WAL_ENABLED` | `false` | read but unused |
| `DOCTOR_WAL_DIR` | `.doctor/wal` | read but unused |
| `DOCTOR_PIPELINE_ENABLED` | `true` | pipeline stages records in the WAL buffers |
| `DOCTOR_INGEST_REQUIRE_DURABLE` | `false` | fail startup when the pipeline has no WAL buffers |
| `DOCTOR_PIPELINE_QUEUE_SIZE` | `1000` | |
| `DOCTOR_PIPELINE_WORKERS` | `4` | |

`DOCTOR_COST_*` was removed with the unused `CostController`; per-partition storage admission uses
`internal/quota` (`storage.max_bytes`).

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
| `DOCTOR_RULES_API_TOKEN` | — | forwarded service credential |
| `DOCTOR_INTERNAL_IDENTITY_SECRET` | — | HMAC secret for signed caller identity to doctor-rules |
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
| `DOCTOR_FEDERATION_DISCOVERY_LIMIT` | `10000` | caps metric descriptor discovery |
| `DOCTOR_FEDERATION_MAX_INFLIGHT` | `64` | |
| `DOCTOR_FEDERATION_MAX_CONCURRENT_TARGETS` | `8` | |
| `DOCTOR_FEDERATION_MAX_ATTEMPTS` | `2` | |
| `DOCTOR_FEDERATION_CLIENT_POOL_MAX` | `10000` | |
| `DOCTOR_FEDERATION_CLIENT_IDLE_TTL` | `15m` | |
| `DOCTOR_FEDERATION_BREAKER_THRESHOLD` | `2` | |
| `DOCTOR_FEDERATION_BREAKER_COOLDOWN` | `15s` | |
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
| `DOCTOR_AUTH_REQUIRE` | `true` | verifier-backed auth rejects anonymous callers |
| `DOCTOR_AUTH_EXEMPT_PATHS` | `/healthz,/ready,/api/echo,/metrics,/api/v1/status/buildinfo` | CSV; telemetry APIs are not exempt |
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
| `DOCTOR_ACCESS_MODE` | `enforce` | `enforce` or `observe` (explicit opt-out) |
| `DOCTOR_ACCESS_ENFORCE_LOGS` / `_TRACES` | `false` | used outside `enforce` |
| `DOCTOR_ACCESS_TEAM_MAPPING` | — | file or inline team map |
| `DOCTOR_ACCESS_POLICY_URL` / `_FILE` | — | enables the repo policy gate; compiled bundle covers logs/traces/partition scope |
| `DOCTOR_ACCESS_OWNERS_ROOT` | `/doctor/v1/endpoints` | |
| `DOCTOR_ACCESS_CACHE_TTL` | `60s` | |
| `DOCTOR_ACCESS_STATIC_ALLOW` / `_ALLOW_GROUPS` / `_DENY` | — | CSV |
| `DOCTOR_ACCESS_TRUST_TENANT_HEADER` | `false` | |
| `DOCTOR_ACCESS_POLICY_TOKEN` | — | policy fetch bearer |
| `DOCTOR_ACCESS_POLICY_INTERVAL` | `30s` | |
| `DOCTOR_AUDIT_CH_URL` | — | enables the ClickHouse `audit.events` reader for `/v1/audit/events` and `/v1/audit/status` |
| `DOCTOR_AUDIT_CH_DATABASE` | `audit` | ClickHouse database containing `events` |
| `DOCTOR_AUDIT_CH_USER` / `DOCTOR_AUDIT_CH_PASSWORD` | — | optional ClickHouse basic auth |
| `DOCTOR_DISPLAY_BUNDLE_URL` / `DOCTOR_DISPLAY_BUNDLE_FILE` | — | display-profile bundle source; mapping is disabled when both are empty |
| `DOCTOR_DISPLAY_BUNDLE_TOKEN` | falls back to `DOCTOR_ACCESS_POLICY_TOKEN` | bearer for the bundle URL |
| `DOCTOR_DISPLAY_REFRESH_INTERVAL` | `30s` | bundle refresh cadence |
| `DOCTOR_PROM_MAX_SAMPLES` | `4000000` | per-PromQL-query loaded sample budget |
| `DOCTOR_PROM_MAX_SERIES` | `100000` | per-PromQL-query loaded series budget |
| `DOCTOR_PROM_GLOBAL_MAX_SAMPLES` | `8000000` | samples held across all concurrent PromQL queries |
| `DOCTOR_QUERY_CACHE_TTL` | `15s` | local log/trace result-cache TTL; `0`/invalid disables caching |
| `DOCTOR_QUERY_DEFAULT_TIMEOUT` | `20s` | default Prometheus query timeout when `?timeout=` is absent; `?timeout=` is capped at 5m |
| `DOCTOR_INGEST_URL` | — | ingest base URL for `/v1/quota/usage` aggregation |
| `DOCTOR_INGEST_AUTH_TOKEN` | — | bearer for the ingest quota-usage call |
| `DOCTOR_MONOFS_TOKEN` | — | file store and log-engine bearer token (dashboards, owners, federation bundle, SQL console) |
| `DOCTOR_MONOFS_CLIENT_ID` | `doctor-query` | |
| `DOCTOR_DASHBOARDS_STORE_ROOT` | `/doctor/v1/dashboards` | |
| `DOCTOR_SENTINEL_URL` | — | enables `/v1/alerts*`, `/v1/incidents*`, `/api/v2/*` proxy |
| `DOCTOR_GCP_ENABLED` | `true` | |
| `DOCTOR_GCP_BACKEND_URL` / `_TOKEN` | — | legacy single GCP pusher |
| `DOCTOR_BACKEND_REQUIRE_HTTPS` / `_ALLOW_LOOPBACK` / `_ALLOW_PRIVATE` / `_ALLOWED_HOSTS` | `false` / `false` / `false` / — | `netguard` URL policy for proxy backends; the zero-value policy is HTTPS-only, and `AllowPrivate`/`AllowLoopback`/`AllowedHosts` are explicit opt-ins |
| `DOCTOR_QUERY_ALLOWED_TENANTS` / `DOCTOR_QUERY_SINGLE_TENANT` | — | tenant guard allowlist / forced single tenant |

### Rules (`internal/config.LoadRules`)

| Variable | Default |
|---|---|
| `DOCTOR_RULES_ENABLED` | `true` |
| `DOCTOR_RULES_HTTP_ADDR` | `:18081` |
| `DOCTOR_TLS_CERT_FILE` / `DOCTOR_TLS_KEY_FILE` | — |
| `DOCTOR_RULES_API_TOKEN` | — | legacy admin credential |
| `DOCTOR_RULES_TOKENS_FILE` | — | per-partition scoped token file |
| `DOCTOR_INTERNAL_IDENTITY_SECRET` | — | verifies signed identities from doctor-query |
| `DOCTOR_RULES_MAX_CATCHUP` | `0` | in-process missed-tick replay window |
| `DOCTOR_RULES_MAX_CATCHUP_TICKS` | `60` | per-cycle replay cap |
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

### Sentinel (`internal/config.LoadSentinel`)

See [alerts.md](../alerts.md#configuration) and [incidents.md](../incidents.md) for the `DOCTOR_SENTINEL_*` and `DOCTOR_FCB_*` variables.

### MonoFS TLS (`internal/store/monofs.TLSConfigFromEnv`)

| Variable | Default | Notes |
|---|---|---|
| `DOCTOR_MONOFS_TLS_CA` | — | CA bundle path |
| `DOCTOR_MONOFS_TLS_CERT` / `_KEY` | — | client certificate and key for mTLS |
| `DOCTOR_MONOFS_TLS_SERVER_NAME` | — | TLS server name override |
| `DOCTOR_MONOFS_TLS_REQUIRE` | `true` when `DOCTOR_MONOFS_TOKEN` is set, otherwise `false` | reject plaintext even without cert files; local development may set `false` explicitly |
| `DOCTOR_MONOFS_TLS_INSECURE` | `false` | explicit plaintext override |

### Doctor telemetry (`internal/telemetry.LoadConfig`)

| Variable | Default |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `DOCTOR_OTEL_ENDPOINT` | disabled when empty |
| `OTEL_SERVICE_NAME` / `DOCTOR_OTEL_SERVICE_NAME` | `doctor.invalid.unconfigured` |
| `OTEL_EXPORTER_OTLP_INSECURE` / `DOCTOR_OTEL_INSECURE` | inferred from endpoint scheme (`https` → false) |
| `DOCTOR_OTEL_METRIC_INTERVAL` | `15s` |
| `HOSTNAME` | instance ID |
| `DOCTOR_PARTITION` | resource partition |

### DuckDB SQL console

`internal/query/sql.go` serves `POST /v1/sql/query`. It validates the request,
resolves the caller's signal-specific partition scope through the access policy,
and calls the MonoFS router's `QueryDuckDBSQL` RPC. The router fans the query
out to every healthy storage node, intersects the requested partitions with its
own policy scope, and returns per-node results. The DuckDB view in MonoFS applies
the partition predicate before user SQL runs. The console is read-only and
rejects DDL/DML and external file access.
