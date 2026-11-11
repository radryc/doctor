# Sentinel alerts

Sentinel (`doctor-sentinel`) is Doctor's alert routing and evaluation service.
It is a separate process from `doctor-query` and stores rules, silences,
alert state, receiver config, and the delivery outbox in MonoFS.

## Sources

- Guardian partition alerts via `POST /v1/alerts/intake/guardian`
  (HMAC-signed with `X-Guardian-Signature`).
- Guardian partition `Error`/`Failing`/`Invalid` and `Attention` state pulled
  from `GET /api/partitions`. This source creates `GuardianPartitionError`
  (critical) and `GuardianPartitionAttention` (warning) alerts and resolves them
  when the partition recovers. Locked-only drift is treated as intentional and
  does not raise `GuardianPartitionAttention`; real errors and failing assets
  still raise `GuardianPartitionError`. Nightwatch reads these through the
  Sentinel API, so Guardian errors and attentions become Nightwatch incidents
  without any Nightwatch-side change.
- AWS CloudWatch alarms pulled through `guardian-pusher-aws`
  (`GET /v1/aws/alarms`).
- Alertmanager-compatible webhooks via
  `POST /v1/alerts/intake/{source}`.
- Sentinel PromQL alert rules evaluated through `doctor-query`.
- Subspace alerts for platform-wide events.

The push and pull Guardian sources are independent. Enable the pull source
(`DOCTOR_SENTINEL_GUARDIAN_ENABLED`) when Guardian's own notification evaluator
is not already forwarding `partition.failing` events to Sentinel; otherwise the
same incident appears under both alert names. The pull source is the recommended
default because it also covers partitions whose Guardian notification state is
missing or stale.

## Delivery

The pipeline is:

```text
source -> intake -> normalize -> fingerprint/state -> route
       -> ack/silence -> outbox -> channel -> delivery log
```

- Delivery is at-least-once with retries and an idempotency key.
- `delivery: sentinel` means Sentinel owns notification delivery.
- `delivery: native` means CloudWatch SNS or Alertmanager owns delivery;
  Sentinel imports and presents the alert only.
- v1 channels: generic webhook, Slack, GoAlert, and PagerDuty Events API v2.

## Alert authoring

Rules can be authored in two modes:

- `guided`: a structured Golden Signal specification (`spec`) that Sentinel
  compiles to PromQL on save. This is the default in the UI and requires no
  PromQL knowledge.
- `promql`: a raw expression (`expr`) written by an advanced user. Sentinel
  stores and evaluates it as written.

Guided signals and their query shapes:

| Signal | Shape |
| --- | --- |
| `errors` | error ratio (`primary / denominator`) or absolute error rate |
| `latency` | histogram quantile (p50/p90/p95/p99), scaled to ms/s |
| `traffic` | request rate, alerting on drop or surge |
| `saturation` | usage-versus-limit ratio or absolute usage |
| `availability` | `up == 0`, `absent()`, or a success ratio |

A guided rule stores both its `spec` and the compiled `expr`. Re-saving
recompiles the expression. Converting a guided rule to PromQL keeps the spec as
recoverable metadata, but the expression becomes authoritative.

## Routing

Routing configuration is partition-owned:

```text
{store}/config/routing/{tenant}/{partition}.yaml
```

Each document contains:

- `receivers`: endpoints and their credential environment variables,
- `alertRoutes`: explicit destinations for source-generated alerts, keyed by
  `source` and `alertname`,
- `partitionReceiver`: optional destination for every alert in the partition,
- `routes`: optional label routes,
- `defaultReceiver`: final fallback.

Precedence:

```text
explicit rule destination (notify)
  > source alert route (source + alertname)
  > partition destination
  > label routes
  > default receiver
```

An explicit rule destination is stored on the rule (`notify`) and is set by the
UI when an alert is dragged onto an endpoint. Source-generated alerts (Guardian
partition health, CloudWatch alarms, and so on) have no rule to store `notify`
on, so the UI writes an `alertRoutes` entry instead. The legacy global
`config/receivers.yaml` is still read as a fallback for partitions without their
own routing document.

Saving a routing document also reconciles firing alerts that were stored before
a destination existed and have never been notified. This means adding a route
for an already-firing Guardian partition alert delivers it immediately instead
of waiting for the partition to recover and fail again.

### Routing Guardian partition alerts

Guardian partition alerts are source-generated and appear in the Routing tab's
**Source alerts** area. They can be routed in two ways:

