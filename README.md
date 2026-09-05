# Doctor

> Part of the **Strata** platform.

Doctor is a privacy-first telemetry backend for OpenTelemetry Collector.

It accepts OTLP traffic, anonymizes sensitive data before persistence, and stores telemetry in MonoFS logengine. The query service exposes trace lookup, log search, metric range queries, and Guardian deployment health APIs, with a built-in browser UI.

## Binaries

| Binary | Purpose | Default Port |
|--------|---------|-------------|
| `doctor-ingest` | OTLP receiver → privacy → write to MonoFS logengine | gRPC `:4317`, HTTP `:4318` |
| `doctor-query` | HTTP query API + browser UI | `:18080` |

## Repository layout

```text
cmd/
  doctor-ingest/
  doctor-query/

frontend/            React/TypeScript UI (Vite)
internal/query/ui/   Embedded UI assets (built from frontend/)

deploy/
  guardian/
    partitions/
      doctor-telemetry/
```

## Prerequisites

Doctor depends on MonoFS logengine for all storage. Both repos must be checked out as siblings:

```bash
git clone <monofs-repo-url> monofs
git clone <doctor-repo-url> doctor
```

`go.mod` contains `replace github.com/radryc/monofs => ../monofs`, so `go build` and `go test` require the sibling checkout to be present.

## Running locally

Both services require `DOCTOR_MONOFS_LOGENGINE_ADDR` pointing at a running MonoFS logengine instance.

```bash
export DOCTOR_PRIVACY_HMAC_KEY=dev-secret
export DOCTOR_MONOFS_LOGENGINE_ADDR=localhost:9090
go run ./cmd/doctor-ingest
```

That starts:

- OTLP gRPC on `:4317`
- OTLP HTTP-protobuf on `:4318`
- Guardian event ingest on `POST /v1/guardian/events`

Run the query API in a second terminal:

```bash
export DOCTOR_MONOFS_LOGENGINE_ADDR=localhost:9090
go run ./cmd/doctor-query
```

That serves on `:18080` by default. Open `http://localhost:18080/` for the browser UI.

For tail-based trace sampling, put an OpenTelemetry Collector in front of `doctor-ingest` and apply the example config in `deploy/guardian/partitions/doctor-telemetry/examples/otel-collector.yaml`. Doctor stores whatever traces it receives; sampling decisions belong in the upstream collector pipeline.

## Environment variables

### Storage (required)

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_MONOFS_LOGENGINE_ADDR` | MonoFS logengine gRPC address | **required** |

### Ingest

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_GRPC_ADDR` | OTLP gRPC listen address | `:4317` |
| `DOCTOR_HTTP_ADDR` | OTLP HTTP-protobuf listen address | `:4318` |
| `DOCTOR_TENANT_HEADER` | HTTP/gRPC metadata key for tenant routing | `X-Doctor-Tenant` |
| `DOCTOR_DEFAULT_TENANT` | Fallback tenant when no header is set | `default` |
| `DOCTOR_PRIVACY_HMAC_KEY` | HMAC key for deterministic tokenization | **required** |

### Write buffers (ingest)

