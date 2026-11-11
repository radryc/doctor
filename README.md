# Doctor

Doctor is a self-hosted observability data plane and query gateway. It receives OpenTelemetry
telemetry, stores it through MonoFS, and serves Prometheus-, Loki-, and Tempo-compatible APIs plus an
embedded React UI. It also federates AWS CloudWatch/X-Ray data from Guardian-managed pushers and can
proxy other observability backends.

## Documentation

The current docs are derived from the source and updated with the code:

- [Documentation index](docs/README.md)
- [Architecture overview](docs/architecture/overview.md)
- [Components and interfaces](docs/architecture/components.md)
- [Data model](docs/architecture/data-model.md)
- [Ingest flow](docs/logical-flows/ingest.md)
- [Query flow](docs/logical-flows/query.md)
- [Federation flow](docs/logical-flows/federation.md)
- [Rules and dashboards](docs/logical-flows/rules-dashboards.md)
- [UI flow](docs/logical-flows/ui.md)
- [Alerts](docs/alerts.md)
- [Partition quotas](docs/partition-quotas.md)
- [SQL console syntax](docs/sql-console-syntax.md)
- [Security architecture](docs/security-architecture.md)

## Services

| Binary | Purpose |
|---|---|
| `doctor-ingest` | OTLP gRPC/HTTP receiver, privacy filter, MonoFS writer |
| `doctor-query` | Prometheus/Loki/Tempo/native APIs, UI, proxy, federation query plane |
| `doctor-rules` | Recording-rule scheduler/evaluator |
| `doctor-federation-controller` | Builds AWS federation catalogs from Guardian |
| `doctor-federation-build` | Offline catalog build CLI |
| `doctor-federation-publish` | Publish a catalog snapshot |

## Build and test

```bash
# Unit tests (requires ../monofs for the go.mod replace directive)
go test ./...

# One service
make build SERVICE=doctor-query

# Frontend into internal/query/ui/dist
make frontend

# Containers (requires ../monofs)
make image-all MONOFS_DIR=../monofs
```

## Minimum runtime configuration

| Service | Required |
|---|---|
| `doctor-ingest` | `DOCTOR_MONOFS_LOGENGINE_ADDR`, `DOCTOR_PRIVACY_HMAC_KEY`, `DOCTOR_INGEST_AUTH_TOKEN` |
| `doctor-query` | `DOCTOR_MONOFS_LOGENGINE_ADDR` |
| `doctor-rules` | `DOCTOR_MONOFS_LOGENGINE_ADDR`, `DOCTOR_MONOFS_TOKEN`, `DOCTOR_RULES_API_TOKEN` |

Authentication and access enforcement are opt-in and default to observe/open. Read
[security-architecture.md](docs/security-architecture.md) before exposing any service to a network.
