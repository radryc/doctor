# Doctor Unified Proxy, SSO/OWNERS Access, and doctor-rules — Design

Status: **Draft for implementation**
Audience: Doctor maintainers, Strata platform engineers
Scope: `doctor-query`, new `doctor-rules`, deployment manifests, Grafana migration
Supersedes: Zipkin-compatible trace API, static `DOCTOR_AWS_*` federation

---

## 1. Summary

Doctor currently stores OTLP telemetry in MonoFS and exposes one
Prometheus/Loki/Zipkin dataspace plus a static AWS (CloudWatch/X-Ray)
federation. This design turns `doctor-query` into a **federation proxy** over
many observability backends ("sources" / "shards"), presented to users as a
single Prometheus, a single Loki, and a single Tempo dataspace:

- **Metrics** stay public. **Logs and traces** are filtered per endpoint.
- Access is decided by a **pluggable resolver**. The default resolver reads
  `.guardian/OWNERS` from MonoFS.
- Authentication is **SSO** (OIDC authorization code + PKCE, bearer for
  machines) reusing `github.com/radryc/monofs/pkg/authz`.
- A new **`doctor-rules`** binary evaluates PromQL recording rules per shard,
  merges results, and writes derived metrics back into MonoFS. Rules are stored
  in MonoFS and permissioned through OWNERS.
- Where two backends return the same series/stream/span, the merge adds a
  **`shard`** label so no data is lost and every record stays attributable.

This document is the implementation contract. It is intentionally phased; each
phase is independently shippable and testable.

---

## 2. Confirmed decisions

| # | Decision | Notes |
|---|---|---|
| D1 | Sources: PromQL (Prometheus HTTP), logs (Loki HTTP), traces (Tempo HTTP), Elasticsearch (ES\|QL), BigQuery (SQL), native MonoFS logengine, AWS CloudWatch/X-Ray | OTLP-as-source explicitly deferred |
| D2 | Overlap disambiguation label: `shard` | `source` keeps the existing provenance meaning (`doctor`, `aws`, `recording`, ...) |
| D3 | Access control ships **observe-first**, then flips to enforce per signal | Metrics never enforce |
| D4 | Backend discovery is driven by **Guardian** (`/api/pushers`, partition/asset endpoints) with a static file fallback | Polling with TTL, like Guardian's own `RegistryResolver` |
| D5 | Recording rules run in a separate **`doctor-rules`** binary with lease-based HA | Lease implemented with MonoFS CAS (`ExpectedVersionID: "absent"`), mirroring Guardian's claim idiom |
| D6 | Rule create/modify requires OWNERS `maintainers` (`ActionModify`) | Evaluated by the same access stack |
| D7 | Elasticsearch is queried through **ES\|QL** with LogQL→ES\|QL translation | Unsupported LogQL fails with a precise 400 |
| D8 | Zipkin trace API is **replaced** by a Tempo-compatible API | Grafana datasources/dashboards migrate `zipkin` → `tempo` |
| D9 | Recording rules evaluate **per shard and merge** | Merge strategy is declared per rule |

---

## 3. Current state (what exists today)

| Concern | Where | Notes |
|---|---|---|
| Query service | `internal/query/service.go` | Single `LogEngineBackend` field (`service.go:37`); `Handler()` route table at `service.go:411-446` |
| Prometheus API | `internal/query/prometheus_compat.go` | Local PromQL engine over `doctorPromQueryable`; AWS merge at `prometheus_compat.go:859,926` |
| Loki API | `internal/query/observability_compat.go` | `parseLokiPlan` + local stages + AWS append in `fetchLokiRecords` |
| Zipkin API | `internal/query/observability_compat.go:299-427` | `zipkinSpan` model; conversion `zipkinSpanFromRecord` (`:621`) |
| AWS federation | `internal/query/aws_federation.go`, `internal/store/aws/client.go` | The existing "second source" precedent: `source="aws"`, per-handler merge |
| Tenant routing | `internal/query/service.go:1085-1097` | `tenant` param → `X-Doctor-Tenant` header → default |
| MonoFS logengine client | `internal/store/monofs/logengine_client.go` | `LogEngineBackend`; tenant is label-based, ignored by MonoFS |
| MonoFS file client | `internal/store/monofs/grpc_client.go` | `ReadFile`/`ListDir` via node `Read`/`ReadDir`; `UpsertPaths` CAS via router |
| Authz primitives | `github.com/radryc/monofs/pkg/authz` | `OIDCVerifier`, `WebAuthenticator`, `Authenticator`, `GrantStore`, `OwnershipResolver`, `CompileGrants` |
| Recording rules | — | Nothing exists |
| Benchmarks | — | No benchmarks exist anywhere in Doctor |

Key structural gaps this design fills:

1. `Service.logEngine` is a single backend; there is no fan-out abstraction.
2. Query handlers hardcode per-signal merge points (Prometheus/Loki/Zipkin).
3. There is no query-plane authentication or authorization.
4. There is no rule evaluation or write-back path.

---

## 4. Target architecture

```
                          +-------------------------------------------------+
 Grafana / UI / API ----> |                  doctor-query                    |
                          |  auth: SSO middleware (monofs pkg/authz)         |
                          |  Identity -> access.Resolver (pluggable)         |
                          |  proxy.Router: registry -> fan-out -> merge      |
                          +---+---------+---------+---------+---------+-------+
                              |         |         |         |         |
                         monofs     Prometheus   Loki      Tempo     ES|QL /
                         logengine  HTTP shards  shards    shards    BigQuery
                              ^
                              | IngestMetrics (write-back)
                     +--------+----------------------------------------+
                     |                   doctor-rules                   |
                     |  rule store (MonoFS files, CAS)                   |
                     |  scheduler -> per-shard eval -> merge -> write    |
                     |  lease leader (MonoFS CAS)                        |
                     +---------------------------------------------------+
```

- Every signal has exactly one public API: Prometheus, Loki, Tempo.
- `proxy.Router` owns all fan-out and merge; handlers only parse/format.
- `access.Resolver` filters which sources are queried for logs/traces.
- `doctor-rules` is the only component that writes rule output.

### 4.1 Package layout

```
internal/proxy/
    backend.go          # Backend model, Signal, capabilities
    registry.go         # Registry interface, static file loader, merge/validation
    discovery_guardian.go# Guardian pusher/partition discovery
    router.go           # fan-out, concurrency, partial results
    merge_metrics.go    # series merge + shard labeling
    merge_logs.go       # stream merge + dedup + shard labeling
    merge_traces.go     # span merge + dedup + shard labeling
    upstream.go         # MetricsUpstream/LogsUpstream/TracesUpstream interfaces
    upstream_prom.go    # Prometheus HTTP API adapter
    upstream_loki.go    # Loki HTTP API adapter
    upstream_tempo.go   # Tempo HTTP API adapter
    upstream_es.go      # Elasticsearch ES|QL adapter
    upstream_bigquery.go# BigQuery SQL adapter (store mode)
    upstream_monofs.go  # native LogEngineBackend adapter
    upstream_aws.go     # existing awsstore adapter
    translate/
        logql_esql.go   # LogQL -> ES|QL
        logql_bigquery.go # LogQL -> BigQuery SQL (store-mode pushdown)
        traceql.go      # TraceQL subset -> native span filters
    *_test.go, bench_test.go

internal/access/
    resolver.go         # Resolver interface, Endpoint, Decision
    owners.go           # OWNERS-backed resolver (default)
    grants.go           # GrantStore-backed resolver
    static.go           # allow/deny lists
    chain.go            # ordered composition + per-endpoint override
    cache.go            # (path,version) cache, singleflight, LRU, inverted index
    monofs_source.go    # OwnersSource over MonoFS file reads
    *_test.go, bench_test.go

internal/auth/
    config.go           # env loading
    verifier.go         # verifier chain construction
    middleware.go       # HTTP middleware + context identity
    webauth.go          # browser login/callback/logout wiring
    *_test.go

internal/tempo/          # public Tempo-compatible API types + handlers
    api.go, handler.go, convert.go, *_test.go

internal/ruler/
    rule.go             # rule/group model, YAML schema, validation
    store.go            # MonoFS-backed rule store (list/get/put/delete, CAS)
    scheduler.go        # min-heap group scheduler
    evaluator.go        # per-shard eval + merge + write-back
    lease.go            # leader lease via MonoFS CAS
    api.go              # CRUD HTTP API
    *_test.go, bench_test.go

cmd/doctor-rules/main.go
```