All three signals (traces, logs, metrics) have independent WAL-backed buffers that absorb bursts and flush on size or time boundaries.

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_TRACE_BUFFER_ENABLED` | Enable trace WAL buffer | `true` |
| `DOCTOR_TRACE_BUFFER_WAL_DIR` | WAL directory for trace batches | `.doctor/trace-buffer` |
| `DOCTOR_TRACE_BUFFER_FLUSH_INTERVAL` | Maximum time traces stay buffered | `5s` |
| `DOCTOR_TRACE_BUFFER_MAX_RECORDS` | Flush after this many trace records | `4096` |
| `DOCTOR_TRACE_BUFFER_MAX_BYTES` | Flush after this many estimated bytes | `4194304` |
| `DOCTOR_LOG_BUFFER_ENABLED` | Enable log WAL buffer | `true` |
| `DOCTOR_LOG_BUFFER_WAL_DIR` | WAL directory for log batches | `.doctor/log-buffer` |
| `DOCTOR_LOG_BUFFER_FLUSH_INTERVAL` | Maximum time logs stay buffered | `5s` |
| `DOCTOR_LOG_BUFFER_MAX_BYTES` | Flush after this many estimated bytes | `4194304` |
| `DOCTOR_METRIC_BUFFER_ENABLED` | Enable metric WAL buffer | `true` |
| `DOCTOR_METRIC_BUFFER_WAL_DIR` | WAL directory for metric batches | `.doctor/metric-buffer` |
| `DOCTOR_METRIC_BUFFER_FLUSH_INTERVAL` | Maximum time metrics stay buffered | `5s` |
| `DOCTOR_METRIC_BUFFER_MAX_BYTES` | Flush after this many estimated bytes | `4194304` |

### Ingest pipeline

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_PIPELINE_ENABLED` | Enable async write pipeline with backpressure | `true` |
| `DOCTOR_PIPELINE_QUEUE_SIZE` | In-flight segment queue depth | `1000` |
| `DOCTOR_PIPELINE_WORKERS` | Writer goroutine count | `4` |
| `DOCTOR_WAL_ENABLED` | Enable legacy WAL (superseded by per-signal buffers) | `false` |
| `DOCTOR_WAL_DIR` | Legacy WAL directory | `.doctor/wal` |

### Cost controls (ingest)

All limits are disabled by default (0 = no limit).

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_COST_MAX_SEGMENTS` | Reject writes when pending segment count exceeds this | `0` |
| `DOCTOR_COST_MAX_SEGMENTS_PER_HOUR` | Reject writes when segment rate exceeds this | `0` |
| `DOCTOR_COST_MAX_TOTAL_BYTES` | Reject writes when total object bytes exceeds this | `0` |

### Query

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_QUERY_HTTP_ADDR` | Query HTTP listen address | `:18080` |
| `DOCTOR_DEFAULT_TENANT` | Default tenant for queries without an explicit tenant | `default` |
| `DOCTOR_DEFAULT_PARTITION` | Default Guardian partition shown in the UI | unset |
| `DOCTOR_GUARDIAN_UI_URL` | URL to the Guardian UI (used by the Doctor UI for deep links) | unset |
| `DOCTOR_GUARDIAN_API_URL` | URL to the Guardian API (used for live topology data) | unset |

### Self-telemetry (both services)

Doctor can emit its own OTLP traces, metrics, and logs. All telemetry is disabled when `DOCTOR_OTEL_ENDPOINT` is unset.

| Variable | Purpose | Default |
| --- | --- | --- |
| `DOCTOR_OTEL_ENDPOINT` | OTLP gRPC endpoint for Doctor's own telemetry | unset (disabled) |
| `DOCTOR_OTEL_SERVICE_NAME` | Service name reported in self-telemetry | `doctor` |
| `DOCTOR_OTEL_INSECURE` | Use plaintext gRPC for OTLP export | `true` |
| `DOCTOR_OTEL_METRIC_INTERVAL` | Self-metric collection interval | `15s` |

## HTTP endpoints

### Ingest (`doctor-ingest`, default `:4317` / `:4318`)

| Endpoint | Purpose |
| --- | --- |
| `POST /v1/traces` | OTLP HTTP trace ingest |
| `POST /v1/logs` | OTLP HTTP log ingest |
| `POST /v1/metrics` | OTLP HTTP metric ingest |
| `POST /v1/guardian/events` | Guardian deployment event ingest (JSON array) |
| `GET /healthz` | Liveness |
| `GET /api/v1/status/buildinfo` | Build metadata |

### Query (`doctor-query`, default `:18080`)