1. Drag a source alert card onto a destination and save routing. The UI writes
   an `alertRoutes` entry for that `source` and `alertname`.
2. Edit the partition routing YAML directly:

```yaml
receivers:
  - name: platform-pagerduty
    type: pagerduty
    url: https://events.pagerduty.com/v2/enqueue
    secretEnv: PLATFORM_PAGERDUTY_ROUTING_KEY
  - name: oncall-slack
    type: slack
    url: https://hooks.slack.com/services/...
alertRoutes:
  - source: guardian
    alertname: GuardianPartitionError
    receivers: [platform-pagerduty]
  - source: guardian
    alertname: GuardianPartitionAttention
    receivers: [oncall-slack]
defaultReceiver: oncall-slack
```

This sends failing partitions to PagerDuty and attention/drift alerts to Slack,
while anything else in the partition falls back to Slack. To route all Guardian
alerts together, use the same receiver for both entries. Removing the explicit
route falls back to the partition destination or default route.


## Guardian alert policy

Guardian partition alerts are source-generated rather than PromQL rules, so
their timing is configured per partition:

```text
{store}/config/sources/guardian/{tenant}/{partition}.yaml
```

```yaml
version: 1
alertOnLocked: false

alerts:
  GuardianPartitionError:
    for: 5m
  GuardianPartitionAttention:
    for: 30m
```

- `for` delays firing until the condition has been observed continuously for
  that duration. While waiting, the alert is `pending` and is not delivered.
- `for: 0s` (or an omitted alert entry) fires on the first observation.
- A condition that clears while pending resolves without notifying.
- `alertOnLocked` defaults to `false`. Set it to `true` only for partitions
  where drift on a locked intent should raise `GuardianPartitionAttention`.
- Existing firing alerts are never downgraded to `pending` when a delay is
  added to the policy.

The policy is available through:

```text
GET /v1/alerts/sources/guardian/policy?tenant=&partition=
PUT /v1/alerts/sources/guardian/policy?tenant=&partition=
```

The Doctor Sources tab exposes the attention delay, error delay, and
`alertOnLocked` checkbox for the selected partition.

## PagerDuty receiver

Create a PagerDuty service with an Events API v2 integration key, then add a
receiver in the partition's routing configuration:

```yaml
receivers:
  - name: payments-pagerduty
    type: pagerduty
    url: https://events.pagerduty.com/v2/enqueue
    secretEnv: PAYMENTS_PAGERDUTY_ROUTING_KEY
routes: []
partitionReceiver: payments-pagerduty
defaultReceiver: payments-pagerduty
```

Sentinel sends `routing_key`, `event_action` (`trigger` or `resolve`), `dedup_key`,
and a payload with summary, source, severity, timestamp, and custom details.
The routing key is read from the named environment variable and is never stored
in MonoFS.

## API

```text
GET    /v1/alerts
GET    /v1/alerts/{fingerprint}
POST   /v1/alerts/{fingerprint}/ack
DELETE /v1/alerts/{fingerprint}/ack
GET    /v1/alerts/summary?tenant=

GET    /v1/alerts/silences
POST   /v1/alerts/silences
DELETE /v1/alerts/silences/{tenant}/{partition}/{id}

GET    /v1/alerts/rules?tenant=&partition=
GET    /v1/alerts/rules/{tenant}/{partition}/{group}
PUT    /v1/alerts/rules/{tenant}/{partition}/{group}
DELETE /v1/alerts/rules/{tenant}/{partition}/{group}
POST   /v1/alerts/rules/preview
POST   /v1/alerts/rules/test

GET    /v1/alerts/sources
GET    /v1/alerts/sources/aws/policy
PUT    /v1/alerts/sources/aws/policy
GET    /v1/alerts/sources/guardian/policy?tenant=&partition=
PUT    /v1/alerts/sources/guardian/policy?tenant=&partition=
GET    /v1/alerts/receivers?tenant=&partition=
PUT    /v1/alerts/receivers?tenant=&partition=
GET    /v1/alerts/routing/overview?tenant=
GET    /v1/alerts/delivery
GET    /v1/alerts/capabilities?tenant=&partition=
POST   /v1/alerts/rules/compile
POST   /v1/alerts/rules/preview

POST   /v1/alerts/subspace
POST   /v1/alerts/subspace/{id}/resolve
POST   /v1/alerts/subspace/preview
```

The UI calls these through `doctor-query` at `/v1/alerts*`.