---

## 5. Backend model and registry

### 5.1 Model

```go
type Signal string // "metrics" | "logs" | "traces"

type Mode string
const (
    ModeProxy Mode = "proxy" // forward the query language to the backend
    ModeStore Mode = "store" // fetch raw records; Doctor evaluates locally
)

type Backend struct {
    Name       string            // stable id; becomes source/shard label
    Kind       string            // prometheus|loki|tempo|elasticsearch|bigquery|monofs|aws
    Mode       Mode              // per-signal modes are allowed (see Modes)
    BaseURL    string
    Token      string
    Tenant     string
    Labels     map[string]string // static labels stamped on every record
    Signals    []Signal
    OwnersPath string            // MonoFS path whose .guardian/OWNERS governs access
    Public     bool              // metrics default true
    Timeout    time.Duration
    Priority   int               // merge order (lower first); ties broken by Name
    Modes      map[Signal]Mode
    TLS        TLSConfig
    // Kind-specific:
    Index      string            // elasticsearch index pattern
    Project    string            // bigquery project
    Dataset    string            // bigquery dataset
    Table      map[Signal]string // bigquery table per signal
    Schema     map[string]string // bigquery/ES column mapping overrides
    Origin     string            // "config" | "guardian-pusher" | "guardian-partition"
}
```

`Backend.Validate()` enforces: non-empty name (`^[a-z][a-z0-9-]*$`), valid kind,
URL scheme (`http`/`https`), known signals, at least one signal, `Mode` present
per signal, and BigQuery/ES required fields.

### 5.2 Registry

```go
type Registry interface {
    Snapshot() []Backend      // immutable, lock-free reads
    Subscribe(func())         // optional change notification
    LastRefresh() time.Time
}
```

- `staticRegistry`: loads `DOCTOR_PROXY_BACKENDS_FILE` (YAML/JSON). Hot reload
  on mtime change; invalid reloads keep the previous snapshot and log.
- `compositeRegistry`: merges static + Guardian discovery by `Name`. Static
  wins on conflict (allows pinning a token/URL), logged as a warning.
- Snapshots are copy-on-write (`atomic.Pointer[[]Backend]`) so thousands of
  readers pay no lock cost. Sorting is `(Priority, Name)` and defines merge
  order determinism.

### 5.3 Guardian discovery (D4)

