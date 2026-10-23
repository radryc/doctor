import type {
  UIConfig,
  RecentTracesResponse,
  TraceResponse,
  LogSearchResponse,
  LogServicesResponse,
  MetricRangeResponse,
  PrometheusBuildInfoResponse,
  PrometheusLabelValuesResponse,
  PrometheusQueryResponse,
  HealthResponse,
  GuardianHealthResponse,
  GuardianOverview,
  GuardianTopologyResponse,
  GuardianEventsResponse,
  RecordingRulesResponse,
  RecordingRulesStatus,
  UIUser,
  ProxyEndpointsResponse,
  ServiceDiscoveryResponse,
  ProbesResponse,
  TempoSearchResponse,
  TempoQueryRangeResponse,
  LokiQueryResponse,
} from "../types/api";
import type { DashboardDocument, DashboardVersion, DashboardBundle } from "../types/dashboard";

async function fetchText(url: string): Promise<string> {
  const res = await fetch(url, {
    headers: { Accept: "application/yaml, text/plain, */*" },
    signal: AbortSignal.timeout(60_000),
  });
  const body = await res.text();
  if (!res.ok) {
    throw new Error(body || `HTTP ${res.status}`);
  }
  return body;
}

async function fetchJSONBody<T>(url: string, method: string, body: unknown): Promise<T> {
  const res = await fetch(url, {
    method,
    headers: { Accept: "application/json", "Content-Type": "application/json" },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(60_000),
  });
  const ct = res.headers.get("content-type") ?? "";
  const data = ct.includes("application/json")
    ? await res.json()
    : { error: await res.text() };
  if (!res.ok)
    throw new Error(
      (data as { error?: string }).error ?? `HTTP ${res.status}`
    );
  return data as T;
}

async function fetchJSON<T>(url: string, method = "GET"): Promise<T> {
  const res = await fetch(url, {
    method,
    headers: { Accept: "application/json" },
    signal: AbortSignal.timeout(60_000),
  });
  const ct = res.headers.get("content-type") ?? "";
  const body = ct.includes("application/json")
    ? await res.json()
    : { error: await res.text() };
  if (!res.ok)
    throw new Error(
      (body as { error?: string }).error ?? `HTTP ${res.status}`
    );
  return body as T;
}

