# Sentinel alerts

Sentinel (`doctor-sentinel`) is Doctor's alert routing and evaluation service.
It is a separate process from `doctor-query` and stores rules, silences,
alert state, receiver config, and the delivery outbox in MonoFS.

## Sources

- Guardian partition alerts via `POST /v1/alerts/intake/guardian`
  (HMAC-signed with `X-Guardian-Signature`).
- AWS CloudWatch alarms pulled through `guardian-pusher-aws`
  (`GET /v1/aws/alarms`).
- Alertmanager-compatible webhooks via
  `POST /v1/alerts/intake/{source}`.
- Sentinel PromQL alert rules evaluated through `doctor-query`.
- Subspace alerts for platform-wide events.

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
- v1 channels: generic webhook and Slack. PagerDuty and email are later.

## API

```text
GET    /v1/alerts
GET    /v1/alerts/{fingerprint}
POST   /v1/alerts/{fingerprint}/ack
DELETE /v1/alerts/{fingerprint}/ack

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
GET    /v1/alerts/receivers
PUT    /v1/alerts/receivers
GET    /v1/alerts/delivery
GET    /v1/alerts/capabilities?tenant=&partition=

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
DOCTOR_SENTINEL_SERVICE_TOKEN=...
DOCTOR_SENTINEL_AWS_ENABLED=true
DOCTOR_SENTINEL_AWS_URL=http://guardian-awsread:19090
DOCTOR_SENTINEL_AWS_TOKEN=...
DOCTOR_SENTINEL_AWS_ALARM_SELECTION=managed-only
```

MonoFS uses `DOCTOR_MONOFS_LOGENGINE_ADDR` and `DOCTOR_MONOFS_TOKEN`.