## CloudWatch alarm selection

`DescribeAlarms` returns every alarm in the account and region. Sentinel applies
a selection policy before importing:

- `managed-only` (default): Guardian-managed/default-pack alarms and explicit
  `sentinel.io/import=true` alarms only.
- `import-all`: every alarm except excludes.
- `ignore-all`: discover for health counts only.

Exclude rules and `sentinel.io/import=false` always win. Alarm state is not a
selection criterion: `OK` alarms must be imported to resolve firing alerts.
Excluded alarms are never stored as Sentinel alerts and never notify.

## Silences

A silence is label matchers plus a time window. It suppresses notifications
only; alert state continues updating.

Scopes, narrowest first:

1. alert instance,
2. rule in a partition,
3. service/group in a partition,
4. entire partition.

The UI defaults to the alert instance. Entire-partition silences require the
`alerts.silence.partition` capability. Partition silences do not suppress
subspace alerts.

## Subspace alerts

Subspace alerts are platform-level alerts with an explicit audience:

- `tenant-admins`
- `partition-owners`
- specific `partitions`
- specific `groups`
- `all`

They are used for channel outages, quota exhaustion, and Sentinel/Guardian/
Doctor platform health. Audience expansion and deduplication happen server-side.
Ack is per user; a partition silence does not suppress them.

## RBAC

Capabilities are checked server-side. Unauthorized partitions return 404.

- Viewer: read alerts, rules, sources, delivery log.
- Responder: viewer + ack + narrow silences.
- Rule editor: viewer + rule CRUD/test.
- Admin: partition-wide silences, source policy, receivers, retries, broadcasts.

`doctor-query` forwards identity headers to Sentinel. For local development,
`DOCTOR_SENTINEL_AUTH_ALLOW_ALL=true` is available but must not be used in
production.

## GoAlert receiver

Create a **Generic API** integration key on the GoAlert service you want to
receive Sentinel alerts:

1. GoAlert → Service → Integration Keys → Add Key → Generic API.
2. Copy the integration key.
3. Add a Sentinel receiver:

```yaml
receivers:
  - name: goalert
    type: goalert
    url: https://goalert.example.com/api/v2/generic/incoming
    secretEnv: GOALERT_INTEGRATION_KEY
routes:
  - name: default
    receivers: [goalert]
```

Sentinel sends `summary`, `details`, `dedup`, and the integration key as form
parameters. Resolved alerts send `action=close` with the same `dedup` value, so
GoAlert closes the matching alert. The integration key is read from the named
environment variable and is never stored in MonoFS.

The Guardian-managed GoAlert deployments (Kubernetes and AWS) live in
`stratatools/partitions/goalert`. Use that partition rather than deploying
GoAlert with raw Kubernetes manifests.

## Configuration

```text
DOCTOR_SENTINEL_HTTP_ADDR=:18082
DOCTOR_SENTINEL_QUERY_URL=http://doctor-query:18080
DOCTOR_SENTINEL_STORE_ROOT=/doctor/v1/alerts
DOCTOR_SENTINEL_DEFAULT_TENANT=default
DOCTOR_SENTINEL_DELIVERY_ENABLED=true
DOCTOR_SENTINEL_AUTH_ALLOW_ALL=false
DOCTOR_SENTINEL_EVAL_SHARDS=1
DOCTOR_SENTINEL_SHARD_INDEX=0
DOCTOR_SENTINEL_LEASE_TTL=30s
DOCTOR_SENTINEL_LEASE_RENEW=10s
DOCTOR_SENTINEL_GUARDIAN_HMAC_SECRET=...
DOCTOR_SENTINEL_GUARDIAN_ENABLED=true
DOCTOR_SENTINEL_GUARDIAN_URL=http://guardian-ui:8090
DOCTOR_SENTINEL_GUARDIAN_TOKEN=...
DOCTOR_SENTINEL_GUARDIAN_POLL_INTERVAL=30s
DOCTOR_SENTINEL_SERVICE_TOKEN=...
DOCTOR_SENTINEL_AWS_ENABLED=true
DOCTOR_SENTINEL_AWS_URL=http://guardian-awsread:19090
DOCTOR_SENTINEL_AWS_TOKEN=...
DOCTOR_SENTINEL_AWS_ALARM_SELECTION=managed-only
```

MonoFS uses `DOCTOR_MONOFS_LOGENGINE_ADDR` and `DOCTOR_MONOFS_TOKEN`.
