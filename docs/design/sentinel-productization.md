# Sentinel — Productization Backlog

- **Status:** Draft for planning
- **Baseline:** working Sentinel implementation on `feat/sentinel`, `feat/sentinel-alarms`, `feat/sentinel-default-alarms`
- **Related:** `docs/alerts.md`, `docs/architecture.md`
- **Purpose:** distinguish “it works” from “we can sell, deploy, operate, and support it as a product.”

## What “productized” means

Sentinel is productized when a new customer/tenant can:

1. deploy it through the supported installation path,
2. connect Guardian, AWS, Alertmanager, or a generic backend without engineering help,
3. select which alarms are processed,
4. define rules and silences with safe defaults,
5. receive notifications through supported channels,
6. manage access with tenant/partition RBAC,
7. understand delivery failures and alert quality,
8. upgrade and roll back without data loss,
9. get support from logs, metrics, diagnostics, runbooks, and docs.

## Estimate

| Stage | Remaining effort | With 2 engineers | With 3 engineers |
|---|---:|---:|---:|
| **Pilot** — one tenant/partition, single shard, manual ops | 4–6 engineer-weeks | 3–4 weeks | 2–3 weeks |
| **Production-ready v1** — HA, RBAC, delivery, AWS, rules, UI, observability | 13–17 engineer-weeks | 7–9 weeks | 5–6 weeks |
| **Productization / GA polish** — onboarding, templates, integrations, packaging, docs, support | +6–9 engineer-weeks | +3–5 weeks | +2–3 weeks |
| **GA total** | **19–26 engineer-weeks** | **10–13 weeks** | **7–9 weeks** |
| **Scale-ready** — 50k pushers, many shards, high rule volume | +6–8 engineer-weeks | +3–4 weeks | +2–3 weeks |

Assumptions: one senior Go engineer familiar with Doctor/Guardian, part-time frontend and SRE support, access to an AWS sandbox account, and no major product-scope changes.

## Workstream A — Reliability and correctness (P0)

Effort: ~3.5 engineer-weeks.

- [ ] CAS retry/conflict handling for alert state and outbox writes.
- [ ] Outbox claim/lease with a `sending` state; prevent duplicate delivery across replicas.
- [ ] Durable delivery history separate from the active outbox.
- [ ] Notification repeat intervals and grouping; current delivery is transition-per-instance.
- [ ] Resolve stale rule series when label sets change, not only when the whole query is empty.
- [ ] Rule scheduler with per-group due times, jitter, and a worker pool.
- [ ] Leader/lease for CloudWatch polling and source reconciliation.
- [ ] Guardian durable retry/outbox instead of retry-on-next-evaluation.
- [ ] HMAC timestamp/replay protection for Guardian and generic intake.
- [ ] Idempotency and dedupe tests for restart, leader loss, and duplicate source events.
- [ ] Backup/restore and retention for MonoFS alert state and logengine history.

Acceptance: no lost firing alerts across restart/failover; duplicate notification rate below 0.1%.

## Workstream B — Security, RBAC, and compliance (P0)

Effort: ~2 engineer-weeks.

- [ ] End-to-end identity headers for every Sentinel route.
- [ ] Partition RBAC integration tests for viewer/responder/rule-editor/admin.
- [ ] Production startup guard rejecting `AUTH_ALLOW_ALL=true`.
- [ ] Direct Sentinel access restricted by network policy/mTLS/service mesh.
- [ ] Persisted audit log for ack, silence, rule CRUD, source policy, receiver changes, retries, broadcasts.
- [ ] Secret review: HMAC keys, service tokens, receiver secrets, MonoFS tokens.
- [ ] PII review for alert labels/annotations and notification payloads.
- [ ] Tenant isolation for rules, silences, receivers, and delivery history.
- [ ] Retention policy for alerts, silences, audit events, and delivery logs.
- [ ] Security review of HMAC verification and request replay.

Acceptance: every read/write is server-side authorized; unauthorized partitions return 404; audit is queryable.

## Workstream C — AWS and CloudWatch (P0)

Effort: ~2.2 engineer-weeks.

- [ ] Per-account/region CloudWatch alarm selection policies.
- [ ] Cache and bound `DescribeAlarms` tag reads; handle throttling and pagination.
- [ ] Server-side pre-filtering for large accounts.
- [ ] Live deployment of the default probe/pusher alarm pack.
- [ ] Verify CloudWatch agent exports `guardian.probe.*` metrics.
- [ ] Validate composite alarm parent/child mapping.
- [ ] Validate native SNS delivery mode and EventBridge/SQS push mode.
- [ ] Alarm quota and cost visibility.
- [ ] AWS IAM least-privilege integration tests in a sandbox account.
- [ ] Document the AWS onboarding flow: role, read API token, selection policy, default alarms.

