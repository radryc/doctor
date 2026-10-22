# ClickHouse Trace Store via MonoFS — Minimal Production Plan

- **Status:** Draft
- **Date:** 2026-10-22
- **Goal:** Guardian deploys ClickHouse; MonoFS uses ClickHouse for **traces only**;
  Doctor and Grafana keep their existing APIs.
- **Non-goals:** moving logs/metrics to ClickHouse, replacing MonoFS, building a
  ClickHouse control plane, or operating a multi-node ClickHouse cluster ourselves.

---

## 1. Architecture

```text
OTLP traces
  -> doctor-ingest
  -> MonoFS router
  -> MonoFS server
  -> ClickHouse (S3 disk)

logs/metrics
  -> doctor-ingest
  -> MonoFS router
  -> MonoFS logengine (local disk or S3)

Doctor query / Grafana Tempo
  -> Doctor
  -> MonoFS router
  -> one MonoFS server
  -> ClickHouse

Guardian
  -> ClickHouse Compute + Config + Secret + small Volume
```

Key decisions:

- **Guardian deploys ClickHouse** as a normal partition asset.
- **MonoFS is the only ClickHouse client.** Doctor does not get a ClickHouse DSN
  and does not know ClickHouse exists.
- **Doctor is unchanged** except for a tenant field added to the MonoFS trace
  protocol. Its Tempo/API surface stays the same.
- **MonoFS is the trace abstraction.** The existing `DoctorBackend` interface is
  implemented by a hybrid backend: logs/metrics go to the logengine, traces go
  to ClickHouse.
- **Traces only.** Logs and metrics remain in MonoFS.
- **S3 is the ClickHouse data tier.** The ClickHouse pod keeps only metadata and
  a small local cache; the trace data lives in an S3 bucket.

This is deliberately smaller than making Doctor a ClickHouse client: one
integration point, one schema, one query path.

---

## 2. MonoFS integration

### 2.1 Existing seam

MonoFS already has the right seam:

```go
// internal/server/doctor_backend.go
type DoctorBackend interface {
    IngestLogs(...)
    IngestMetrics(...)
    IngestTraces(...)
    StreamLogs(...)
    StreamMetrics(...)
    StreamTraces(...)
    StreamMetricSeries(...)
    QueryLogs(...)
    QueryMetrics(...)
    QueryTraces(...)
    Stats(...)
}
```

The current implementation is `*logengine.LogEngine`. Add a hybrid:

```go
type TelemetryBackend struct {
    logs   *logengine.LogEngine
    traces *clickhouse.TraceStore
}

func (b *TelemetryBackend) IngestTraces(ctx context.Context, chunkID string, spans []logengine.SpanRecord) error {
    return b.traces.Write(ctx, spans)
}

func (b *TelemetryBackend) StreamTraces(ctx context.Context, traceID, service string, from, to time.Time, limit int, yield func(logengine.SpanRecord) error) error {
    return b.traces.Stream(ctx, traceID, service, from, to, limit, yield)
}

// All other methods delegate to b.logs.
```

`monofs-server` chooses the backend at startup:

```text
MONOFS_TRACE_BACKEND=logengine   # default, current behavior
MONOFS_TRACE_BACKEND=clickhouse  # hybrid backend
```

The logengine stays enabled for logs/metrics in both modes.

### 2.2 Ingest

- `monofs-router` continues to hash trace chunks to a node.
- That node batches spans and inserts them into ClickHouse.
- All MonoFS nodes use the same ClickHouse DSN, so the trace table is global.
- Batch size 1–5k spans or 1–5 seconds; no per-span inserts.
- On ClickHouse failure, return an error to Doctor. Doctor's existing trace WAL
  (`DOCTOR_TRACE_BUFFER_WAL_DIR`) provides the retry buffer.
- No dual-write in v1. Keep `MONOFS_TRACE_BACKEND=logengine` as the rollback.

### 2.3 Query

The router currently fans out trace queries to **all** MonoFS nodes. That is
correct for sharded logengine chunks, but wrong for a shared ClickHouse table:
every node would return the same global result, producing duplicates.

Change the router when `MONOFS_TRACE_BACKEND=clickhouse`:

- `QueryTraces` / `StreamQueryTraces` pick **one healthy node**.
- Any node can serve the query because ClickHouse is shared; fail over to
  another healthy node on error.
- Ingest stays hashed; all nodes can write to the shared table.

