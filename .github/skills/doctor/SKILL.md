---
name: doctor
description: "Privacy-first OTLP telemetry backend. Use when: working on doctor-ingest, doctor-query, doctor-indexer, doctor-compactor, OTLP ingestion, privacy engine, HMAC tokenization, trace indexing, log search, metric queries, segment storage, catalog manifests, retention compaction, or telemetry pipelines. Also use for Doctor↔MonoFS catalog integration and Doctor↔Guardian deployment manifests."
---

# Doctor — Privacy-First Telemetry Backend

## What It Does

Doctor is an OpenTelemetry Collector backend that:
- Accepts OTLP traffic over gRPC (`:4317`) and HTTP-protobuf (`:4318`)
- Anonymizes sensitive data via HMAC-based tokenization and pattern-based redaction before persistence
- Stores immutable telemetry segments as gzip-compressed JSON in S3-compatible object storage
- Maintains manifests and trace indexes in a versioned catalog (filesystem or MonoFS-backed)
- Serves query APIs for trace lookup, log search, and metric range queries

Module: `github.com/rydzu/ainfra/doctor`  
Go version: 1.25

## When to Use This Skill

- Adding or modifying OTLP ingest pipelines
- Extending the privacy engine (tokenization patterns, redaction rules)
- Working with the catalog store (manifests, trace indexes)
- Building new query endpoints or modifying existing ones
- Configuring Doctor to use MonoFS vs local filesystem
- Setting up S3 object storage
- Writing tests for ingest, privacy, or query paths
- Debugging segment storage or trace index reconciliation
- Working on compaction/retention logic

## Architecture

### Binaries

| Binary | Purpose | Default Port |
|--------|---------|-------------|
| `doctor-ingest` | OTLP receiver → privacy → write segments | gRPC `:4317`, HTTP `:4318` |
| `doctor-query` | HTTP query API for traces/logs/metrics | `:18080` |
| `doctor-indexer` | Background trace index reconciler | — (interval: 30s, lookback: 24h) |
| `doctor-compactor` | Retention cleanup worker | — (interval: 5m, retention: 7d) |
| `doctor-guardian` | Guardian integration: ingest events, topology, health | `:8090` |

### Data Flow — Ingest

```
OTLP gRPC/HTTP → tenant extraction (header or default)
  → otlp.FlattenTraces/Logs/Metrics() → privacy.Engine.Sanitize()
  → writer.WriteTraces/Logs/Metrics() → group by (tenant, service, date, hour)
    → segment.EncodeEnvelope() (gzip+JSON) → objectstore.PutObject()
    → catalog.PutManifest()
    → [traces only] catalog.UpsertTraceIndex() per traceID
```

### Data Flow — Query

```
GET /v1/traces/{traceID} → catalog.GetTraceIndex() → objectstore.GetObject() per segment
  → segment.DecodeEnvelope() → filter by traceID → sorted by StartTime

GET /v1/logs/search?q=...&from=...&to=... → catalog.ListSignalManifests()
  → objectstore.GetObject() per manifest → filter/search → sorted by timestamp desc

GET /v1/metrics/range?metric=...&from=...&to=... → catalog.ListSignalManifests()
  → objectstore.GetObject() → filter by metric name → collate points
```

### Data Flow — Indexer

```
Every 30s: for each tenant → catalog.ListSignalManifests(traces, now-24h, now+1h)
  → for each manifest → for each traceID → catalog.UpsertTraceIndex() (retry on version conflict)
```

### Data Flow — Compactor

```
Every 5m: cutoff = now - 7d
  for each signal, tenant → catalog.ListSignalManifests(time.Zero, cutoff)
  → for each manifest where MaxTime ≤ cutoff → objectstore.DeleteObject() + catalog.DeleteManifest()
```

### Data Flow — Guardian Integration

```
# Ingest (doctor-guardian or doctor-ingest)
POST /v1/guardian/events (JSON array of GuardianEvent)
  → fill defaults (tenant, timestamp) → group by (tenant, partition, date, hour)
  → segment.EncodeEnvelope(SignalGuardian) → objectstore.PutObject() → catalog.PutManifest()

# Topology query
GET /v1/guardian/topology?tenant=&partition=&from=&to=
  → catalog.ListSignalManifests(guardian, from, to) → decode envelopes
  → TopologyBuilder.BuildTopology() → nodes (partition/intent/asset) + edges (contains/joins)
  → per-node health scores from recent event ratios

# Event timeline query
GET /v1/guardian/events?tenant=&partition=&intent=&kind=&from=&to=&limit=
  → catalog.ListSignalManifests(guardian, from, to) → decode + filter → sorted by timestamp desc

# Health feedback query
GET /v1/guardian/health?tenant=&partition=&intent=&window=
  → catalog.ListSignalManifests(guardian, now-window, now) → decode + aggregate
  → HealthAdvisor.ComputeFeedback() → recommendation: proceed / hold / rollback
```