Acceptance: one real AWS account can install the pusher, discover alarms, import only selected alarms, and deliver a probe alarm.

## Workstream D — Delivery and integrations (P0/P1)

Effort: ~3 engineer-weeks.

- [ ] Slack, generic webhook, PagerDuty Events API v2, email.
- [ ] Routing policies: matchers, grouping, repeat interval, continue/stop.
- [ ] Receiver/route UI builder with validation and test notification.
- [ ] Channel health checks and fallback chain.
- [ ] Native delivery mode warnings and ownership display.
- [ ] Delivery log with retry, suppression reason, and audit.
- [ ] Alertmanager webhook/pull and optional silence proxy.
- [ ] SNS/EventBridge intake for AWS-native events.
- [ ] Notification templates with labels, annotations, runbook, and deep links.
- [ ] Per-tenant receiver/route isolation.

Acceptance: a channel outage falls back without duplicate pages; delivery history explains every attempt.

## Workstream E — Alert rules, noise control, and quality (P0/P1)

Effort: ~3 engineer-weeks.

- [ ] Rule editor validation, preview, and 24h backtest.
- [ ] Rule version history, diff, restore, clone, enable/disable.
- [ ] Rule health: last evaluation, lag, error, last value, firing count.
- [ ] Grouping, inhibition, maintenance windows, and flap detection.
- [ ] Silence UI scopes and impact preview; partition-wide permission.
- [ ] Default rule packs:
  - AWS: Lambda errors/throttles, ALB 5xx/latency, RDS CPU/storage, SQS age, DynamoDB throttles.
  - K8s: CrashLoopBackOff, replica mismatch, node not ready, PVC full, job failed.
  - Guardian: partition failing, probe down, drift.
- [ ] Alert quality dashboard: top noisy rules, MTTA/MTTR, flapping, coverage gaps.
- [ ] K8s Warning event ingestion.
- [ ] Rule migration/import from Prometheus/Alertmanager.

Acceptance: an operator can create, test, backtest, version, silence, and tune a rule without API calls.

## Workstream F — Subspace and platform alerts (P1)

Effort: ~2 engineer-weeks.

- [ ] Owner resolution from Guardian `OWNERS`, not just requested partition headers.
- [ ] Per-audience/per-owner receiver routing.
- [ ] Built-in producers: channel outage, quota exhaustion, Sentinel/Guardian/Doctor health.
- [ ] Audience preview and impact counts.
- [ ] Per-user ack and explicit subspace silence.
- [ ] Broadcast composer with audit and expiry.
- [ ] Fallback routing that never uses the failed channel.
- [ ] Subspace alert coverage tests for PagerDuty/quota scenarios.

Acceptance: a PagerDuty outage notifies the right owners through fallback channels and is visible only to the audience.

## Workstream G — UI, onboarding, and product UX (P0/P1)

Effort: ~4 engineer-weeks.

- [ ] Live UI e2e against `doctor-query` + Sentinel + MonoFS.
- [ ] First-run onboarding:
  - connect Guardian,
  - connect AWS account/region,
  - preview/select alarms,
  - add Slack/webhook,
  - invite/verify RBAC groups,
  - create first rule from a template.
- [ ] Pagination/virtualization for large alert lists.
- [ ] Real source health and excluded-alarm list.
- [ ] Rule editor with form/YAML, validation, preview chart, backtest, version history.
- [ ] Receiver/route builder; no raw JSON-only editing.
- [ ] Alert detail deep links to logs/traces/metrics and source console.
- [ ] Saved views, filters, URL state, and command palette entries.
- [ ] Empty/loading/error/403/404 states and capability-aware actions.
- [ ] Audit log page.
- [ ] Mobile/compact layout for on-call.

Acceptance: a new admin can onboard a source and route an alert without editing YAML or calling the API.

## Workstream H — Packaging and deployment (P0)

Effort: ~2 engineer-weeks.

