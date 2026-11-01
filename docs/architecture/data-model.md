# Data model and storage

## 1. Telemetry record model

All ingest, storage, query, ruler, and federation paths exchange `internal/segment` records. The
model is deliberately flat: nested OTLP structures are converted to strings at the ingest boundary.

### TraceRecord

```go
type TraceRecord struct {
    Tenant, TraceID, SpanID, ParentSpanID string
    Name, Kind, Service string
    Partition, Intent, Asset string
    StartTime, EndTime time.Time
    StatusCode, StatusMessage string
    ResourceAttributes map[string]string
    Attributes map[string]string
    Events []TraceEvent
}
```

- Trace/span IDs are canonical lowercase hex at ingest.
- `Service` is normally the canonical `<partition>.<intent>.<asset>` name; short producer names are
  preserved when no Guardian coordinates exist.
- `Partition`/`Intent`/`Asset` are the denormalized canonical identity used by access filtering.
- Events carry a name, timestamp, and attribute map.

### LogRecord

```go
type LogRecord struct {
    Tenant, Service string
    Partition, Intent, Asset string
    Timestamp time.Time
    SeverityNumber int32
    SeverityText string
    Body string
    TraceID, SpanID string
    ResourceAttributes, Attributes map[string]string
}
```

### MetricPointRecord

```go
type MetricPointRecord struct {
    Tenant, Service string
    Partition, Intent, Asset string
    TraceID, SpanID string
    MetricName, Description, Unit, Type string
    Timestamp, StartTime time.Time
    Temporality string
    Attributes, ResourceAttributes map[string]string
    HasValue bool
    Value float64
    Raw json.RawMessage
}
```

- Gauges/sums keep one point.
- Histograms become `_bucket` (with `le`), `_count`, and `_sum` series.
- Exponential histograms lose bucket detail and keep `_count`/`_sum`.
- Summaries become quantile series plus `_count`/`_sum`.
- `TraceID`/`SpanID` are exemplar correlation fields, not series identity.

### MetricSeriesMeta

Series-index metadata used by discovery and presence aggregations:

```go
type MetricSeriesMeta struct {
    Tenant, Service, Partition, Intent, Asset string
    MetricName string
    Labels map[string]string
    MinTime, MaxTime time.Time
}
```

`IsUnboundedMetricLabel` drops `trace_id`, `span_id`, `task_id`, `log_id`, `request_id`, and Guardian
task/correlation IDs from series labels. Without this, every sample would become a new series.

## 2. Identity model

### Canonical name

```text
<partition>.<intent>.<asset>
```

Examples:

```text
doctor.query.query
monofs.monofs-storage.node-a
guardian-configs.guardian.guardiand
```

Rules from `internal/identity`:

- exactly three non-empty segments;
- maximum 200 characters;
- if too long, keep a prefix and append `.` + first 10 hex characters of SHA-256(JSON([full]));
- truncation is deterministic across ingest and query.

### Identity resolution at ingest

`telemetryIdentity` fills fields from resource attributes in this order:

| Field | Fallbacks |
|---|---|
| service | `service.name`, `app.kubernetes.io/name`, deployment/statefulset/daemonset/job/cronjob, pod, instance ID |
| partition | `guardian.partition`, `partition`, `k8s.namespace.name`, `namespace` |
| intent | `guardian.intent`, `intent`, `app.kubernetes.io/component` |
| asset | `guardian.asset`, `asset`, instance ID, pod, container, `app.kubernetes.io/instance` |

If all three explicit Guardian fields exist, `CanonicalServiceName` becomes the service name and the
original service is stored as `service.alias`. If the service is already canonical, its segments
become the identity. Otherwise the fields are left as-is; Doctor does not invent identity from a pod
name alone.

### Producer vs subject identity for metrics

The metric normalizer distinguishes:

- **producer**: the component that emitted the metric (resource identity);
- **subject**: the component the metric describes (point-level `guardian.target.*` / `service`).

Resource identity wins. Point identity is rewritten as `guardian.target.partition/intent/asset/
service`, and the producer is recorded as `guardian.producer.service`. This keeps a control-plane
metric about an asset from being renamed to that asset.

### Projection into query labels

`internal/query/labels.go` is the single shared projection:

