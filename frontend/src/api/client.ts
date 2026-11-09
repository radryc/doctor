import type {
  UIConfig,
  RecentTracesResponse,
  TraceResponse,
  LogSearchResponse,
  LogServicesResponse,
  MetricRangeResponse,
  MetricNameCatalog,
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
  FederationStatus,
  FederationTargetsResponse,
  TempoSearchResponse,
  TempoQueryRangeResponse,
  LokiQueryResponse,
  DuckDBSQLResponse,
} from "../types/api";
import type { DashboardDocument, DashboardVersion, DashboardBundle } from "../types/dashboard";
import type {
  Alert,
  AlertListResponse,
  Capabilities,
  Silence,
  SilenceListResponse,
  AWSPolicy,
  GCPPolicy,
  SourceListResponse,
  DeliveryResponse,
  ReceiverConfig,
  SubspaceRequest,
  AlertRuleGroupsResponse,
  AlertRuleFile,
  AlertPreviewResponse,
  AlertSpec,
  RoutingOverviewResponse,
  AlertSummaryResponse,
} from "../types/alerts";

// trackedFetch emits request start/end events so the shell can display a
// consistent refresh indicator on every page.
async function trackedFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  if (typeof window !== "undefined") {
    window.dispatchEvent(new Event("doctor:request-start"));
  }
  try {
    return await globalThis.fetch(input, init);
  } finally {
    if (typeof window !== "undefined") {
      window.dispatchEvent(new Event("doctor:request-end"));
    }
  }
}

