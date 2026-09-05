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
  ServiceDiscoveryResponse,
  ServiceSuggestResponse,
} from "../types/api";

async function fetchJSON<T>(url: string): Promise<T> {
  const res = await fetch(url, {
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

  getHealth: () => fetchJSON<HealthResponse>("/healthz"),

  getBuildInfo: () =>
    fetchJSON<PrometheusBuildInfoResponse>("/api/v1/status/buildinfo"),

  getRecentTraces: (params: URLSearchParams) =>
    fetchJSON<RecentTracesResponse>(`/v1/traces/recent?${params}`),

  getTrace: (traceId: string, tenant: string) =>
    fetchJSON<TraceResponse>(
      `/v1/traces/${encodeURIComponent(traceId)}?tenant=${encodeURIComponent(tenant)}`
    ),

  getLogServices: (tenant: string) =>
    fetchJSON<LogServicesResponse>(
      `/v1/logs/services?tenant=${encodeURIComponent(tenant)}`
    ),

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

  discoverServices: (params: URLSearchParams) =>
    fetchJSON<ServiceDiscoveryResponse>(`/v1/services/discover?${params}`),

  suggestServiceQueries: (params: URLSearchParams) =>
    fetchJSON<ServiceSuggestResponse>(`/v1/services/suggest?${params}`),
};