## Package Map

| Package | Path | Purpose |
|---------|------|---------|
| `config` | `internal/config/` | Env-var config loading: `LoadIngest()`, `LoadQuery()`, `LoadIndexer()`, `LoadCompactor()`, `LoadGuardian()` |
| `segment` | `internal/segment/` | Core types: `Envelope`, `SegmentManifest`, `TraceRecord`, `LogRecord`, `MetricPointRecord`, `TraceIndexEntry`, `GuardianEvent`, `DeploymentTopology`, `HealthFeedback` |
| `catalog` | `internal/catalog/` | Versioned catalog: `PutManifest()`, `GetTraceIndex()`, `UpsertTraceIndex()`, `ListSignalManifests()` |
| `ingest` | `internal/ingest/` | Service (gRPC+HTTP registration), Writer (group+encode+store+index), Guardian event writer |
| `otlp` | `internal/otlp/` | Proto flatteners: `FlattenTraces()`, `FlattenLogs()`, `FlattenMetrics()` with privacy integration |
| `privacy` | `internal/privacy/` | HMAC tokenization + pattern redaction: `SanitizeAttributes()`, `SanitizeText()`, `Tokenize()` |
| `query` | `internal/query/` | HTTP API: `/v1/traces/{traceID}`, `/v1/logs/search`, `/v1/metrics/range`, `/v1/guardian/*` |
| `guardian` | `internal/guardian/` | Topology builder + health advisor: `TopologyBuilder.BuildTopology()`, `HealthAdvisor.ComputeFeedback()` |
| `indexer` | `internal/indexer/` | Background reconciler for trace indexes |
| `lifecycle` | `internal/lifecycle/` | Retention compactor (delete expired manifests+objects) |
| `store` | `internal/store/` | Versioned catalog backend interface: `ReadFile`, `ListDir`, `Stat`, `UpsertFiles`, `DeletePaths` |
| `store/fs` | `internal/store/fs/` | Local filesystem store with `.versions/` tracking |
| `store/monofs` | `internal/store/monofs/` | MonoFS adapter via gRPC (uses Guardian token for auth) |
| `objectstore` | `internal/objectstore/` | Object storage interface: `PutObject`, `GetObject`, `DeleteObject` |
| `objectstore/fs` | `internal/objectstore/fs/` | Local filesystem object store |
| `objectstore/s3` | `internal/objectstore/s3/` | AWS S3/MinIO object store |

## Key Types

```go
// Core data
segment.Envelope         // Top-level container: signal, tenant, service, records
segment.SegmentManifest  // Metadata: ID, signal, tenant, date, hour, object key, time range, counts, traceIDs
segment.TraceRecord      // Flattened span with attributes
segment.LogRecord        // Log entry with severity, body, optional trace correlation
segment.MetricPointRecord // Metric point: gauge, sum, histogram, etc.
segment.TraceIndexEntry  // Trace→segments lookup with refs

// Guardian integration
segment.GuardianEvent    // Deployment event: kind, partition, intent, asset, status, message, metadata
segment.DeploymentNode   // Topology node: partition, intent, or asset with health score
segment.DeploymentEdge   // Topology edge: contains or joins relationship
segment.DeploymentTopology // Full graph: nodes + edges
segment.HealthScore      // Per-node health: score (0-1), successes, failures, drifts, last check
segment.HealthFeedback   // Advisor output: partition, intent, recommendation, score, counts, window

// Guardian event kinds
segment.EventStateTransition  // Intent status changed
segment.EventDriftDetected    // Asset drifted from declared state
segment.EventApplySucceeded   // Apply completed successfully
segment.EventApplyFailed      // Apply failed
segment.EventCheckFailed      // Health check failed
segment.EventRollback         // Rollback triggered
segment.EventHealthCheck      // Periodic health check
segment.EventReconcile        // Reconciliation cycle

// Storage backend
store.Store              // Versioned catalog (optimistic locking via ExpectedVersionID)
objectstore.Store        // Blob storage (S3 or filesystem)

// Privacy
privacy.Engine           // HMAC key + pattern matchers for tokenization/redaction
```