- typed record identity wins over attribute identity;
- canonical name wins over producer service; producer name becomes `service_alias`;
- partition/intent/asset are emitted both as `guardian_partition`/`guardian_intent`/
  `guardian_asset` and (for short label compatibility) `partition`/`intent`/`asset`;
- `tenant` and `source` are emitted; native telemetry defaults to `source="doctor"`;
- attributes are normalized to Prometheus-safe names (`guardian.partition` → `guardian_partition`,
  dots → underscores);
- `service_full` is the canonical identity when available.

Loki additionally applies a curated `lokiLabelAllowlist` to keep streams low-cardinality. Prometheus
keeps all normalized attributes except unbounded correlation labels.

### Query-time identity filters

Native and compatibility endpoints accept `partition`, `guardian_partition`, `intent`, and `asset`.
The native log/trace path also renders these as LogQL/stream matchers:

```text
{service="...",guardian_partition="...",guardian_intent="...",guardian_asset="..."}
```

The service discovery catalog aggregates identity from logs, traces, metrics, and AWS X-Ray.

## 3. Storage encodings

### MonoFS log-engine

`internal/store/monofs/logengine_client.go` maps `segment` records to MonoFS protobuf entries:

| Segment field | MonoFS field |
|---|---|
| `Tenant` | **dropped** (comment: “tenant parameter is ignored by monofs”) |
| `Service` | `Service` |
| `MetricName`, `Value` | `MetricName`, `Value` |
| `TraceID`, `SpanID` | typed fields for logs/traces; correlation labels for metrics |
| `Timestamp` / `StartTime`+`EndTime` | `TimestampUnixNano` / `StartUnixNano`+`EndUnixNano` |
| `ResourceAttributes`, `Attributes`, identity | merged `Labels` / `Attributes` |
| `Description`, `Unit`, `Type`, `Temporality` | **dropped** for metrics |
| `SeverityNumber` | **dropped** for logs (only `SeverityText`) |
| `Events` | **dropped** for traces |
| `Kind` | **dropped** for traces |
| `StatusCode` | kept for traces |
| `HasValue` | **dropped** (only value is sent) |

This is a lossy boundary. Query-side records are reconstructed from storage; fields absent from the
log-engine response are empty.

Chunk IDs are `chunk-<unix-nano>-<unix-nano>` for ingest and `rules-<unix-nano>-<count>` for the
ruler.

### Local WAL buffers

When buffers are used, each signal has a length-prefixed JSON WAL:

```text
uint32 little-endian payload length | JSON batch
```

- trace batch: `{id, appended_at, records: [TraceRecord]}`
- log batch: `{id, appended_at, records: [LogRecord]}`
- metric batch: `{id, appended_at, records: [MetricPointRecord]}`

`Replace` writes a temp file, fsyncs, and renames atomically. There is no checksum, version field, or
segment rotation; the WAL is a single file per signal. Replay flushes to MonoFS then truncates the
WAL.

### Dashboards

`doctor.dashboard/v1` YAML:

```yaml
version: doctor.dashboard/v1
id: ...
title: ...
tenant: default
variables: [...]
layout:
  - id: ...
    kind: timeseries|stat|toplist|gauge|table|logs|traces|topology|markdown
    gridPos: {x, y, w, h}
    queries:
      - refId: A
        datasource: prometheus|loki|tempo|native_logs|native_traces
        expr: ...
        legendFormat: ...
```

Stored at `/doctor/v1/dashboards/<tenant>/<id>.yaml`. Built-in presets have tenant `*` and are
read-only.

### Rules

`version: 1` YAML with groups:

```yaml
version: 1
groups:
  - name: group
    tenant: default
    interval: 30s
    evaluation_delay: 0s
    rules:
      - record: metric_name
        expr: sum(rate(...))
        labels: {k: v}
        sources: [...]
        merge: sum|max|min|last|keep
```

Stored at `/doctor/v1/rules/<tenant>/<group>.yaml`.

### Federation snapshot

```json
{
  "version": "controller-...",
  "generated_at": "...",
  "targets": [
    {
      "id": "aws-<account>-<region>",
      "account": "...",
      "region": "...",
      "partitions": ["..."],
      "endpoint": "https://...",
      "token_ref": "guardian-pusher://...",
      "token_env": "...",
      "capabilities": ["metrics","logs","traces"],
      "labels": {},
      "health": {},
      "metadata_fetched_at": "...",
      "metric_names": [],
      "log_groups": [],
      "xray_services": []
    }
  ]
}
```

