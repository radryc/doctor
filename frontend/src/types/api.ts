// ── UI Config ────────────────────────────────────────────────────────────────
export interface UIConfig {
  default_tenant: string;
  guardian_url: string;
  default_partition: string;
}

export interface UIUser {
  authenticated: boolean;
  subject?: string;
  email?: string;
  groups?: string[];
  client_id?: string;
  display?: string;
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
  duration_ms?: number;
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
  partitions: string[];
  log_groups?: string[];
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

export interface PrometheusHistogram {
  count?: string;
  sum?: string;
  buckets?: unknown[];
}

export type PrometheusHistogramPoint = [number, PrometheusHistogram];

export interface PrometheusVectorResult {
  metric: PrometheusSeriesMetric;
  value: [number, string];
  // Native Prometheus histograms use `histogram` instead of `value`.
  histogram?: PrometheusHistogramPoint;
}

export interface PrometheusMatrixResult {
  metric: PrometheusSeriesMetric;
  values: Array<[number, string]>;
  // Native Prometheus histograms use `histograms` instead of `values`.
  histograms?: PrometheusHistogramPoint[];
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

// ── Proxy Endpoints ──────────────────────────────────────────────────────────
export interface ProxyEndpointSummary {
  total: number;
  healthy: number;
  unhealthy: number;
  byKind?: Record<string, number>;
  byStatus?: Record<string, number>;
  byRegion?: Record<string, number>;
  byAccount?: Record<string, number>;
  byCluster?: Record<string, number>;
  byVersion?: Record<string, number>;
}

export interface ProxyEndpointInfo {
  name: string;
  kind: string;
  signals: string[];
  ownersPath?: string;
  origin?: string;
  public?: boolean;
  healthy?: boolean;
  allowed?: boolean;
  resolver?: string;
  reason?: string;
  provider?: string;
  version?: string;
  cluster?: string;
  account?: string;
  region?: string;
  capabilities?: string[];
  status?: string;
  lastHeartbeat?: string;
  // Optional UI-facing fields retained for forward compatibility with older
  // Doctor responses.
  health?: "healthy" | "degraded" | "offline" | string;
  circuit_open?: boolean;
  latency_ms?: number;
  policy?: string;
}

export interface ProxyEndpointsResponse {
  endpoints: ProxyEndpointInfo[];
  summary?: ProxyEndpointSummary;
  nextCursor?: string;
  totalMatched?: number;
}

// ── Guardian Synthetic Probes ───────────────────────────────────────────────
export interface ProbeResult {
  probeID: string;
  service: string;
  fullName?: string;
  pusher: string;
  cluster?: string;
  partition: string;
  intent: string;
  asset: string;
  name: string;
  type: string;
  mode: string;
  target: string;
  status: string;
  success: boolean;
  durationMs: number;
  statusCode?: number;
  error?: string;
  checkedAt: string;
  lastSuccessAt?: string;
  lastFailureAt?: string;
  consecutiveFailures: number;
  path?: string;
  labels?: Record<string, string>;
}

export interface ProbesResponse {
  probes: ProbeResult[];
}

// ── Tempo search & TraceQL metrics ──────────────────────────────────────────
export interface TempoSearchTrace {
  traceID: string;
  rootServiceName?: string;
  rootTraceName?: string;
  startTimeUnixNano?: string;
  durationMs?: number;
}

export interface TempoSearchResponse {
  traces: TempoSearchTrace[];
}

export interface TempoMetricLabelValue {
  stringValue?: string;
  intValue?: string;
  doubleValue?: number;
  boolValue?: boolean;
}

export interface TempoMetricLabel {
  key: string;
  value: TempoMetricLabelValue;
}

export interface TempoMetricSample {
  timestampMs: number;
  value: number;
}

export interface TempoTimeSeries {
  labels?: TempoMetricLabel[];
  samples: TempoMetricSample[];
}

export interface TempoQueryRangeResponse {
  series: TempoTimeSeries[];
}

// ── Loki range query ────────────────────────────────────────────────────────
export interface LokiStream {
  stream: Record<string, string>;
  values: Array<[string, string]>;
}

export interface LokiMatrixSeries {
  metric: Record<string, string>;
  values: Array<[number, string]>;
}

export interface LokiQueryResponse {
  status: "success" | "error";
  data: {
    resultType: "streams" | "matrix" | string;
    result: LokiStream[] | LokiMatrixSeries[];
  };
}

// ── Service Discovery ─────────────────────────────────────────────────────────
export interface DiscoveredService {
  /** Canonical service name (matches the backend `name` field). */
  name: string;
  /** @deprecated Compatibility alias emitted by older Doctor backends. */
  service?: string;
  full_name?: string;
  truncated?: boolean;
  partition?: string;
  intent?: string;
  asset?: string;
  cluster?: string;
  pusher?: string;
  last_seen?: string;
  has_metrics: boolean;
  has_traces: boolean;
  has_logs: boolean;
  trace_count?: number;
  log_count?: number;
  error_rate?: number;
  metric_names?: string[];
  suggested_dashboards?: string[];
  // Enriched by TraceQL RED metrics (populated client-side or by a separate query)
  rps?: number;
  latency_p95_ms?: number;
  latency_p99_ms?: number;
}

export interface ServiceDiscoveryResponse {
  services: DiscoveredService[];
  total_services?: number;
  window?: string;
  discovered_at?: string;
}

// ── Recording Rules ─────────────────────────────────────────────────────────
export interface RecordingRuleGroupSummary {
  tenant: string;
  group: string;
  interval?: string;
  rules: number;
}

export interface RecordingRulesResponse {
  groups: RecordingRuleGroupSummary[];
}

export interface RecordingRulesStatus {
  enabled: boolean;
  rulerId: string;
  leader: boolean;
  lease?: {
    rulerId: string;
    leader: boolean;
    ttl: string;
    renew: string;
    path: string;
    since?: string;
  };
  scheduler?: {
    groups: number;
    due: number;
    evaluated: number;
    lastLoad: string;
    lastError?: string;
  };
}