- [ ] Versioned `doctor-sentinel` image in the release pipeline.
- [ ] Guardian partition intent and local docker/kind manifest.
- [ ] AWS CDK `doctor-sentinel` service and default alarm pack.
- [ ] Helm chart or equivalent supported install path.
- [ ] Config schema validation and upgrade/migration notes.
- [ ] Feature flags for delivery, AWS polling, subspace, and rules.
- [ ] Staging/shadow/canary/cutover runbook.
- [ ] Rollback to Guardian-native delivery.
- [ ] Air-gapped/offline image notes if required.
- [ ] Multi-arch image if required.

Acceptance: a fresh environment can install Sentinel and pass a smoke test from the docs.

## Workstream I — Observability and supportability (P0)

Effort: ~2 engineer-weeks.

- [ ] SLOs: delivery latency, outbox depth, eval lag, channel failure rate, source lag.
- [ ] Dashboard for Sentinel health.
- [ ] Self-alerts for outbox depth, stale source, rule errors, lease loss, channel down.
- [ ] Diagnostics bundle: config summary, shard/lease status, source health, recent delivery errors.
- [ ] Runbooks: channel outage, stuck outbox, HMAC rotation, rule migration, shard failover, rollback.
- [ ] Error catalog and actionable messages.
- [ ] Support matrix: supported AWS services, K8s versions, Alertmanager versions.
- [ ] Capacity model and quota limits.

Acceptance: on-call can diagnose a delivery outage from dashboards and a diagnostics bundle without shell access.

## Workstream J — Scale and performance (P1/P2)

Effort: +6–8 engineer-weeks.

- [ ] Dynamic shard assignment/rebalancing.
- [ ] Per-shard rule index; avoid N× rule scans.
- [ ] Outbox sharding and claim tuning at 10k+ notifications/min.
- [ ] MonoFS state compaction/archive tiering.
- [ ] 50k-pusher CloudWatch discovery with server-side filtering.
- [ ] Multi-region/account policy distribution.
- [ ] Load tests: 10k rules, 50k alarms, 100k metric series, leader loss.
- [ ] Chaos tests: MonoFS latency, channel outages, pod restarts.

Acceptance: documented capacity envelope with headroom and failure behavior.

## Workstream K — Documentation, release, and training (P0/P1)

Effort: ~1.5 engineer-weeks.

- [ ] Admin guide: install, upgrade, SSO/RBAC, sources, receivers, retention.
- [ ] User guide: alerts, silences, rules, backtests, subspaces.
- [ ] API reference with stable versioning.
- [ ] AWS onboarding guide with IAM and default alarm pack.
- [ ] K8s/Alertmanager onboarding guide.
- [ ] Migration guide from Guardian webhooks / Grafana alerts.
- [ ] Runbooks and troubleshooting.
- [ ] Release notes, changelog, deprecation policy.
- [ ] Internal training/demo script.
- [ ] Support escalation path.

Acceptance: docs are sufficient for a new admin to install and operate a pilot.

## Workstream L — Commercialization and metering (P2)

Only if Sentinel is sold as a metered/editioned product.

- [ ] Usage metrics: rules evaluated, alerts ingested, notifications sent, sources, retained history.
- [ ] Per-tenant quotas and limit enforcement.
- [ ] Billing/usage export.
- [ ] Edition/feature gating.
- [ ] License/terms review.
- [ ] Tenant-level cost visibility.

## Release gates

### Pilot gate

- one tenant/partition
- single shard
- webhook delivery
- manual onboarding/ops
- no known P0 correctness bugs
- rollback tested

### GA gate

- HA delivery and shard failover
- full partition RBAC
- CloudWatch selection and default alarms validated in AWS
- supported channels and fallback
- onboarding flow
- SLOs, dashboards, runbooks
- documented install/upgrade/rollback
- load test at target capacity
- two weeks of production pilot with no Sev-2 Sentinel incidents

### Scale gate

- 50k-pusher discovery validated
- multi-shard rebalancing
- capacity model
- chaos tests passed
- cost/quota controls

## Critical path

1. Reliability/CAS/outbox and RBAC.
2. AWS validation and source policy.
3. Live UI and onboarding.
4. Observability/runbooks.
5. Staging/shadow/canary.
6. Productization UX and docs.
7. Scale work only after GA.

## Risks

- Identity header/RBAC integration can expand if Guardian’s OWNERS model is not authoritative for every route.
- AWS alarms and CloudWatch agent metric export may require additional pusher work.
- Notification grouping/repeat semantics can become a large product design effort.
- Subspace per-owner routing needs a reliable owner directory and channel mapping.
- Scope creep from P1/P2 can double the GA estimate.
- The pre-existing Guardian `internal/bootstrap` test failure must be resolved or quarantined.
