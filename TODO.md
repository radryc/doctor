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

### [ ] 1. Add mTLS/credentials to MonoFS transport

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

### [ ] 2. Make tenant a real isolation boundary

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

### [ ] 3. Harden dashboard access and audit

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

### [ ] 4. Add remote-backend allowlisting and egress controls

**Problem:** Static and Guardian-discovered backend URLs are trusted and receive bearer tokens.
A malicious Guardian record or config file can point Doctor at internal services (SSRF).

**Where:**
- `internal/proxy/discovery_guardian.go`
- `internal/federation/guardian_feed.go`
- `internal/proxy/upstream_*`
- `internal/store/aws/client.go`

**Do:**
- Allowlist schemes (`https` in production), hosts, and optionally ports.
- Block loopback, link-local, and private ranges unless explicitly allowed for development.
- Use separate credentials per backend where possible.
- Add redirect handling rules; do not follow cross-host redirects with bearer tokens.
- Add tests for blocked loopback/private URLs and allowed production URLs.

**Acceptance:**
- A discovered backend pointing at `127.0.0.1`, `169.254.169.254`, or a private range is rejected.
- Tokens are never sent to a non-allowlisted host.

---

## P1 — isolation, audit, and operability

### [ ] 5. Add structured audit logging and request IDs

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

### [ ] 6. Stop leaking internal errors to clients

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

### [ ] 7. Add dashboard import conflict handling

**Problem:** Import calls `Put` with an empty `If-Match`, which resolves to the latest version and
can overwrite a concurrent edit.

**Where:** `internal/query/dashboards_handlers.go`, `internal/dashboards/store.go`.

**Do:**
- Support an import mode (`overwrite`, `skip-existing`, `fail-on-conflict`).
- Default to `fail-on-conflict` or `skip-existing`.
- Return per-dashboard conflict details.

**Acceptance:**
- Concurrent import/update cannot silently lose an edit.
- Tests cover each import mode.

---

### [ ] 8. Populate or remove the federation trace index

**Problem:** `Catalog.byTrace` is read by `ByTrace` but never written; trace-ID planning always falls
back to a full catalog scan or `scope required`.

**Where:** `internal/federation/catalog.go`, `internal/federation/planner.go`,
`internal/federation/builder.go`.

**Do:**
- If pushers can publish a trace index, add it to `Target` and populate `byTrace` during catalog
  construction.
- Otherwise remove `byTrace`/`ByTrace` and document the fallback and fan-out cost.

**Acceptance:**
- Trace-ID planning is either indexed or explicitly documented as unindexed.
- 150k-target load test asserts the chosen behavior.

---

### [ ] 9. Filter the proxy fleet summary by visibility

**Problem:** `EndpointsPage` returns an unfiltered `EndpointSummary` (counts by account/region/
cluster/status) even though the page items have per-item access decisions.

**Where:** `internal/proxy/router.go`, `internal/query/proxy_handlers.go`.

**Do:**
- Add `SummarizeEndpointsFor(ctx)` that applies the same visibility as the page.
- Or restrict the summary to admins and return only aggregate safe counts to everyone else.
- Cache per-identity summaries if 50k-endpoint cost is too high.

**Acceptance:**
- A caller cannot infer hidden endpoint/account/region counts from the summary.
- Page and summary agree for the same identity.

---

### [ ] 10. Bound federation discovery/list responses

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

### [ ] 12. Add deployment manifests and health probes

**Problem:** Only one Guardian partition metadata file exists; there are no Compose/Kubernetes
manifests for the six binaries, no resource limits, and no probes.

**Do:**
- Add manifests for ingest/query/rules/controller/CLI jobs, MonoFS, and Guardian integration.
- Configure liveness/readiness (`/healthz`, `/ready`), resource requests/limits, non-root UIDs,
  read-only root filesystems, and network policies.
- Mount TLS/secret material and persistent WAL/session volumes.
- Document the required collector/gateway topology.

**Acceptance:**
- `docker compose` or a kind/k8s manifest starts the stack.
- Probes and resource limits are present; pprof is not publicly exposed.

---

## P2 — hardening and maintainability

### [ ] 13. Add frontend tests and contract checks to CI

**Problem:** CI now builds the frontend, but there are no frontend unit/component/e2e tests, and the
Grafana contract scripts are not wired.

**Do:**
- Add a test runner (Vitest/Jest) for `lib/variables`, `lib/logs`, chart helpers, and critical pages.
- Add `npm run lint`/typecheck to CI.
- Run `scripts/verify-grafana-contract.sh` against a local stack in a job.
- Add `govulncheck` and `staticcheck` (or golangci-lint) to CI.

**Acceptance:**
- CI fails on frontend type/test/lint regressions.
- Grafana contract failures are caught before merge.

---

### [ ] 14. Add security headers/CSP to the embedded UI

**Problem:** `handleUI` sets only a content type. There is no CSP, `X-Content-Type-Options`, frame
protection, or referrer policy.

**Do:**
- Add a small middleware for UI responses.
- Start with `default-src 'self'`, `connect-src 'self'`, `img-src 'self' data:`, and
  `frame-ancestors 'none'`; adjust for the actual asset/CDN setup.
- Add `X-Content-Type-Options: nosniff`, `Referrer-Policy`, and `Permissions-Policy`.

**Acceptance:**
- Browser security headers are present on HTML/static responses.
- The UI and Grafana embedding requirements are documented.

---

### [ ] 15. Remove structural duplication

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

### [ ] 17. Fix remaining correctness/quality nits

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
