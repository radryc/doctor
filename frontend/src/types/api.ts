// ── UI Config ────────────────────────────────────────────────────────────────
export interface UIConfig {
  default_tenant: string;
  guardian_url: string;
  default_partition: string;
}

// ── Traces ────────────────────────────────────────────────────────────────────
export interface TraceRecord {
  tenant: string;
  trace_id: string;
  span_id: string;
  parent_span_id?: string;
  name: string;
  kind: string;
  service: string;
  partition?: string;
  intent?: string;
  asset?: string;
  start_time: string;
  end_time: string;
  status_code?: string;
  status_message?: string;
  resource_attributes?: Record<string, string>;
  attributes?: Record<string, string>;
  events?: TraceEvent[];
  // computed client-side
  duration_ms?: string;
}

export interface TraceEvent {
  name: string;
  timestamp: string;
  attributes?: Record<string, string>;
}

export interface TraceResponse {
  trace_id: string;
  spans: TraceRecord[];
}

export interface TraceSummary {
  trace_id: string;
  service: string;
  partition?: string;
  intent?: string;
  asset?: string;
  span_count: number;
  min_time: string;
  max_time: string;
}

export interface RecentTracesResponse {
  traces: TraceSummary[];
}

// ── Logs ──────────────────────────────────────────────────────────────────────
export interface LogRecord {
  tenant: string;
  service: string;
  partition?: string;
  intent?: string;
  asset?: string;
  timestamp: string;
  severity_number?: number;
  severity_text?: string;
  body: string;
  trace_id?: string;
  span_id?: string;
  resource_attributes?: Record<string, string>;
  attributes?: Record<string, string>;
}

export interface LogSearchResponse {
  results: LogRecord[];
}

export interface LogServicesResponse {
  services: string[];
}

// ── Metrics ───────────────────────────────────────────────────────────────────
export interface MetricPoint {
  timestamp: string;
  value: number;
  labels?: Record<string, string>;
}

export interface MetricSeries {
  label: string;
  points: MetricPoint[];
  labels?: Record<string, string>;
}

export interface MetricRangeResponse {
  metric: string;
  points: MetricPoint[];
}

// ── Status / Prometheus compatibility ───────────────────────────────────────
export interface HealthResponse {
  status: string;
}

export interface PrometheusEnvelope<T> {
  status: "success" | "error";
  data: T;
  errorType?: string;
  error?: string;
}

export type PrometheusBuildInfoResponse = PrometheusEnvelope<{
  version?: string;
  revision?: string;
  branch?: string;
  buildUser?: string;
  buildDate?: string;
  goVersion?: string;
}>;

export type PrometheusLabelValuesResponse = PrometheusEnvelope<string[]>;

export interface PrometheusSeriesMetric {
  [key: string]: string;
}

export interface PrometheusVectorResult {
  metric: PrometheusSeriesMetric;
  value: [number, string];
}

export interface PrometheusMatrixResult {
  metric: PrometheusSeriesMetric;
  values: Array<[number, string]>;
}

export interface PrometheusScalarData {
  resultType: "scalar" | "string";
  result: [number, string];
}

export interface PrometheusVectorData {
  resultType: "vector";
  result: PrometheusVectorResult[];
}

export interface PrometheusMatrixData {
  resultType: "matrix";
  result: PrometheusMatrixResult[];
}

export type PrometheusQueryResponse = PrometheusEnvelope<
  PrometheusScalarData | PrometheusVectorData | PrometheusMatrixData
>;

export interface GuardianHealthResponse {
  partition?: string;
  intent?: string;
  overall_health?: {
    score?: number;
    level?: string;
    reason?: string;
    check_at?: string;
  };
  recent_failures?: number;
  recent_drifts?: number;
  recommendation?: string;
  window?: string;
  computed_at?: string;
}

export interface GuardianTopologyHealth {
  score?: number;
  level?: string;
}

export interface GuardianTopologyNode {
  id: string;
  type: "partition" | "intent" | "asset" | string;
  name: string;
  partition?: string;
  intent?: string;
  asset?: string;
  status?: string;
  flow_state?: string;
  flow_source?: string;
  flow_updated_at?: string;
  health?: GuardianTopologyHealth;
  meta?: Record<string, string>;
}

export interface GuardianTopologyEdge {
  from: string;
  to: string;
  relation?: string;
}

export interface GuardianTopologyResponse {
  tenant?: string;
  partition?: string;
  nodes: GuardianTopologyNode[];
  edges: GuardianTopologyEdge[];
  built_at?: string;
}

export interface GuardianEventRecord {
  kind?: string;
  partition?: string;
  intent?: string;
  asset?: string;
  timestamp?: string;
  status?: string;
  message?: string;
}

export interface GuardianEventsResponse {
  tenant?: string;
  partition?: string;
  intent?: string;
  events: GuardianEventRecord[];
  returned?: number;
}

export type GuardianOverview = {
  generatedAt: string;
  summary: {
    partitions: number;
    intents: number;
    assets: number;
    healthyAssets: number;
    attentionAssets: number;
    failingAssets: number;
    healthyIntents: number;
    driftedIntents: number;
    failedIntents: number;
    servicesHealthy: number;
    servicesAttention: number;
  };
  partitions: Array<{
    name: string;
    status: string;
    displayStatus: string;
    health: string;
    intentCount: number;
    assetCount: number;
    healthyAssets: number;
    attentionAssets: number;
    failingAssets: number;
    healthyIntents: number;
    driftedIntents: number;
    failedIntents: number;
    lastReconciledAt: string;
  }>;
};

// ── Service Discovery ───────────────────────────────────────────────────────
export interface DiscoveredService {
  name: string;
  has_metrics: boolean;
  has_traces: boolean;
  has_logs: boolean;
  metric_names?: string[];
  partition?: string;
  intent?: string;
  last_seen: string;
  trace_count: number;
  log_count: number;
  error_rate?: number;
  suggested_dashboards?: string[];
}

export interface ServiceDiscoveryResponse {
  tenant: string;
  window: string;
  services: DiscoveredService[];
  total_services: number;
  discovered_at: string;
}

export interface ServiceSuggestion {
  label: string;
  query: string;
  description: string;
  type: "rate" | "latency" | "errors" | "saturation";
  window: string;
}

export interface ServiceSuggestResponse {
  service: string;
  window: string;
  suggestions: ServiceSuggestion[];
  generated_at: string;
}