export const api = {
  getConfig: () => fetchJSON<UIConfig>("/v1/ui/config"),

  getUser: () => fetchJSON<UIUser>("/v1/ui/user"),

  getHealth: () => fetchJSON<HealthResponse>("/healthz"),

  getBuildInfo: () =>
    fetchJSON<PrometheusBuildInfoResponse>("/api/v1/status/buildinfo"),

  getRecentTraces: (params: URLSearchParams) =>
    fetchJSON<RecentTracesResponse>(`/v1/traces/recent?${params}`),

  getTrace: (traceId: string, tenant: string) =>
    fetchJSON<TraceResponse>(
      `/v1/traces/${encodeURIComponent(traceId)}?tenant=${encodeURIComponent(tenant)}`
    ),

  getLogServices: (tenant: string, partition = "", includeTraces = false, includeAWS = false) => {
    const params = new URLSearchParams({ tenant });
    if (partition) params.set("partition", partition);
    const include: string[] = [];
    if (includeTraces) include.push("traces");
    if (includeAWS) include.push("aws");
    if (include.length > 0) params.set("include", include.join(","));
    return fetchJSON<LogServicesResponse>(`/v1/logs/services?${params}`);
  },

  searchLogs: (params: URLSearchParams) =>
    fetchJSON<LogSearchResponse>(`/v1/logs/search?${params}`),

  getMetricRange: (params: URLSearchParams) =>
    fetchJSON<MetricRangeResponse>(`/v1/metrics/range?${params}`),

  getMetricNames: (params?: URLSearchParams) => {
    const query = params?.toString();
    return fetchJSON<PrometheusLabelValuesResponse>(
      `/api/v1/label/__name__/values${query ? `?${query}` : ""}`
    );
  },

  queryPrometheus: (params: URLSearchParams) =>
    fetchJSON<PrometheusQueryResponse>(`/api/v1/query?${params}`),

  queryPrometheusRange: (params: URLSearchParams) =>
    fetchJSON<PrometheusQueryResponse>(`/api/v1/query_range?${params}`),

  getGuardianHealth: (params: URLSearchParams) =>
    fetchJSON<GuardianHealthResponse>(`/v1/guardian/health?${params}`),

  getGuardianTopology: (params: URLSearchParams) =>
    fetchJSON<GuardianTopologyResponse>(`/v1/guardian/topology?${params}`),

  getGuardianEvents: (params: URLSearchParams) =>
    fetchJSON<GuardianEventsResponse>(`/v1/guardian/events?${params}`),

  guardianOverview: () =>
    fetchJSON<GuardianOverview>("/v1/guardian/overview"),

  getRecordingRuleGroups: () =>
    fetchJSON<RecordingRulesResponse>("/v1/rules"),

  getRecordingRuleStatus: () =>
    fetchJSON<RecordingRulesStatus>("/v1/rules/status"),

  getRecordingRuleGroup: (tenant: string, group: string) =>
    fetchText(`/v1/rules/${encodeURIComponent(tenant)}/${encodeURIComponent(group)}`),

  evaluateRecordingRuleGroup: (tenant: string, group: string) => {
    const params = new URLSearchParams({ tenant, group });
    return fetchJSON<unknown>(`/v1/rules/evaluate?${params}`, "POST");
  },

  // ── Proxy Endpoints ─────────────────────────────────────────────────────────
  getProxyEndpoints: (params?: URLSearchParams) => {
    const query = params?.toString();
    return fetchJSON<ProxyEndpointsResponse>(
      `/v1/proxy/endpoints${query ? `?${query}` : ""}`
    );
  },

  // Generic native API GET used by dashboard table widgets.
  getNativeJSON: <T>(path: string) => fetchJSON<T>(path),

  // ── Guardian Synthetic Probes ───────────────────────────────────────────────
  getProbes: (params?: URLSearchParams) => {
    const query = params?.toString();
    return fetchJSON<ProbesResponse>(
      `/v1/guardian/probes${query ? `?${query}` : ""}`
    );
  },

  // ── Tempo search & TraceQL metrics ─────────────────────────────────────────
  searchTraces: (params: URLSearchParams) =>
    fetchJSON<TempoSearchResponse>(`/api/search?${params}`),

  queryTempoMetricsRange: (params: URLSearchParams) =>
    fetchJSON<TempoQueryRangeResponse>(`/api/metrics/query_range?${params}`),

  // ── Loki range search ───────────────────────────────────────────────────────
  queryLokiRange: (params: URLSearchParams) =>
    fetchJSON<LokiQueryResponse>(`/loki/api/v1/query_range?${params}`),

  // ── Service Discovery ────────────────────────────────────────────────────────
  discoverServices: (params: URLSearchParams) =>
    fetchJSON<ServiceDiscoveryResponse>(`/v1/services/discover?${params}`),

  suggestServices: (params: URLSearchParams) =>
    fetchJSON<{ suggestions: string[] }>(`/v1/services/suggest?${params}`),

  // ── Loki ─────────────────────────────────────────────────────────────────────
  lokiLabels: (params: URLSearchParams) =>
    fetchJSON<{ status: string; data: string[] }>(`/loki/api/v1/labels?${params}`),

  lokiLabelValues: (label: string, params: URLSearchParams) =>
    fetchJSON<{ status: string; data: string[] }>(
      `/loki/api/v1/label/${encodeURIComponent(label)}/values?${params}`
    ),

  lokiVolumeRange: (params: URLSearchParams) =>
    fetchJSON<unknown>(`/loki/api/v1/index/volume_range?${params}`),

  lokiStats: (params: URLSearchParams) =>
    fetchJSON<unknown>(`/loki/api/v1/index/stats?${params}`),

  // ── Dashboards ───────────────────────────────────────────────────────────────
  listDashboards: (params: URLSearchParams) =>
    fetchJSON<{ dashboards: DashboardDocument[] }>(`/v1/dashboards?${params}`),

  getDashboard: (tenant: string, id: string) =>
    fetchJSON<DashboardDocument>(
      `/v1/dashboards/${encodeURIComponent(tenant)}/${encodeURIComponent(id)}`
    ),

  putDashboard: (tenant: string, id: string, doc: DashboardDocument, ifMatch?: string) => {
    const url = `/v1/dashboards/${encodeURIComponent(tenant)}/${encodeURIComponent(id)}`;
    return fetch(url, {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json",
        ...(ifMatch ? { "If-Match": ifMatch } : {}),
      },
      body: JSON.stringify(doc),
    }).then(async (res) => {
      const data = await res.json().catch(() => ({ error: `HTTP ${res.status}` }));
      if (!res.ok) throw new Error(data?.error ?? `HTTP ${res.status}`);
      return data as { status: string };
    });
  },

  deleteDashboard: (tenant: string, id: string) =>
    fetchJSON<{ status: string }>(
      `/v1/dashboards/${encodeURIComponent(tenant)}/${encodeURIComponent(id)}`,
      "DELETE"
    ),

  getDashboardVersions: (tenant: string, id: string) =>
    fetchJSON<{ versions: DashboardVersion[] }>(
      `/v1/dashboards/${encodeURIComponent(tenant)}/${encodeURIComponent(id)}/versions`
    ),

  importDashboards: (body: DashboardDocument | DashboardBundle, tenant?: string) => {
    const params = tenant ? `?tenant=${encodeURIComponent(tenant)}` : "";
    return fetchJSONBody<{ imported: number }>(`/v1/dashboards/import${params}`, "POST", body);
  },

  importDashboardsText: (text: string, tenant?: string, contentType = "application/json") => {
    const params = tenant ? `?tenant=${encodeURIComponent(tenant)}` : "";
    return fetch(`/v1/dashboards/import${params}`, {
      method: "POST",
      headers: { "Content-Type": contentType, Accept: "application/json" },
      body: text,
    }).then(async (res) => {
      const data = await res.json().catch(() => ({ error: `HTTP ${res.status}` }));
      if (!res.ok) throw new Error(data?.error ?? `HTTP ${res.status}`);
      return data as { imported: number };
    });
  },

  dashboardDownloadURL: (tenant: string, id: string, format: "json" | "yaml") =>
    `/v1/dashboards/${encodeURIComponent(tenant)}/${encodeURIComponent(id)}?format=${format}&download=1`,

  exportDashboards: (params: URLSearchParams) =>
    `/v1/dashboards/export?${params}`,
};
