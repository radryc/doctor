# TODO — remaining findings

This is the actionable backlog for findings that were **not** fixed in the remediation pass. The
completed work is recorded in [`docs/internal/remediation-log.md`](docs/internal/remediation-log.md).
Check an item only when code, tests, and docs are all updated.

Priority key:

- **P0** — security/data-integrity blocker for an Internet-facing deployment
- **P1** — important correctness, isolation, or operability gap
- **P2** — hardening, scale, or maintainability work

## Accepted design decisions (not TODO)

### Metrics are globally readable

Metrics are intentionally readable without partition filtering. `/api/v1` is intentionally exempt
from bearer enforcement, and the shared policy evaluator always allows `SignalMetrics`. Metrics-based
dashboards are a supported product use case and must remain usable in the intended deployment.

Consequences accepted by design:

- metric names, series, and dashboards can reveal service/partition names;
- Prometheus API clients do not need OIDC unless a deployment overrides the exempt paths;
- the repo policy applies only to logs and traces.

This is not a security backlog item. If a deployment must protect metrics, it must do so through an
external gateway or network policy, not by changing Doctor’s default metrics semantics.

---

## P0 — security blockers

### [~] 1. Add mTLS/credentials to MonoFS transport

**2026-11-01:** TLS/mTLS configuration, CA/client-cert loading, and a production fail-closed path landed; add a real mTLS round-trip test before closing.

**Problem:** `store/monofs` still uses `insecure.NewCredentials()`, and `LogEngineClient` has no
credential field. Ingest/query/rules log traffic and file-store traffic are protected only by network
reachability.

**Where:**
- `internal/store/monofs/logengine_client.go`
- `internal/store/monofs/grpc_client.go`
- `internal/config/config.go`
- call sites in `cmd/doctor-ingest`, `cmd/doctor-query`, `cmd/doctor-rules`,
  `cmd/doctor-federation-*`

**Do:**
- Load CA, client cert, client key, and server name from configuration/secret mounts.
- Use `credentials.NewTLS` for router and node connections.
- Add an auth metadata token to `LogEngineClient` (mirroring the file-store token if MonoFS supports
  it).
- Keep an explicit development-only insecure mode, gated by config and logged loudly.
- Document certificate rotation.

**Acceptance:**
- Production startup fails if MonoFS TLS is required but misconfigured.
- A plaintext MonoFS endpoint is rejected in production mode.
- mTLS round-trip tests cover router and node connections.

---

### [~] 2. Make tenant a real isolation boundary

**2026-11-01:** A tenant guard now supports allowlists and a forced single tenant; MonoFS storage keys still ignore tenant, so physical/logical storage isolation remains.

**Problem:** Ingest now restricts tenant claims, but query accepts any tenant and MonoFS ignores the
tenant field entirely. Dashboards/rules use tenant as a path segment with no authorization.

**Where:**
- `internal/query/service.go` (`tenantFromRequest`)
- `internal/store/monofs/logengine_client.go`
- `internal/dashboards/store.go`
- `internal/ruler/store.go`
- `internal/config/config.go`

**Do:**
- Bind tenant to the authenticated principal (OIDC claim, service credential, or gateway header
  allowlist).
- Enforce the tenant in every query/storage key, or document an explicitly single-tenant mode.
- Add tenant authorization to dashboards and rules (tenant owner/admin).
- Add a negative test: user A cannot read/write tenant B.

**Acceptance:**
- A caller cannot change tenant by editing a query param/header.
- MonoFS records are physically or logically scoped by tenant.
- Cross-tenant dashboard/rule access returns 403 and is audited.

---

### [~] 3. Harden dashboard access and audit

**2026-11-01:** CSRF/origin checks and structured audit events landed; per-tenant/OWNERS authorization for dashboard reads and writes remains.

**Problem:** Any authenticated caller can read/write any tenant’s dashboards. Writes are attributed
to the `doctor-query` service principal; there is no CSRF token and no audit log.

**Where:**
- `internal/query/dashboards_handlers.go`
- `internal/dashboards/store.go`
- `internal/auth` / shared `webauth`

**Do:**
- Add tenant/OWNERS/admin authorization for dashboard mutations and tenant-level reads.
- Keep metrics-based dashboards a first-class use case: do not remove metric data visibility as part
  of dashboard authorization.
- Record caller subject/client, tenant, dashboard ID, action, and version in an audit log.
- Add CSRF protection for cookie-authenticated mutations (token or strict origin check).
- Use CAS for imports; do not silently overwrite concurrent edits.
- Add an ACL test matrix (owner, viewer, foreign tenant, anonymous, admin).

**Acceptance:**
- Non-owner mutation is rejected and audited.
- Export/import respects tenant visibility.
- Metrics-based dashboards remain readable for intended users.
- CSRF attempt is rejected.

---

### [~] 5. Add structured audit logging and request IDs

**2026-11-01:** Request IDs and structured audit events cover query mutations; gRPC/downstream propagation and the full endpoint matrix remain.

**Problem:** There is no audit trail for dashboard/rule mutations, federation publishes, break-glass
use, admin endpoints, or data exports. Ingest has no access log.

**Do:**
- Generate/propagate a request ID (`X-Request-ID`) through HTTP, gRPC, and downstream calls.
- Emit structured events with caller principal, tenant, action, resource, result, and version.
- Cover dashboard CRUD/import/export, rules CRUD/evaluate, federation status/targets/plan, Guardian
  proxies, and break-glass authentication.
- Ship audit events separately from debug logs.