| Endpoint | Purpose |
| --- | --- |
| `GET /` | Browser UI |
| `GET /healthz` | Liveness |
| `GET /ready` | Readiness |
| `GET /api/v1/status/buildinfo` | Build metadata |
| `GET /v1/spans/search?tenant=&service=&trace_id=&from=&to=&q=&limit=` | Span search |
| `GET /v1/traces/recent?tenant=` | Recently ingested traces |
| `GET /v1/traces/{traceID}?tenant=` | Trace lookup by ID |
| `GET /v1/logs/services?tenant=` | List services that have logs |
| `GET /v1/logs/search?tenant=&service=&from=&to=&q=` | Log search |
| `GET /v1/metrics/range?tenant=&metric=&from=&to=` | Metric range query |
| `GET /v1/guardian/topology?tenant=&partition=&from=&to=` | Deployment topology |
| `GET /v1/guardian/events?tenant=&partition=&intent=&kind=&from=&to=&limit=` | Guardian event timeline |
| `GET /v1/guardian/health?tenant=&partition=&intent=&window=` | Rollout health feedback |
| `GET /v1/ui/config` | UI runtime configuration |

The query service also exposes read-only Prometheus and Loki compatibility APIs for Grafana datasource integration:

| Prefix | Purpose |
| --- | --- |
| `GET /api/v1/query`, `/api/v1/query_range`, `/api/v1/labels`, `/api/v1/label/*`, `/api/v1/series`, `/api/v1/metadata` | Prometheus HTTP API |
| `GET /loki/api/v1/query`, `/loki/api/v1/query_range`, `/loki/api/v1/labels`, `/loki/api/v1/label/*`, `/loki/api/v1/series` | Loki HTTP API |
| `GET /api/v2/services`, `/api/v2/spans`, `/api/v2/traces`, `/api/v2/trace/*` | Zipkin HTTP API |

## Correlation verification query

Use the verification script to confirm one trace carries the same correlation IDs across traces, logs, and metrics:

```bash
cd doctor
chmod +x scripts/verify-correlation.sh
./scripts/verify-correlation.sh
```

Optional inputs:

```bash
DOCTOR_QUERY_URL=http://localhost:18080 \
DOCTOR_TENANT=default \
DOCTOR_SERVICE=guardian \
TRACE_ID=<known-trace-id> \
VERIFY_FROM=2026-08-11T10:00:00Z \
VERIFY_TO=2026-08-11T11:00:00Z \
./scripts/verify-correlation.sh
```

What it verifies:

- Trace lookup returns spans for the selected trace ID.
- Log search returns records whose `trace_id` matches that trace ID.
- Prometheus-compatible series discovery finds metric series labeled with that same `trace_id`.

Equivalent raw verification query for metrics:

```bash
curl -s "http://localhost:18080/api/v1/series?tenant=default&match[]={trace_id=\"<trace-id>\"}" | jq
```

If metrics return no matching `trace_id`, ensure OTLP exemplars are enabled in your SDK/collector pipeline.

## Building containers

The Dockerfile requires access to the MonoFS checkout because of the `go.mod` replace directive. Pass it as a named build context:

```bash
docker buildx build \
  --build-context monofs=../monofs \
  --build-arg DOCTOR_SERVICE=doctor-ingest \
  -t doctor-ingest:latest .

docker buildx build \
  --build-context monofs=../monofs \
  --build-arg DOCTOR_SERVICE=doctor-query \
  -t doctor-query:latest .
```

The Makefile wraps this:

```bash
make docker-build SERVICE=doctor-ingest MONOFS_DIR=../monofs
make docker-build SERVICE=doctor-query  MONOFS_DIR=../monofs
```

Pass `BUILD_VERSION`, `BUILD_COMMIT`, and `BUILD_TIME` to embed version metadata reported by `/api/v1/status/buildinfo`.

## Guardian deployment

Guardian manifests live under:

```text
deploy/guardian/partitions/doctor-telemetry/
```

These manifests assume a running MonoFS logengine and a Guardian token for auth. An example Collector pipeline is provided in `deploy/guardian/partitions/doctor-telemetry/examples/otel-collector.yaml`.
