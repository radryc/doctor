# Logical flow: UI

The UI is a React 19 + TypeScript + Vite single-page application under `frontend/src`. It is built
into `internal/query/ui/dist` and embedded into `doctor-query` with `//go:embed`.

## 1. Bootstrap

```
main.tsx
  -> App.tsx
     -> ConfigProvider (GET /v1/ui/config)
     -> HashRouter
        -> AppLayout
           -> TimeRangeProvider
           -> InspectorProvider
           -> Sidebar / TopBar / CommandPalette / InspectorDrawer
           -> route element
```

- Routing is hash-based (`HashRouter`), so links look like `#/logs?partition=...`.
- `ConfigProvider` supplies `default_tenant`, `guardian_url`, and `default_partition`.
- `TimeRangeProvider` owns the global relative/absolute time window, auto-refresh interval, step
  interval, and refresh tick. Pages subscribe to `resolve()`, `rangeString()`, `stepInterval()`, and
  `refreshTick`.
- `InspectorProvider` opens a right-hand drawer for traces, logs, services, or rollouts.
- `AppLayout` installs keyboard shortcuts: `Cmd/Ctrl+K` or `/` opens the command palette; `g` then
  `d/b/s/r/t/l/m/f/p` navigates to on-call/dashboards/services/rules/traces/logs/metrics/fleet/
  synthetics.
- `TopBar` fetches `/v1/ui/user` and shows the display identity; it also links `/auth/logout`.
- `FederationStatusPill` polls federation status for the top bar.

## 2. Routes

| Route | Page | Primary data sources |
|---|---|---|
| `/` | `DashboardPage` (on-call triage) | `/v1/guardian/overview` |
| `/dashboards` | `DashboardsPage` (browser) | `/v1/dashboards` |
| `/dashboards/:id` | `DashboardsPage` (viewer/editor) | `/v1/dashboards/{tenant}/{id}`, widget queries |
| `/services` | `ServicesPage` catalog | `/v1/services/discover`, Guardian overview for partitions |
| `/services/:name` | `ServicesPage` detail | `/v1/services/discover`, Prometheus, Tempo metrics, logs, traces |
| `/federation` | `FederationCatalogPage` | `/v1/federation/status`, `/v1/federation/targets` |
| `/probes` | `ProbesPage` | `/v1/guardian/probes` |
| `/rules` | `RecordingRulesPage` | `/v1/rules`, `/v1/rules/status`, `/v1/rules/{tenant}/{group}`, evaluate |
| `/traces` | `TracesPage` | `/v1/traces/recent`, `/v1/logs/services`, `/v1/traces/{id}` |
| `/logs` | `LogsPage` | `/v1/logs/search`, `/v1/logs/services` |
| `/metrics` | `MetricsPage` | Prometheus labels/query APIs, Guardian overview, log services |
| `/fleet` | `FleetDashboard` | Guardian overview/topology/health/events |
| `/partition/:name` | `PartitionViewPage` | Guardian topology/health/events |
| `/partition/:name/assets` | `PartitionAssetsPage` | Guardian topology, Prometheus, logs, traces, Tempo metrics |
| `*` | redirect `/` | |

`FleetViewPage.tsx` exists but is not referenced by `App.tsx`; it is dead UI code. `FleetView.tsx`
is a shared component used by `FleetDashboard` and the partition asset pages.

## 3. Page flows

### 3.1 On-call (`DashboardPage`)

- Loads Guardian overview; auto-refreshes every 30s (toggleable).
- Computes overall tone (healthy/attention/failing) and a triage list of unhealthy partitions sorted
  by tone, failing assets, then attention assets.
- Shows summary cards, pie/bar charts, partition triage rows.
- “Copy triage” builds deep links to partition assets/logs/traces/metrics and copies them.
- Links to `/fleet` for the full fleet view.

### 3.2 Dashboards (`DashboardsPage`)

- Browser view: list, search, open, clone, delete, import, export.
- Viewer: loads document, renders variables and widgets; edit mode adds widget editor and layout
  controls; saves with `meta.versionId` as `If-Match`.
- `DashboardWidgetCard` dispatches by widget kind:
  - markdown → static;
  - stat → Prometheus instant or Tempo metrics instant;
  - timeseries → Prometheus range, Loki range, or Tempo metrics range;
  - toplist → Prometheus instant vector sorted;
  - gauge → single value;
  - table → native JSON endpoint or query results;
  - logs → `/v1/logs/search`;
  - traces → `/api/search`;
  - topology → native topology data.
- Variable interpolation supports `$name`, `${name}`, `$__interval`, `$__range`,
  `$__rate_interval`, and `$__all` → `.*` / alternation.

### 3.3 Services (`ServicesPage`)

- Catalog: `discoverServices` with the global window; filter by partition/intent/search; show
  signals (M/T/L), trace/log counts, error rate, alias, kind.
- Detail: RED cards (rate, errors, p95) queried from Prometheus with fallbacks to Tempo metrics;
  probe cards from Prometheus; recent traces via `/v1/traces/recent` and `/v1/traces/{id}`; recent
  logs via `/v1/logs/search`.

### 3.4 Traces (`TracesPage`)

- List: `/v1/traces/recent` with server-side partition/intent/asset/service filters and cursor
  pagination; service/identity options from `/v1/logs/services?include=traces`.
- A complete trace ID typed in the search box is an exact lookup: navigate to
  `/traces?trace_id=...` and render `SingleTraceView` (`/v1/traces/{id}`).
- `TraceWaterfall`, `TraceFlow`, `TraceDetails` render spans, service/resource attributes, events, and
  error status.
- Live refresh every 30s when enabled.
- Copy view link includes range and identity filters.

### 3.5 Logs (`LogsPage`)