## Storage Layout

```
# Catalog (fs or MonoFS at /doctor/v1/catalog/)
manifests/{signal}/{tenant}/{date}/{hour}/{id}.json
index/traces/{tenant}/{prefix}/{traceID}.json

# Object store (S3 or fs)
{signal}/tenant={tenant}/date={date}/hour={hour}/service={service}/segment-{id}.json.gz
```

## Cross-Project Integration

### Doctor → MonoFS
- Catalog backend can use MonoFS instead of local fs: `DOCTOR_CATALOG_MODE=monofs`
- Connects to MonoFS router via gRPC at `DOCTOR_MONOFS_ROUTER_ADDR`
- Authenticates with `DOCTOR_GUARDIAN_TOKEN`
- Logical catalog path: `/doctor/v1/catalog/` (legacy `/partitions/doctor-system/catalog/` also works)

### Doctor → Guardian
- Guardian pushes deployment events to Doctor via `POST /v1/guardian/events`
- Doctor builds deployment topology graphs from ingested events (partitions, intents, assets, joins)
- Doctor computes health feedback: `GET /v1/guardian/health` returns proceed/hold/rollback recommendation
- Guardian uses health feedback to steer deployments and trigger rollbacks
- Guardian deployment manifests in `deploy/guardian/partitions/doctor-telemetry/`
- Guardian manages Doctor's infrastructure lifecycle (containers, storage, networking)

## Configuration

All via environment variables — no YAML/flags:

| Variable | Default | Purpose |
|----------|---------|---------|
| `DOCTOR_PRIVACY_HMAC_KEY` | (required for ingest) | HMAC key for deterministic tokenization |
| `DOCTOR_CATALOG_MODE` | `fs` | Catalog backend: `fs` or `monofs` |
| `DOCTOR_OBJECT_STORE_MODE` | `fs` | Object store: `fs` or `s3` |
| `DOCTOR_GRPC_ADDR` | `:4317` | Ingest gRPC listen |
| `DOCTOR_HTTP_ADDR` | `:4318` | Ingest HTTP listen |
| `DOCTOR_QUERY_HTTP_ADDR` | `:18080` | Query HTTP listen |
| `DOCTOR_MONOFS_ROUTER_ADDR` | — | MonoFS router for catalog |
| `DOCTOR_GUARDIAN_TOKEN` | — | Auth token for MonoFS/Guardian |
| `DOCTOR_S3_BUCKET` | — | S3 bucket for objects |
| `DOCTOR_S3_ENDPOINT` | — | S3 endpoint (MinIO) |
| `DOCTOR_GUARDIAN_HTTP_ADDR` | `:8090` | Guardian integration HTTP listen |

## Build & Test

```bash
# Build
make build SERVICE=doctor-ingest
go build ./cmd/doctor-ingest

# Test
go test ./...
go test ./internal/privacy -run '^TestSanitize'
go test ./internal/ingest -run '^TestWriter'
go test ./internal/guardian -v  # Guardian integration tests

# Run locally
export DOCTOR_PRIVACY_HMAC_KEY=dev-secret
go run ./cmd/doctor-ingest    # gRPC :4317, HTTP :4318
go run ./cmd/doctor-query     # HTTP :18080
go run ./cmd/doctor-indexer   # background worker
go run ./cmd/doctor-compactor # background worker
go run ./cmd/doctor-guardian  # Guardian integration :8090

# Docker
docker build --build-arg DOCTOR_SERVICE=doctor-ingest -t doctor-ingest .
```

## Conventions

- Tenant routing: extracted from configurable HTTP header, with default fallback
- Segmentation: data grouped by `(tenant, service, date, hour)` — one object per group
- ManifestID: nanosecond timestamp (`segment.NewID()`)
- Optimistic versioning: catalog writes use `ExpectedVersionID`, trace index upserts retry up to 5 times on conflict
- Privacy: HMAC-SHA256 produces deterministic `tok_{hex}` tokens; same input → same token across requests
- Sensitive key patterns: `password|secret|token|authorization|cookie|session|apikey|jwt|bearer|credential`
- Tokenizable key patterns: `email|user_id|account_id|client_id|ip|remote_addr`
- All mutations tracked with `PrincipalID` and `Reason` via `MutationContext`