async function fetchText(url: string): Promise<string> {
  const res = await trackedFetch(url, {
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
  const res = await trackedFetch(url, {
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

async function fetchJSONText<T>(
  url: string,
  method: string,
  body: string
): Promise<T> {
  const res = await trackedFetch(url, {
    method,
    headers: { Accept: "application/json", "Content-Type": "application/yaml" },
    body,
    signal: AbortSignal.timeout(60_000),
  });
  const ct = res.headers.get("content-type") ?? "";
  const data = ct.includes("application/json")
    ? await res.json()
    : { error: await res.text() };
  if (!res.ok) {
    throw new Error((data as { error?: string }).error ?? `HTTP ${res.status}`);
  }
  return data as T;
}

async function fetchJSON<T>(url: string, method = "GET"): Promise<T> {
  const res = await trackedFetch(url, {
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

async function fetchMetricCatalog(url: string): Promise<MetricNameCatalog> {
  const res = await trackedFetch(url, {
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
  const data = Array.isArray((body as { data?: unknown }).data)
    ? ((body as { data: string[] }).data ?? [])
    : [];
  const rawTotal = Number(res.headers.get("x-doctor-metric-total"));
  return {
    data,
    total: Number.isFinite(rawTotal) && rawTotal >= 0 ? rawTotal : data.length,
    truncated:
      (res.headers.get("x-doctor-metric-truncated") ?? "").toLowerCase() === "true",
    partial: res.headers.get("x-doctor-partial") ?? "",
  };
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

  queryDuckDBSQL: (body: {
    dataset: "logs" | "metrics" | "traces";
    query: string;
    from?: string;
    to?: string;
    limit?: number;
  }) => fetchJSONBody<DuckDBSQLResponse>("/v1/sql/query", "POST", body),

  getMetricRange: (params: URLSearchParams) =>
    fetchJSON<MetricRangeResponse>(`/v1/metrics/range?${params}`),

  getMetricNames: (params?: URLSearchParams) => {
    const query = params?.toString();
    return fetchJSON<PrometheusLabelValuesResponse>(
      `/api/v1/label/__name__/values${query ? `?${query}` : ""}`
    );
  },

  /** Metric-name catalog with server-side source filtering, search and limit. */
  getMetricCatalog: (params?: URLSearchParams) => {
    const query = params?.toString();
    return fetchMetricCatalog(
      `/api/v1/label/__name__/values${query ? `?${query}` : ""}`
    );
  },

  getMetricLabelValues: (label: string, params?: URLSearchParams) => {
    const query = params?.toString();
    return fetchJSON<PrometheusLabelValuesResponse>(
      `/api/v1/label/${encodeURIComponent(label)}/values${query ? `?${query}` : ""}`
    );
  },

  getMetricLabels: (params?: URLSearchParams) => {
    const query = params?.toString();
    return fetchJSON<{ status: string; data: string[] }>(
      `/api/v1/labels${query ? `?${query}` : ""}`
    );
  },

  getMetricMetadata: (metric?: string) => {
    const query = metric ? `?metric=${encodeURIComponent(metric)}` : "";
    return fetchJSON<{
      status: string;
      data: Record<string, { type?: string; help?: string; unit?: string }>;
    }>(`/api/v1/metadata${query}`);
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

  // ── Federation Catalog ──────────────────────────────────────────────────────
  getFederationStatus: () => fetchJSON<FederationStatus>("/v1/federation/status"),

  getFederationTargets: (params: URLSearchParams) =>
    fetchJSON<FederationTargetsResponse>(`/v1/federation/targets?${params}`),

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

  // ── Sentinel alerts ─────────────────────────────────────────────────────────
  getAlerts: (params: URLSearchParams) =>
    fetchJSON<AlertListResponse>(`/v1/alerts?${params}`),

  getAlert: (tenant: string, partition: string, fingerprint: string) =>
    fetchJSON<Alert>(
      `/v1/alerts/${encodeURIComponent(fingerprint)}?tenant=${encodeURIComponent(tenant)}&partition=${encodeURIComponent(partition)}`
    ),

  ackAlert: (tenant: string, partition: string, fingerprint: string, acked = true) =>
    fetchJSON<Alert>(
      `/v1/alerts/${encodeURIComponent(fingerprint)}/ack?tenant=${encodeURIComponent(tenant)}&partition=${encodeURIComponent(partition)}`,
      acked ? "POST" : "DELETE"
    ),

  getSilences: (params: URLSearchParams) =>
    fetchJSON<SilenceListResponse>(`/v1/alerts/silences?${params}`),

  createSilence: (tenant: string, partition: string, silence: Partial<Silence>) =>
    fetchJSONBody<Silence>(
      `/v1/alerts/silences?tenant=${encodeURIComponent(tenant)}&partition=${encodeURIComponent(partition)}`,
      "POST",
      silence
    ),

  deleteSilence: (tenant: string, partition: string, id: string) =>
    fetchJSON<{ status: string }>(
      `/v1/alerts/silences/${encodeURIComponent(tenant)}/${encodeURIComponent(partition)}/${encodeURIComponent(id)}`,
      "DELETE"
    ),

  getAlertCapabilities: (tenant: string, partition: string) =>
    fetchJSON<Capabilities>(
      `/v1/alerts/capabilities?tenant=${encodeURIComponent(tenant)}&partition=${encodeURIComponent(partition)}`
    ),

  getSources: (params: URLSearchParams) =>
    fetchJSON<SourceListResponse>(`/v1/alerts/sources?${params}`),

  getAWSPolicy: (params: URLSearchParams) =>
    fetchJSON<AWSPolicy>(`/v1/alerts/sources/aws/policy?${params}`),

  putAWSPolicy: (params: URLSearchParams, policy: AWSPolicy) =>
    fetchJSONBody<{ status: string; policy: AWSPolicy }>(
      `/v1/alerts/sources/aws/policy?${params}`,
      "PUT",
      policy
    ),

  getGCPPolicy: (params: URLSearchParams) =>
    fetchJSON<GCPPolicy>(`/v1/alerts/sources/gcp/policy?${params}`),

  putGCPPolicy: (params: URLSearchParams, policy: GCPPolicy) =>
    fetchJSONBody<{ status: string; policy: GCPPolicy }>(
      `/v1/alerts/sources/gcp/policy?${params}`,
      "PUT",
      policy
    ),

  getDelivery: (params: URLSearchParams) =>
    fetchJSON<DeliveryResponse>(`/v1/alerts/delivery?${params}`),

  createSubspaceAlert: (tenant: string, req: SubspaceRequest) =>
    fetchJSONBody<{ status: string; alert: Alert }>(
      `/v1/alerts/subspace?tenant=${encodeURIComponent(tenant)}`,
      "POST",
      req
    ),

  resolveSubspaceAlert: (tenant: string, id: string) =>
    fetchJSON<Alert>(
      `/v1/alerts/subspace/${encodeURIComponent(id)}/resolve?tenant=${encodeURIComponent(tenant)}`,
      "POST"
    ),

  getAlertRuleGroups: (tenant: string, partition: string) =>
    fetchJSON<AlertRuleGroupsResponse>(
      `/v1/alerts/rules?tenant=${encodeURIComponent(tenant)}&partition=${encodeURIComponent(partition)}`
    ),

  previewAlertRule: (
    tenant: string,
    partition: string,
    body: { spec?: AlertSpec; expr?: string }
  ) =>
    fetchJSONBody<AlertPreviewResponse>(
      `/v1/alerts/rules/preview?tenant=${encodeURIComponent(tenant)}&partition=${encodeURIComponent(partition)}`,
      "POST",
      body
    ),

  compileAlertRule: (tenant: string, partition: string, spec: AlertSpec) =>
    fetchJSONBody<{ expr: string }>(
      `/v1/alerts/rules/compile?tenant=${encodeURIComponent(tenant)}&partition=${encodeURIComponent(partition)}`,
      "POST",
      { spec }
    ),

  getRoutingOverview: (tenant: string) =>
    fetchJSON<RoutingOverviewResponse>(
      `/v1/alerts/routing/overview?tenant=${encodeURIComponent(tenant)}`
    ),

  getAlertSummary: (tenant: string) =>
    fetchJSON<AlertSummaryResponse>(
      `/v1/alerts/summary?tenant=${encodeURIComponent(tenant)}`
    ),

  getAlertRuleGroup: (tenant: string, partition: string, group: string) =>
    fetchJSON<AlertRuleFile>(
      `/v1/alerts/rules/${encodeURIComponent(tenant)}/${encodeURIComponent(partition)}/${encodeURIComponent(group)}`
    ),

  putAlertRuleGroup: (
    tenant: string,
    partition: string,
    group: string,
    file: AlertRuleFile | string
  ) => {
    if (typeof file === "string") {
      return fetchJSONText<{ status: string }>(
        `/v1/alerts/rules/${encodeURIComponent(tenant)}/${encodeURIComponent(partition)}/${encodeURIComponent(group)}`,
        "PUT",
        file
      );
    }
    return fetchJSONBody<{ status: string }>(
      `/v1/alerts/rules/${encodeURIComponent(tenant)}/${encodeURIComponent(partition)}/${encodeURIComponent(group)}`,
      "PUT",
      file
    );
  },

  deleteAlertRuleGroup: (tenant: string, partition: string, group: string) =>
    fetchJSON<{ status: string }>(
      `/v1/alerts/rules/${encodeURIComponent(tenant)}/${encodeURIComponent(partition)}/${encodeURIComponent(group)}`,
      "DELETE"
    ),

  testAlertRuleGroup: (tenant: string, partition: string, group: string) =>
    fetchJSON<unknown>(
      `/v1/alerts/rules/test?tenant=${encodeURIComponent(tenant)}&partition=${encodeURIComponent(partition)}&group=${encodeURIComponent(group)}`,
      "POST"
    ),

  getReceivers: (params: URLSearchParams) =>
    fetchJSON<ReceiverConfig>(`/v1/alerts/receivers?${params}`),

  putReceivers: (params: URLSearchParams, config: ReceiverConfig) =>
    fetchJSONBody<{ status: string }>(`/v1/alerts/receivers?${params}`, "PUT", config),

  // ── Dashboards ───────────────────────────────────────────────────────────────
  listDashboards: (params: URLSearchParams) =>
    fetchJSON<{ dashboards: DashboardDocument[] }>(`/v1/dashboards?${params}`),

  getDashboard: (tenant: string, id: string) =>
    fetchJSON<DashboardDocument>(
      `/v1/dashboards/${encodeURIComponent(tenant)}/${encodeURIComponent(id)}`
    ),

  putDashboard: (tenant: string, id: string, doc: DashboardDocument, ifMatch?: string) => {
    const url = `/v1/dashboards/${encodeURIComponent(tenant)}/${encodeURIComponent(id)}`;
    return trackedFetch(url, {
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
    return trackedFetch(`/v1/dashboards/import${params}`, {
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
