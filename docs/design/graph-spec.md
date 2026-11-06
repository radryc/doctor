# Doctor graph specification

Status: current · Applies to Doctor `doctor-query` UI and dashboards · Last updated 2026-11-05

This document defines the durable graph formats that can be created outside Doctor
and imported into it. It is the contract used by the `create-doctor-graphs` agent
skill in the Strata agent repository
(`agent/.github/skills/create-doctor-graphs/`).

## 1. Scope

Doctor accepts two importable graph formats:

| Format | Storage | Best for |
|---|---|---|
| **Saved Graph Bundle v1** | Browser `localStorage` | Quick line graphs and small ad-hoc boards in a partition view |
| **Doctor Dashboard v1 / Bundle v1** | Server-side MonoFS dashboard store | Multi-panel dashboards with gauges, stats, tables, logs, traces, and layout |

Both formats accept **JSON or YAML**. YAML is a serialization of the same data;
there is no behavioural difference after parsing.

The format does not define telemetry or PromQL semantics. A graph is only as good
as the query and labels behind it. Discover live metric names and labels before
choosing a metric.

## 2. Graph model

- A **graph** is one panel: a title, one or more queries, a time range, and a
  visualization. Every new graph also declares the **partitions it affects**.
- A **dashboard** is a collection of graphs laid out in a 12-column grid.
- A **saved graph bundle** contains flat saved graphs plus simple saved
  dashboards.
- A **dashboard document** contains typed widgets and dashboard-level variables.
- A **dashboard bundle** contains one or more dashboard documents.
- `partitions` is the RBAC scope: the partitions whose data the graph is allowed
  to aggregate, display, or link to. It is not a query filter by itself; it tells
  the server whose permissions to check when storing the object.

Supported widget kinds (`kind`):

| Kind | Purpose | Notes |
|---|---|---|
| `timeseries` | Time-series charts | `displayMode`: `line`, `area`, or `bars` |
| `stat` | Big current value | Uses the latest sample from the query |
| `gauge` | Current value against 0–100% | Query should return a percentage |
| `toplist` | Ranked values | Good for `topk()` |
| `table` | Tabular result | Prometheus/Loki/Tempo results |
| `logs` | Log stream | Use `datasource: loki` or `native_logs` |
| `traces` | Trace list | Use `datasource: tempo` or `native_traces` |
| `topology` | Guardian/fleet map | No query required |
| `markdown` | Notes and headings | `visualization.markdownContent` |

Datasources: `prometheus`, `loki`, `tempo`, `native_logs`, `native_traces`.

The Metrics explorer supports `line`, `area`, `bars`, `scatter`, and `gauge`
display modes interactively. Persisted dashboard widgets support `line`, `area`,
and `bars` for `timeseries`; use `kind: gauge` for a gauge panel.

## 3. Saved Graph Bundle v1

Canonical shape:

```yaml
version: 1
exportedAt: "2026-11-05T08:00:00Z"
savedGraphs:
  - id: api-p95-latency
    title: API p95 latency
    query: >-
      histogram_quantile(0.95,
        sum by (le, service) (
          rate(http_server_request_duration_seconds_bucket{guardian_partition="agent"}[5m])
        )
      )
    rangeMinutes: 60
    refreshIntervalSec: 30
    createdAt: "2026-11-05T08:00:00Z"
    partitions: [agent]
savedDashboards:
  - id: api-overview
    name: API overview
    partitions: [agent]
    graphs:
      - title: Request rate
        query: >-
          sum by (service) (
            rate(http_server_request_duration_seconds_count{guardian_partition="agent"}[5m])
          )
        rangeMinutes: 60
        refreshIntervalSec: 30
        partitions: [agent]
    createdAt: "2026-11-05T08:00:00Z"
    updatedAt: "2026-11-05T08:00:00Z"
```

### Top level

| Field | Required | Type | Rules |
|---|---|---|---|
| `version` | yes | integer | `1` |
| `exportedAt` | recommended | RFC 3339 string | Informational |
| `savedGraphs` | yes | array | May be empty |
| `savedDashboards` | yes | array | May be empty |

### SavedGraph

| Field | Required | Type | Default | Rules |
|---|---|---|---|---|
| `id` | recommended | string | generated | Unique within the imported file |
| `title` | yes | string | — | Human-readable panel title |
| `query` | yes | string | — | PromQL query |
| `rangeMinutes` | recommended | number | `60` | Preferred values: `15`, `60`, `360`, `1440` |
| `refreshIntervalSec` | recommended | number | `0` | `0` disables auto-refresh; minimum effective refresh is 5s |
| `createdAt` | recommended | RFC 3339 string | now | Informational |
| `partitions` | yes for new files | string[] | — | Partitions this graph affects; at least one. `*` means every partition and is admin-only |