This is a small router change. Do not query ClickHouse directly from the router
in v1; keep one client implementation in `monofs-server`.

### 2.4 Tenant isolation

The existing trace protocol has no tenant:

```proto
message SpanEntry {
  int64 start_unix_nano = 1;
  ...
  map<string, string> attributes = 9;
}
```

For production, add:

```proto
message SpanEntry {
  ...
  string tenant = 10;
}

message QueryTracesRequest {
  ...
  string tenant = 7;
}
```

Doctor sets the tenant from its existing tenant context; MonoFS writes it to the
`tenant` column and filters every query by it. Without this, all users share one
trace namespace.

If the proto change must wait, use a single `MONOFS_DEFAULT_TENANT` for the
initial demo, but do not call that production-ready.

### 2.5 MonoFS logengine S3 backend (separate workstream)

MonoFS's logengine currently uses `MockS3Store`, which is a local filesystem.
For fully stateless MonoFS nodes, add a real `S3ObjectStore` implementing the
existing `ObjectStoreBackend`:

```go
type ObjectStoreBackend interface {
    Write(ctx context.Context, path string, reader io.Reader) error
    Read(ctx context.Context, path string) (io.ReadSeekCloser, error)
    ListChunks(ctx context.Context, prefix string) ([]string, error)
}
```

This is optional for the first ClickHouse integration because ClickHouse already
removes traces from the MonoFS disks. It becomes important when logs/metrics
need to scale beyond per-node PVCs. Do not block traces on it.

---

## 3. ClickHouse schema

One table. No materialized views or projections in v1.

```sql
CREATE DATABASE IF NOT EXISTS strata;

CREATE TABLE IF NOT EXISTS strata.spans
(
    tenant          LowCardinality(String),
    timestamp       DateTime64(9, 'UTC'),
    end_time        DateTime64(9, 'UTC'),
    duration_ns     UInt64,
    trace_id        String,
    span_id         String,
    parent_span_id  String,
    service         LowCardinality(String),
    name            String,
    status_code     LowCardinality(String),
    attributes      Map(String, String),
    INDEX trace_id_idx trace_id TYPE bloom_filter GRANULARITY 4,
    INDEX service_idx service TYPE set(1000) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (tenant, service, toStartOfHour(timestamp), trace_id, span_id)
TTL toDateTime(timestamp) + INTERVAL 30 DAY DELETE
SETTINGS
    index_granularity = 8192,
    ttl_only_drop_parts = 1,
    storage_policy = 's3_main';
```

Notes:

- `trace_id` is high-cardinality; do **not** use `LowCardinality`.
- `duration_ns` is computed at ingest from `end_time - timestamp`.
- `attributes` is enough for the existing `SpanRecord`; arbitrary tag search is
  not a v1 feature.
- Daily partitions keep TTL deletes efficient. 30 days ≈ 30 active partitions.
- `ORDER BY` supports service/time queries and trace-ID lookups within a time
  range. The bloom filter helps trace-ID lookups across partitions.
- Direct trace links should pass a time range. Default to 7 days when the caller
  provides none; do not scan all history for every lookup.
- Add a `trace_index` table only if p95 trace lookup misses the SLO:

```sql
CREATE TABLE IF NOT EXISTS strata.trace_index
(
    tenant String,
    trace_id String,
    min_time DateTime64(9, 'UTC'),
    max_time DateTime64(9, 'UTC'),
    span_count UInt64
)
ENGINE = ReplacingMergeTree
ORDER BY (tenant, trace_id);
```

Do not add it in v1.

---

## 4. Production S3 backend

ClickHouse uses an **S3 disk** for table data and a **small local volume** for
metadata, S3 disk metadata, and cache. The local volume must be durable; S3 is
not a backup of ClickHouse's local metadata.

### 4.1 ClickHouse storage config

`/etc/clickhouse-server/config.d/guardian.xml`:

