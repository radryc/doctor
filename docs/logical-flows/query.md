# Logical flow: query

## 1. Startup and wiring

`cmd/doctor-query/main.go` builds the service in this order:

1. `config.LoadQuery`, OTel telemetry, heartbeat.
2. Required MonoFS log-engine address → `LogEngineClient`.
3. `query.NewServiceLogEngine` (installs the ClickHouse audit reader from `DOCTOR_AUDIT_CH_*` and the
   display-profile loader from `DOCTOR_DISPLAY_*`), then `SetTraceDiscoveryCacheTTL`,
   `SetTraceDiscoveryWindow`, `SetTenantHeader`, Guardian URLs, default partition, rules
   URL/token/identity secret, Sentinel URL.
4. Shared file store:
   - if `DOCTOR_MONOFS_TOKEN` is set, connect `GRPCClient` (writable) and use it for dashboards;
   - if a file token is configured, a MonoFS connection failure is fatal;
   - without a file token, use `ruler.NewMemoryStore()` and log “using in-memory store”.
5. Optional repo policy gate from `DOCTOR_ACCESS_POLICY_URL`/`_FILE`; `SetPolicyGate` wraps the log
   backend with `policyBackend`.
6. Federation token resolver (env/Guardian/bundle).
7. Optional AWS and GCP:
   - federation enabled: load inline JSON → MonoFS snapshot → file fallback; build catalog, manager(s) for AWS and GCP,
     metrics, optional reloader, `go manager.Run`; `svc.SetAWSBackend` / `svc.SetGCPBackend`;
   - legacy single backends: `store/aws.Client`, `store/gcp.Client`.
8. `setupSecurity`: verifier (OIDC + break-glass), auth middleware, browser login, access resolver,
   legacy `access.Gate`.
9. `setupProxyRouter`: always wires `proxy.Router` with the local MonoFS backend, optional AWS/GCP
   backends, and (when `DOCTOR_PROXY_ENABLED=true`) static registry and/or Guardian discovery.
10. Optional `quota.Engine` (`DOCTOR_QUOTA_ENABLED`), `TenantGuard`, and HTTP server.

## 2. Middleware and request identity

Execution order from the outside in:

```mermaid
flowchart LR
  A[HTTP request] --> B[WebAuthenticator.Handler]
  B --> C[TenantTrust.Wrap]
  C --> D[TenantGuard.Wrap]
  D --> E[auth.Middleware.Wrap]
  E --> F[PolicyGate.Middleware]
  F --> G[quota.Engine.Middleware]
  G --> H[withWarningHeaders]
  H --> I[guardianTokenMiddleware]
  I --> J[query.Service.Handler mux]
```

| Layer | Behavior |
|---|---|
| `WebAuthenticator` | resolves `doctor_session` cookie (JWT verified, then opaque session fallback) or bearer; optionally redirects anonymous browser navigations to login; mounts `/auth/login`, `/auth/callback`, `/auth/logout` |
| `TenantTrust` | if no verified identity and tenant header/query param exists, attaches `ClientID=tenant:<name>`, group `doctor:tenant`; verified identity wins |
| `TenantGuard` | enforces `DOCTOR_QUERY_ALLOWED_TENANTS` or forces `DOCTOR_QUERY_SINGLE_TENANT`; with neither configured it fails closed and accepts only the default tenant |
| `auth.Middleware` | re-verifies bearer if present; if `AuthRequire` and not exempt, rejects anonymous with 401; attaches identity |
| `PolicyGate.Middleware` | under enforcement, rejects whole log/trace requests when the policy denies or is not loaded; metrics paths pass |
| `quota.Engine.Middleware` | enforces per-partition/subject query rate, concurrency, and cost budgets |
| `withHTTPMetrics`, `withSecurityHeaders`, `withBodyLimit`, `withRequestID`, `withMutationOriginCheck`, `withAuditLogging` | applied by `Service.Handler`: per-route metrics, CSP/security headers, request size cap, `X-Request-ID`, cross-origin mutation rejection, and audit events |
| `withWarningHeaders` | attaches `proxy.WarningCollector` and federation `RequestMeta`; emits `X-Doctor-Partial*` headers |
| `guardianTokenMiddleware` | copies the caller's bearer or `doctor_session` cookie into context for server-side Guardian reads |
| `withDisplayMapping` | outermost wrapper inside `Service.Handler`: translates display query keys, resolves the caller's profile, maps JSON response keys for `/v1/`, `/loki/`, `/api/`, `/api/v2/`, and sets `X-Strata-Display-Profile` / `X-Strata-Display-Version` |
| mux | route handlers |