### SavedDashboard

| Field | Required | Type | Rules |
|---|---|---|---|
| `id` | recommended | string | Unique within the imported file |
| `name` | yes | string | Dashboard name |
| `graphs` | yes | array | Each entry is a SavedGraph without `id` |
| `createdAt` | recommended | RFC 3339 string | Informational |
| `updatedAt` | recommended | RFC 3339 string | Informational |
| `partitions` | yes for new files | string[] | Union of the graphs' partitions; at least one |

### Rendering rule

The saved graph board renders each query as a **line time series**. The query must
return a matrix result. Use `rate()`, `avg_over_time()`, `histogram_quantile()`,
or another range function rather than a bare instant vector.

For bar, gauge, stat, table, log, or trace panels, use a Doctor Dashboard document
instead.

### Import

In Doctor, open the partition view and find **Ad-hoc Boards (local to this
browser)** in the observability tab, then click **Import JSON/YAML** and select
the file. Imports are additive; exact duplicate titles+queries and duplicate
dashboard names are skipped. The bundle is stored in the browser, so it is
per-browser, not shared. The UI marks these entries **Local · ad-hoc** and offers
**Promote** to copy a saved graph into a permanent, RBAC-enforced dashboard.

## 4. Doctor Dashboard v1

Canonical document:

```yaml
version: doctor.dashboard/v1
id: api-overview
title: API overview
description: Request, latency, error, and log panels for one partition.
tenant: default
tags: [api, red]
defaultTimeRange: 1h
refreshInterval: 30s
partitions: [agent]
variables:
  - name: partition
    label: Partition
    type: partition
    defaultValue: "$__all"
    includeAll: true
layout:
  - id: request-rate
    title: Request rate
    kind: timeseries
    gridPos: {x: 0, y: 0, w: 6, h: 6}
    queries:
      - refId: A
        datasource: prometheus
        expr: >-
          sum by (service) (
            rate(http_server_request_duration_seconds_count{guardian_partition=~"$partition"}[$__rate_interval])
          )
        legendFormat: "{{service}}"
    visualization:
      displayMode: line
      unit: reqps
  - id: p95-latency
    title: p95 latency
    kind: timeseries
    gridPos: {x: 6, y: 0, w: 6, h: 6}
    queries:
      - refId: A
        datasource: prometheus
        expr: >-
          histogram_quantile(0.95,
            sum by (le, service) (
              rate(http_server_request_duration_seconds_bucket{guardian_partition=~"$partition"}[$__rate_interval])
            )
          )
        legendFormat: "{{service}}"
    visualization:
      displayMode: line
      unit: seconds
  - id: error-ratio
    title: Error ratio
    kind: gauge
    gridPos: {x: 0, y: 6, w: 3, h: 4}
    queries:
      - refId: A
        datasource: prometheus
        expr: >-
          100 * sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5..",guardian_partition=~"$partition"}[$__rate_interval]))
          / clamp_min(sum(rate(http_server_request_duration_seconds_count{guardian_partition=~"$partition"}[$__rate_interval])), 0.001)
    visualization:
      unit: percent
      thresholds:
        - {value: 0, color: green}
        - {value: 5, color: amber}
        - {value: 10, color: red}
  - id: error-logs
    title: Recent error logs
    kind: logs
    gridPos: {x: 3, y: 6, w: 9, h: 4}
    queries:
      - refId: A
        datasource: loki
        expr: '{guardian_partition=~"$partition"} |= "error"'
```

### DashboardDocument

| Field | Required | Type | Rules |
|---|---|---|---|
| `version` | yes | string | Exactly `doctor.dashboard/v1` |
| `id` | yes | string | Stable, URL-safe slug; unique per tenant |
| `title` | yes | string | Human-readable |
| `tenant` | yes | string | Usually `default`; `*` is used by built-in presets |
| `description` | no | string | — |
| `tags` | no | string[] | Lowercase keywords |
| `defaultTimeRange` | no | duration string | e.g. `15m`, `1h`, `24h` |
| `refreshInterval` | no | duration string | e.g. `30s`, `1m` |
| `partitions` | yes for new documents | string[] | Partitions this dashboard affects; union of widget scopes when omitted. `*` is admin-only |
| `variables` | no | array | See below |
| `layout` | yes | widget[] | Must be non-empty for a useful dashboard |
| `meta` | no | object | Server-managed version metadata; omit on import |

### DashboardVariable

| Field | Required | Type | Rules |
|---|---|---|---|
| `name` | yes | string | Used as `$name` in queries |
| `label` | yes | string | Picker label |
| `type` | yes | string | `label_values`, `custom`, `text`, `partition`, `service` |
| `labelKey` | conditional | string | Required for `label_values` |
| `metricFilter` | no | string | Restrict label discovery |
| `options` | conditional | string[] | Required for `custom` |
| `defaultValue` | yes | string | Use `$__all` for an all-selection |
| `multi` | no | boolean | Allow multi-select |
| `includeAll` | no | boolean | Add **All** to the picker |

