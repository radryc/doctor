# Doctor

Doctor is a self-hosted observability data plane and query gateway. It receives OpenTelemetry
telemetry, stores it through MonoFS, and serves Prometheus-, Loki-, and Tempo-compatible APIs plus an
embedded React UI. It also federates AWS CloudWatch/X-Ray and GCP Cloud Monitoring/Trace data from
Guardian-managed pushers and can proxy other observability backends.

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
- [Incidents and postmortems](docs/incidents.md)
- [Receiver plugins](docs/receiver-plugins.md)
- [Sentinel receiver spec](docs/sentinel-receiver-spec.md)
- [Partition quotas](docs/partition-quotas.md)
- [SQL console syntax](docs/sql-console-syntax.md)
- [Security architecture](docs/security-architecture.md)

## Services

| Binary | Purpose |
|---|---|
| `doctor-ingest` | OTLP gRPC/HTTP receiver, privacy filter, MonoFS writer |
| `doctor-query` | Prometheus/Loki/Tempo/native APIs, UI, proxy, federation query plane |
| `doctor-rules` | Recording-rule scheduler/evaluator |
| `doctor-sentinel` | Alert evaluation, routing, delivery, and incident/postmortem service |
| `doctor-federation-controller` | Builds AWS federation catalogs from Guardian |
| `doctor-federation-build` | Offline catalog build CLI |
| `doctor-federation-publish` | Publish a catalog snapshot |
| `doctor-loadgen` | OTLP/query load-generation CLI |

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
| `doctor-ingest` | `DOCTOR_MONOFS_LOGENGINE_ADDR`, `DOCTOR_PRIVACY_HMAC_KEY`, `DOCTOR_INGEST_TOKENS_FILE` (or `DOCTOR_INGEST_AUTH_TOKEN`) |
| `doctor-query` | `DOCTOR_MONOFS_LOGENGINE_ADDR` |
| `doctor-rules` | `DOCTOR_MONOFS_LOGENGINE_ADDR`, `DOCTOR_MONOFS_TOKEN`, `DOCTOR_RULES_TOKENS_FILE` (or `DOCTOR_RULES_API_TOKEN`) |
| `doctor-sentinel` | none for in-memory dev; `DOCTOR_MONOFS_LOGENGINE_ADDR` + `DOCTOR_MONOFS_TOKEN` for durable MonoFS state |

Authentication and access enforcement are opt-in and default to observe/open. Read
[security-architecture.md](docs/security-architecture.md) before exposing any service to a network.

## Transport security (TLS/mTLS)

Doctor uses the shared Strata `STRATA_TLS_*` contract
([`tlsx`](../tlsx)). `STRATA_TLS_MODE=off` is the default and leaves every
listener plaintext (the legacy `DOCTOR_*_TLS_*` certificate variables keep
working unchanged). Set `STRATA_TLS_MODE=required` to serve TLS only and verify
peers.

| Variable | Default | Meaning |
|---|---|---|
| `STRATA_TLS_MODE` | `off` | `off`, `optional`, `required` |
| `STRATA_TLS_CERT_FILE` | `/var/run/secrets/strata-tls/tls.crt` | leaf certificate (server, and client when available) |
| `STRATA_TLS_KEY_FILE` | `/var/run/secrets/strata-tls/tls.key` | leaf key |
| `STRATA_TLS_CA_FILE` | `/var/run/secrets/strata-tls/ca.crt` | CA bundle for peer verification |
| `STRATA_TLS_SERVER_NAME` | | override the dialed server name |
| `STRATA_TLS_REQUIRE_CLIENT_CERT` | `true` | require client certificates on servers (mTLS) |
| `STRATA_TLS_ALLOWED_SPIFFE_IDS` | | server-side peer SPIFFE allowlist |
| `STRATA_TLS_EXPECTED_SPIFFE_IDS` | | client-side server SPIFFE allowlist |

Behaviour by service:

| Service | Listener | Outbound |
|---|---|---|
| `doctor-query` | HTTPS (mTLS when a client CA is mounted) | Guardian, rules, sentinel, ingest usage |
| `doctor-ingest` | OTLP gRPC `:4317` + HTTP `:4318` | MonoFS logengine |
| `doctor-rules` | HTTPS | doctor-query, MonoFS |
| `doctor-sentinel` | HTTPS | doctor-query, Guardian, MonoFS |

In `required` mode outbound URLs configured as `http://` are upgraded to
`https://`, and the CA bundle is used as `RootCAs`. The MonoFS client keeps the
existing `DOCTOR_MONOFS_TLS_*` variables; when none of them are set and
`STRATA_TLS_MODE!=off`, it falls back to the shared client contract (CA at
minimum, client certificate when present). A bearer token is still refused over
plaintext.

Generate a local PKI and run the mTLS compose override:

```bash
(cd ../tlsx && go run ./cmd/tlsx gen --dir ../doctor/.tls --reuse-ca)
docker compose -f docker-compose.yml \
  -f deploy/guardian/partitions/doctor-telemetry/docker-compose.mtls.yml up -d
```

The generated CA and 30-day leaf certificates are for local development and
bootstrap only. Production replaces the local CA with cert-manager, Vault, or
SPIFFE-issued certificates mounted at the same paths; no application changes are
required.

