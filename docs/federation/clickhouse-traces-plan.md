# ClickHouse as the Trace Store — Minimal Integration Plan

- **Status:** Draft
- **Date:** 2026-10-22
- **Goal:** use ClickHouse for **traces only**, with the smallest possible integration
  surface and support burden.
- **Non-goals:** replacing MonoFS, moving logs/metrics, building a ClickHouse
  control plane, or operating a large ClickHouse cluster ourselves.

---

## 1. Recommendation

Treat ClickHouse as an **external trace database behind a small `TraceStore`
interface**. Do not make it a MonoFS storage engine and do not let it absorb
logs/metrics.

```
OTLP traces -> doctor-ingest -> TraceStore -> ClickHouse
logs/metrics -> doctor-ingest -> MonoFS logengine (unchanged)

Doctor query -> TraceStore -> ClickHouse (traces)
Doctor query -> MonoFS logengine (logs/metrics)

Grafana -> Doctor Tempo API -> ClickHouse
```

Why this is the simplest option:

- MonoFS keeps doing what it is good at: logs, metrics, catalog, Guardian paths.
- ClickHouse is isolated to one interface and one database schema.
- Doctor remains the only query API; Grafana keeps using the existing Tempo datasource.
- No changes to Guardian's reconciler, MonoFS storage nodes, or the federation catalog.
- We can swap the trace store later (Tempo/Jaeger) without touching the rest.

**Operational recommendation:** use **ClickHouse Cloud or a managed ClickHouse**
for production. Self-host a single-node ClickHouse only for the kind demo. Do
not run our own multi-node ClickHouse cluster unless there is a dedicated owner.

---

## 2. Scope

### In scope

- One ClickHouse database/table for spans.
- `doctor-ingest` writes spans to ClickHouse.
- `doctor-query` reads traces from ClickHouse through the existing trace APIs.
- TTL retention for spans (e.g. 30 days).
- Feature flag and fallback to MonoFS traces.
- Guardian deployment of a demo ClickHouse (or an external DSN secret).

### Deliberately out of scope (v1)

- Moving logs or metrics to ClickHouse.
- Arbitrary span-tag search / full TraceQL.
- Materialized views, projections, or custom aggregations.
- Multi-region ClickHouse, sharding, replication tuning.
- ClickHouse backup/restore automation beyond managed-service defaults.
- A ClickHouse UI. Doctor/Grafana remain the UI.
- Replacing MonoFS trace chunks historically; keep them for the transition.

---

## 3. Minimal schema

One table. No materialized views in v1.

```sql
CREATE TABLE IF NOT EXISTS strata.spans
(
    tenant              LowCardinality(String),
    partition           LowCardinality(String),
    service             LowCardinality(String),
    trace_id            String,
    span_id             String,
    parent_span_id      String,
    name                String,
    kind                LowCardinality(String),
    start_time          DateTime64(9, 'UTC'),
    end_time            DateTime64(9, 'UTC'),
    duration_ns         UInt64,
    status_code         LowCardinality(String),
    status_message      String,
    resource_attributes Map(String, String),
    attributes          Map(String, String),
    events              String, -- JSON array; avoids a second table in v1
    INDEX trace_id_idx trace_id TYPE bloom_filter GRANULARITY 4,
    INDEX service_idx service TYPE set(1000) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(start_time)
ORDER BY (tenant, service, toStartOfHour(start_time), trace_id, span_id)
TTL toDateTime(start_time) + INTERVAL 30 DAY DELETE
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1;
```

Notes:

- `trace_id` is high-cardinality; do **not** use `LowCardinality`.
- `Map(String, String)` is enough for attributes; arbitrary tag search is not a
  v1 feature.
- `events` as JSON keeps the schema to one table.
- TTL handles retention without a custom job.
- Month partitions + hourly sort key make service/time queries efficient.
- Trace-ID lookup uses the bloom filter and a bounded time range. The UI always
  has a time range; default to 24h for direct trace links if none is supplied.
- Add a separate `trace_index` table only if trace-ID lookup proves too slow:

```sql
CREATE TABLE strata.trace_index
(
    tenant String,
    trace_id String,
    min_start DateTime64(9, 'UTC'),
    max_start DateTime64(9, 'UTC'),
    span_count UInt64
)
ENGINE = ReplacingMergeTree
ORDER BY (tenant, trace_id);
```

Do not add it in v1.

---

## 4. Integration points

### 4.1 Doctor ingest

Current:

```
doctor-ingest -> MonoFSWriter -> MonoFS logengine (logs, metrics, traces)
```