Exempt paths (when no custom list is set): `/healthz`, `/ready`, `/api/echo`, `/metrics`, and
`/api/v1/status/buildinfo`. The Prometheus API is **not** exempt, so Grafana must send a bearer token
when a verifier is configured. Metrics are globally visible to authenticated callers; Doctor does
not apply per-partition metric scoping.

## 3. Route map

### Health, UI, and metadata

| Method | Path | Handler | Notes |
|---|---|---|---|
| GET | `/healthz` | `handleHealth` | constant `{"status":"ok"}` |
| GET | `/ready` | `handleLokiReady` | 200 when log engine is configured and enforced policy is loaded; otherwise 503 |
| GET | `/metrics` | `promhttp.Handler()` | Prometheus scrape endpoint |
| GET | `/debug/pprof/*` | `net/http/pprof` | registered on the query HTTP listener (no separate localhost guard) |
| GET | `/v1/ui/config` | `handleUIConfig` | default tenant, Guardian URL, default partition |
| GET | `/v1/ui/user` | `handleUIUser` | identity fields |
| GET | `/` and unknown non-API paths | `handleUI` | embedded SPA; API-prefixed missing routes → 404 |

### Prometheus-compatible

| Method | Path | Handler |
|---|---|---|
| GET/POST | `/api/v1/query` | `handlePrometheusQuery` |
| GET/POST | `/api/v1/query_range` | `handlePrometheusQueryRange` |
| GET/POST | `/api/v1/labels` | `handlePrometheusLabels` |
| GET | `/api/v1/label/{name}/values` | `handlePrometheusLabelValues` |
| GET/POST | `/api/v1/series` | `handlePrometheusSeries` |
| GET | `/api/v1/metadata` | `handlePrometheusMetadata` |
| GET/POST | `/api/v1/query_exemplars` | `handlePrometheusQueryExemplars` |
| GET | `/api/v1/rules` | `handlePrometheusRules` (empty groups) |
| GET | `/api/v1/status/buildinfo` | `handlePrometheusBuildInfo` |

### Loki-compatible

| Method | Path | Handler |
|---|---|---|
| GET/POST | `/loki/api/v1/query` | `handleLokiQuery` |
| GET/POST | `/loki/api/v1/query_range` | `handleLokiQueryRange` |
| GET | `/loki/api/v1/labels` | `handleLokiLabels` |
| GET | `/loki/api/v1/label/{name}/values` | `handleLokiLabelValues` |
| GET | `/loki/api/v1/series` | `handleLokiSeries` |
| GET | `/loki/api/v1/index/stats` | `handleLokiIndexStats` |
| GET | `/loki/api/v1/index/volume` | `handleLokiIndexVolume` |
| GET | `/loki/api/v1/index/volume_range` | `handleLokiIndexVolumeRange` |
| GET | `/loki/api/v1/patterns` | empty list |
| GET | `/loki/api/v1/detected_fields` | empty list |
| GET | `/loki/api/v1/detected_labels` | empty list |
| GET | `/loki/api/v1/status/buildinfo` | version `3.5.0` |

### Tempo-compatible

| Method | Path | Handler |
|---|---|---|
| GET | `/api/echo` | empty object |
| GET | `/api/status/buildinfo` | version `2.1.0` |
| GET | `/api/traces/{traceID}` | Tempo v1 trace (JSON or protobuf) |
| GET | `/api/v2/traces/{traceID}` | Tempo v2 `TraceByIDResponse` (JSON or protobuf) |
| GET | `/api/search` | tag search / TraceQL subset / span sets |
| GET | `/api/search/tags` | Tempo v1 tag names |
| GET | `/api/search/tag/{tag}/values` | Tempo v1 tag values |
| GET | `/api/v2/search/tags` | Tempo v2 tag scopes |
| GET | `/api/v2/search/tag/{tag}/values` | Tempo v2 tag values |
| GET | `/api/metrics/query_range` | TraceQL metrics range |
| GET | `/api/metrics/query` | TraceQL metrics instant |

