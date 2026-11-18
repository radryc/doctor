# Doctor engineering documentation

This documentation set is derived from the source tree and is updated with the code. The previous
`docs/` tree and the root `README.md` were replaced during the October 2026 documentation pass.

Scope:

- every Go package under `cmd/` and `internal/`
- the React/TypeScript frontend under `frontend/src`
- deployment, build, scripts, and CI files
- runtime contracts with MonoFS, Guardian, guardian-pushers, OIDC, and Grafana

The statements below are based on the code as it exists in this working tree. Where a behavior is
surprising, broken, duplicated, or missing, the docs call it out explicitly instead of describing
the intended design.

## Document map

| Document | Audience | Contents |
|---|---|---|
| [architecture/overview.md](architecture/overview.md) | everyone | system context, binaries, layers, invariants, deployment topology |
| [architecture/components.md](architecture/components.md) | engineers | package-by-package responsibilities, interfaces, storage layout, concurrency |
| [architecture/data-model.md](architecture/data-model.md) | engineers | telemetry records, identity, labels, storage encodings, tenancy |
| [logical-flows/ingest.md](logical-flows/ingest.md) | ingest/SRE | OTLP request to MonoFS, buffering, privacy, error mapping |
| [logical-flows/query.md](logical-flows/query.md) | query/SRE | auth chain, Prometheus/Loki/Tempo/native routes, local + proxy + AWS paths |
| [logical-flows/federation.md](logical-flows/federation.md) | platform/SRE | catalog controller, snapshots, tokens, planner, fan-out |
| [logical-flows/rules-dashboards.md](logical-flows/rules-dashboards.md) | platform/SRE | recording rules, dashboards, CAS storage |
| [logical-flows/ui.md](logical-flows/ui.md) | frontend/SRE | page routes, data dependencies, refresh/correlation behavior |
| [alerts.md](alerts.md) | SRE/operators | Sentinel alert sources, rules, silences, routing, and delivery |
| [incidents.md](incidents.md) | SRE/responders | alert sets, incident lifecycle, partition policy, agent-assisted postmortems, plugins |
| [receiver-plugins.md](receiver-plugins.md) | platform/extension authors | receiver interface, registry, webhook and Android extension guide |
| [sentinel-receiver-spec.md](sentinel-receiver-spec.md) | integration authors | HTTP webhook and receiver wire contract |
| [partition-quotas.md](partition-quotas.md) | platform/SRE | partition, user, group, and client quota model, enforcement, dashboard |
| [sql-console-syntax.md](sql-console-syntax.md) | console users / SRE | SQL dialect, quoting, datasets, label/time filters, limits |
| [design/graph-spec.md](design/graph-spec.md) | graph authors / agents | importable graph, dashboard, and saved-graph bundle schemas, query rules, and examples |
| [security-architecture.md](security-architecture.md) | security/SRE | trust boundaries, authentication, authorization, secrets, threats, controls |

## Repository map

```
cmd/                         service entrypoints
  doctor-ingest/             OTLP receiver + writer
  doctor-query/              query HTTP API + embedded UI + proxy/federation wiring
  doctor-rules/              recording-rule evaluator
  doctor-sentinel/           alert evaluation, routing, delivery, and incidents
  doctor-loadgen/            OTLP/query load-generation CLI
  doctor-federation-build/   offline catalog build CLI
  doctor-federation-controller/ periodic Guardian-to-catalog controller
  doctor-federation-publish/ publish an existing snapshot
internal/
  access/ auth/              query authentication and partition authorization
  alerting/                  Sentinel alert rules, intake, silences, routing, outbox
  config/ buildinfo/         environment configuration and build metadata
  dashboards/                portable dashboard schema and CAS store
  delivery/                  notification receiver registry and channel senders
  federation/                AWS/GCP multi-target catalog, planner, fan-out, tokens
  identity/                  canonical <partition>.<intent>.<asset> names
  incident/                  alert sets, incident lifecycle, postmortem engines
  ingest/ otlp/ privacy/     OTLP flattening, sanitization, buffering, writing
  loadtest/                  opt-in federation, proxy, and rules load tests
  netguard/                  outbound URL/SSRF validation policy
  proxy/ translate/          multi-backend router, adapters, query translation
  query/                     Prometheus/Loki/Tempo/SQL/native HTTP handlers
  quota/                     per-partition and subject admission/storage quotas
  ruler/                     rule storage, scheduling, evaluation, leader lease
  segment/                   shared telemetry record types
  store/                     MonoFS gRPC clients, AWS/GCP pusher clients
  telemetry/ telemetrylabels/ Doctor's own OTLP telemetry and canonical label helpers
  tempo/ tracecontext/       Tempo and X-Ray/W3C wire-format helpers
  wal/                       length-prefixed JSON write-ahead logs
frontend/src/                React 19 + Vite UI, embedded at build time
deploy/                      Guardian partition metadata, Kubernetes, and kind manifests
scripts/                     curl-based contract/correlation verification
.github/                     Go tests and federation load-test workflows
```

## Build and run (from the code)

```bash
# Go tests. The go.mod replaces github.com/radryc/monofs with ../monofs.
go test ./...

# Build one service (SERVICE=doctor-ingest|doctor-query|doctor-rules|doctor-sentinel)
make build SERVICE=doctor-query

# Build the frontend into internal/query/ui/dist, then the container images.
make frontend
make image-all MONOFS_DIR=../monofs

# Local federation stress suite (opt-in)
make loadtest-federation
```

Runtime requirements observed in code:

| Service | Required configuration | Listens on |
|---|---|---|
| `doctor-ingest` | `DOCTOR_MONOFS_LOGENGINE_ADDR`, `DOCTOR_PRIVACY_HMAC_KEY`, `DOCTOR_INGEST_TOKENS_FILE` (or `DOCTOR_INGEST_AUTH_TOKEN`) | gRPC `:4317`, HTTP `:4318`, pprof `127.0.0.1:6060` |
| `doctor-query` | `DOCTOR_MONOFS_LOGENGINE_ADDR` | HTTP `:18080` |
| `doctor-rules` | `DOCTOR_MONOFS_LOGENGINE_ADDR`, `DOCTOR_MONOFS_TOKEN`, `DOCTOR_RULES_TOKENS_FILE` (or `DOCTOR_RULES_API_TOKEN`) | HTTP `:18081` |
| `doctor-sentinel` | none for in-memory dev; `DOCTOR_MONOFS_LOGENGINE_ADDR` + `DOCTOR_MONOFS_TOKEN` for durable MonoFS state | HTTP `:18082` |
| `doctor-federation-controller` | Guardian URL/token, MonoFS router/token | no listener; periodic job |
| `doctor-federation-build` | input YAML/JSON | no listener; CLI |
| `doctor-federation-publish` | snapshot file, MonoFS router/token | no listener; CLI |
| `doctor-loadgen` | target endpoint | CLI (optional metrics HTTP listener) |

See [architecture/components.md](architecture/components.md) for the full configuration matrix and
[logical-flows/query.md](logical-flows/query.md) for every HTTP route.

## Reading order

1. [architecture/overview.md](architecture/overview.md)
2. [architecture/data-model.md](architecture/data-model.md)
3. [logical-flows/ingest.md](logical-flows/ingest.md)
4. [logical-flows/query.md](logical-flows/query.md)
5. [security-architecture.md](security-architecture.md)