Target:

```
doctor-ingest
  ├── MonoFSWriter (logs, metrics)
  └── ClickHouseTraceWriter (traces)
```

Implementation:

- Add a `TraceStore` interface in `doctor/internal/segment` or
  `doctor/internal/trace`:
  ```go
  type TraceStore interface {
      Write(ctx context.Context, spans []segment.TraceRecord) error
      Recent(ctx context.Context, q TraceQuery) ([]TraceSummary, error)
      GetTrace(ctx context.Context, traceID string, from, to time.Time) ([]segment.TraceRecord, error)
      Search(ctx context.Context, q TraceQuery) ([]TraceSummary, error)
  }
  ```
- Add `doctor/internal/store/clickhouse` implementing it with
  `clickhouse-go/v2`.
- In `doctor-ingest`, split the writer: logs/metrics to `MonoFSWriter`,
  traces to `TraceStore`. Use the existing WAL/buffer for traces; a failed
  ClickHouse insert retries from the WAL.
- Batch inserts: 1–5 seconds or 1–5k spans; use ClickHouse async insert if the
  driver supports it. One connection pool; no per-span inserts.
- Config:
  ```
  DOCTOR_TRACE_BACKEND=monofs|clickhouse|dual
  DOCTOR_CLICKHOUSE_DSN=clickhouse://user:pass@host:9000/strata
  DOCTOR_CLICKHOUSE_TABLE=strata.spans
  DOCTOR_TRACE_TTL_DAYS=30
  DOCTOR_TRACE_BATCH_SIZE=2000
  DOCTOR_TRACE_FLUSH_INTERVAL=2s
  ```
- Feature flag `dual` writes both stores for one release, then switch reads.

### 4.2 Doctor query

Current trace endpoints use `LogEngineBackend.QueryTraces` and
`QueryTraces`-style methods:

- `/v1/traces/recent`
- `/v1/traces/{id}`
- `/v1/spans/search`
- `/api/search` (Tempo)
- `/api/v2/search/tags`
- `/api/metrics/query_range` (TraceQL metrics)

Target:

- Add `Service.traceStore TraceStore` alongside `logEngine`.
- Route trace handlers through `traceStore` when the backend is ClickHouse.
- Keep logs/metrics handlers on `logEngine`.
- Implement only:
  - recent traces by service/time/partition/status
  - trace by ID (with a time range)
  - span search by service/time/status/duration
- Return empty/unsupported for TraceQL metrics and tag discovery in v1.
- Keep the existing Tempo response shapes so Grafana's datasource does not
  change.

### 4.3 Guardian deployment

Two options:

**A. External managed ClickHouse (recommended for production)**

- ClickHouse Cloud or an existing managed ClickHouse.
- Guardian stores only a secret with the DSN; no ClickHouse partition/assets.
- Doctor ingest/query get `DOCTOR_CLICKHOUSE_DSN` from the secret.
- No ClickHouse operations in Guardian.

**B. Demo ClickHouse in kind (recommended for the demo)**

- Add a `clickhouse` partition/asset to the demo:
  - `Compute` with `clickhouse/clickhouse-server` image
  - `Volume` for `/var/lib/clickhouse`
  - Service on `9000` (native) and `8123` (HTTP)
- Store the password in a Guardian `Secret`.
- Doctor points at `clickhouse:9000`.
- Single node, no replication. This is demo-only.

Do not build ClickHouse cluster orchestration into Guardian. If we ever need
HA, use a managed service or a dedicated ClickHouse operator.

### 4.4 MonoFS

- Keep MonoFS as the logs/metrics store and the federation catalog store.
- Stop writing traces to MonoFS after the dual-write transition.
- Historical trace chunks in MonoFS remain readable until their existing
  retention/cleanup; no migration required.
- The federation catalog, Guardian paths, and dashboards are unaffected.
- If ClickHouse is unavailable, fall back to MonoFS traces via the feature flag
  for one release.

---

## 5. Query semantics (v1)

Supported:

| Query | ClickHouse |
| --- | --- |
| Recent traces by service | `WHERE tenant=? AND service=? AND start_time BETWEEN ? AND ? ORDER BY start_time DESC LIMIT ?` |
| Traces by partition | `WHERE tenant=? AND partition=? ...` |
| Trace by ID | `WHERE tenant=? AND trace_id=? AND start_time BETWEEN ? AND ?` |
| Error traces | `WHERE status_code IN ('ERROR','STATUS_CODE_ERROR') ...` |
| Slow traces | `WHERE duration_ns > ? ...` |
| Span search | service/time/status/duration; no arbitrary tags |