### DashboardWidget

| Field | Required | Type | Rules |
|---|---|---|---|
| `id` | yes | string | Unique within the dashboard |
| `title` | yes | string | Panel title |
| `description` | no | string | Panel subtitle |
| `kind` | yes | string | One of the widget kinds above |
| `gridPos` | yes | object | `{x, y, w, h}` |
| `queries` | yes* | array | *Not required for `markdown` and `topology` |
| `visualization` | no | object | Display settings |
| `partitions` | no | string[] | Per-panel override; inherits the dashboard scope when empty |

### Grid rules

Doctor uses a 12-column grid:

- `x`: `0..11`
- `y`: `>= 0`
- `w`: `1..12`
- `h`: `1..24`
- `x + w <= 12`
- Avoid overlapping widgets at the same `y` unless intentionally layered.

### WidgetQuery

| Field | Required | Type | Rules |
|---|---|---|---|
| `refId` | yes | string | `A`, `B`, `C`, ... |
| `datasource` | yes | string | `prometheus`, `loki`, `tempo`, `native_logs`, `native_traces` |
| `expr` | yes | string | PromQL, LogQL, or TraceQL depending on datasource |
| `legendFormat` | no | string | Prometheus-style template, e.g. `{{service}}` |
| `disabled` | no | boolean | Hide without deleting |

### WidgetVisualization

| Field | Required | Type | Rules |
|---|---|---|---|
| `displayMode` | no | string | `line`, `area`, `bars` (timeseries only) |
| `stacked` | no | boolean | Stack area/bar series |
| `showExemplars` | no | boolean | Where supported |
| `unit` | no | string | `none`, `short`, `percent`, `seconds`, `ms`, `bytes`, `bps`, `reqps` |
| `decimals` | no | number | Display precision |
| `thresholds` | no | array | `{value, color}` where color is `green`, `amber`, or `red` |
| `markdownContent` | conditional | string | Required for `markdown` |

A `gauge` widget renders the latest query value against a 0–100 scale. Use a
percentage expression if the value is not already 0–100.

## 5. Dashboard Bundle v1

```yaml
version: doctor.dashboard.bundle/v1
exportedAt: "2026-11-05T08:00:00Z"
dashboards:
  - version: doctor.dashboard/v1
    id: api-overview
    title: API overview
    tenant: default
    layout: []
```

The bundle version must be exactly `doctor.dashboard.bundle/v1`. Each dashboard
keeps its own `version: doctor.dashboard/v1`.

## 6. Query contract

### Prometheus / PromQL

- Use real metric names discovered from `/api/v1/label/__name__/values`.
- Scope by canonical labels where possible:
  `guardian_partition`, `guardian_intent`, `guardian_asset`,
  `guardian_component`, `service`, `namespace`, `pod`.
- Prefer aggregation over high-cardinality labels:
  `sum by (service) (...)` rather than plotting every pod UID.
- Use range functions for trends: `rate(...[$__rate_interval])`,
  `avg_over_time(...[5m])`, `histogram_quantile(0.95, sum by (le) (...))`.
- Metric names and label names are case-sensitive; label values are exact unless
  a regex matcher (`=~`) is used.

Supported macros inside dashboard queries:

| Macro | Expands to |
|---|---|
| `$__interval` | Query step, e.g. `30s` |
| `$__range` | Dashboard time range, e.g. `1h` |
| `$__rate_interval` | Safe `rate()` window derived from the range (at least 60s) |
| `$tenant` | Current tenant |
| `$name` | Value of the dashboard variable `name` |

For an all-selection, variables expand to `.*`; use `=~` with the variable.
Multi-select values expand to a regex alternation, e.g. `(api|worker)`.

`legendFormat` templates use `{{label}}`, for example `{{service}}` or
`{{guardian_partition}}/{{service}}`.

### Loki / LogQL

- Discover real labels before writing selectors.
- Severity is the `level` label; use `level=~"(?i)warn"` rather than a body match.
- Example: `{guardian_partition="$partition"} |= "error"`.
- Use `kind: logs` with `datasource: loki`.

### Tempo / TraceQL

- Use `kind: traces` with `datasource: tempo`.
- Example: `{ resource.service.name = "monofs-router" && status = error }`.
- Discover tags with Doctor's TraceQL tag endpoint before inventing names.

## 7. Choosing a graph type