**Acceptance:**
- Every state-changing endpoint emits one audit record.
- Records include the end-user principal, not only the service principal.
- A request can be traced end-to-end by request ID.

---

### [~] 6. Stop leaking internal errors to clients

**2026-11-01:** 5xx responses are sanitized behind a generic envelope and request ID; audit the remaining 4xx handlers for internal detail leaks.

**Problem:** Many handlers return `err.Error()` directly. This can expose upstream URLs, hostnames,
policy reasons, and internal details.

**Where:** `internal/query` handlers, `internal/query/dashboards_handlers.go`,
`internal/query/proxy_handlers.go`, `internal/query/service.go`, federation handlers.

**Do:**
- Introduce a stable error envelope (`{"error":"...","requestId":"..."}`).
- Log full errors server-side; return a generic message and status to clients.
- Preserve special client-visible errors (scope required, too broad, parse error, access denied).

**Acceptance:**
- No handler returns raw dial/HTTP/internal errors to clients.
- Error responses remain decodable in the Prometheus/Loki/Tempo formats.

---

### [~] 10. Bound federation discovery/list responses

**2026-11-01:** Service discovery already caps scanned records; add explicit pagination/truncation to the remaining federation ListMetrics/ListServices/ListLogGroups responses.

**Problem:** `ListMetrics`, `ListServices`, and `ListLogGroups` can serialize fleet-wide results for
50k targets. `include_metadata=true` can attach large per-target metadata.

**Where:** `internal/federation/manager.go`, `internal/query/service.go`
(`/v1/federation/targets`).

**Do:**
- Make discovery endpoints paginated/filterable.
- Cap `include_metadata` to a single target or a small page.
- Return counts and an explicit `truncated` flag when capped.

**Acceptance:**
- A 50k-target catalog cannot produce an unbounded JSON response.
- The UI can page/filter without loading full metadata.

---

### [ ] 11. Add retention, key rotation, and secret-manager integration

**Problem:** No retention/compaction policy, no privacy HMAC key rotation/versioning, and the
federation token key is a plain environment variable.

**Do:**
- Define retention/compaction ownership with MonoFS and expose current/oldest data age.
- Version tokenization keys and support re-tokenization without breaking correlation.
- Load the federation token key and MonoFS credentials from a secret manager/file mount.
- Document rotation procedures for OIDC client secrets, MonoFS tokens, and pusher tokens.

**Acceptance:**
- Keys can be rotated without downtime or silent decryption failures.
- Operators can see retention age and stale federation metadata.

---

### [~] 15. Remove structural duplication

**2026-11-01:** Deleted `internal/guardian`, `internal/store/fs`, and the unused FleetViewPage; the larger ingest/buffer/router/parser consolidation remains.

Track the consolidation work in [`docs/internal/duplicate-functionality.md`](docs/internal/duplicate-functionality.md).
Highest-value slices:

- [ ] Choose one ingest execution model (pipeline or WAL-backed buffers) and delete the other.
- [ ] Replace three buffer/WAL implementations with a generic signal buffer.
- [ ] Make the proxy router the only query path, or make the direct path a router with only local.
- [ ] Merge the legacy access gate into the repo policy gate.
- [ ] Share one LogQL/TraceQL parser between query, proxy, and federation.
- [ ] Define one trace-summary model/merge function for Tempo, native pagination, proxy, and federation.
- [ ] Delete unused `internal/store/fs`, `internal/guardian`, `ingest/cost.go`, `wal/wal.go`, and
      `FleetViewPage.tsx`.

**Acceptance:**
- Each signal has one execution path and one merge implementation.
- Dead packages/files are removed with tests updated.

---

### [ ] 16. Split oversized query/frontend components

Track details in [`docs/internal/overcomplexity.md`](docs/internal/overcomplexity.md).

- [ ] Split `internal/query/service.go` into handlers/domain/params files.
- [ ] Split `internal/query/prometheus_compat.go` into storage/discovery/handlers.
- [ ] Split `internal/federation/manager.go` into admin/discovery/query/convert.
- [ ] Split `internal/proxy/router.go` into routing/inventory/resilience.
- [ ] Extract asset-detail hooks/components from `PartitionAssetsPage.tsx` and `HubDetailView.tsx`.
- [ ] Extract `usePromQuery`/`useMetricDiscovery`/results components from `MetricsPage.tsx`.

**Acceptance:**
- No production file over ~800 lines without a documented reason.
- Asset detail has one implementation shared by fleet and partition routes.

---

### [~] 17. Fix remaining correctness/quality nits

**2026-11-01:** Added a global request body limit and `make verify`; WAL frame metadata, metric metadata, trace/metric field preservation, and unused settings remain.

- [ ] Add key/version metadata to WAL frames.
- [ ] Make `/api/v1/metadata` report stored metric metadata instead of always `untyped`.
- [ ] Preserve trace kind/events and metric unit/description/type across the MonoFS boundary.
- [ ] Add a request-size/body limit to every remaining handler.
- [ ] Add a `Content-Security-Policy` and CSRF tests.
- [ ] Replace `envDuration` integer-as-hours with a strict unit or document it.
- [ ] Remove the unused `DOCTOR_WAL_*` and `DOCTOR_COST_*` settings or implement them.
- [ ] Pin GitHub Actions to commit SHAs.
- [ ] Add a `make verify` target that runs tests, vet, frontend build, and contract scripts.

---

## Verification commands

Run before closing any item:

```bash
go test ./...
go vet ./...
npm --prefix frontend run build
./scripts/verify-grafana-contract.sh
```

Load/scale items additionally require:

```bash
make loadtest-federation
```