- `searchLogs` with time range, tenant, text query, service/partition/intent/asset, log group, limit
  500.
- Service/identity/partition options from `/v1/logs/services?include=aws`; log groups from AWS.
- Client-side filtering: severity toggles, text, facets. Facet include sets a field filter; facet
  exclude adds `AND NOT field="value"` to the query text (this is forwarded as free text, not
  necessarily valid LogQL).
- Auto-selects the first AWS log group when native results are empty and groups exist.
- Log rows link to traces via `trace_id`; copy view link includes filters.
- `LogHistogram`, `LogFacetSidebar` support volume/faceting.

### 3.6 Metrics (`MetricsPage`)

- Metric names from `/api/v1/label/__name__/values` with tenant/time/partition.
- Intent/asset label values from `/api/v1/label/guardian_intent/values` and
  `guardian_asset/values`.
- Partitions from Guardian overview plus `/v1/logs/services`.
- Query editor supports instant/range, step, timeout, graph/table/raw views, URL state, quick ranges,
  and a metric picker (250 names).
- Auto-runs a preferred metric when opened with a query/metric or when a partition has data.
- Follows the global time picker; `$__rate_interval` is not used here—step is explicit.

### 3.7 Fleet and partition views

`FleetDashboard`:

- loads Guardian overview + topology every 30s;
- builds a partition model (intents → assets) from topology nodes;
- KPI cards, search/sort, compact/detailed modes, topology map;
- selecting an asset opens `HubDetailView` and loads Guardian health/events for the partition;
- supports a “helper manifest” JSON upload used by asset pages to map asset types to metric queries.

`PartitionViewPage`:

- loads topology + health + events for one partition;
- shows asset counts, health score, recommendation, recent events;
- “copy diagnostics” builds a text bundle.

`PartitionAssetsPage` (3k+ lines):

- loads partition topology, builds intents/assets, then on asset selection loads:
  - key metric discovery and a key metric chart;
  - health metric rules;
  - custom graph query;
  - latency/throughput charts via Prometheus range queries;
  - recent traces (with per-service fallback) and a trace waterfall/flow;
  - recent logs (per-service fallback);
- supports ad-hoc graph boards, saved graphs, saved dashboards, import/export JSON, and a location
  state editor.

`HubDetailView` duplicates much of the asset detail logic for the fleet page (metric selection,
health rules, traces, logs, graphs).

### 3.8 Federation (`FederationCatalogPage`)

- loads status + paginated targets;
- shows fleet summary, filters (account/region/partition/query), target detail with metadata counts
  and optional full metric/log-group/X-Ray lists;
- no token display (backend never returns tokens).

### 3.9 Synthetics (`ProbesPage`)

- loads `/v1/guardian/probes` every 30s;
- computes passing/failing/stale, pusher summary, partition/type filters;
- probe cards include status, duration, target, failure reason, links to logs/traces.

### 3.10 Recording rules (`RecordingRulesPage`)

- polls `/v1/rules` and `/v1/rules/status` every 15s;
- opens a group’s YAML via the query-service rules proxy;
- manual evaluate posts `/v1/rules/evaluate`;
- displays scheduler/lease status and an example rule.

## 4. Shared libraries/components

| Path | Purpose |
|---|---|
| `api/client.ts` | typed fetch wrappers for every backend route; 60s abort timeout |
| `context/ConfigContext.tsx` | UI config |
| `context/TimeRangeContext.tsx` | global time range, step, refresh, zoom/shift |
| `context/InspectorContext.tsx` | right-hand detail drawer |
| `lib/variables.ts` | dashboard variable/macro interpolation |
| `lib/logs.ts` | client-side identity/service resolution for log records |
| `lib/theme.ts` | theme persistence (`doctor.theme`) |
| `lib/chartTheme.ts` | ECharts theme tokens |
| `lib/clipboard.ts`, `lib/download.ts` | copy/download helpers |
| `components/layout/*` | shell, sidebar, top bar, command palette, time picker, federation pill, theme picker, inspector |
| `components/charts/*` | ECharts wrapper, time series, metric line, mini line, histogram, waterfall, flow, details, legend, query builder, expand modal |
| `components/dashboards/*` | variable bar, widget card, widget editor, import/export modal |
| `components/logs/LogFacetSidebar.tsx` | log facet UI |
| `components/topology/*` | fleet/partition graphs, node details, node-kind helpers |
| `components/ui/*` | table and stat card primitives |

## 5. UI state and URL conventions

- Global time range is in React state, not URL.
- Page filters are often mirrored to URL query params (`partition`, `intent`, `asset`, `service`,
  `range`, `sev`, `q`, `trace_id`, `logGroup`, metric `query`/`from`/`to`/`step`/`mode`).
- Dashboard variables use `var-*` params (client helper supports them), though the viewer currently
  mostly keeps them in component state.
- Hash routing means deep links are `origin/path#/route?params`.

## 6. UI gaps and risks

- No frontend tests, lint script, or CI build step. The `frontend` npm scripts are dev/build/preview
  only.
- `FleetViewPage.tsx` is unused.
- `PartitionAssetsPage`, `HubDetailView`, and `FleetDashboard` contain duplicated fetching and
  asset-detail logic; many effects are independent and can issue overlapping requests.
- Several pages fall back from native data to Prometheus/Tempo/log searches with heuristic metric
  names; failures can look like “no data” rather than a backend error.
- Logs “exclude facet” writes `AND NOT ...` into the free-text query; the backend treats it as a
  literal line filter, not a structured boolean expression.
- Dashboard queries are interpolated entirely client-side; there is no server-side validation of
  datasource names, backends, or query syntax.
- The UI does not expose or manage OIDC login state beyond `/v1/ui/user` and the sign-out link.