The serialized target never includes the resolved `Token` field (`json:"-"`). The manifest adds
shard references:

```json
{
  "version": "...",
  "generated_at": "...",
  "shards": [
    {"id":"0000","path":"/doctor/v1/federation/<tenant>/shards/<publish>/0000.json","targets":500,"sha256":"..."}
  ]
}
```

Token shards are either legacy plaintext:

```json
{"version":"...","tokens":{"target-id":"token-value"}}
```

or encrypted with AES-256-GCM when `DOCTOR_FEDERATION_TOKEN_KEY` is set:

```json
{"version":"...","ciphertext":"<base64 nonce+ciphertext>"}
```

The controller requires a key for any non-empty token map and rewrites every shard so removed
targets are revoked.

### Leases and metadata cache

- controller lease: `{holder, expires_at, updated_at}`
- ruler lease: `{ruler_id, acquired_at, expires_at}`
- metadata cache: `{entries: {pusher-name: {meta, fetched_at, pusher_version}}}`

All are JSON and CAS-versioned.

## 4. Time semantics

- OTLP timestamps are converted from Unix nanoseconds to `time.Time` UTC; zero stays zero.
- Prometheus timestamps are accepted as RFC3339Nano or float seconds.
- Prometheus range queries use millisecond steps internally; response timestamps are float seconds.
- Loki timestamps are accepted as RFC3339Nano, nanoseconds, milliseconds, or seconds; responses use
  nanosecond strings for log entries and float seconds for metric samples.
- Tempo uses Unix nanosecond strings in JSON and OTLP protobuf.
- Trace pagination orders newest-first by `(MinTime, canonical trace ID)`; the cursor is
  base64url(JSON `{min_time, trace_id}`).
- The Prometheus lookback delta is 5 minutes.
- Discovery scans are clamped to 15 minutes for local metric labels/series; the default trace
  discovery window is 1 hour.

## 5. Tenancy

Doctor has a **tenant label**, not a tenant isolation boundary:

- tenant is read from `tenant` query parameter or `X-Doctor-Tenant` header (native APIs) and from
  HTTP header or gRPC metadata (ingest);
- ingest restricts tenants to `DOCTOR_INGEST_ALLOWED_TENANTS` (default: the configured default
  tenant), but the collector token is shared;
- query accepts any tenant; there is no tenant allowlist on the query plane;
- MonoFS log-engine ignores the tenant parameter entirely;
- dashboards and rules use tenant as a path segment, with no per-tenant authorization;
- the repo policy is partition-based, not tenant-based;
- metrics are globally readable by design, and metrics-based dashboards are an intended use case.

Therefore two tenants in the same MonoFS deployment are not isolated from each other unless an
external gateway enforces tenant identity and partition policy. This is a security finding, not just
a data-model detail.

## 6. Data quality and lossiness

Observed issues:

1. **Metric metadata loss** at the MonoFS boundary (unit, description, type, temporality). Query
   responses report synthesized types (`gauge`/untyped in metadata) rather than stored metadata.
2. **Log severity number loss**; only text survives.
3. **Trace kind and events loss**; Tempo output always uses empty scope and no events from storage.
4. **Canonicalization heuristics**: `dottedLabelName` converts any underscore to a dot when retrying
   series-index discovery; this can be ambiguous for labels that genuinely contain underscores.
5. **Service alias ambiguity**: short service names and canonical names can both exist; query
   projections prefer canonical but preserve aliases.
6. **AWS partition fallback**: when no policy binding matches, region is used as the partition, which
   can collide with a real partition name.
7. **Federation `byTrace` index is dead**: the map is read by `Catalog.ByTrace` but never written, so
   trace-ID planning always falls back to a full/small-catalog fan-out.
8. **AWS dimension-only metrics** are kept even when a partition filter is active; they can reveal
   metrics outside the selected partition.
9. **Trace discovery cache** entries are keyed by tenant+identity+tag and never globally evicted on
   expiry; a large user population accumulates entries.