Not supported in v1:

- `TraceQL` metrics (`/api/metrics/query_range`).
- Tag discovery over attributes.
- Join/aggregate across traces.
- Full-text span search.

These can be added later without changing the storage interface.

---

## 6. Delivery plan

### Phase 0 — Bootstrap (1–2 days)

- Run a single-node ClickHouse locally (Docker) or provision managed ClickHouse.
- Create the `strata.spans` table.
- Add `clickhouse-go/v2` to Doctor.
- Smoke test: insert a span, query by trace ID/service.

### Phase 1 — TraceStore adapter (3–4 days)

- `TraceStore` interface + ClickHouse implementation.
- Batch insert, retry, TTL.
- Unit tests with `clickhouse-go` against a local/container ClickHouse or a fake.
- No Doctor wiring yet.

### Phase 2 — Doctor ingest/query wiring (3–4 days)

- `doctor-ingest` writes traces to ClickHouse (feature flag).
- `doctor-query` trace endpoints use `TraceStore`.
- `DOCTOR_TRACE_BACKEND=monofs|clickhouse|dual`.
- Grafana Tempo datasource smoke test.
- Keep MonoFS trace fallback.

### Phase 3 — Demo deployment + retention (2–3 days)

- Guardian demo asset for single-node ClickHouse (or DSN secret).
- TTL verification.
- Backpressure/failure test: ClickHouse down, ingest buffers, recovers.
- Docs/runbook.

### Phase 4 — Optional hardening (3–5 days, only if needed)

- `trace_index` table for fast trace-ID lookup.
- Tag/attribute search.
- Managed ClickHouse HA/backup.
- Load test at target span rate.

**Total:** ~2 weeks for the demo/minimal path; ~3–4 weeks for a production
single-region setup. The 50k-account scale is mostly an ingest-rate and
retention-cost problem, not a schema problem.

---

## 7. Support burden controls

To avoid spending years supporting ClickHouse:

1. **Use managed ClickHouse** for production; self-host only the demo.
2. **One table, one schema, no materialized views/projections in v1.**
3. **No ClickHouse-specific query language in Doctor**; only parameterized SQL
   for recent/get/search.
4. **Keep `TraceStore` small**; no ClickHouse types leak into Guardian/MonoFS.
5. **TTL handles retention**; no custom cleanup job.
6. **Feature flag + MonoFS fallback**; ClickHouse can be removed without
   changing the rest of the platform.
7. **No multi-region/sharding** until a real requirement appears.
8. **Do not make ClickHouse the logs/metrics store**; that doubles the support
   surface.

---

## 8. Risks and exit strategy

| Risk | Mitigation |
| --- | --- |
| ClickHouse operational burden | Managed ClickHouse; no self-hosted cluster |
| Ingest overload / too many parts | Batch inserts, async insert, max parts, TTL |
| Trace-ID lookup slow | Time-bounded query + bloom filter; add `trace_index` only if needed |
| Schema change | `CREATE TABLE IF NOT EXISTS`; additive columns only |
| ClickHouse outage | WAL buffer in doctor-ingest; MonoFS fallback flag |
| Vendor lock-in | `TraceStore` interface; Tempo/Jaeger adapter possible |
| Cost | TTL 30 days default; sampling at collector; no logs/metrics duplication |

Exit strategy: set `DOCTOR_TRACE_BACKEND=monofs`, keep the interface, and delete
the ClickHouse adapter. No Guardian/MonoFS migration is required.

---

## 9. Decision points

1. **Managed vs self-hosted:** recommend managed ClickHouse for production;
   single-node Docker/partition for the demo.
2. **Dual-write duration:** one release, then stop MonoFS trace writes.
3. **TTL:** 30 days default; configurable per tenant later.
4. **Trace-ID lookup:** bloom filter + time range in v1; `trace_index` only if
   p95 exceeds SLO.
5. **Grafana:** keep the existing Tempo datasource; Doctor translates.

---

## 10. Bottom line

The simplest viable design is:

- **ClickHouse only for spans**
- **one table**
- **Doctor ingest/query behind a `TraceStore` interface**
- **MonoFS stays for logs/metrics/catalog**
- **Grafana keeps using Doctor's Tempo API**
- **managed ClickHouse for production, single-node for the demo**
- **TTL, batching, feature flag, MonoFS fallback**

That gives long-term trace storage without making ClickHouse a second platform
to operate.