| User asks for | Use |
|---|---|
| "show latency / CPU / memory over time" | `timeseries` or saved graph |
| "compare series" | `timeseries` with `sum by (...)` |
| "stacked traffic" | `timeseries`, `displayMode: bars` or `area`, `stacked: true` |
| "current error rate / saturation" | `gauge` or `stat` |
| "top 10 endpoints" | `toplist` with `topk(10, ...)` |
| "table of services" | `table` |
| "show errors" | `logs` |
| "slow traces" | `traces` |
| "fleet map" | `topology` |
| "notes" | `markdown` |

## 8. RBAC and partition scope

`partitions` is an authorization declaration, not just documentation.

### Server-side dashboards

When Doctor access enforcement is active (`PolicyGate.Enforced()`):

| Operation | Required access |
|---|---|
| Create or update a dashboard | `maintainer` or `admin` role on **every** effective partition (document + widget overrides) |
| Delete a dashboard | `maintainer` or `admin` role on every effective partition of the stored document |
| Read or export a dashboard | partition visibility on every effective partition |
| List dashboards | the dashboard is omitted unless every effective partition is visible |

Rules:

- A document with no `partitions` is treated as legacy/built-in. It stays
  readable, but a non-admin write is rejected until an admin adds a partition
  scope.
- `partitions: ["*"]` is a global scope and is effectively admin-only because
  non-admin grants do not match the wildcard partition.
- A global `admin` role bypasses the partition checks.
- Observe mode (gate configured but not enforcing) keeps writes open so enabling
  the policy does not surprise existing writers.
- Validation happens before any write. A bundle import that declares one
  unauthorized dashboard is rejected as a whole; partial imports are not written.
- Per-widget `partitions` widen the required scope. A dashboard with an `agent`
  document scope and a `monofs` widget requires maintainer/admin on both.

### Saved graph bundles

Saved graphs are stored in the user's browser (`localStorage`) and are not
shared server-side state. `partitions` is preserved as metadata and used for
display/export, but **cannot be enforced by the server**. Use a Doctor Dashboard
when a graph must be stored under partition RBAC.

Telemetry reads are still filtered independently: metrics are globally readable
by design, while logs and traces are filtered per partition at query time.

### Example failure

A user with `maintainer` on `agent` only:

```yaml
partitions: [agent, monofs]
```

is rejected with `403 forbidden: maintainer or admin role required on partition
"monofs"`. Adding the `monofs` panel or changing the scope cannot be used to
escalate access.

## 9. Validation checklist

Before claiming a graph is ready:

- [ ] Metric names come from discovery, not memory.
- [ ] A real query returned at least one series and one sample for the target range.
- [ ] The query is bounded (`sum by`, `topk`, explicit label filters).
- [ ] `rangeMinutes` and the query window agree.
- [ ] `refreshIntervalSec` is `0` or at least `5`.
- [ ] IDs are stable, unique, and URL-safe.
- [ ] `partitions` is present and lists every partition the graph/dashboard affects.
- [ ] The author has maintainer/admin on every listed partition (server dashboards).
- [ ] `partitions: ["*"]` is intentional and the author is a global admin.
- [ ] Every widget has a title and a valid kind.
- [ ] `gridPos.x + w <= 12`; widgets do not overlap accidentally.
- [ ] `displayMode` and `kind` agree (`bars` only on `timeseries`; gauge uses `kind: gauge`).
- [ ] `unit` is correct for the value (percent values are already 0–100).
- [ ] No secrets, tokens, or internal credentials appear in labels or annotations.

## 10. Import and export paths

### Saved graphs

- UI: partition view → **Ad-hoc and Persistent Dashboards** → **Import JSON/YAML**
- Export: same panel → **Export JSON** or **Export YAML**
- Storage: browser `localStorage`, additive merge, duplicates skipped

### Dashboards

- UI: **Dashboards** → **Import** (JSON or YAML)
- Export: **Dashboards** → **Export JSON** or **Export YAML**
- API import:

```bash
curl -sS -X POST "$DOCTOR_URL/v1/dashboards/import?tenant=default&mode=overwrite" \
  -H 'Content-Type: application/yaml' \
  --data-binary @dashboard.yaml
```

Modes: `fail-on-conflict` (default), `skip-existing`, `overwrite`.

- API export:

```bash
curl -sS "$DOCTOR_URL/v1/dashboards/export?tenant=default&format=yaml"
```

Built-in presets are read-only. Clone them before editing.

## 11. Compatibility rules

- Keep `version` fields exact. A reader may reject an unknown version.
- Additive fields are safe. Do not rename existing fields.
- Unknown fields are ignored by the current importers but may not survive a
  round-trip export.
- The saved graph importer accepts a single graph, an array of graphs, a single
  dashboard, a bundle, and common aliases (`name`, `expr`, `promql`,
  `range_minutes`, `refresh_interval_sec`, `affected_partitions`, `affects`). The
  canonical form in this document should still be used for generated files.