```xml
<clickhouse>
  <listen_host>0.0.0.0</listen_host>
  <max_server_memory_usage>6442450944</max_server_memory_usage>
  <mark_cache_size>1073741824</mark_cache_size>
  <uncompressed_cache_size>0</uncompressed_cache_size>

  <logger>
    <level>information</level>
    <console>1</console>
  </logger>

  <prometheus>
    <endpoint>/metrics</endpoint>
    <port>9363</port>
  </prometheus>

  <storage_configuration>
    <disks>
      <s3>
        <type>s3</type>
        <endpoint>https://s3.us-east-1.amazonaws.com/strata-clickhouse-prod/clickhouse/</endpoint>
        <region>us-east-1</region>
        <use_environment_credentials>true</use_environment_credentials>
        <metadata_path>/var/lib/clickhouse/disks/s3/</metadata_path>
      </s3>
      <s3_cache>
        <type>cache</type>
        <disk>s3</disk>
        <path>/var/lib/clickhouse/disks/s3_cache/</path>
        <max_size>20Gi</max_size>
      </s3_cache>
    </disks>
    <policies>
      <s3_main>
        <volumes>
          <main>
            <disk>s3_cache</disk>
          </main>
        </volumes>
      </s3_main>
    </policies>
  </storage_configuration>
</clickhouse>
```

- AWS credentials come from `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` env
  vars (mounted from a Guardian Secret) or an instance role.
- For MinIO, use `http://minio.minio.svc.cluster.local:9000/strata-clickhouse/`
  and the MinIO access/secret keys. The endpoint needs the bucket root.
- `s3_cache` keeps hot data local. Set `max_size` to fit the node's local
  volume; do not make it larger than the PVC.
- The table's `storage_policy = 's3_main'` makes S3 the source of truth.

### 4.2 Small production sizing

Initial single-node deployment:

| Resource | Value |
| --- | --- |
| ClickHouse CPU | 2–4 vCPU |
| ClickHouse memory | 4–8 GiB |
| Local volume | 20–50 GiB (metadata + cache) |
| S3 | unlimited; trace data |
| ClickHouse replicas | 1 (not HA) |
| MonoFS nodes | 2–3 |
| Trace TTL | 30 days |

This is intentionally small. S3 removes the storage scaling problem; the
remaining scaling levers are ClickHouse CPU/memory and read replicas.

### 4.3 Scaling path

1. **Vertical:** increase ClickHouse CPU/memory and local cache.
2. **Read replicas:** add ClickHouse replicas with `ReplicatedMergeTree` and
   Keeper, or move to ClickHouse Cloud. MonoFS points at a load-balanced DSN;
   no code change.
3. **Managed:** ClickHouse Cloud is the recommended production option if HA,
   backups, and upgrades should not be our problem.
4. **MonoFS:** trace nodes are stateless with respect to ClickHouse; add/remove
   MonoFS nodes freely. Router reads one node; writes are hashed.

A single self-hosted ClickHouse node is **not** highly available. If trace
queries are business-critical, use ClickHouse Cloud or a 3-node cluster from the
start. Do not build cluster orchestration into Guardian.

### 4.4 S3 bucket and IAM

Bucket:

```text
s3://strata-clickhouse-prod/clickhouse/
```

Recommended bucket settings:

- Block all public access.
- SSE-S3 or SSE-KMS encryption.
- Versioning on.
- Lifecycle: abort incomplete multipart uploads after 7 days; expire
  noncurrent versions after 30 days.
- Do **not** set a lifecycle rule that deletes current objects; ClickHouse TTL
  owns retention.

