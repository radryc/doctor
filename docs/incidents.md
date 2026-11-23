# Incidents and postmortems

Sentinel turns alert sets into operator-facing incidents. Alerts are signals;
an incident is the unit responders acknowledge, mitigate, and resolve.

## Alert sets

An alert set is a stable selector over alert labels. Matching alerts are
grouped by `groupBy` (default `partition`) and each group becomes one incident.

```yaml
tenant: default
partition: doctor
name: platform-critical
severityFloor: critical
match:
  - {name: severity, operator: "=", value: critical}
exclude: []
respectSilences: true
respectAcks: false
groupBy: [partition, service]
notify:
  receivers: [teams-oncall, zoom-bridge]
  updateInterval: 30m
  reminders: true
  updateWhileAcked: false
postmortem:
  engine: google-docs
  options:
    credentialsEnv: GOOGLE_APPLICATION_CREDENTIALS
    folderId: ""
  autoPublish: false
  analysisProvider: agent
```

- Matchers use the same operators as silences (`=`, `!=`, `=~`, `!~`).
- `severityFloor` defaults to `critical`, so quota breaches capped at warning
  never open a critical incident.
- Silenced alerts can open incidents only when `respectSilences` is false.
- A set's `notify.receivers` must reference incident-capable receivers in the
  partition routing document. The incident-capable types are `webhook`, `slack`, `teams`, `zoom`,
  `android`, and `fcb` (`fcm-bridge`); GoAlert and PagerDuty are alert-only. `urlEnv` keeps webhook
  credentials out of config.

## Lifecycle

```text
open ──ack──> acknowledged
 │                 │
 └── mitigate ─────┴──> mitigated ──resolve──> resolved
                            │                     │
                    member alert refires     reopen window
                            ▼                     ▼
                     open / acknowledged        open
```

- **open**: a member alert is firing.
- **acknowledged**: a responder owns the incident.
- **mitigated**: bleeding stopped. Automatic when every member alert has been
  clear for `mitigateDelay`; manual with a note (and `override` while alerts
  are still firing).
- **resolved**: only from `mitigated`. Resolution generates a postmortem and
  runs agent analysis. A manual resolve closes the reopen window; an
  auto-resolved incident can reopen when a member refires within
  `reopenWindow`.

## Partition policy

Lifecycle timing is configured per partition at
`{store}/config/incident-policy/{tenant}/{partition}.yaml`. The default is a
**30 minute** mitigate delay measured from the moment the last member alert
clears.

```yaml
version: 1
mitigateDelay: 30m
autoMitigate: true
autoResolve: false
autoResolveDelay: 30m
resumeWindow: 2h
reopenWindow: 1h
maxUpdatesPerHour: 12
analysisProvider: agent
analysisTimeout: 120s
postmortemEngine: builtin
autoPublish: false
```

Alert sets do not carry lifecycle timing; every incident in a partition follows
the same policy. A policy change recomputes `mitigateDueAt` for incidents that
are waiting to mitigate.

## Postmortems

Resolution always writes a Doctor markdown draft. The analysis provider then
fills the narrative:

- `agent`: calls the Strata agent's `POST /v1/investigations`, backed by the
  Nightwatch `Investigator`. The agent collects metrics, logs, traces, and
  topology under its partition-scoped tool allowlist.
- `none`: deterministic template only.

Publishing is pluggable. Engines register at startup and expose option metadata
through `GET /v1/postmortem-engines`, so the UI renders their forms without a
frontend change.

| Engine | Purpose |
| --- | --- |
| `builtin` | Store the markdown draft in Doctor and return the internal link. |
| `webhook` | POST `{incident, postmortem, markdown}` to an external tool; expects `{url, externalId}`. |
| `google-docs` | Create a Google Doc with a service account and share it with configured emails. |

### Google Docs

The engine reads service-account JSON from `credentialsEnv` (default
`GOOGLE_APPLICATION_CREDENTIALS`). It creates a document in the optional
`folderId` and inserts the markdown using the Docs API. Options:

```yaml
engine: google-docs
options:
  credentialsEnv: GOOGLE_APPLICATION_CREDENTIALS
  folderId: "1AbC..."
  shareWith: "oncall@example.com,platform@example.com"
  subject: ""            # optional Workspace impersonation
```

Adding another engine requires implementing `incident.Engine` and registering
it in `cmd/doctor-sentinel`; the built-ins are installed by
`incident.NewEngineRegistry()` (builtin, webhook, Google Docs). No frontend
change is needed. Runtime integrations should use the generic webhook engine.

## Notifications

Incident events reuse the Sentinel outbox and the partition receiver config.
The dispatcher is kind-aware: incidents use `doctor.incident/v1` payloads and
channel-specific layouts, while alert webhook payloads remain unchanged.

Events: `incident.opened`, `incident.acknowledged`, `incident.members_cleared`,
`incident.mitigated`, `incident.refired`, `incident.resolved`,
`incident.reopened`, `incident.note`, `incident.update`, and
`postmortem.published`.

## API

```text
GET/POST        /v1/alert-sets
GET/PUT/DELETE  /v1/alert-sets/{name}
POST            /v1/alert-sets/preview
GET/PUT         /v1/incident-policy
GET             /v1/incidents
GET             /v1/incidents/{id}
POST            /v1/incidents/{id}/ack|unack|mitigate|resolve|reopen|notes
GET/PUT         /v1/incidents/{id}/postmortem
POST            /v1/incidents/{id}/postmortem/analyze|publish
GET             /v1/postmortem-engines
```

Reads require `alerts.read`; ack, mitigate, resolve, reopen, notes, and
postmortem draft edits/analyze/publish require `alerts.ack`. Alert-set and
partition-policy management require `alerts.rules.write`. All checks use the
existing partition-scoped identity headers set by `doctor-query`.

## Operations

| Variable | Default | Purpose |
| --- | --- | --- |
| `DOCTOR_SENTINEL_INCIDENTS_ENABLED` | `false` | Enable reconciliation and the incident API. |
| `DOCTOR_SENTINEL_INCIDENT_TICK` | `15s` | Reconciler interval. |
| `DOCTOR_SENTINEL_AGENT_URL` | empty | Strata agent Nightwatch base URL for analysis. |
| `DOCTOR_SENTINEL_AGENT_TOKEN` | empty | Service token for `POST /v1/investigations`. |
| `DOCTOR_SENTINEL_AGENT_TIMEOUT` | `120s` | Analysis deadline. |
| `DOCTOR_SENTINEL_BASE_URL` | empty | Base URL used in incident deep links. |
| `DOCTOR_SENTINEL_WEBHOOK_REQUIRE_HTTPS` | `false` | Require HTTPS for webhook receivers. |
| `DOCTOR_SENTINEL_WEBHOOK_ALLOW_PRIVATE` | `false` | Allow RFC1918 webhook targets. |
| `DOCTOR_SENTINEL_WEBHOOK_ALLOW_LOOPBACK` | `false` | Allow loopback webhook targets. |