### Native UI/API

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/traces/recent` | paginated trace summaries |
| GET | `/v1/traces/{id}` | full trace as `{trace_id, spans}` |
| GET | `/v1/spans/search` | span search by service/trace/time |
| GET | `/v1/logs/services` | service/partition/identity/log-group catalog |
| GET | `/v1/logs/search` | log search |
| GET | `/v1/metrics/range` | raw metric point range (legacy) |
| GET | `/v1/services/discover` | APM service catalog |
| GET | `/v1/services/suggest` | PromQL suggestions |
| GET | `/v1/federation/status` | catalog status |
| GET | `/v1/federation/targets` | paginated target view |
| GET | `/v1/federation/plan` | planner debug (admin-only when policy enforced) |
| GET | `/v1/proxy/endpoints` | proxy backend inventory/health |
| GET/POST | `/v1/rules`, `/v1/rules/*` | rules list/detail/evaluate proxy |
| ANY | `/v1/alerts`, `/v1/alerts/*` | Sentinel alerts/rules/silences/routing proxy |
| ANY | `/v1/incidents`, `/v1/incidents/*`, `/v1/alert-sets*`, `/v1/incident-policy`, `/v1/postmortem-engines` | Sentinel incidents/postmortems proxy |
| ANY | `/api/v2/*` | Alertmanager-compatible v2 API proxy to Sentinel |
| POST | `/v1/sql/query` | read-only DuckDB/ClickHouse SQL console |
| GET | `/v1/audit/events` | query-plane audit events (requires audit or admin scope when a policy gate is configured) |
| GET | `/v1/audit/status` | aggregate audit-pipeline health (no event content; 503 on stale/conflicting/unavailable) |
| GET | `/v1/display-profiles` | available compiled display profiles and bundle version |
| GET | `/v1/display-profiles/effective` | caller's effective profile id (empty when unmapped) |
| GET | `/v1/quota` | partition and subject quota limits and usage |
| GET/POST/PUT/DELETE | `/v1/dashboards*` | dashboard CRUD, versions, import/export |
| GET | `/v1/guardian/topology` | Guardian topology projection |
| GET | `/v1/guardian/events` | Guardian timeline projection |
| GET | `/v1/guardian/health` | derived health score |
| GET | `/v1/guardian/overview` | raw Guardian overview proxy |
| GET | `/v1/guardian/probes` | Guardian probe proxy + summary |

## 4. Prometheus flow

### 4.1 Query execution

```mermaid
flowchart TD
  A[/api/v1/query or query_range/] --> B[parse query, time, step, timeout]
  B --> C{proxyRouter?}
  C -- yes --> D[Router.MetricsInstant/Range]
  C -- no --> E{AWS-only?}
  E -- yes --> F[AWS client PrometheusQuery]
  E -- no --> G[local PromQL engine]
  G --> H{count by __name__ fast path?}
  H -- yes --> I[series index presence scan]
  H -- no --> J[doctorPromQuerier.Select]
  J --> K[stream matching metric points]
  K --> L[build in-memory PromQL series]
  L --> M[execute PromQL]
  F --> N[merge local+AWS]
  D --> N
  I --> N
  M --> N
  N --> O[filter by partition/intent/asset]
  O --> P[Prometheus response]
```

Local engine details:

- `doctorPromQueryable.Querier` returns a `doctorPromQuerier` bound to tenant and optional partition.
- `Select` loads an index per matcher set, streams metric points, deduplicates points, sorts series,
  and applies matchers client-side.
- `__name__` matchers are expanded to exact stored names using the manifest-backed discovery mode;
  above 16 names the store is scanned once and filtered client-side.
- Per-query budgets: 4M samples, 100k series; global 8M samples. Exceeding them returns HTTP 422.
- `count by (__name__)` / bare `count` are served from the chunk series index, not samples.
- Native-histogram functions (`histogram_avg`, `histogram_count`, `histogram_sum`,
  `histogram_stddev`, `histogram_stdvar`, `histogram_quantile`, `histogram_fraction`) synthesize a
  native histogram series from the classic `_bucket` / `_count` / `_sum` siblings of an exact base
  metric name. This keeps existing storage and historical data queryable with those functions;
  stored native histogram samples are not required. Explicit `_bucket` selectors keep using the
  classic bucket path. The synthesized custom-bucket histogram starts its real buckets at 0 for
  non-negative observations so `histogram_stddev` / `histogram_stdvar` stay finite.
- Broad all-series range selectors (`{__name__=~".+"}`) are rejected when the range exceeds 15
  minutes.
- A `?timeout=` duration is honored up to 5 minutes; otherwise the engine uses
  `DOCTOR_QUERY_DEFAULT_TIMEOUT` (default 20s) or the caller deadline, whichever is longer.

Provider selection:

- `source="aws"`, `source="gcp"`, and `source=~"...|..."` select the matching provider backend and
  are stripped before forwarding.
- If the local result is empty and a fallback backend is ready, the original selector is forwarded
  verbatim.
- Local + remote results are merged with `source` labels.

Router behavior:

- `Router.MetricsInstant/Range` selects backends, prunes by query labels (account/region/cluster/
  partition/backend/log group), applies circuit breakers, runs local first, then primary remotes, then
  fallback backends if the local result is empty.
- Results are merged; collisions get `shard`; partial failures become warnings.
- An associative outer aggregate (`sum`, `min`, `max`, `count`, `group`) is pushed down as its inner
  expression and applied once across the merged result. The response carries
  `X-Doctor-Federation-Aggregation: global|per-shard`. A partial fleet never produces a global
  aggregate; it returns per-shard data with a warning. `topk`, `quantile`, `stddev`, `stdvar`, and
  mixed-provider `source=~` queries remain per-shard.

### 4.2 Discovery endpoints

- `/api/v1/labels`, `/label/{name}/values`, `/series`, `/metadata` use `proxy.Router`, so local,
  static Prometheus, Guardian-discovered, AWS, and GCP backends contribute to Grafana autocomplete.
- Local discovery first tries the manifest-backed modes (`__doctor_discovery__=metric_names`,
  `label_names`, `label_values:<name>`), then falls back to sample or series-index scans.
- Discovery range is clamped to 15 minutes; 10k records / 200k series caps.
- AWS discovery adds `source` and uses `ListMetrics` for `__name__` when CloudWatch's Prometheus API
  has no label-values route.
- `/api/v1/query_exemplars` scans stored points for `trace_id`/`span_id`, capped at 500 series, 100
  exemplars/series, 5k total. It remains local-only.

## 5. Loki flow

```mermaid
flowchart TD
  A[Loki query] --> B[parseLokiPlan]
  B --> C[selector + line filters + parser stages + label filters + optional metric]
  C --> D{proxyRouter?}
  D -- yes --> E[Router.Logs]
  D -- no --> F[fetch from logEngine]
  E --> G[merge local/AWS/remotes]
  F --> G
  G --> H[apply logfmt/json/label filters locally]
  H --> I{metric query?}
  I -- yes --> J[count_over_time aggregation]
  I -- no --> K[group into Loki streams]
  J --> L[Loki JSON]
  K --> L
```

Supported LogQL subset:

- stream selector with `=`, `!=`, `=~`, `!~`;
- line filters `|=`, `!=`, `|~`, `!~`;
- parser stages recognized (and mostly ignored): `logfmt`, `json`, `line_format`, `label_format`,
  `pattern`, `regexp`, `unpack`, `decolorize`, `drop`, `keep`;
- label filter stages (`| label=...`);
- `count_over_time(...)` with optional `sum`, `sum by (...)`, and postfix grouping;
- range durations including `d`/`w`.

Storage reduction: only the selector + line filters are sent to storage. Parser/label filters and
aggregation are evaluated locally. This is why Loki responses are correct for the supported subset
but incomplete for arbitrary LogQL.

AWS logs:

- `proxy.AWSLogsForLoki` only activates when the selector matches `source="aws"`.
- log-group matchers are parsed; regex groups are resolved via the catalog or by listing groups;
- line filters become CloudWatch Logs Insights `filter @message like /.../` conditions;
- results are converted to `segment.LogRecord` with AWS partition/account/region labels.

Volume/index endpoints:

- stats and volume count stored records; chunks is always 0;
- volume_range groups by `targetLabels` and caps series/buckets;
- patterns and detected fields/labels return valid empty payloads;
- buildinfo advertises Loki 3.5.0 so Grafana enables the Drilldown UI, even though pattern mining is
  not implemented.

## 6. Tempo flow

### Search

`GET /api/search` parses either `q` (TraceQL) or `tags`/time/limit/minDuration/maxDuration.

- If a proxy router exists:
  - backends implementing `TraceQLUpstream` receive native TraceQL;
  - non-capable backends (local, AWS, GCP) receive the supported TraceQL subset folded into tag
    matchers via `ApplyTraceQL`, with warnings when a query uses unsupported constructs.
- Local, AWS, and GCP backends are merged by the router. `/v1/traces/recent` and
  `/api/search` pagination also merge a GCP trace page: `queryGCPTracePage` fetches Cloud Trace
  summaries, maps trace IDs to application-log partitions when summaries lack the Cloud Run service,
  applies the cursor/limit, and `mergeTracePagesWithGCP` combines the local/AWS/GCP pages.
- `spss` requests span sets; failures are non-fatal enrichment failures.
- Results are deduplicated by trace ID, newest first.

TraceQL subset (`internal/proxy/translate/traceql.go`):

- `{}`, `true`, `false`;
- `nestedSetParent<0`, `nestedSetParent=-1` for root spans;
- `name`, `status`, `kind`, `duration`, `service.name`/`resource.service.name`;
- `resource.<attr>`, `span.<attr>`, bare attributes;
- `=`, `!=`, `=~`, `!~`, `nil`, `!= nil`, `= nil`;
- `&&`, same-key `||`, duration comparisons.

Unsupported TraceQL constructs return 400.

### Trace detail

- `latest`, `latest-aws`, `latest-doctor`, `latest-service:<prefix>` (and their magic 32-hex aliases)
  resolve to the newest matching trace in the requested window.
- `loadFederatedTrace` checks AWS first; if found, it uses the AWS span timestamps ±30 minutes as the
  local lookup window, merges local spans, and reports a partial warning if policy hides some AWS
  spans.
- Otherwise it uses the proxy router or local store.
- `Accept: application/protobuf` returns OTLP protobuf; v2 wraps it in a Tempo `TraceByIDResponse`.
- JSON v1 returns `{batches: ...}`; JSON v2 returns `{trace: ..., status: "COMPLETE"}`.

### Tags and metrics

- Tag names/values are collected from the router or local/AWS; per-identity TTL cache (30s default).
- Tempo v2 scopes classify names into intrinsic/resource/span.
- TraceQL metrics supports `rate()`, `count_over_time()`, and `quantile_over_time(duration, q...)`,
  optional `by (...)`, and ignores `with (sample=true)`.
- Evaluation loads up to 20k spans, aggregates into buckets, and caps 500 series / 5k buckets /
  500k samples.

## 7. Native APIs

### Traces

`/v1/traces/recent`:

- parses service, partition/intent/asset, from/to, limit (default 100, max 500), cursor;
- `queryLocalTracePage` scans up to 8 batches of 2k spans, builds trace summaries, and only emits a
  trace once the scan watermark is below its earliest span start (so the summary is complete);
- `queryAWSTracePage` reads up to 200 X-Ray summaries, applies the same cursor/partition rules;
- `queryGCPTracePage` reads a Cloud Trace page (at least the AWS page limit), resolves partitions
  from GCP application logs when summaries omit the Cloud Run service, and applies the cursor;
- `mergeTracePagesWithGCP` deduplicates by canonical trace ID and applies the cursor and limit;
- returns `{returned, traces, has_more, next_cursor, page_size}`.

`/v1/traces/{id}` resolves latest magics, then uses the proxy router; returns
`{trace_id, spans}` sorted by start time.

`/v1/spans/search` searches by service and/or trace ID; with a router it first searches summaries
and hydrates each trace, then filters by service.

### Logs

`/v1/logs/search`:

- tenant, partition, intent, asset, service, free text, log group, from/to, limit 1–1000;
- rejects broad live-AWS searches with 422 if the federation planner cannot scope them;
- calls `Router.Logs`; the router owns AWS/GCP and the local backend;
- applies service/identity filters, sorts newest first, returns `{results, returned, ...}`.

`/v1/logs/services` scans 2k logs (and optionally traces) to build components, partitions,
identities, and AWS log groups. It applies the policy gate to AWS services/groups but does not
separately filter the log-derived lists beyond the already-filtered records.

### Metrics

`/v1/metrics/range` streams raw points for one metric name, caps at 200k points, returns
`{metric, points}`. Points include the stored `metric_type`, `temporality`, `start_time`, `unit`,
and `description`. It is a legacy UI convenience, not a PromQL endpoint.

`/api/v1/metadata` serves real Prometheus types to Grafana from the chunk series index:
`counter`, `gauge` (including `up_down_counter`), `histogram` (including
`exponential_histogram`), `summary`, or `untyped`. Histogram and summary suffix series are collapsed
onto the base family name, and stored help/unit are returned.

### Services

`/v1/services/discover`:

- scans up to 5k traces, 5k logs, and 10k metric points over a window; with a policy catalog
  it scans each partition with a 1k-record budget and falls back to the single global scan;
- builds a catalog of service name, canonical full name, alias, kind (runtime/asset), asset type,
  identity kind, partitions, intents, assets, cluster, pusher, last seen, trace/log counts, error
  rate, metric names, suggested dashboards;
- adds AWS X-Ray services;
- filters by policy `CanSeePartition` when enforcement is active;
- `include_assets=true` includes observation-only assets.

`/v1/services/suggest` returns canned PromQL suggestions (rate, errors, latency p50/p95/p99, log
volume, error logs) for a service name.

### Federation and proxy admin

- `/v1/federation/status`: manager status, optionally filtered by visible partitions.
- `/v1/federation/targets`: paginated target view; no tokens; optional metadata.
- `/v1/federation/plan`: planner debug; admin-only whenever a policy gate is configured.
- `/v1/proxy/endpoints`: full endpoint list or filtered page/summary; local AWS health probe is
  reported into the router; access decision is per returned page item.

### Dashboards

See [rules-dashboards.md](rules-dashboards.md).

### Guardian

- topology: fetch per-partition topology from Guardian, or all partitions concurrently (capped at 8
  concurrent fetches); map to Doctor node/edge shape with health scores.
- events: fetch history per partition, filter by window/intent, sort newest first, cap limit.
- health: aggregate history into success/failure/drift counts and a score/recommendation.
- overview: raw proxy with 1 MiB cap.
- probes: proxy Guardian `/api/probes`, filter partitions, add pusher summary (freshness ≤3 min).

### Audit status

`/v1/audit/status` returns aggregate pipeline health from the configured `StatusReader` (the
ClickHouse `audit.events` reader when `DOCTOR_AUDIT_CH_URL` is set). The payload contains
`ok`, `clickhouse`, `eventsTotal`, `eventsRecent` (last 15 minutes), `lastEventAt`,
`conflictingSequences`, and `maxEventAge`. It is 503 when the store is unreachable or `ok` is false;
`ok` is false when no event has been seen, the newest event is older than 15 minutes, or a
`(writer_id, seq)` group has more than one distinct event hash. The endpoint never returns event
content.

### Display profiles

- `/v1/display-profiles` returns `{profiles, version}` from the compiled bundle; it is 503 when the
  bundle cannot be loaded and returns the last good bundle after a refresh failure.
- `/v1/display-profiles/effective` resolves the caller's default profile from email/user and
  groups/teams, returning `{"profile": "<id>"}` (empty when no profile applies).
- Requests select a profile with `X-Strata-Display-Profile` or `?displayProfile=`. Mapping applies
  to Prometheus (`/api/v1/`), Loki (`/loki/`), Tempo (`/api/v2/`), and native `/v1/` JSON responses,
  except `/v1/audit*` and `/v1/display*`. `X-Strata-Display-Keep-Canonical: 1` retains canonical
  keys alongside display keys; the response echoes the applied profile and bundle version.
- Dashboards and Sentinel alert rules may pin `displayProfile`; the UI switches to that profile when
  opening them. Mapping changes presentation only, not values, ACLs, or stored data.

## 8. Proxy and federation data path

```mermaid
flowchart LR
  Q[query.Service] --> R[proxy.Router]
  R --> S[registry snapshot]
  S --> L[local doctor backend]
  S --> C[static config backends]
  S --> G[Guardian-discovered backends]
  S --> A[AWS federated backend]
  S --> GC[GCP federated backend]
  L --> LE[LogEngineClient]
  C --> U[Prom/Loki/Tempo/ES/BQ adapters]
  G --> U
  A --> FM[federation.Manager]
  GC --> GM[federation.Manager]
  FM --> P[selected pusher targets]
  GM --> P
```

Target selection for remote backends uses `parseSelectorCriteria` on PromQL/LogQL/TraceQL text:
equality/alternation matchers for region, account, cluster, partition, backend/shard/pusher, and
Guardian log-group names. Federated AWS/GCP backends skip this pruning because their planner owns
target selection.

Fan-out is bounded by `MaxInflight` and `MaxFanoutBackends`; local failures are fatal, remote failures
become warnings. Merge rules:

- metrics: tag remote series with `source`; add `shard` when the same label set appears from >1
  backend; skip type-mismatched results.
- logs: dedupe by timestamp/service/trace/span/body/sorted attributes; same-backend duplicates drop;
  cross-backend duplicates get shard labels.
- traces: dedupe by trace/span; same-source duplicates get shard labels; cross-source duplicates are
  merged and annotated with `sources`/`backends`.

## 9. Access filtering

Two layers can apply:

1. **`policyBackend`** decorates the local log engine:
   - logs and raw log queries → `PolicyGate.FilterLogs`;
   - traces → `PolicyGate.FilterSpans`;
   - metrics pass through.
2. **`proxy.Router`** calls the legacy `access.Gate` for remote log/trace backends. In observe mode
   denials produce warnings; in enforce mode denied backends are excluded. If every enforced backend
   is denied, the request returns `access.ErrAccessDenied` (403 in handlers).
   When the repo-managed trace policy is enforced for traces (`tracePolicyScoped`), the trace
   adapters already filter every span/summary by partition, so the legacy endpoint OWNERS gate is
   skipped for trace targets; applying both would hide federated traces from callers granted the
   partition but not the endpoint name (`internal/proxy/router_traces.go`).

Tenant selection is also bounded before authorization: `TenantGuard` fails closed to
`DOCTOR_DEFAULT_TENANT` unless an explicit allowlist or single-tenant pin is configured.

The repo policy gate also:
- rejects whole log/trace requests when enforcement is on and policy is unavailable/anonymous;
- hides federation targets/status partitions;
- hides service catalog entries by partition;
- filters Guardian probes by partition.

Metrics require authentication and are globally visible to any authenticated caller; Doctor does
not partition-scope metric data. Unless the caller authenticates, `/api/v1` is not reachable.

## 10. Error and response conventions

| Family | Success | Error shape |
|---|---|---|
| Prometheus | `{"status":"success","data":...}` | `{"status":"error","errorType":...,"error":...}`; 400 bad_data, 422 execution/broad, 503 timeout/canceled |
| Loki | `{"status":"success","data":...}` | `{"status":"error","error":...}` |
| Tempo | JSON/protobuf | `{"status":"error","error":...}` with 400/403/500 |
| Native | JSON | `{"error": "..."}`, or plain `http.Error` text for older handlers |
| Partial failures | normal success body | `X-Doctor-Partial`, `X-Doctor-Partial-Count`, `X-Doctor-Access-Observed`, `X-Doctor-Federation-Version`, `X-Doctor-Fanout-Matched`, `X-Doctor-Fanout-Selected` |

Access enforcement maps to 403; planner scope errors map to 422 in Prometheus/query handlers.

## 11. Important correctness caveats

- TraceQL search now sends native TraceQL to capable backends and the supported subset to other
  backends, reporting a warning when the subset is unsupported.
- The proxy's Prometheus series/labels/label-values paths strip Doctor-only target labels before
  forwarding to AWS.
- `handlePrometheusMetadata` reports stored metric types; AWS-only metrics remain `untyped`.
- `handlePrometheusRules` always returns no groups, even when doctor-rules has rules.
- `/ready` checks the log engine and enforced policy state, but not federation freshness or Guardian.
- `/v1/audit/events` falls back to the local JSONL audit reader unless `DOCTOR_AUDIT_CH_URL` is set;
  `/v1/audit/status` requires a reader that implements `StatusReader`, otherwise it returns 503.
- Display mapping is skipped for audit/display endpoints and does not apply when no bundle is
  configured; `Canonical`/empty profile ids leave responses untouched.
- `proxy.Router` is always installed; `DOCTOR_PROXY_ENABLED` only controls external registry
  discovery. Legacy direct branches remain as unreachable rollback code.