`discovery_guardian.go` polls every `DOCTOR_GUARDIAN_DISCOVERY_INTERVAL`
(default `30s`, matching Guardian's `observed.RegistryResolver` TTL):

1. `GET {DOCTOR_GUARDIAN_API_URL}/api/pushers` →
   `{"pushers":[{name, observedApiUrl, token, tokenEnv, account, region, labels}]}`.
   - `labels.kind` selects the adapter (`prometheus`, `loki`, `tempo`,
     `elasticsearch`, `bigquery`, `aws`); missing `kind` defaults to `aws`
     for backward compatibility with `guardian-pusher-aws`.
   - `labels.signals` (comma list) selects signals; default per kind.
   - `labels.ownersPath` overrides the default OWNERS path.
   - `token`/`tokenEnv` become `Backend.Token`.
2. `GET /api/m2m/partitions` → `PartitionSummary.labels.endpoint` entries of
   the form `kind=loki;url=http://...;signals=logs` or a bare URL with kind
   inferred from the partition name. Only partitions with an `observability`
   label are considered.
3. `GET /api/m2m/partitions/{name}` → `intents[].assets[].properties.endpoint`
   and `intents[].assets[].outputs` for resolved asset endpoints.

Auth: `DOCTOR_GUARDIAN_TOKEN` is attached as `Authorization: Bearer` by
`guardianGET`. This is required because `/api/pushers` is not login-exempt when
`MONOFS_AUTHZ_ENFORCE_UI=true` (today Doctor calls Guardian with no token).
A 401/403 disables discovery for that cycle, keeps the previous snapshot, and
increments `doctor_proxy_discovery_errors_total`.

Health: each backend gets a lightweight probe (metrics: `/api/v1/status/buildinfo`;
loki: `/ready`; tempo: `/api/echo`; ES: `/`; BigQuery: cached token check).
Failed probes mark the backend `unhealthy` for fan-out (excluded, reported in
`X-Doctor-Partial`) but do not remove it from the registry.

### 5.4 AWS migration

`upstream_aws.go` wraps the existing `internal/store/aws/client.go`. The static
`DOCTOR_AWS_*` triplet is translated into a synthetic backend named `aws`
(`origin: config`) so existing deployments keep working with zero config
changes. Its `OwnersPath` defaults to `/partitions/aws-demo` (configurable).
When Guardian discovery is enabled, `pushers` with `kind: aws` supersede the
static entry.

---

## 6. Upstream adapters

### 6.1 Interfaces

```go
type MetricsUpstream interface {
    PromInstant(ctx context.Context, req PromInstantRequest) (PromData, error)
    PromRange(ctx context.Context, req PromRangeRequest) (PromData, error)
    PromSeries(ctx context.Context, match []string, start, end time.Time) ([]map[string]string, error)
    PromLabels(ctx context.Context, start, end time.Time, match []string) ([]string, error)
    PromLabelValues(ctx context.Context, label string, start, end time.Time, match []string) ([]string, error)
    PromMetadata(ctx context.Context, metric string) ([]MetadataEntry, error)
}

type LogsUpstream interface {
    QueryLogs(ctx context.Context, req LogQuery) ([]segment.LogRecord, error) // instant/range normalized by adapter
    LogLabels(ctx context.Context, start, end time.Time) ([]string, error)
    LogLabelValues(ctx context.Context, label string, start, end time.Time) ([]string, error)
    LogSeries(ctx context.Context, match []string, start, end time.Time) ([]map[string]string, error)
}

type TracesUpstream interface {
    SearchTraces(ctx context.Context, req TraceSearch) ([]TraceSummary, error)
    GetTrace(ctx context.Context, traceID string, start, end time.Time) ([]segment.TraceRecord, error)
    TraceTags(ctx context.Context) ([]string, error)
    TraceTagValues(ctx context.Context, tag string) ([]string, error)
}
```

Adapters advertise capability per signal; unsupported methods return
`ErrUnsupported`, which the router surfaces as a partial warning rather than a
request failure.

### 6.2 Adapter matrix

| Kind | Metrics | Logs | Traces | Query language | Notes |
|---|---|---|---|---|---|
| `monofs` | proxy (local engine) | proxy | proxy | PromQL/LogQL/TraceQL subset | Existing `LogEngineBackend` |
| `prometheus` | proxy | — | — | PromQL | Standard Prometheus HTTP API |
| `loki` | — | proxy | — | LogQL | Standard Loki HTTP API |
| `tempo` | — | — | proxy | TraceQL + tag search | Tempo HTTP API |
| `elasticsearch` | store | proxy via ES\|QL | store (ES\|QL) | ES\|QL (translated) | See §7 |
| `bigquery` | store | store | store | SQL (translated pushdown) | See §7 |
| `aws` | proxy | proxy | proxy | existing awsread API | Unchanged behavior |

Proxy mode is a verbatim forward (with `source` stripping for AWS as today).
Store mode fetches raw records through a pushdown interface and reuses Doctor's
existing local evaluation:

- Metrics store mode implements `StreamMetrics`-shaped row streaming; the
  existing Prometheus engine (`newDoctorPrometheusEngine`, `buildIndex`) is
  reused unchanged.
- Logs store mode implements the `rawLogQueryBackend` pushdown shape; the
  existing `parseLokiPlan`/`applyStages`/`evaluateRange` pipeline runs locally.
- Traces store mode implements span search by `(service, time, trace_id)`;
  TraceQL is translated to the same native filters as the monofs path.

### 6.3 Prometheus / Loki / Tempo adapters

- `upstream_prom.go`: `/api/v1/query`, `/query_range`, `/series`, `/labels`,
  `/label/{name}/values`, `/metadata`. Passes through `source`-stripped queries
  verbatim; decodes Prometheus JSON into the shared `PromData` shape already
  used by `aws_metrics.go`.
- `upstream_loki.go`: `/loki/api/v1/query`, `/query_range`, `/labels`,
  `/label/{name}/values`, `/series`. Converts Loki streams to
  `[]segment.LogRecord` (same conversion as `lokiPlan` output) so merge logic
  is source-agnostic.
- `upstream_tempo.go`: `/api/search` (tags and TraceQL), `/api/traces/{id}`,
  `/api/search/tags`, `/api/search/tag/{tag}/values`. Converts OTLP-JSON
  batches into `segment.TraceRecord`.

---

## 7. Query translation (D7)

### 7.1 Elasticsearch: LogQL → ES|QL

`translate/logql_esql.go` consumes the existing parsed `lokiPlan` (from
`internal/query/loki_query.go`) and emits ES|QL. Supported subset:

| LogQL construct | ES\|QL |
|---|---|
| `{service="api", k8s_pod_name="x"}` | `FROM <index> \| WHERE service.name == "api" AND k8s.pod.name == "x"` |
| `{service=~"api.*"}` | `WHERE service.name RLIKE "api.*"` |
| `!=`, `!~` | `!=`, `NOT ... RLIKE` |
| `\|= "text"` | `WHERE body LIKE "*text*"` |
| `\|~ "re"` | `WHERE body RLIKE "re"` |
| `!= "text"`, `!~ "re"` | `WHERE NOT body LIKE "*text*"`, `WHERE NOT body RLIKE "re"` |
| `\| logfmt` | `DISSECT body "%{key=\"%{val}\"} ..."` or `EVAL k = ...` (see §7.1.1) |
| `\| json` | `EVAL k = json_extract(body, "k")` |
| label filter `level="error"` | `WHERE level == "error"` |
| `count_over_time({...}[5m])` | `STATS c = COUNT(*) BY BUCKET(@timestamp, 5m)` |
| `sum by (service) (count_over_time(...))` | `STATS c = COUNT(*) BY BUCKET(@timestamp, 5m), service.name` |
| `rate(...)`, `sum(...)`, `avg(...)` | `STATS SUM/AVG(...)` with `BUCKET` |
| `limit n` / `sort` | `SORT @timestamp DESC \| LIMIT n` |

Field mapping is configurable (`Backend.Schema`) because Elasticsearch OTel
mappings vary (`body`, `message`, `attributes.*`, `resource.attributes.*`).
Defaults target the OTel data streams (`logs-*.otel-*`): `body` →
`body.text`, `severity` → `severity_text`, `trace_id` → `trace.id`,
`service` → `resource.attributes.service.name`.

`translate/logql_esql.go` is pure (no I/O) and table-tested. Unsupported
constructs (e.g. `unwrap`, `label_format`, multiline stages, nested
aggregations) return `ErrUnsupportedLogQL` with the offending stage; the router
records `doctor_proxy_translation_errors_total{kind="elasticsearch"}` and
returns HTTP 400 naming the backend and construct. Strictness is controlled by
`DOCTOR_PROXY_TRANSLATE_STRICT` (default `true`; when `false`, unsupported
backends are skipped with a partial warning).

#### 7.1.1 logfmt/json parser stages

Logfmt is not natively parseable in ES|QL without a known field set. Strategy:
`DISSECT` with a generated pattern per stage, and when the stage is followed by
label filters, only the filtered keys are extracted. This keeps generated
queries small. If pattern generation fails, the backend is skipped in
non-strict mode / 400 in strict mode.

### 7.2 BigQuery: store mode + SQL pushdown

Full PromQL/LogQL→SQL is out of scope. BigQuery runs in **store mode**: Doctor
pushes a selector/time-range predicate to BigQuery and evaluates the query
locally with the existing engines.

Metrics schema contract (configurable columns via `Backend.Schema`):

```sql
-- metric samples
timestamp TIMESTAMP, metric_name STRING, value FLOAT64,
labels JSON, resource_labels JSON, service STRING
```

Pushdown SQL for `MetricQuery{MetricName, Service, LabelMatchers}`:

```sql
SELECT timestamp, metric_name, value, labels, resource_labels, service
FROM `project.dataset.metric_samples`
WHERE timestamp BETWEEN @from AND @to
  AND metric_name IN UNNEST(@names)
  AND (@service IS NULL OR service = @service)
```

Rows stream through `jobs.query` → `getQueryResults` pagination (page size
10k) into `[]segment.MetricPointRecord`; `buildIndex` then runs the full PromQL
engine. Discovery (`labels`, `label values`, `series`) uses
`SELECT DISTINCT`/`JSON_KEYS` queries with a small result cache.

Logs pushdown: selector + line filters become `WHERE` on `body`/labels;
parser stages, label filters, and metric aggregation run in Doctor's LogQL
pipeline. Traces pushdown: `WHERE trace_id = @id` or
`(service = @service AND start_time BETWEEN ...)`; TraceQL filtering runs
locally via the shared span matcher.

Credentials: Application Default Credentials or
`DOCTOR_BIGQUERY_CREDENTIALS_FILE` (service-account JSON). Queries are
read-only (`jobs.query` with `useLegacySql=false`), capped by
`Backend.Timeout` and a per-query `maximumBytesBilled` from config. Results are
cached per `(query fingerprint, minute)` in a small LRU to protect BigQuery
costs when dashboards auto-refresh.

---

## 8. Fan-out and merge (D2, D9)

### 8.1 Router

```go
type Router struct {
    registry Registry
    access   access.Resolver
    auth     *auth.Middleware
    metrics  map[string]MetricsUpstream
    logs     map[string]LogsUpstream
    traces   map[string]TracesUpstream
    maxInflight int
    perBackendTimeout time.Duration
    breakers map[string]*breaker
}

func (r *Router) QueryMetrics(ctx, id authz.Identity, req PromQuery) (PromData, []Warning, error)
func (r *Router) QueryLogs(ctx, id authz.Identity, req LogQuery) ([]segment.LogRecord, []Warning, error)
func (r *Router) SearchTraces(ctx, id authz.Identity, req TraceSearch) ([]TraceSummary, []Warning, error)
func (r *Router) GetTrace(ctx, id authz.Identity, traceID string) ([]segment.TraceRecord, []Warning, error)
```

- Sources are selected by signal capability + `access.Allowed`.
- Fan-out uses a bounded worker pool (`DOCTOR_PROXY_MAX_INFLIGHT`, default 32),
  per-backend timeout (default 15s), and a per-backend circuit breaker
  (5 consecutive failures → open 30s).
- Partial failures never fail the request; they produce `Warning{Backend,
  Message}` returned in `X-Doctor-Partial` and counted in
  `doctor_proxy_backend_errors_total`.
- If access denies **all** sources for a signal, return HTTP 403 with a JSON
  body listing denied endpoints (not an empty 200) once enforcement is on.

### 8.2 Labeling rules

1. **Provenance:** every remote record is stamped `source=<kind>`
   (`doctor` for native, `aws`, `recording` for rule output). Static
   `Backend.Labels` are applied first so they never overwrite provenance.
2. **Collision:** identity per signal is
   - metrics: canonical series labels (sorted, normalized);
   - logs: `(timestamp_unix_nano, body, labels)` dedup key;
   - traces: `(trace_id, span_id)`.
   When two or more backends produce the same identity, add
   `shard=<backend.Name>`.
3. **Residual collision:** if the same `shard` value still collides (duplicate
   backend name or backend-emitted label), append `#<n>` in merge order:
   `shard=prom-a#2`. Merge order is `(Priority, Name)`, so results are
   reproducible.
4. **No collision:** records are merged into one series/stream/trace with no
   `shard` label, preserving the "present as one" requirement.

### 8.3 Metrics merge

`merge_metrics.go`:

- Instant vector: group samples by series key; append samples; on collision
  relabel with `shard` and retry. Result order is deterministic (backend order,
  then original sample order).
- Range matrix: same per-series merge; sample timestamps unioned and sorted.
- Scalar/string: local wins; remote scalars merged as vectors under `source`.
- Series/labels/label-values/metadata: union with dedup; `source` always
  present in discovery responses (existing behavior).
- Overlap example:

  ```
  # prom-a returns: up{instance="x"} 1
  # prom-b returns: up{instance="x"} 0
  # merged:
  up{instance="x",source="prometheus",shard="prom-a"} 1
  up{instance="x",source="prometheus",shard="prom-b"} 0
  ```

### 8.4 Logs merge

- Convert all sources to `[]segment.LogRecord`.
- Dedup identical keys (same backend returning the same line twice) keeping the
  first in backend order.
- Sort by timestamp descending (Loki semantics) after merging, then apply
  `limit`. Limit is applied **after** merge; each backend is asked for `limit`
  records, so worst-case memory is `limit × backends`.
- Direction (`forward`/`backward`) and `since` handled by the handlers as today.

### 8.5 Traces merge

- `GetTrace`: merge spans from all sources by trace ID, dedup by span ID
  preferring local (`doctor`); duplicates from other sources keep both when
  attributes differ, each with `shard` set, and collapse to one with
  `shard="a,b"` when byte-identical.
- `Search`: union summaries keyed by trace ID; when the same trace is returned
  by multiple sources, merge summaries (max span count, earliest start,
  longest duration) and set `shard` on the summary only if root service/name
  differ.

---

## 9. Tempo replaces Zipkin (D8)

### 9.1 Public API

Removed: `/api/v2/services`, `/api/v2/spans`, `/api/v2/traces`,
`/api/v2/trace/{id}` (Zipkin).

Added (Grafana Tempo datasource compatible):

| Method | Path | Response |
|---|---|---|
| GET | `/api/echo` | `{}` (readiness) |
| GET | `/api/traces/{traceID}` | OTLP-JSON `{batches:[...]}` (protobuf when `Accept: application/protobuf`) |
| GET | `/api/search` | `{traces:[{traceID, rootServiceName, rootTraceName, startTimeUnixNano, durationMs, spanSets?}]}` |
| GET | `/api/search/tags` | `{tagNames:[...]}` |
| GET | `/api/search/tag/{tag}/values` | `{tagValues:[{type, value}]}` |

`/api/search` supports both legacy tag search
(`tags=service.name%3Dapi&minDuration=&maxDuration=&limit=&start=&end=`) and
TraceQL (`q={...}`). TraceQL is forwarded verbatim to Tempo backends; native
MonoFS/AWS/ES/BigQuery sources run the shared TraceQL subset matcher
(`translate/traceql.go`). Unsupported TraceQL returns 400 with the construct.

### 9.2 Internal model

- Keep `segment.TraceRecord` as the internal span model; do not introduce a
  second model.
- `internal/tempo/convert.go` maps `segment.TraceRecord` ↔ OTLP-JSON batches
  and trace summaries.
- `service.go` handlers `handleTraceByID` and `/v1/spans/search` (native UI)
  remain and reuse the same router.

### 9.3 Migration

1. `deploy`/`stratatools` Grafana datasource `doctor-traces` changes
   `type: zipkin` → `type: tempo` (`jsonData.httpMethod: GET`,
   `url: <doctor-query>`); dashboards replace panel/query `type` `zipkin` →
   `tempo` and keep `queryType: traceId` (Tempo uses `traceId`/`traceql`/
   `search`). The JSON dashboard files under
   `stratatools/partitions/monitoring/intents/grafana.yaml` are regenerated.
2. Zipkin endpoints are deleted in the same release; release notes flag the
   breaking change. `scripts/verify-correlation.sh` and
   `scripts/verify-aws-federation.sh` are updated to the Tempo API.
3. A compatibility shim behind `DOCTOR_TRACES_ZIPKIN_COMPAT=true` (default
   false) can be enabled for one release if an external consumer needs it;
   it translates Zipkin requests onto the Tempo internals and is removed after.

---

## 10. SSO

Reuse `github.com/radryc/monofs/pkg/authz` (module replace already present).

### 10.1 Wiring

- Verifier chain: `OIDCVerifier` (when `DOCTOR_OIDC_ISSUER`+`AUDIENCE` set) →
  `StaticTokenVerifier` (service tokens, e.g. Grafana) →
  `NewBreakGlassVerifier` (emergency admin). `ChainVerifier` returns the first
  non-anonymous identity.
- API: `authz.Authenticator.HTTPMiddleware` attaches `Identity` to request
  context. `DOCTOR_AUTH_REQUIRE` (default `false` in phase P4, flipped later)
  controls rejection. When false the middleware is observe-only.
- Browser: `authz.WebAuthenticator` mounts `/auth/login`, `/auth/callback`,
  `/auth/logout` with PKCE and a persistent session store
  (`DOCTOR_SESSION_DIR`, default `.doctor/sessions`). `ExemptPaths` includes
  `/healthz`, `/ready`, `/metrics`, `/assets`, `/auth`.
- Metrics endpoints (`/api/v1/*` Prometheus) are always exempt from
  enforcement; identity is still attached when a token is presented so
  auditing works. Logs (`/loki/api/v1/*`) and traces (`/api/*` Tempo, `/v1/*`)
  enforce when the flag is on.

### 10.2 Identity propagation to backends

Backends do not receive user tokens. `doctor-query` acts as a confidential
client: `Backend.Token` (static or Guardian-discovered) is attached per
backend. `X-Doctor-Tenant` remains the tenant mechanism; the tenant is added to
the upstream query as a label matcher only for backends configured with
`TenantLabel` (opt-in), preserving today's label-based tenancy.

### 10.3 Config

| Env | Default | Purpose |
|---|---|---|
| `DOCTOR_OIDC_ISSUER` | — | OIDC issuer |
| `DOCTOR_OIDC_AUDIENCE` | — | Expected audience |
| `DOCTOR_OIDC_JWKS_URL` | discovered | JWKS override |
| `DOCTOR_OIDC_CLIENT_ID` | — | Browser login client id |
| `DOCTOR_OIDC_CLIENT_SECRET` | — | Browser login client secret |
| `DOCTOR_OIDC_REDIRECT_URL` | — | Callback URL |
| `DOCTOR_OIDC_SCOPES` | `openid email groups profile` | Scopes |
| `DOCTOR_SESSION_DIR` | `.doctor/sessions` | Session store |
| `DOCTOR_AUTH_REQUIRE` | `false` | Enforce auth |
| `DOCTOR_AUTH_EXEMPT_PATHS` | health/ready/metrics/assets | Comma list |

---

## 11. Pluggable access resolution (D3, D6)

### 11.1 Interface

```go
type Endpoint struct {
    Name       string
    Signals    []proxy.Signal
    OwnersPath string
    Public     bool
}

type Decision struct {
    Allowed bool
    Resolver string
    Reason  string
}

type Resolver interface {
    Allowed(ctx context.Context, id authz.Identity, ep Endpoint, signal proxy.Signal) (Decision, error)
    Visible(ctx context.Context, id authz.Identity, eps []Endpoint, signal proxy.Signal) (map[string]Decision, error)
    ResolverName() string
}
```

Implementations:

| Name | Behavior |
|---|---|
| `owners` (default) | OWNERS in MonoFS (see §11.2) |
| `grants` | `authz.GrantStore` loaded from `DOCTOR_ACCESS_GRANTS_PATH`/inline JSON |
| `static` | `allowSubjects`, `allowGroups`, `denySubjects` lists |
| `chain` | First resolver with a decision wins; `owners` then `grants` then `static` |
| `allow-all` | Observe/no-op |

Selection: `DOCTOR_ACCESS_RESOLVER` (default `owners`). Per-endpoint override:
`Backend.Labels["accessResolver"]`.

### 11.2 OWNERS resolver (default)

- Endpoint `OwnersPath` default: `/doctor/v1/endpoints/<name>`.
- Read `.guardian/OWNERS` walking ancestors deepest→root via
  `authz.OwnersPath(dir)`; first file wins for a given role, but roles union
  across ancestors (viewer in parent + maintainer in child both grant view).
- `authz.ParseOwners` → `authz.CompileGrants(endpointName, owners, teamMapping)`
  → `GrantStore.Can(id, endpointName, authz.ActionView)`.
  - `OwnershipResolver.IsOwner` only checks `maintainers`; using
    `CompileGrants` is deliberate so `viewers` also pass.
  - `teamMapping` from `DOCTOR_ACCESS_TEAM_MAPPING` (YAML file or inline),
    parsed by `authz.ParseTeamMapping`; unmapped teams fall back to the handle
    (warned once per endpoint).
- Rule permission uses `ActionModify` on the same endpoint name
  (`doctor-rules` calls the same resolver with signal-agnostic endpoints).

### 11.3 MonoFS file source

```go
type OwnersSource interface {
    LoadOwners(ctx context.Context, dir string) (*authz.OwnersFile, error)
    Version(ctx context.Context, path string) (string, error)
}
```

- Production implementation: `monofs_source.go` over the existing
  `internal/store/monofs.GRPCClient.ReadFile`/`ListDir` (node `Read` RPC), with
  an optional `DOCTOR_MONOFS_TOKEN` for rule writes via `UpsertPaths`.
- Missing file → `(nil, nil)` (ungoverned). Parse errors fail **closed** in
  enforce mode (deny), and are surfaced as warnings in observe mode.

### 11.4 Cache and scale

- Cache key `(ownersPath, version)`; `version` from
  `ListGuardianVersions` when a token is configured, else file mtime inferred
  from a short-TTL metadata read. Content cached as compiled grants.
- Singleflight per path prevents stampedes on cold cache.
- Inverted index maintained per snapshot: `subject → set(endpoint)`,
  `group → set(endpoint)`. Decision is O(1) hash lookups plus chain order.
- Per-request identity decision memoized for the request lifetime.
- Optional invalidation via MonoFS `SubscribeGuardianChanges`; default is TTL
  (`DOCTOR_ACCESS_CACHE_TTL`, 60s).
- Target: 5,000 endpoints × 50 teams resolve in < 1 ms with warm cache
  (`BenchmarkOwnersResolve10kEndpoints`).

### 11.5 Observe-first rollout

- `DOCTOR_ACCESS_MODE=observe` (default): every decision is computed, logged at
  debug, and exported via `doctor_access_decisions_total{endpoint,signal,allowed,resolver}`,
  but **never filters**. An `X-Doctor-Access-Observed: denied=<n>` header is
  added when any denial would have occurred.
- Flip per signal: `DOCTOR_ACCESS_ENFORCE_LOGS=true`,
  `DOCTOR_ACCESS_ENFORCE_TRACES=true`. Metrics never enforce.
- Denial response (enforce): HTTP 403 JSON
  `{"status":"error","errorType":"access_denied","endpoints":[...]}`.

### 11.6 Endpoint discovery API

`GET /v1/proxy/endpoints?signal=logs` returns the registry snapshot with the
caller's decisions:

```json
{"endpoints":[{"name":"loki-edge","kind":"loki","signals":["logs"],
  "ownersPath":"/doctor/v1/endpoints/loki-edge","allowed":true,
  "resolver":"owners","health":"healthy"}]}
```

Metrics are reported `allowed: true` regardless of identity. This powers the UI
endpoint picker and is the primary access-audit surface.

---

## 12. doctor-rules (D5, D6, D9)

### 12.1 Binary

New `cmd/doctor-rules`. Flags/env:

| Env | Default | Purpose |
|---|---|---|
| `DOCTOR_RULES_ENABLED` | `true` | Master switch |
| `DOCTOR_RULES_HTTP_ADDR` | `:18081` | CRUD + health |
| `DOCTOR_RULES_QUERY_URL` | — | `doctor-query` base URL for evaluation |
| `DOCTOR_RULES_QUERY_TOKEN` | — | Bearer for query API (service token) |
| `DOCTOR_RULES_WORKERS` | `8` | Evaluation workers |
| `DOCTOR_RULES_EVAL_TIMEOUT` | `30s` | Per-rule timeout |
| `DOCTOR_RULES_LEASE_TTL` | `30s` | Leader lease duration |
| `DOCTOR_RULES_LEASE_RENEW` | `10s` | Renew interval |
| `DOCTOR_RULES_RULER_ID` | hostname | Lease identity |
| `DOCTOR_RULES_STORE_ROOT` | `/doctor/v1/rules` | MonoFS path |
| `DOCTOR_MONOFS_LOGENGINE_ADDR` | — | Write-back target |
| `DOCTOR_MONOFS_TOKEN` | — | Guardian token for file CAS + rule writes |
| `DOCTOR_ACCESS_*` | — | Rule CRUD authorization |

### 12.2 Rule model

Stored at `<root>/<tenant>/<group>.yaml`:

```yaml
version: 1
groups:
  - name: shard-health
    tenant: default
    interval: 30s
    evaluation_delay: 10s
    rules:
      - record: doctor:shard_up:sum
        expr: sum(up) by (job)
        labels: { tier: edge }
        sources: [prom-a, prom-b]   # optional allowlist
        merge: sum                  # sum|max|min|last|keep
```

- `record` must be a valid Prometheus metric name (normalized like
  `normalizePrometheusMetricName`).
- `merge` semantics (D9):
  - `sum` (default): add values of the same output series across shards.
  - `max`/`min`: element-wise extremum across shards.
  - `last`: newest timestamp wins.
  - `keep`: do not merge; preserve per-shard series with `shard` label.
- Validation rejects empty groups, duplicate `record` within a group,
  intervals < 5s, and unknown merge strategies.

### 12.3 Store

- `ruler.Store` over `store.Store` (MonoFS `GRPCClient`), paths
  `<root>/<tenant>/<group>.yaml`, list via `ListDir` per tenant.
- Writes use CAS: create with `ExpectedVersionID:"absent"`, update with the
  loaded version; conflicts return 409.
- A local in-memory index is rebuilt on change; reload poll every 30s plus
  optional watch. 10k rules parse in a single pass
  (`BenchmarkRuleStoreLoad10k`).

### 12.4 Scheduler

- Groups are the scheduling unit: one min-heap entry per `(tenant, group)`
  keyed by `nextEval = align(now, interval) + jitter`.
- Worker pool pulls due groups; each group's rules evaluate sequentially within
  the group (preserves ordering/state), groups run concurrently.
- Within a tick, identical `(expr, sources, at)` queries are deduped via a
  short-lived map so N rules over the same expression issue one query.
- Backpressure: if the heap lags more than `interval`, skip to the next aligned
  tick and increment `doctor_rules_lag_seconds`.
- Scale target: 10k rules / 200 groups on one leader with `DOCTOR_RULES_WORKERS=8`.

### 12.5 Per-shard evaluation and merge (D9)

For each rule:

1. Resolve target sources: `rule.sources` if set, else all sources visible to
   the ruler's service identity with `signal=metrics`.
2. Issue one instant PromQL query per source through `doctor-query`
   (`/api/v1/query`), in parallel with bounded concurrency.
   - Per-shard results are already `source`-tagged by the proxy.
3. Normalize each result series to the output metric name + rule labels +
   static labels; drop `source`/`shard` from identity when `merge != keep`.
4. Merge by `merge` strategy across shards (same output label set):
   - `sum`: `out = Σ shard_values` (counter/rate-safe).
   - `max`/`min`: element-wise.
   - `last`: max timestamp wins.
   - `keep`: retain `shard` label per source.
5. If a shard fails, evaluate the rest and mark the rule evaluation partial;
   do not write a series that mixes partial and complete windows for `sum`
   unless `DOCTOR_RULES_PARTIAL=sum` (default `skip` for correctness).
6. Write via `LogEngineClient.IngestMetrics` in one batch per tick with labels
   `source="recording"`, `rule_group`, `rule`, `tenant`, plus output labels;
   `ruler_replica` is added for debugging and stripped by the ruler's own
   read path if dedup mode is used.
7. Emit metrics: `doctor_rules_evaluations_total{group,result}`,
   `doctor_rules_evaluation_duration_seconds`, `doctor_rules_samples_written_total`,
   `doctor_rules_skipped_partial_total`.

Evaluation API: `POST /v1/rules/evaluate?group=` for one-shot manual runs
(also used by tests).

### 12.6 Lease-based HA

Mirrors Guardian's claim idiom (`awsscan`/task claims): a JSON lease at
`<root>/.lease/<tenant>.json`:

```json
{"ruler_id":"doctor-rules-0","acquired_at":"...","expires_at":"..."}
```

- Acquire: `UpsertFiles` with `ExpectedVersionID:"absent"`; on
  `store.ErrConflict`, read the lease; if expired (by `expires_at`), replace
  with exact-version CAS; else stand by.
- Renew: exact-version CAS every `LEASE_RENEW`; failure demotes to follower.
- Follower poll: `LEASE_RENEW` interval; takeover within `LEASE_TTL`.
- Only the leader schedules; all replicas serve CRUD and health.
- Clock skew tolerance: `LEASE_TTL ≥ 3 × LEASE_RENEW` enforced at startup.

### 12.7 Rule CRUD and permissions (D6)

| Method | Path | Permission |
|---|---|---|
| GET | `/v1/rules?tenant=` | any authenticated (filtered by endpoint visibility) |
| GET | `/v1/rules/{tenant}/{group}` | `ActionView` on endpoint |
| PUT | `/v1/rules/{tenant}/{group}` | `ActionModify` on endpoint |
| DELETE | `/v1/rules/{tenant}/{group}` | `ActionModify` on endpoint |

Endpoint mapping for rules: `<root>/<tenant>/<group>`; the OWNERS resolver
walks `/doctor/v1/rules/<tenant>/<group>/.guardian/OWNERS` ancestors up to
`/doctor/v1/rules/.guardian/OWNERS` and `/doctor/v1/.guardian/OWNERS`. In
observe mode, writes are allowed and the would-be denial is audited; flipping
`DOCTOR_ACCESS_ENFORCE_RULES=true` enforces.

---

## 13. Public API surface after this design

| Signal | Endpoint | Change |
|---|---|---|
| Metrics | `/api/v1/query`, `/query_range`, `/labels`, `/label/*`, `/series`, `/metadata`, `/status/buildinfo` | Same paths, federated results |
| Logs | `/loki/api/v1/query`, `/query_range`, `/labels`, `/label/*`, `/series`, `/ready` | Same paths, federated + access-filtered |
| Traces | `/api/echo`, `/api/traces/{id}`, `/api/search`, `/api/search/tags`, `/api/search/tag/{tag}/values` | **New Tempo API** |
| Traces (removed) | `/api/v2/*` Zipkin | Removed (optional compat flag) |
| Native | `/v1/spans/search`, `/v1/traces/*`, `/v1/logs/*`, `/v1/metrics/range`, `/v1/services/*`, `/v1/guardian/*`, `/v1/ui/config` | Unchanged, routed through proxy |
| Proxy | `/v1/proxy/endpoints` | New |
| Auth | `/auth/login`, `/auth/callback`, `/auth/logout` | New |
| Rules | `/v1/rules`, `/v1/rules/{tenant}/{group}`, `/v1/rules/evaluate` | New (`doctor-rules`) |

---

## 14. Scale strategy

### 14.1 Thousands of endpoints

- Immutable registry snapshots; merge order precomputed once per snapshot.
- Fan-out bounded by `DOCTOR_PROXY_MAX_INFLIGHT`; a global semaphore prevents
  thousands of simultaneous upstream calls.
- Per-backend circuit breaker + health probe; unhealthy backends excluded
  without touching the rest.
- Discovery diffing: only changed backends rebuild clients/breakers.
- Access decisions indexed by subject/group (no endpoint scans).
- Discovery responses (`labels`, `series`) are unioned with per-backend result
  caches (`DOCTOR_PROXY_DISCOVERY_CACHE_TTL`, default 30s).

### 14.2 Tens of thousands of recording rules

- Heap-per-group scheduling (200 groups, not 10k timers).
- Worker pool with per-rule timeout; group-level sequential evaluation.
- In-tick query dedup; expression fingerprint cache for `(expr, at, sources)`.
- Rule store pagination + single-pass parse; 30s reload.
- Write-back batches per tick; no per-rule gRPC call.
- Benchmarks assert: 10k-rule load < 500 ms; scheduler tick with 200 due groups
  < 1 ms; evaluation throughput ≥ 500 rules/s with 8 workers against fake
  backends.

---

## 15. Configuration reference (new env)

| Env | Default | Component |
|---|---|---|
| `DOCTOR_PROXY_ENABLED` | `true` | proxy |
| `DOCTOR_PROXY_BACKENDS_FILE` | — | static registry |
| `DOCTOR_PROXY_MAX_INFLIGHT` | `32` | fan-out |
| `DOCTOR_PROXY_BACKEND_TIMEOUT` | `15s` | fan-out |
| `DOCTOR_PROXY_DISCOVERY_CACHE_TTL` | `30s` | discovery |
| `DOCTOR_PROXY_TRANSLATE_STRICT` | `true` | translation |
| `DOCTOR_PROXY_PARTIAL_HEADER` | `X-Doctor-Partial` | fan-out |
| `DOCTOR_GUARDIAN_DISCOVERY_ENABLED` | `false` | discovery |
| `DOCTOR_GUARDIAN_DISCOVERY_INTERVAL` | `30s` | discovery |
| `DOCTOR_GUARDIAN_TOKEN` | — | discovery auth |
| `DOCTOR_ACCESS_RESOLVER` | `owners` | access |
| `DOCTOR_ACCESS_MODE` | `observe` | access |
| `DOCTOR_ACCESS_ENFORCE_LOGS` | `false` | access |
| `DOCTOR_ACCESS_ENFORCE_TRACES` | `false` | access |
| `DOCTOR_ACCESS_ENFORCE_RULES` | `false` | access |
| `DOCTOR_ACCESS_TEAM_MAPPING` | — | access |
| `DOCTOR_ACCESS_GRANTS_PATH` | — | access |
| `DOCTOR_ACCESS_CACHE_TTL` | `60s` | access |
| `DOCTOR_ACCESS_OWNERS_ROOT` | `/doctor/v1/endpoints` | access |
| `DOCTOR_MONOFS_TOKEN` | — | MonoFS file CAS |
| `DOCTOR_MONOFS_CLIENT_ID` | `doctor-query` | MonoFS client |
| `DOCTOR_TRACES_ZIPKIN_COMPAT` | `false` | migration |
| `DOCTOR_BIGQUERY_CREDENTIALS_FILE` | ADC | BigQuery |
| `DOCTOR_BIGQUERY_MAX_BYTES_BILLED` | `0` | BigQuery |

---

## 16. Observability

New Prometheus metrics (served on `/metrics` once added to Doctor's self
telemetry or the debug listener):

- `doctor_proxy_backend_up{backend,kind,signal}`
- `doctor_proxy_requests_total{signal,result}` (result: `ok|partial|error|denied`)
- `doctor_proxy_backend_errors_total{backend,signal,reason}`
- `doctor_proxy_backend_duration_seconds{backend,signal}`
- `doctor_proxy_translation_errors_total{kind,construct}`
- `doctor_proxy_discovery_backends{origin}`
- `doctor_proxy_discovery_errors_total{reason}`
- `doctor_access_decisions_total{endpoint,signal,allowed,resolver}`
- `doctor_rules_evaluations_total{group,result}`
- `doctor_rules_evaluation_duration_seconds{group}`
- `doctor_rules_samples_written_total{group}`
- `doctor_rules_skipped_partial_total{group}`
- `doctor_rules_leader{replica}` (1 for leader)

Logs: structured (`slog` style via `log.Printf` today) with `backend`,
`signal`, `trace_id`, `identity_subject` where applicable. Never log tokens or
raw authorization headers.

---

## 17. Testing and benchmarks

### 17.1 Unit tests

| Area | Tests |
|---|---|
| Registry | parse/validate, hot reload, composite precedence, Guardian diffing |
| Discovery | fake Guardian HTTP server; 401 keeps snapshot; TTL behavior |
| Adapters | `httptest` servers per kind; error/status mapping; timeout |
| Merge metrics | no overlap → merged; overlap → `shard`; residual → `#n`; determinism |
| Merge logs | dedup, ordering, limit-after-merge, direction |
| Merge traces | span dedup, trace summary union, `shard` |
| Translation | LogQL→ES\|QL table tests incl. unsupported constructs; BigQuery pushdown SQL golden files |
| Tempo API | golden OTLP-JSON; Grafana-style search/tag requests; TraceQL passthrough |
| Auth | middleware observe/enforce, bearer/cookie, exempt paths |
| Access | OWNERS parse/ancestors/team mapping, cache invalidation, chain, inverted index |
| Ruler | model validation, store CAS conflicts, scheduler alignment/jitter/dedup, merge strategies, lease acquire/renew/takeover, CRUD permissions |

### 17.2 Integration tests

- End-to-end HTTP through `Service.Handler()` with 3 fake upstreams
  (prom/loki/tempo) + auth + access observe/enforce.
- Tempo trace-by-ID fan-out and dedup with overlapping spans.
- `doctor-rules` against a fake query API + fake logengine writer: per-shard
  eval, merge, write labels, partial failure, lease takeover with two stores.
- Guardian discovery against a fake Guardian API exposing pushers and
  partitions.

### 17.3 Benchmarks

| Benchmark | Target |
|---|---|
| `BenchmarkMergeSeriesOverlap` (16 backends × 10k series) | < 100 ms, no alloc blowup |
| `BenchmarkFanout1000Backends` (fake, 32 inflight) | bounded memory, linear scaling |
| `BenchmarkOwnersResolve10kEndpoints` | < 1 ms warm |
| `BenchmarkRuleScheduler10k` | tick < 1 ms |
| `BenchmarkRuleStoreLoad10k` | < 500 ms |
| `BenchmarkLogQLToESQL` | < 50 µs/query |
| `BenchmarkTempoConvert10kSpans` | < 20 ms |

All tests run under `go test -race ./...`; benchmarks run with `-benchmem` in
CI on a schedule, not on every PR.

---

## 18. Phased implementation plan

Each phase lands on `main` independently with tests green.

### P0 — Groundwork refactor (no behavior change) — landed

- Created `internal/proxy`: `Signal`/`Mode`/`Backend` model, upstream
  interfaces (`MetricsUpstream`/`LogsUpstream`/`TracesUpstream`), the
  `AWSClient` interface plus `AWSUpstream` adapter, and merge/labeling
  primitives. `source` carries provenance (backend kind); `shard` carries the
  backend name and is only added on cross-backend overlap.
- Query handlers now call the extracted classification/merge helpers
  (`AWSQueryMode`, `AWSMatches`, `MergePrometheusQueryData`, `MergeLogs`,
  `AWSLogsForLoki`, `AWSMetricNames`); `Service.aws` is typed as
  `proxy.AWSClient`. Native log/local queries are untouched.
- Traces orchestration stays in `internal/query` until the P2 Tempo rewrite
  replaces the Zipkin conversion wholesale.
- Added unit tests for the backend model, AWS classification/conversion,
  merge/labeling (including cross-backend `shard` behavior), and benchmarks
  (`BenchmarkMergePrometheusSeriesOverlap`: 16 shards × 10k overlapping series
  ≈ 104 ms on a 13900KF).
- **Acceptance:** existing tests pass unchanged; AWS output is byte-identical.

P1 wires the registry and the fan-out router on top of these primitives and
migrates the discovery handlers.

### P1 — Registry, discovery, fan-out/merge — landed

- Static file registry with hot reload (`DOCTOR_PROXY_BACKENDS_FILE`),
  Guardian discovery for `/api/pushers` and `/api/m2m/partitions` with
  `DOCTOR_GUARDIAN_TOKEN`, and a composite registry where static entries win.
- `proxy.Router`: bounded fan-out (`DOCTOR_PROXY_MAX_INFLIGHT`), per-backend
  timeout, circuit breaker (5 failures → 30s open), partial-failure warnings
  (`X-Doctor-Partial` for Prometheus responses), and a query policy
  (`always`/`fallback`) that preserves the legacy AWS fallback semantics.
- Prometheus and Loki HTTP adapters. Query handlers use the router when
  `DOCTOR_PROXY_ENABLED=true` (default); tests and any deployment without a
  router keep the legacy path.
- `/v1/proxy/endpoints` lists backends with health state.
- **Acceptance:** integration test merges local + two Prometheus shards with
  `shard` labels through the real handler, and local + Loki through the Loki
  handler; `BenchmarkRouterFanout256Backends` ≈ 0.6 ms.

Deferred within P1: router-backed discovery unions (Prometheus labels/series
and Loki labels still use the legacy local+AWS path until P3 adapters land),
active health probes (health is currently derived from the circuit breaker),
and trace fan-out (P2 replaces Zipkin with Tempo).

### P2 — Tempo replaces Zipkin — landed

- `internal/tempo` wire types and conversion from `segment.TraceRecord`
  (batches grouped by service, canonical labels merged into span attributes by
  the query adapter). Handlers: `/api/echo`, `/api/traces/{id}`, `/api/search`
  (tag search and TraceQL), `/api/search/tags`, `/api/search/tag/{tag}/values`.
- Zipkin routes and conversions removed entirely (no compat flag); native trace
  APIs remain and continue to merge AWS.
- Router trace fan-out: `SearchTraces`/`GetTrace`/`TraceTags`/`TraceTagValues`
  with summary dedup, span-level dedup preferring local, `shard` labels on
  cross-backend overlap, and TraceQL routed only to `TraceQLUpstream` backends
  (400 when none are configured).
- Tempo HTTP upstream adapter; local traces adapter with tag filtering,
  duration filters, and canonical label search.
- Grafana migration (`zipkin` → `tempo` datasource and panel query types) lands
  as a separate commit in the `stratatools` repository; verify scripts updated.
- **Acceptance:** Tempo handler tests cover trace-by-ID OTLP-JSON, search
  summaries, tag discovery, AWS fallback, and TraceQL rejection without a Tempo
  backend; router tests cover merge/shard/union.

Deferred to P3: the local TraceQL subset matcher (TraceQL currently requires a
Tempo backend).

### P3 — Tempo/Elasticsearch/BigQuery adapters + translation — landed

- Tempo upstream landed in P2.
- `internal/proxy/translate`: LogQL selector/line-filter parsing, LogQL→ES|QL
  translation, LogQL→BigQuery SQL translation, and the shared TraceQL subset
  matcher. Parser stages, label filters, and metric aggregation stay local, so
  the translators only emit selector + line-filter pushdown.
- Elasticsearch adapter over the ES|QL `_query` API with configurable index and
  field schema (OTel defaults). BigQuery adapter over `jobs.query` with
  pagination, read-only SQL, and schema mapping.
- The router builds ES/BigQuery upstreams from the registry; TraceQL requests
  without a Tempo backend fall back to the subset matcher on local/AWS sources
  (400 for unsupported constructs).
- **Acceptance:** translator table tests, ES|QL fake-server test, BigQuery
  fake `jobs.query` + pagination test, TraceQL subset filtering test.

Deferred: Elasticsearch traces and metrics (logs only for now), BigQuery
metrics/traces, ES|QL strict-mode request failure (translation errors currently
surface as backend warnings), and `DOCTOR_PROXY_TRANSLATE_STRICT` wiring.

### P4 — SSO + access (observe-first) — landed

- `internal/auth`: OIDC verifier chain (`OIDCVerifier` + break-glass),
  `WebAuthenticator` browser login wiring, and HTTP middleware that enforces
  `DOCTOR_AUTH_REQUIRE` outside exempt paths (metrics/health stay public).
- `internal/access`: `Resolver` interface with `owners` (default, MonoFS
  `.guardian/OWNERS` via a file reader), `grants`, `static`, `chain`, and
  `allow-all`; `Gate` applies observe/enforce per signal and records decisions;
  `ErrAccessDenied` drives HTTP 403.
- Router integration: logs/traces backends are filtered by the gate; denied
  backends are warned about in observe mode and excluded in enforce mode; an
  all-denied signal returns 403. `GET /v1/proxy/endpoints` reports per-endpoint
  decisions.
- `X-Doctor-Access-Observed` counts observed denials; `X-Doctor-Partial`
  carries the reasons.
- **Acceptance:** `BenchmarkOwnersResolve10kEndpoints` ≈ 1.9 ms for all 10k
  endpoints warm (≈ 0.95 ms for 5k, meeting the target); access and auth unit
  tests cover observe/enforce, chain, OWNERS ancestor/team matching, caching,
  and router filtering.

Deferred to P6: routing the native `/v1/*` log/trace handlers through the
router (they still bypass access), UI login state, and flipping enforcement in
deployment manifests.

### P5 — doctor-rules — landed

- `internal/ruler`: rule model with `sum|max|min|last|keep` merge strategies,
  MonoFS-backed store with CAS preconditions, heap scheduler with jitter and a
  worker pool, per-shard evaluation via doctor-query with write-back through
  the MonoFS logengine, and a CRUD/evaluate HTTP API.
- Lease HA via MonoFS CAS (`ExpectedVersionID: "absent"` acquire, version CAS
  renew, expired-lease takeover), mirroring Guardian's claim idiom.
- Partial sums are skipped (`X-Doctor-Partial` detection) to avoid incorrect
  totals; `keep` preserves `shard` labels.
- New `cmd/doctor-rules` binary and `DOCTOR_RULES_*` / `DOCTOR_MONOFS_*`
  configuration.
- **Acceptance:** `BenchmarkRuleStoreLoad10k` ≈ 39 ms;
  `BenchmarkEvaluatorMerge10k` ≈ 3.2 ms; unit tests cover parsing/validation,
  store CAS, merge strategies, partial skipping, API CRUD, and lease takeover.

Deferred to P6: OWNERS-based rule CRUD authorization wiring (the API accepts an
optional `Authorizer`; observe-first leaves it open), per-source query routing
(per-shard evaluation currently merges the proxy's tagged results), and
rule-output read-path filtering.

### P6 — Enforce readiness + load test — landed

- Native `/v1/traces/{id}`, `/v1/spans/search`, and `/v1/logs/search` now route
  through the proxy router when it is configured, so access enforcement covers
  the UI APIs as well as the Grafana APIs (403 on enforced denial, partial
  warnings surfaced).
- Enforced denial integration test covers the native trace and log handlers.
- `internal/loadtest` harness (gated by `DOCTOR_LOADTEST=1`) and the report
  `docs/design/unified-proxy-loadtest.md`: 2,000 endpoints × 100 concurrent
  queries ≈ 11.6 ms/query; 20,000 rules listed in 74 ms and evaluated in 33 ms.
- Deploy manifests carry proxy/auth/access env with observe-first defaults and
  a `doctor-rules` intent; README documents every new variable.
- **Acceptance:** enforced handler denial tests pass; load test report
  committed.

Deferred (documented, not blocking observe-first rollout): the React UI login
state, endpoint picker, and rules page; routing service-discovery handlers
through the router; an OWNERS `ActionModify` adapter for rule CRUD (the API
accepts an `Authorizer`, left open by default); and shipping enforce defaults
in manifests (operators flip `DOCTOR_ACCESS_MODE=enforce`).

---

## 19. Risks and mitigations

| Risk | Mitigation |
|---|---|
| LogQL→ES\|QL coverage gaps | Strict errors name the construct; store-mode fallback on the ES `_search` API is a follow-up option |
| BigQuery cost from dashboard refresh | Pushdown only, `maximumBytesBilled`, result cache, discovery cache |
| Cross-shard aggregation semantics | Explicit per-rule `merge` strategy; `keep` for per-shard series; documented that proxy mode cannot globally aggregate non-additive functions |
| Partial results hide outages | `X-Doctor-Partial` + metrics + observe-mode dashboards; never silently return empty |
| Tempo API break for Zipkin consumers | One-release compat flag + release note; regenerate Grafana config |
| OWNERS read latency / thundering herd | `(path,version)` cache + singleflight + inverted index; observe-first |
| Lease split-brain | `TTL ≥ 3×RENEW`, CAS renew, takeover only after `expires_at`; per-replica metrics |
| Guardian discovery auth | `DOCTOR_GUARDIAN_TOKEN`; discovery failure keeps last snapshot |
| Memory blowup from `limit × backends` logs | Global cap `DOCTOR_PROXY_MAX_LOG_RECORDS`; post-merge truncation |

---

## 20. Open follow-ups (not blocking)

1. OTLP as a source or ingest fan-out (deferred by D1).
2. Elasticsearch `_search` store-mode fallback for unsupported ES|QL.
3. TraceQL coverage expansion for native/ES/BigQuery spans.
4. True cross-shard PromQL via store API mode (Thanos-style) if non-additive
   global aggregation is required.
5. `doctor-ingest` fan-out to multiple MonoFS shards (currently out of scope).

---

## 21. References

- MonoFS authz: `pkg/authz/{authz,verifier,grants,owners,resolver,compiler,oidc,pkce,webauth,interceptor}.go`
- MonoFS router ownership: `internal/router/{ownership,authz_ingest,authz_merge_gate}.go`
- Guardian discovery: `internal/registry/store.go`, `internal/observed/resolver.go`,
  `internal/ui/server.go` (`/api/pushers`, `/api/m2m/partitions`)
- Guardian lease idiom: `internal/pusher/runtime/runtime.go` (`tryClaimTask`),
  `internal/awsscan/queue.go`
- Doctor current: `internal/query/service.go`, `internal/query/aws_federation.go`,
  `internal/query/prometheus_compat.go`, `internal/query/observability_compat.go`,
  `internal/query/loki_query.go`, `internal/store/monofs/*`
- Grafana migration target: `stratatools/partitions/monitoring/intents/grafana.yaml`