IAM policy for the ClickHouse role/user:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:ListBucket",
        "s3:GetBucketLocation",
        "s3:AbortMultipartUpload",
        "s3:ListMultipartUploadParts"
      ],
      "Resource": [
        "arn:aws:s3:::strata-clickhouse-prod",
        "arn:aws:s3:::strata-clickhouse-prod/*"
      ]
    }
  ]
}
```

Use a dedicated role/user. Do not reuse the MonoFS bucket credentials.

### 4.5 Backup and recovery

S3 stores data parts, but not everything:

- ClickHouse metadata and S3 disk metadata live under `/var/lib/clickhouse`.
- Back up that local volume, or use ClickHouse Cloud/managed backups.
- For self-hosted, use `clickhouse-backup` to S3 on a schedule, or snapshot the
  metadata PVC.
- Test restore of a single table before calling it production-ready.

---

## 5. Query semantics (v1)

MonoFS's existing trace API supports `traceID`, `service`, time range, and limit.
Implement exactly that in ClickHouse first:

| Query | ClickHouse |
| --- | --- |
| Trace by ID | `WHERE tenant=? AND trace_id=? AND timestamp BETWEEN ? AND ?` |
| Recent by service | `WHERE tenant=? AND service=? AND timestamp BETWEEN ? AND ? ORDER BY timestamp DESC LIMIT ?` |
| Recent all | `WHERE tenant=? AND timestamp BETWEEN ? AND ? ORDER BY timestamp DESC LIMIT ?` |
| Span stream | Same filters, stream rows in timestamp order |

Not in v1:

- Arbitrary attribute/tag search.
- TraceQL metrics.
- Aggregations across traces.
- Full-text span search.
- Cross-tenant queries.

These can be added later without changing the MonoFS/Doctor API.

---

## 6. Failure behavior

| Failure | Behavior |
| --- | --- |
| ClickHouse down | MonoFS returns error; Doctor trace WAL retries; logs/metrics unaffected |
| One MonoFS node down | Router fails over trace read to another node; ingest rehashes |
| S3 unavailable | ClickHouse inserts/queries fail; same as ClickHouse down |
| Local cache full | ClickHouse evicts cache; S3 remains source of truth |
| Schema mismatch | Additive `ALTER TABLE` only; startup checks fail fast |
| Bad deploy | Roll back `MONOFS_TRACE_BACKEND=logengine`; traces return to MonoFS |

---

## 7. Delivery plan

### Phase 0 — MonoFS ClickHouse backend, local disk (4–6 days)

- Add `clickhouse-go/v2` to MonoFS.
- Implement `TraceStore` (write, query, stream, schema bootstrap).
- Add hybrid `DoctorBackend`.
- Add `MONOFS_TRACE_BACKEND` and ClickHouse config.
- Unit/integration tests against a local ClickHouse container.
- No S3, no Guardian yet.

### Phase 1 — Router + tenant + protocol (3–4 days)

- Single-node trace read in `monofs-router` when ClickHouse is enabled.
- Add `tenant` to `SpanEntry` and `QueryTracesRequest`; update Doctor client.
- Verify no duplicate results with 2+ MonoFS nodes.
- Grafana/Tempo smoke test through Doctor.

### Phase 2 — Guardian + S3 (3–5 days)

- Add `clickhouse` partition: Compute, Config, Secret, small Volume.
- Configure S3 disk and cache.
- Wire ClickHouse DSN into MonoFS nodes.
- Verify TTL, restart, and S3 persistence.
- Document IAM/bucket setup.

### Phase 3 — Production hardening (3–5 days)

- Load test at expected spans/sec.
- Tune batch size, cache, memory limits, parts.
- Add alerts: insert failures, query p95, disk cache, part count, S3 errors.
- Backup/restore test.
- Runbook.

**Total:** ~3–4 weeks for a small single-node production setup. HA/managed
ClickHouse is a deployment choice, not additional code.

---

## 8. Support-burden controls

1. **One client:** MonoFS. Doctor and Guardian do not import ClickHouse code.
2. **One table:** `strata.spans`; no materialized views or projections in v1.
3. **One storage policy:** S3 disk + local cache.
4. **TTL owns retention:** no custom cleanup jobs.
5. **No cluster orchestration in Guardian:** use managed ClickHouse or a
   dedicated operator for HA.
6. **Feature flag + rollback:** `MONOFS_TRACE_BACKEND=logengine`.
7. **No logs/metrics in ClickHouse:** keeps the support surface small.
8. **No arbitrary tag search in v1:** add only when a concrete query requires it.

---

## 9. Decision points

1. **Self-hosted vs managed:** recommended managed ClickHouse for production;
   self-hosted single node is acceptable for a small initial deployment.
2. **S3 vs local disk:** S3 for production; local disk for the first MonoFS
   integration tests.
3. **Tenant protocol:** required for production; do not ship multi-tenant traces
   without it.
4. **Trace retention:** 30 days default; adjust based on cost.
5. **Router read:** one node when ClickHouse is shared; do not fan out.
6. **MonoFS logengine S3:** separate workstream; not a blocker for traces.

---

## 10. Bottom line

The smallest production-shaped design is:

- **Guardian deploys ClickHouse** (generic Compute + Config + Secret + small
  Volume).
- **MonoFS uses ClickHouse for traces** through its existing `DoctorBackend`.
- **Doctor and Grafana are unchanged** except for tenant propagation.
- **ClickHouse data lives in S3**; only metadata/cache is local.
- **One table, TTL, batch inserts, one reader node, feature flag.**
- **Scale by adding ClickHouse compute/replicas or moving to managed
  ClickHouse; do not rewrite the platform.**

That gives production S3-backed trace storage without turning ClickHouse into a
second platform to operate.
