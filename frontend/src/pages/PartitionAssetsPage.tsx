import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { motion } from "framer-motion";
import {
  ArrowLeft,
  Upload,
  Activity,
  Server,
  AlertTriangle,
  CheckCircle2,
  Layers,
  Network,
  ZoomIn,
  ZoomOut,
  RotateCcw,
  Link2,
  BarChart3,
  GanttChartSquare,
  ScrollText,
  Loader2,
  Check,
} from "lucide-react";
import type { AssetGroup, HealthStatus, PartitionModel } from "./FleetView";
import type { AssetObservabilityConfig, HelperFileManifest } from "../types/observability";
import type {
  GuardianTopologyEdge,
  GuardianTopologyNode,
  GuardianOverview,
  LogRecord,
  TraceSummary,
  TraceRecord,
  PrometheusMatrixResult,
  PrometheusSeriesMetric,
} from "../types/api";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import TraceWaterfall from "../components/charts/TraceWaterfall";
import TraceFlow from "../components/charts/TraceFlow";
import GraphQueryBuilder from "../components/charts/GraphQueryBuilder";
import { resolveNodeKind } from "../components/topology/nodeKind";
import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  Tooltip,
  ResponsiveContainer,
} from "recharts";

interface LayoutNode extends GuardianTopologyNode {
  x: number;
  y: number;
}

interface MetricPoint {
  time: string;
  p95: number;
  p99: number;
  rps: number;
}

interface SourceQuery {
  query: string;
  source: string;
}

interface RouteStatePayload {
  partitionModel?: PartitionModel;
  topology?: { nodes: GuardianTopologyNode[]; edges: GuardianTopologyEdge[] };
  helperManifest?: HelperFileManifest;
}

interface CustomSeries {
  key: string;
  label: string;
  color: string;
}

type CustomChartPoint = Record<string, number | string>;

interface AdhocGraphCard {
  id: string;
  title: string;
  query: string;
  rangeMinutes: number;
  refreshIntervalSec: number;
  data: CustomChartPoint[];
  series: CustomSeries[];
  loading: boolean;
  error: string | null;
}

interface SavedGraphDefinition {
  id: string;
  title: string;
  query: string;
  rangeMinutes: number;
  refreshIntervalSec: number;
  createdAt: string;
}

interface SavedDashboard {
  id: string;
  name: string;
  graphs: Array<{ title: string; query: string; rangeMinutes: number; refreshIntervalSec: number }>;
  createdAt: string;
  updatedAt: string;
}

interface SavedBundle {
  version: number;
  exportedAt: string;
  savedGraphs: SavedGraphDefinition[];
  savedDashboards: SavedDashboard[];
}

const SAVED_GRAPHS_KEY = "doctor.savedGraphs.v1";
const SAVED_DASHBOARDS_KEY = "doctor.savedDashboards.v1";

function healthColor(level?: string): string {
  const n = (level || "").toLowerCase();
  if (n === "healthy") return "var(--ui-green)";
  if (n === "degraded" || n === "attention" || n === "warning") return "var(--ui-amber)";
  if (n === "unhealthy" || n === "failing" || n === "failed") return "var(--ui-red)";
  return "var(--ui-faint)";
}

// Match Guardian's topology default: hide low-level CFN plumbing (VPCs,
// subnets, security groups, IAM roles, ...) so the flow reads as services.
function isInfrastructureNode(node: GuardianTopologyNode): boolean {
  const tier = String(node.meta?.tier ?? "").toLowerCase();
  if (tier === "infrastructure") return true;
  const cfnType = String(node.meta?.cfnType ?? "");
  return cfnType.startsWith("AWS::EC2::") || cfnType.startsWith("AWS::IAM::");
}

type HealthMetricMode = "errors" | "latency" | "saturation" | "availability";

interface HealthMetricRule {
  metric: string;
  label: string;
  mode: HealthMetricMode;
  /** Healthy at or below this value (at or above for availability). */
  healthy: number;
  /** Warning at or below this value (at or above for availability). */
  warning: number;
  unit?: string;
  source: string;
}

function autoHealthMetric(
  node: LayoutNode | null,
  config: AssetObservabilityConfig | null,
): HealthMetricRule | null {
  if (!node) return null;
  if (config?.health?.metric) {
    return {
      metric: config.health.metric,
      label: "Configured health metric",
      mode: config.health.mode ?? "errors",
      healthy: config.health.healthy ?? 0,
      warning: config.health.warning ?? 1,
      unit: config.health.unit,
      source: "helper manifest",
    };
  }
  if (node.type === "intent") {
    // An intent has no CFN type of its own. Doctor components expose
    // doctor_component_up; everything else is scraped by the OTel collector
    // and exposes the standard up metric.
    if (node.partition === "doctor") {
      return {
        metric: "doctor_component_up",
        label: "Availability",
        mode: "availability",
        healthy: 1,
        warning: 0.5,
        source: "intent default",
      };
    }
    return {
      metric: "up",
      label: "Availability",
      mode: "availability",
      healthy: 1,
      warning: 0.5,
      source: "intent default",
    };
  }
  if (node.type !== "asset") return null;
  const cfnType = String(node.meta?.cfnType ?? "");
  const assetType = String(node.meta?.type ?? "");
  const configured = config?.metrics?.find((metric) => metric.trim());
  const rule = (
    metric: string,
    label: string,
    mode: HealthMetricMode,
    healthy: number,
    warning: number,
    unit?: string,
    source = "asset type",
  ): HealthMetricRule => ({
    metric,
    label,
    mode,
    healthy,
    warning,
    unit,
    source,
  });

  switch (cfnType) {
    case "AWS::SQS::Queue":
      return rule("ApproximateAgeOfOldestMessage", "Oldest message age", "latency", 60, 300, "s");
    case "AWS::SNS::Topic":
      return rule("NumberOfNotificationsFailed", "Failed notifications", "errors", 0, 1);
    case "AWS::Lambda::Function":
      return rule("Errors", "Invocation errors", "errors", 0, 1);
    case "AWS::DynamoDB::Table":
      return rule("ThrottledRequests", "Throttled requests", "errors", 0, 1);
    case "AWS::ECS::Service":
      return rule("CPUUtilization", "CPU utilization", "saturation", 70, 85, "%");
    case "AWS::ElasticLoadBalancingV2::LoadBalancer":
      return rule("HTTPCode_Target_5XX_Count", "Target 5xx responses", "errors", 0, 1);
    case "AWS::EC2::Instance":
      return rule("StatusCheckFailed", "Status checks failed", "errors", 0, 1);
    case "AWS::Events::Rule":
      return rule("FailedInvocations", "Failed invocations", "errors", 0, 1);
    case "AWS::StepFunctions::StateMachine":
      return rule("ExecutionsFailed", "Failed executions", "errors", 0, 1);
    case "AWS::Kinesis::Stream":
      return rule("WriteProvisionedThroughputExceeded", "Write throttles", "errors", 0, 1);
    case "AWS::RDS::DBInstance":
      return rule("CPUUtilization", "CPU utilization", "saturation", 70, 85, "%");
    case "AWS::Logs::LogGroup":
      return null;
    default:
      break;
  }

  if (configured) {
    return rule(configured, "Configured metric", "errors", 0, 1, undefined, "helper manifest");
  }
  if (assetType === "CDKStack") return null;
  if (node.partition === "doctor") {
    return rule("doctor_component_up", "Availability", "availability", 1, 0.5, undefined, "partition default");
  }
  return rule("up", "Availability", "availability", 1, 0.5, undefined, "default");
}

function healthStatusFromValue(
  rule: HealthMetricRule,
  value: number,
): "healthy" | "warning" | "failing" {
  if (rule.mode === "availability") {
    if (value >= rule.healthy) return "healthy";
    if (value >= rule.warning) return "warning";
    return "failing";
  }
  if (value <= rule.healthy) return "healthy";
  if (value <= rule.warning) return "warning";
  return "failing";
}

function healthRuleText(rule: HealthMetricRule): string {
  if (rule.mode === "availability") {
    return `healthy ≥ ${rule.healthy}, warning ≥ ${rule.warning}`;
  }
  return `healthy ≤ ${rule.healthy}${rule.unit ?? ""}, warning ≤ ${rule.warning}${rule.unit ?? ""}`;
}

function formatHealthValue(value: number, unit?: string): string {
  if (unit === "%") return `${value.toFixed(1)}%`;
  if (unit === "s") return `${value.toFixed(1)}s`;
  return Number.isInteger(value) ? String(value) : value.toFixed(2);
}

// Candidate metrics in preference order. The first one that returns a sample
// becomes the automatically created key-metric chart.
function candidateMetricNames(node: LayoutNode | null): string[] {
  if (!node) return ["up"];
  const partition = (node.partition || "").toLowerCase();
  const intent = (node.intent || "").toLowerCase();
  const names: string[] = [];
  if (partition === "monofs" || intent === "monofs-storage") {
    names.push(
      "kvs_active_keys",
      "monofs_router_ingest_bytes_total",
      "monofs_server_ingest_bytes_total",
      "monofs_router_guardian_upsert_bytes_total",
      "process_cpu_seconds_total",
      "process_resident_memory_bytes",
      "up",
    );
  } else if (partition === "doctor") {
    names.push(
      "doctor_component_up",
      "process_cpu_seconds_total",
      "process_resident_memory_bytes",
      "container_cpu_usage_seconds_total",
      "up",
    );
  } else if (
    partition === "guardian" ||
    partition === "guardian-configs" ||
    partition === "otel" ||
    partition === "opentelemetry" ||
    partition === "agent" ||
    partition === "monitoring"
  ) {
    names.push(
      "up",
      "process_cpu_seconds_total",
      "process_resident_memory_bytes",
      "container_cpu_usage_seconds_total",
    );
  } else {
    names.push(
      "CPUUtilization",
      "Errors",
      "ThrottledRequests",
      "ApproximateAgeOfOldestMessage",
      "HTTPCode_Target_5XX_Count",
      "Invocations",
      "up",
    );
  }
  return [...new Set(names)];
}

function metricQueryWithSelector(metric: string, selector: string): string {
  const isCounter = metric.endsWith("_total") || metric.endsWith("_count");
  const isErrorCount = /error|throttle|failed|5xx|4xx/i.test(metric);
  if (isCounter) return `sum(rate(${metric}{${selector}}[5m]))`;
  if (isErrorCount) return `sum(${metric}{${selector}})`;
  return `max(${metric}{${selector}})`;
}

function metricQuery(metric: string, serviceRegex: string): string {
  return metricQueryWithSelector(metric, `service=~"${serviceRegex}"`);
}

function escapeLabelValue(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

function isAWSNode(node: LayoutNode): boolean {
  return String(node.meta?.cfnType ?? "").startsWith("AWS::");
}

// Local metrics carry the resolved Guardian identity as labels. Prefer those
// over the broad service regex, which for an intent such as "ingest" would
// otherwise also match every service in the "doctor" partition.
function nodeMetricSelectors(node: LayoutNode, serviceRegex: string): string[] {
  if (isAWSNode(node) || !node.partition || !node.intent) {
    return [`service=~"${serviceRegex}"`];
  }
  const parts = [
    `guardian_partition="${escapeLabelValue(node.partition)}"`,
    `guardian_intent="${escapeLabelValue(node.intent)}"`,
  ];
  if (node.type === "asset" && node.asset) {
    parts.push(`guardian_asset="${escapeLabelValue(node.asset)}"`);
  }
  const selectors = [parts.join(", ")];

  // Some local workloads predate the resolved identity labels. Fall back to a
  // service-name regex for intents and compute/load-balancer assets only; for
  // volumes, configs, images, and secrets a partition-wide regex would match
  // unrelated services and make an unmeasured resource look healthy.
  const assetType = String(node.meta?.type ?? "");
  const allowServiceFallback =
    node.type === "intent" ||
    assetType === "Compute" ||
    assetType === "LoadBalancer" ||
    assetType === "";
  if (allowServiceFallback) {
    const tokens =
      node.type === "intent"
        ? [node.name, node.id, node.intent || ""]
        : [node.name, node.id, node.asset || ""];
    const specific = prometheusServiceRegex(tokens);
    selectors.push(`service=~"${specific}"`);
  }
  return selectors;
}

function buildGraph(nodes: GuardianTopologyNode[]): LayoutNode[] {
  const laidOut: LayoutNode[] = [];
  const intents = nodes.filter((n) => resolveNodeKind(n) === "intent");
  const assets = nodes.filter((n) => resolveNodeKind(n) === "asset");

  const columns = Math.min(intents.length, 4) || 2;
  const intentSpacing = 280;
  const assetSpacingX = 200;
  const assetSpacingY = 80;

  intents.forEach((node, i) => {
    const col = i % columns;
    const row = Math.floor(i / columns);
    laidOut.push({
      ...node,
      x: 120 + col * intentSpacing,
      y: 60 + row * 250,
    });
  });

  assets.forEach((node, i) => {
    const intentIdx = intents.findIndex((in2) => in2.name === node.intent);
    if (intentIdx >= 0) {
      const base = laidOut[intentIdx];
      const col = i % 2;
      const row = Math.floor(i / 2);
      laidOut.push({
        ...node,
        x: base.x - 60 + col * assetSpacingX,
        y: base.y + 90 + row * assetSpacingY,
      });
    } else {
      const col = i % 3;
      const row = Math.floor(i / 3);
      laidOut.push({
        ...node,
        x: 120 + col * assetSpacingX,
        y: 400 + row * assetSpacingY,
      });
    }
  });

  return laidOut;
}

function escapeRegex(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// Prometheus label matchers are fully anchored. Identity tokens such as
// "monofs-storage" must therefore be wrapped in .* to match a service like
// "monofs.monofs-storage.node-a".
function prometheusServiceRegex(tokens: string[]): string {
  const escaped = tokens
    .map((token) => token.trim())
    .filter(Boolean)
    .map(escapeRegex);
  if (escaped.length === 0) return ".*";
  return `.*(${escaped.join("|")}).*`;
}

function toIsoRange(minutes: number): { from: string; to: string } {
  const to = new Date();
  const from = new Date(to.getTime() - minutes * 60_000);
  return { from: from.toISOString(), to: to.toISOString() };
}

function stepForMinutes(minutes: number): string {
  if (minutes <= 60) return "30s";
  if (minutes <= 6 * 60) return "60s";
  return "120s";
}

function matrixPoints(result?: PrometheusMatrixResult[]): Array<{ ts: number; value: number }> {
  if (!result || result.length === 0) return [];
  const first = result[0];
  return (first.values || [])
    .map(([ts, value]) => ({ ts, value: Number(value) }))
    .filter((row) => Number.isFinite(row.value));
}

function mergeMetricPoints(
  p95Points: Array<{ ts: number; value: number }>,
  p99Points: Array<{ ts: number; value: number }>,
  rpsPoints: Array<{ ts: number; value: number }>
): MetricPoint[] {
  const byTs = new Map<number, MetricPoint>();

  for (const row of p95Points) {
    byTs.set(row.ts, {
      time: new Date(row.ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }),
      p95: row.value,
      p99: 0,
      rps: 0,
    });
  }

  for (const row of p99Points) {
    const existing = byTs.get(row.ts);
    if (existing) {
      existing.p99 = row.value;
    } else {
      byTs.set(row.ts, {
        time: new Date(row.ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }),
        p95: 0,
        p99: row.value,
        rps: 0,
      });
    }
  }

  for (const row of rpsPoints) {
    const existing = byTs.get(row.ts);
    if (existing) {
      existing.rps = row.value;
    } else {
      byTs.set(row.ts, {
        time: new Date(row.ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }),
        p95: 0,
        p99: 0,
        rps: row.value,
      });
    }
  }

  return [...byTs.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([, value]) => value);
}

async function runRangeQuery(
  query: string,
  tenant: string,
  from: string,
  to: string,
  step: string,
  partition?: string
) {
  const params = new URLSearchParams({ query, start: from, end: to, step, tenant });
  if (partition) params.set("partition", partition);
  const response = await api.queryPrometheusRange(params);
  if (response.status !== "success") return [];
  if (response.data.resultType !== "matrix") return [];
  return matrixPoints(response.data.result);
}

async function firstNonEmptyRangeQuery(
  queries: SourceQuery[],
  tenant: string,
  from: string,
  to: string,
  step: string
): Promise<{ points: Array<{ ts: number; value: number }>; source: string }> {
  for (const q of queries) {
    try {
      const points = await runRangeQuery(q.query, tenant, from, to, step);
      if (points.length > 0) return { points, source: q.source };
    } catch {
    }
  }
  return { points: [], source: "none" };
}

function sourceBadgeClass(source: string): string {
  if (source.startsWith("asset:")) return "bg-emerald-500/10 text-emerald-300 border-emerald-500/30";
  if (source.startsWith("partition:")) return "bg-sky-500/10 text-sky-300 border-sky-500/30";
  if (source.startsWith("fleet:")) return "bg-amber-500/10 text-amber-300 border-amber-500/30";
  return "bg-slate-700/30 text-slate-300 border-slate-600/40";
}

function sourceBadgeLabel(source: string): string {
  if (source === "none") return "no data";
  return source;
}

function mapLevelToHealth(level?: string): HealthStatus {
  const normalized = (level || "").toLowerCase();
  if (normalized === "healthy") return "healthy";
  if (normalized === "degraded" || normalized === "attention") return "degraded";
  return "critical";
}

function partitionSummary(
  partitionName: string,
  assets: AssetGroup[],
  intentCount: number,
  overview: GuardianOverview | null
): PartitionModel["summary"] {
  const summary = overview?.partitions?.find((p) => p.name === partitionName);
  const healthyAssets = assets.filter((asset) => asset.status === "healthy").length;
  const attentionAssets = assets.filter((asset) => asset.status === "degraded").length;
  const failingAssets = assets.filter((asset) => asset.status === "critical").length;
  return {
    health: summary?.health ?? (failingAssets > 0 ? "failing" : attentionAssets > 0 ? "attention" : "healthy"),
    status: summary?.displayStatus || summary?.status,
    assetCount: summary?.assetCount ?? assets.length,
    intentCount: summary?.intentCount ?? intentCount,
    healthyAssets: summary?.healthyAssets ?? healthyAssets,
    attentionAssets: summary?.attentionAssets ?? attentionAssets,
    failingAssets: summary?.failingAssets ?? failingAssets,
  };
}

function buildPartitionModelFromSources(
  partitionName: string,
  topology: GuardianTopologyResponseLike | null,
  overview: GuardianOverview | null
): PartitionModel | null {
  if (!partitionName) return null;
  const nodes = (topology?.nodes || []).filter((n) => n.partition === partitionName);
  const intents = new Map<string, PartitionModel["intents"][number]>();

  for (const node of nodes.filter((n) => resolveNodeKind(n) === "intent")) {
    const intentName = node.name || node.intent || "Intent";
    if (!intents.has(intentName)) intents.set(intentName, { name: intentName, assets: [] });
  }

  for (const node of nodes.filter((n) => resolveNodeKind(n) === "asset")) {
    const intentName = node.intent || "Services";
    if (!intents.has(intentName)) intents.set(intentName, { name: intentName, assets: [] });
    const status = mapLevelToHealth(node.health?.level);
    intents.get(intentName)?.assets.push({
      id: node.id,
      name: node.name,
      status,
      partition: partitionName,
      intent: intentName,
    });
  }

  const sortedIntents = [...intents.values()]
    .map((entry) => ({ ...entry, assets: [...entry.assets].sort((a, b) => a.name.localeCompare(b.name)) }))
    .sort((a, b) => a.name.localeCompare(b.name));

  if (sortedIntents.length > 0) {
    const assets = sortedIntents.flatMap((intent) => intent.assets);
    return {
      id: partitionName,
      region: partitionName,
      intents: sortedIntents,
      summary: partitionSummary(partitionName, assets, sortedIntents.length, overview),
    };
  }

  const summary = overview?.partitions?.find((p) => p.name === partitionName);
  if (!summary) return null;
  const fallbackAsset: AssetGroup = {
    id: `${partitionName}-summary`,
    name: partitionName,
    status: mapLevelToHealth(summary.health),
    partition: partitionName,
    intent: "Services",
  };
  return {
    id: partitionName,
    region: partitionName,
    intents: [{ name: "Services", assets: [fallbackAsset] }],
    summary: partitionSummary(partitionName, [fallbackAsset], 1, overview),
  };
}

function metricSeriesLabel(metric: PrometheusSeriesMetric, index: number): string {
  const preferred = metric.service || metric.asset || metric.intent || metric.partition || metric.instance || metric.job;
  if (preferred) return preferred;
  const compact = Object.entries(metric)
    .filter(([k]) => k !== "__name__")
    .slice(0, 2)
    .map(([k, v]) => `${k}=${v}`)
    .join(" ");
  return compact || `series-${index + 1}`;
}

type GuardianTopologyResponseLike = { nodes: GuardianTopologyNode[]; edges: GuardianTopologyEdge[] };

function minutesLabel(minutes: number): string {
  if (minutes < 60) return `${minutes}m`;
  if (minutes < 60 * 24) return `${minutes / 60}h`;
  return `${minutes / (60 * 24)}d`;
}

function sevKey(text: string): string {
  const s = (text ?? "").toUpperCase();
  if (s.includes("FATAL") || s.includes("ERROR")) return "ERR";
  if (s.includes("WARN")) return "WARN";
  if (s.includes("DEBUG") || s.includes("TRACE")) return "DBG";
  return "INFO";
}

function logSevBadge(k: string): string {
  return {
    ERR: "bg-red-500/10 text-red-400 border-red-500/30",
    WARN: "bg-amber-500/10 text-amber-400 border-amber-500/30",
    INFO: "bg-sky-500/10 text-sky-400 border-sky-500/30",
    DBG: "bg-slate-500/10 text-slate-400 border-slate-500/30",
  }[k] || "bg-slate-500/10 text-slate-400 border-slate-500/30";
}

function hasHelperManifestSections(json: unknown): boolean {
  if (!json || typeof json !== "object") return false;
  const manifest = json as Record<string, unknown>;
  return Boolean(
    (manifest.assets && typeof manifest.assets === "object") ||
    (manifest.intents && typeof manifest.intents === "object") ||
    (manifest.partitions && typeof manifest.partitions === "object")
  );
}

function mergeObservabilityConfigs(
  ...configs: Array<AssetObservabilityConfig | undefined>
): AssetObservabilityConfig | null {
  const metrics = new Set<string>();
  const logs = new Map<string, { query: string; level?: string }>();
  const traces = new Map<string, { service: string; operation?: string; thresholdMs?: number }>();
  let health: AssetObservabilityConfig["health"];

  for (const config of configs) {
    if (!config) continue;
    if (config.health) health = config.health;
    for (const metric of config.metrics ?? []) {
      const normalized = metric.trim();
      if (normalized) metrics.add(normalized);
    }
    for (const log of config.logs ?? []) {
      const normalized = log.query.trim();
      if (!normalized) continue;
      const key = `${log.level ?? ""}|${normalized}`;
      if (!logs.has(key)) {
        logs.set(key, { query: normalized, level: log.level });
      }
    }
    for (const trace of config.traces ?? []) {
      const normalized = trace.service.trim();
      if (!normalized) continue;
      const key = `${normalized}|${trace.operation ?? ""}|${trace.thresholdMs ?? ""}`;
      if (!traces.has(key)) {
        traces.set(key, { service: normalized, operation: trace.operation, thresholdMs: trace.thresholdMs });
      }
    }
  }

  if (metrics.size === 0 && logs.size === 0 && traces.size === 0 && !health) return null;
  return {
    metrics: [...metrics],
    logs: [...logs.values()],
    traces: [...traces.values()],
    ...(health ? { health } : {}),
  };
}

function resolveObservabilityConfig(
  node: LayoutNode | null,
  manifest: HelperFileManifest | null,
): AssetObservabilityConfig | null {
  if (!node || !manifest) return null;
  return mergeObservabilityConfigs(
    manifest.partitions?.[node.partition || ""],
    manifest.intents?.[node.intent || ""],
    manifest.assets[node.name],
  );
}

export default function PartitionAssetsPage() {
  const { name } = useParams<{ name: string }>();
  const location = useLocation();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { config } = useConfig();
  const tenant = config.default_tenant || "default";

  const routeState = (location.state as RouteStatePayload | null) || null;
  const [partitionModel, setPartitionModel] = useState<PartitionModel | null>(routeState?.partitionModel || null);
  const [rawTopology, setRawTopology] = useState<GuardianTopologyResponseLike | null>(routeState?.topology || null);
  const [partitionLoading, setPartitionLoading] = useState<boolean>(!(routeState?.partitionModel && routeState?.topology));
  const [partitionError, setPartitionError] = useState<string | null>(null);
  const [helperManifest, setHelperManifest] = useState<HelperFileManifest | null>(routeState?.helperManifest || null);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [selectedNode, setSelectedNode] = useState<LayoutNode | null>(null);
  const [showInfrastructure, setShowInfrastructure] = useState(false);
  const [zoom, setZoom] = useState(1);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const [dragging, setDragging] = useState(false);
  const dragRef = useRef<{ sx: number; sy: number; px: number; py: number } | null>(null);
  const tabsRef = useRef<HTMLDivElement | null>(null);
  const graphBootstrapRef = useRef("");

  const [activeTab, setActiveTab] = useState<"metrics" | "traces" | "logs" | "graphs">(
    () => (searchParams.get("tab") as "metrics" | "traces" | "logs" | "graphs") || "metrics"
  );
  const [timeRange, setTimeRange] = useState<number>(
    () => Number(searchParams.get("range")) || 60
  );
  const [copied, setCopied] = useState(false);

  const [metricData, setMetricData] = useState<MetricPoint[]>([]);
  const [metricLoading, setMetricLoading] = useState(false);
  const [metricSource, setMetricSource] = useState("none");
  const [metricError, setMetricError] = useState<string | null>(null);
  const [discoveredMetrics, setDiscoveredMetrics] = useState<string[]>([]);
  const [detectedMetric, setDetectedMetric] = useState("");
  const [metricsProbing, setMetricsProbing] = useState(false);
  const [metricsAvailable, setMetricsAvailable] = useState(false);
  const [keyMetricData, setKeyMetricData] = useState<Array<{ time: string; value: number }>>([]);
  const [keyMetricLoading, setKeyMetricLoading] = useState(false);
  const [keyMetricError, setKeyMetricError] = useState<string | null>(null);
  const [healthMetricValue, setHealthMetricValue] = useState<number | null>(null);
  const [healthMetricLoading, setHealthMetricLoading] = useState(false);
  const [healthMetricError, setHealthMetricError] = useState<string | null>(null);
  const [healthMetricStatus, setHealthMetricStatus] = useState<"healthy" | "warning" | "failing" | "unknown">("unknown");

  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [selectedTrace, setSelectedTrace] = useState<TraceSummary | null>(null);
  const [traceSpans, setTraceSpans] = useState<TraceRecord[]>([]);
  const [tracesLoading, setTracesLoading] = useState(false);
  const [tracesFallback, setTracesFallback] = useState(false);
  const [tracesError, setTracesError] = useState<string | null>(null);

  const [logs, setLogs] = useState<LogRecord[]>([]);
  const [logsLoading, setLogsLoading] = useState(false);
  const [logsError, setLogsError] = useState<string | null>(null);

  const [customQuery, setCustomQuery] = useState(
    `histogram_quantile(0.95, sum by (le,service) (rate(http_request_duration_seconds_bucket{service=~"$serviceRegex"}[5m])))`
  );
  const [customData, setCustomData] = useState<CustomChartPoint[]>([]);
  const [customSeries, setCustomSeries] = useState<CustomSeries[]>([]);
  const [customLoading, setCustomLoading] = useState(false);
  const [customError, setCustomError] = useState<string | null>(null);

  const [adhocGraphs, setAdhocGraphs] = useState<AdhocGraphCard[]>([]);
  const [savedGraphs, setSavedGraphs] = useState<SavedGraphDefinition[]>([]);
  const [savedDashboards, setSavedDashboards] = useState<SavedDashboard[]>([]);
  const [dashboardName, setDashboardName] = useState("");
  const [graphTitle, setGraphTitle] = useState("Adhoc Graph");
  const [graphsNotice, setGraphsNotice] = useState<string | null>(null);
  const [draggingGraphId, setDraggingGraphId] = useState<string | null>(null);
  const importInputRef = useRef<HTMLInputElement | null>(null);

  const [stateEditorOpen, setStateEditorOpen] = useState(false);
  const [stateEditorText, setStateEditorText] = useState("");
  const [stateEditorError, setStateEditorError] = useState<string | null>(null);

  useEffect(() => {
    if (routeState?.partitionModel) setPartitionModel(routeState.partitionModel);
    if (routeState?.topology) setRawTopology(routeState.topology);
    if (routeState?.helperManifest) setHelperManifest(routeState.helperManifest);
    if (routeState?.partitionModel && routeState?.topology) {
      setPartitionLoading(false);
      setPartitionError(null);
    }
  }, [routeState?.partitionModel, routeState?.topology, routeState?.helperManifest]);

  useEffect(() => {
    try {
      const rawGraphs = localStorage.getItem(SAVED_GRAPHS_KEY);
      if (rawGraphs) {
        const parsed = JSON.parse(rawGraphs) as SavedGraphDefinition[];
        if (Array.isArray(parsed)) {
          setSavedGraphs(
            parsed
              .filter((g) => typeof g?.query === "string" && typeof g?.title === "string")
              .map((g) => ({
                ...g,
                rangeMinutes: Number(g.rangeMinutes) || 60,
                refreshIntervalSec: Number(g.refreshIntervalSec) || 0,
              }))
          );
        }
      }

      const rawDashboards = localStorage.getItem(SAVED_DASHBOARDS_KEY);
      if (rawDashboards) {
        const parsed = JSON.parse(rawDashboards) as SavedDashboard[];
        if (Array.isArray(parsed)) {
          setSavedDashboards(
            parsed
              .filter((d) => typeof d?.name === "string" && Array.isArray(d?.graphs))
              .map((d) => ({
                ...d,
                graphs: d.graphs
                  .filter((g) => typeof g?.query === "string" && typeof g?.title === "string")
                  .map((g) => ({
                    title: g.title,
                    query: g.query,
                    rangeMinutes: Number(g.rangeMinutes) || 60,
                    refreshIntervalSec: Number(g.refreshIntervalSec) || 0,
                  })),
              }))
          );
        }
      }
    } catch {
    }
  }, []);

  useEffect(() => {
    try {
      localStorage.setItem(SAVED_GRAPHS_KEY, JSON.stringify(savedGraphs));
    } catch {
    }
  }, [savedGraphs]);

  useEffect(() => {
    try {
      localStorage.setItem(SAVED_DASHBOARDS_KEY, JSON.stringify(savedDashboards));
    } catch {
    }
  }, [savedDashboards]);

  useEffect(() => {
    let cancelled = false;

    async function loadPartitionFallback() {
      const requestedPartition = name || config.default_partition || "";
      const hasMatchingModel = partitionModel && partitionModel.id === requestedPartition;
      const hasTopology = rawTopology !== null;
      if (requestedPartition && hasMatchingModel && hasTopology) return;

      setPartitionLoading(true);
      setPartitionError(null);
      try {
        const topologyParams = new URLSearchParams({ tenant });
        if (requestedPartition) topologyParams.set("partition", requestedPartition);

        const [overview, topology] = await Promise.all([
          api.guardianOverview(),
          api.getGuardianTopology(topologyParams),
        ]);

        if (cancelled) return;
        const inferredName =
          requestedPartition ||
          overview.partitions[0]?.name ||
          topology.partition ||
          "";
        const built = buildPartitionModelFromSources(inferredName, topology, overview);
        setRawTopology(topology);
        setPartitionModel(built);
        if (!built) setPartitionError(`No partition data found for "${inferredName}"`);
      } catch (err) {
        if (!cancelled) {
          setPartitionError(err instanceof Error ? err.message : "Failed to load partition data");
        }
      } finally {
        if (!cancelled) setPartitionLoading(false);
      }
    }

    void loadPartitionFallback();
    return () => { cancelled = true; };
  }, [name, tenant, config.default_partition, partitionModel, rawTopology]);

  const handleFileUpload = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setUploadError(null);
    const file = e.target.files?.[0];
    if (!file) return;
    if (!file.name.endsWith(".json")) { setUploadError("Only JSON files are supported"); return; }
    const reader = new FileReader();
    reader.onload = (ev) => {
      try {
        const json = JSON.parse(ev.target?.result as string);
        if (!json.version || !hasHelperManifestSections(json)) throw new Error("Invalid");
        setHelperManifest(json as HelperFileManifest);
      } catch { setUploadError("Failed to parse helper file"); }
    };
    reader.onerror = () => setUploadError("Failed to read file");
    reader.readAsText(file);
    e.target.value = "";
  }, []);

  const partitionTopologyNodes = useMemo(
    () => (rawTopology?.nodes ?? []).filter((node) => node.partition === name),
    [rawTopology, name],
  );

  const visibleTopologyNodes = useMemo(
    () =>
      showInfrastructure
        ? partitionTopologyNodes
        : partitionTopologyNodes.filter((node) => !isInfrastructureNode(node)),
    [partitionTopologyNodes, showInfrastructure],
  );

  const hiddenInfrastructureCount = partitionTopologyNodes.length - visibleTopologyNodes.length;

  const layoutNodes = useMemo(() => buildGraph(visibleTopologyNodes), [visibleTopologyNodes]);

  const nodeById = useMemo(() => new Map(layoutNodes.map((n) => [n.id, n])), [layoutNodes]);

  const filteredEdges = useMemo(() => {
    if (!rawTopology?.edges) return [];
    return rawTopology.edges.filter((e) => nodeById.has(e.from) && nodeById.has(e.to));
  }, [rawTopology, nodeById]);

  // Restore state from URL params
  useEffect(() => {
    const nodeId = searchParams.get("node");
    const tab = searchParams.get("tab");
    const range = searchParams.get("range");

    if (tab === "metrics" || tab === "traces" || tab === "logs" || tab === "graphs") {
      setActiveTab(tab);
    }
    if (range) {
      const r = Number(range);
      if ([15, 60, 360, 1440].includes(r)) setTimeRange(r);
    }

    if (nodeId && layoutNodes.length > 0) {
      const found = layoutNodes.find((n) => n.id === nodeId);
      if (found) {
        setSelectedNode(found);
        setMetricsProbing(true);
        setLogsLoading(true);
      }
    }
  }, [searchParams, layoutNodes]);

  const svgW = Math.max(1200, layoutNodes.length > 0 ? Math.max(...layoutNodes.map((n) => n.x)) + 220 : 1200);
  const svgH = Math.max(600, layoutNodes.length > 0 ? Math.max(...layoutNodes.map((n) => n.y)) + 140 : 600);

  // Update URL when selection changes
  const updateSearchParams = useCallback(
    (updates: Record<string, string>) => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev);
        Object.entries(updates).forEach(([k, v]) => {
          if (v) next.set(k, v);
          else next.delete(k);
        });
        return next;
      });
    },
    [setSearchParams]
  );

  const handleNodeSelect = useCallback(
    (node: LayoutNode) => {
      setSelectedNode(node);
      setMetricsProbing(true);
      setLogsLoading(true);
      updateSearchParams({ node: node.id, tab: activeTab, range: String(timeRange) });
      // The inspector lives below the topology; make the click visibly do
      // something even on tall pages.
      window.requestAnimationFrame(() => {
        tabsRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
      });
    },
    [activeTab, timeRange, updateSearchParams]
  );

  const handleTabChange = useCallback(
    (tab: "metrics" | "traces" | "logs" | "graphs") => {
      setActiveTab(tab);
      if (selectedNode) {
        updateSearchParams({ node: selectedNode.id, tab, range: String(timeRange) });
      }
    },
    [selectedNode, timeRange, updateSearchParams]
  );

  const handleRangeChange = useCallback(
    (minutes: number) => {
      setTimeRange(minutes);
      if (selectedNode) {
        updateSearchParams({ node: selectedNode.id, tab: activeTab, range: String(minutes) });
      }
    },
    [selectedNode, activeTab, updateSearchParams]
  );

  // Copy dashboard link
  const handleCopyLink = useCallback(() => {
    const url = window.location.href;
    navigator.clipboard.writeText(url).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }).catch(() => {});
  }, []);

  const hydrateStateEditor = useCallback(() => {
    const payload: RouteStatePayload = {
      partitionModel: partitionModel || undefined,
      topology: rawTopology || undefined,
      helperManifest: helperManifest || undefined,
    };
    setStateEditorText(JSON.stringify(payload, null, 2));
    setStateEditorError(null);
  }, [partitionModel, rawTopology, helperManifest]);

  const applyStateEditor = useCallback(() => {
    try {
      const parsed = JSON.parse(stateEditorText) as RouteStatePayload;
      setPartitionModel(parsed.partitionModel || null);
      setRawTopology(parsed.topology || null);
      setHelperManifest(parsed.helperManifest || null);
      setPartitionError(null);
      setPartitionLoading(false);
      setStateEditorError(null);
      navigate(`${location.pathname}${location.search}`, { replace: true, state: parsed });
    } catch (err) {
      setStateEditorError(err instanceof Error ? err.message : "Invalid JSON payload");
    }
  }, [stateEditorText, navigate, location.pathname, location.search]);

  const executeGraphQuery = useCallback(async (query: string, rangeOverrideMinutes?: number) => {
    if (!selectedNode) {
      return {
        data: [] as CustomChartPoint[],
        series: [] as CustomSeries[],
        error: "Select an asset node first",
      };
    }

    const macros: Record<string, string> = {
      "$asset": selectedNode.name,
      "$node": selectedNode.id,
      "$intent": selectedNode.intent || "",
      "$partition": selectedNode.partition || "",
      "$serviceRegex": prometheusServiceRegex([
        selectedNode.name,
        selectedNode.id,
        selectedNode.intent || "",
        selectedNode.partition || "",
      ]),
    };

    let resolved = query;
    for (const [k, v] of Object.entries(macros)) {
      resolved = resolved.split(k).join(v);
    }

    const rangeMinutes = rangeOverrideMinutes && rangeOverrideMinutes > 0 ? rangeOverrideMinutes : timeRange;
    const { from, to } = toIsoRange(rangeMinutes);
    const step = stepForMinutes(rangeMinutes);
    const colors = ["var(--chart-1)", "var(--chart-5)", "var(--chart-2)", "var(--chart-8)", "var(--chart-3)", "var(--chart-4)"];

    try {
      const response = await api.queryPrometheusRange(
        new URLSearchParams({
          query: resolved,
          start: from,
          end: to,
          step,
          tenant,
        })
      );

      if (response.status !== "success" || response.data.resultType !== "matrix") {
        return {
          data: [] as CustomChartPoint[],
          series: [] as CustomSeries[],
          error: "Query did not return a range matrix",
        };
      }

      const matrix = response.data.result.slice(0, 6);
      const tsMap = new Map<number, CustomChartPoint>();
      const seriesMeta: CustomSeries[] = [];

      matrix.forEach((series, idx) => {
        const key = `s${idx}`;
        seriesMeta.push({
          key,
          label: metricSeriesLabel(series.metric || {}, idx),
          color: colors[idx % colors.length],
        });
        (series.values || []).forEach(([ts, value]) => {
          const num = Number(value);
          if (!Number.isFinite(num)) return;
          const existing = tsMap.get(ts) || {
            time: new Date(ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }),
          };
          existing[key] = num;
          tsMap.set(ts, existing);
        });
      });

      const points = [...tsMap.entries()]
        .sort((a, b) => a[0] - b[0])
        .map(([, point]) => point);

      return {
        data: points,
        series: seriesMeta,
        error: points.length === 0 ? "No samples returned for current time range" : null,
      };
    } catch (err) {
      return {
        data: [] as CustomChartPoint[],
        series: [] as CustomSeries[],
        error: err instanceof Error ? err.message : "Failed to run query",
      };
    }
  }, [selectedNode, tenant, timeRange]);

  const runCustomQuery = useCallback(async () => {
    setCustomLoading(true);
    setCustomError(null);
    const result = await executeGraphQuery(customQuery);
    setCustomData(result.data);
    setCustomSeries(result.series);
    setCustomError(result.error);
    setCustomLoading(false);
  }, [customQuery, executeGraphQuery]);

  const observabilityConfig = useMemo(
    () => resolveObservabilityConfig(selectedNode, helperManifest),
    [selectedNode, helperManifest]
  );

  const healthRule = useMemo(
    () => autoHealthMetric(selectedNode, observabilityConfig),
    [selectedNode, observabilityConfig]
  );

  // Probe the node's likely metrics. The first metric with a sample becomes the
  // automatically created Key Metric chart. If none exist, the Metrics/Graphs
  // tabs are hidden instead of showing permanently empty charts.
  useEffect(() => {
    const node = selectedNode;
    if (!node) {
      setMetricsProbing(false);
      setMetricsAvailable(false);
      setDetectedMetric("");
      return;
    }
    let cancelled = false;
    setMetricsProbing(true);
    setMetricsAvailable(false);
    setDetectedMetric("");

    async function detectMetrics(target: LayoutNode) {
      const serviceRegex = prometheusServiceRegex([
        target.name,
        target.id,
        target.intent || "",
        target.partition || "",
      ]);
      const selectors = nodeMetricSelectors(target, serviceRegex);
      for (const metric of candidateMetricNames(target)) {
        for (const selector of selectors) {
          try {
            const params = new URLSearchParams({
              tenant,
              query: metricQueryWithSelector(metric, selector),
            });
            if (!isAWSNode(target) && target.partition) params.set("partition", target.partition);
            const response = await api.queryPrometheus(params);
            if (cancelled) return;
            if (
              response.status === "success" &&
              response.data.resultType === "vector" &&
              response.data.result.length > 0
            ) {
              setDetectedMetric(metric);
              setMetricsAvailable(true);
              return;
            }
          } catch {
            // Try the next selector or candidate.
          }
        }
      }
    }

    void detectMetrics(node).finally(() => {
      if (!cancelled) setMetricsProbing(false);
    });
    return () => { cancelled = true; };
  }, [selectedNode?.id, tenant]);

  useEffect(() => {
    const node = selectedNode;
    const metric = detectedMetric;
    if (!node || !metric) {
      setKeyMetricData([]);
      setKeyMetricError(null);
      return;
    }
    let cancelled = false;
    async function loadKeyMetric(target: LayoutNode, metricName: string) {
      setKeyMetricLoading(true);
      setKeyMetricError(null);
      try {
        const serviceRegex = prometheusServiceRegex([
          target.name,
          target.id,
          target.intent || "",
          target.partition || "",
        ]);
        const selectors = nodeMetricSelectors(target, serviceRegex);
        const { from, to } = toIsoRange(timeRange);
        const step = stepForMinutes(timeRange);
        const partition = isAWSNode(target) ? "" : target.partition || "";
        let points: Array<{ ts: number; value: number }> = [];
        for (const selector of selectors) {
          points = await runRangeQuery(
            metricQueryWithSelector(metricName, selector),
            tenant,
            from,
            to,
            step,
            partition
          );
          if (points.length > 0) break;
        }
        if (cancelled) return;
        setKeyMetricData(points.map((point) => ({
          time: new Date(point.ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }),
          value: point.value,
        })));
        if (points.length === 0) setKeyMetricError("No samples for this metric in the selected window");
      } catch (err) {
        if (!cancelled) setKeyMetricError(err instanceof Error ? err.message : "Failed to query key metric");
      } finally {
        if (!cancelled) setKeyMetricLoading(false);
      }
    }
    void loadKeyMetric(node, metric);
    return () => { cancelled = true; };
  }, [selectedNode?.id, detectedMetric, timeRange, tenant]);

  useEffect(() => {
    if (!detectedMetric) return;
    setCustomQuery(metricQuery(detectedMetric, "$serviceRegex"));
  }, [detectedMetric, selectedNode?.id]);

  useEffect(() => {
    const node = selectedNode;
    const rule = healthRule;
    if (!node || !rule) {
      setHealthMetricValue(null);
      setHealthMetricStatus("unknown");
      setHealthMetricError(null);
      return;
    }
    setHealthMetricValue(null);
    setHealthMetricStatus("unknown");
    setHealthMetricError(null);
    let cancelled = false;
    async function loadHealthMetric(target: LayoutNode, metricRule: HealthMetricRule) {
      setHealthMetricLoading(true);
      setHealthMetricError(null);
      try {
        const serviceRegex = prometheusServiceRegex([
          target.name,
          target.id,
          target.intent || "",
          target.partition || "",
        ]);
        const aggregation =
          metricRule.mode === "errors"
            ? "sum"
            : metricRule.mode === "availability"
              ? "min"
              : "max";
        const selectors = nodeMetricSelectors(target, serviceRegex);
        const partition = isAWSNode(target) ? "" : target.partition || "";
        let value: number | null = null;
        for (const selector of selectors) {
          try {
            const params = new URLSearchParams({
              tenant,
              query: `${aggregation}(${metricRule.metric}{${selector}})`,
            });
            if (partition) params.set("partition", partition);
            const response = await api.queryPrometheus(params);
            if (
              response.status === "success" &&
              response.data.resultType === "vector" &&
              response.data.result.length > 0
            ) {
              const parsed = Number(response.data.result[0].value[1]);
              if (Number.isFinite(parsed)) {
                value = parsed;
                break;
              }
            }
          } catch {
            // Try the next selector.
          }
        }
        if (cancelled) return;
        if (value === null) {
          setHealthMetricValue(null);
          setHealthMetricStatus("unknown");
          setHealthMetricError("No samples in the current window");
          return;
        }
        setHealthMetricValue(value);
        setHealthMetricStatus(healthStatusFromValue(metricRule, value));
      } catch (err) {
        if (!cancelled) {
          setHealthMetricValue(null);
          setHealthMetricStatus("unknown");
          setHealthMetricError(err instanceof Error ? err.message : "Failed to query health metric");
        }
      } finally {
        if (!cancelled) setHealthMetricLoading(false);
      }
    }
    void loadHealthMetric(node, rule);
    return () => { cancelled = true; };
  }, [selectedNode?.id, selectedNode?.name, selectedNode?.intent, selectedNode?.partition, healthRule, tenant]);

  useEffect(() => {
    if (!selectedNode) {
      setDiscoveredMetrics([]);
      return;
    }

    let cancelled = false;
    const serviceRegex = prometheusServiceRegex([
      selectedNode.name,
      selectedNode.id,
      selectedNode.intent || "",
      selectedNode.partition || "",
    ]);

    async function loadMetricDiscovery() {
      try {
        const params = new URLSearchParams({ tenant });
        params.append("match[]", `service=~"${serviceRegex}"`);
        const response = await api.getMetricNames(params);
        if (!cancelled) {
          setDiscoveredMetrics((response.data ?? []).slice(0, 24));
        }
      } catch {
        if (!cancelled) {
          setDiscoveredMetrics([]);
        }
      }
    }

    void loadMetricDiscovery();
    return () => { cancelled = true; };
  }, [selectedNode?.id, selectedNode?.name, selectedNode?.intent, selectedNode?.partition, tenant]);

  // Fetch metrics for selected node
  useEffect(() => {
    if (!selectedNode) return;
    let cancelled = false;

    const node = selectedNode;

    async function loadMetrics() {
      setMetricLoading(true);
      setMetricError(null);
      setMetricSource("none");

      const { from, to } = toIsoRange(timeRange);
      const step = stepForMinutes(timeRange);

      const serviceRegex = prometheusServiceRegex([
        node.name,
        node.id,
        node.intent || "",
        node.partition || "",
      ]);

      const p95Queries: SourceQuery[] = [
        { query: `histogram_quantile(0.95, sum by (le) (rate(request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`, source: `asset:${node.name}` },
        { query: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`, source: `asset:${node.name}` },
        { query: `histogram_quantile(0.95, sum by (le) (rate(request_duration_seconds_bucket[5m])))`, source: "fleet:latency" },
        { query: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`, source: "fleet:latency" },
      ];

      const p99Queries: SourceQuery[] = [
        { query: `histogram_quantile(0.99, sum by (le) (rate(request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`, source: `asset:${node.name}` },
        { query: `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`, source: `asset:${node.name}` },
        { query: `histogram_quantile(0.99, sum by (le) (rate(request_duration_seconds_bucket[5m])))`, source: "fleet:latency" },
        { query: `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`, source: "fleet:latency" },
      ];

      const rpsQueries: SourceQuery[] = [
        { query: `sum(rate(http_requests_total{service=~"${serviceRegex}"}[5m]))`, source: `asset:${node.name}` },
        { query: `sum(rate(http_server_requests_seconds_count{service=~"${serviceRegex}"}[5m]))`, source: `asset:${node.name}` },
        { query: `sum(rate(http_requests_total[5m]))`, source: "fleet:rps" },
        { query: `sum(rate(http_server_requests_seconds_count[5m]))`, source: "fleet:rps" },
      ];

      try {
        const [p95, p99, rps] = await Promise.all([
          firstNonEmptyRangeQuery(p95Queries, tenant, from, to, step),
          firstNonEmptyRangeQuery(p99Queries, tenant, from, to, step),
          firstNonEmptyRangeQuery(rpsQueries, tenant, from, to, step),
        ]);

        if (cancelled) return;

        const merged = mergeMetricPoints(p95.points, p99.points, rps.points);
        setMetricData(merged);
        const src = p95.source !== "none" ? p95.source : p99.source !== "none" ? p99.source : rps.source;
        setMetricSource(src);
        if (merged.length === 0) setMetricError("No matching time-series for this window");
      } catch (err) {
        if (!cancelled) setMetricError(err instanceof Error ? err.message : "Failed to load metrics");
      } finally {
        if (!cancelled) setMetricLoading(false);
      }
    }

    void loadMetrics();
    return () => { cancelled = true; };
  }, [selectedNode?.id, timeRange, tenant]);

  // Fetch traces for selected node
  useEffect(() => {
    if (!selectedNode) return;
    let cancelled = false;

    const node = selectedNode;

    async function loadTraces() {
      setTracesLoading(true);
      setTracesError(null);
      setTracesFallback(false);
      setSelectedTrace(null);
      setTraceSpans([]);

      const { from, to } = toIsoRange(timeRange);
      const tokenSet = [node.name, node.id, node.intent || "", node.partition || ""]
        .map((v) => v.trim())
        .filter(Boolean);
      const partition = (node.partition || name || "").trim();

      try {
        const traceParams = new URLSearchParams({ tenant, from, to, limit: "60" });
        if (partition) traceParams.set("partition", partition);

        const serviceList = await api.getLogServices(tenant);
        const matchingServices = (serviceList.services || [])
          .filter((service) => {
            const value = service.toLowerCase();
            return tokenSet.some((token) => value.includes(token.toLowerCase()));
          })
          .slice(0, 3);

        let traceResults: TraceSummary[] = [];
        for (const serviceName of matchingServices) {
          const scoped = new URLSearchParams(traceParams);
          scoped.set("service", serviceName);
          const result = await api.getRecentTraces(scoped);
          if ((result.traces || []).length > 0) {
            traceResults = result.traces;
            break;
          }
        }

        if (traceResults.length === 0 && partition) {
          // Do not fall back to every tenant trace. If the asset has no
          // telemetry, show only traces from its own partition.
          const fallback = await api.getRecentTraces(traceParams);
          traceResults = fallback.traces || [];
          if (!cancelled && traceResults.length > 0) setTracesFallback(true);
        }

        if (cancelled) return;

        const top = traceResults.slice(0, 10);
        setTraces(top);
        if (top.length === 0) {
          setTracesError(
            partition
              ? `No traces for this asset or partition "${partition}" in the selected window`
              : "No traces in selected time window",
          );
          return;
        }

        if (top[0]) {
          setSelectedTrace(top[0]);
          const traceData = await api.getTrace(top[0].trace_id, tenant);
          if (!cancelled) setTraceSpans(traceData.spans || []);
        }
      } catch (err) {
        if (!cancelled) setTracesError(err instanceof Error ? err.message : "Failed to load traces");
      } finally {
        if (!cancelled) setTracesLoading(false);
      }
    }

    void loadTraces();
    return () => { cancelled = true; };
  }, [selectedNode?.id, timeRange, tenant]);

  // Fetch logs for selected node
  useEffect(() => {
    if (!selectedNode) return;
    let cancelled = false;

    const node = selectedNode;

    async function loadLogs() {
      setLogsLoading(true);
      setLogs([]);
      setLogsError(null);

      const { from, to } = toIsoRange(timeRange);
      // Log search is a substring match, not a structured matcher. The old
      // code joined every identity token (including "intent:..."), which made
      // intent nodes such as monofs-storage return zero results. Try the most
      // specific human-readable token first and fall back progressively.
      const candidates = [...new Set(
        [node.name, node.asset, node.intent, node.partition]
          .map((value) => (value || "").trim())
          .filter(Boolean),
      )];

      try {
        let results: LogRecord[] = [];

        // First try exact service names. The text search ("q") only matches
        // message bodies; service names such as monofs.monofs-storage.router-a
        // are not necessarily present in the body.
        let matchingServices: string[] = [];
        try {
          const serviceList = await api.getLogServices(tenant);
          matchingServices = (serviceList.services || [])
            .filter((service) => {
              const value = service.toLowerCase();
              return candidates.some((candidate) => value.includes(candidate.toLowerCase()));
            })
            .slice(0, 6);
        } catch {
          matchingServices = [];
        }

        // The canonical identity service name is not always present in the
        // recent log-service catalog (for example a router with sparse logs),
        // so query it explicitly as well.
        const canonicalService = [node.partition, node.intent, node.asset || node.name]
          .filter(Boolean)
          .join(".");
        const servicesToQuery = [...new Set([...matchingServices, canonicalService].filter(Boolean))]
          .slice(0, 8);

        for (const serviceName of servicesToQuery) {
          if (cancelled) return;
          const params = new URLSearchParams({
            tenant,
            from,
            to,
            limit: "100",
            service: serviceName,
          });
          const data = await api.searchLogs(params);
          results.push(...(data.results || []));
        }

        if (results.length === 0) {
          // Fall back to the body text search and keep only records whose
          // identity matches the node. Unrelated matches are dropped.
          for (const candidate of candidates) {
            if (cancelled) return;
            const params = new URLSearchParams({
              tenant,
              from,
              to,
              limit: "200",
              q: candidate,
            });
            const data = await api.searchLogs(params);
            if ((data.results || []).length > 0) {
              results = data.results || [];
              break;
            }
          }
          results = results.filter((log) => {
            const haystack = [log.service, log.partition, log.intent, log.asset]
              .filter(Boolean)
              .join(" ")
              .toLowerCase();
            return candidates.some((candidate) => haystack.includes(candidate.toLowerCase()));
          });
        }

        if (cancelled) return;
        if (results.length > 0) {
          const seen = new Set<string>();
          results = results
            .filter((log) => {
              const key = `${log.timestamp}|${log.service}|${log.body}`;
              if (seen.has(key)) return false;
              seen.add(key);
              return true;
            })
            .sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime())
            .slice(0, 200);
        }
        setLogs(results);
        setLogsError(results.length === 0 ? "No logs found for the selected node" : null);
      } catch (err) {
        if (!cancelled) setLogsError(err instanceof Error ? err.message : "Failed to load logs");
      } finally {
        if (!cancelled) setLogsLoading(false);
      }
    }

    void loadLogs();
    return () => { cancelled = true; };
  }, [selectedNode?.id, timeRange, tenant]);

  const openTrace = useCallback(async (trace: TraceSummary) => {
    setSelectedTrace(trace);
    try {
      const traceData = await api.getTrace(trace.trace_id, tenant);
      setTraceSpans(traceData.spans || []);
    } catch {
      setTraceSpans([]);
    }
  }, [tenant]);

  useEffect(() => {
    if (!stateEditorOpen) return;
    hydrateStateEditor();
  }, [stateEditorOpen, hydrateStateEditor]);

  useEffect(() => {
    setCustomData([]);
    setCustomSeries([]);
    setCustomError(null);
  }, [selectedNode?.id, timeRange]);

  useEffect(() => {
    if (!graphsNotice) return;
    const timer = setTimeout(() => setGraphsNotice(null), 2400);
    return () => clearTimeout(timer);
  }, [graphsNotice]);

  const handleApplyGraphQuery = useCallback((query: string) => {
    setCustomQuery(query);
    setCustomError(null);
    setActiveTab("metrics");
    if (selectedNode) {
      updateSearchParams({ node: selectedNode.id, tab: "metrics", range: String(timeRange) });
    }
  }, [selectedNode, timeRange, updateSearchParams]);

  const refreshAdhocGraph = useCallback(async (graphId: string, query: string, rangeMinutes?: number) => {
    setAdhocGraphs((prev) => prev.map((g) => (g.id === graphId ? { ...g, loading: true, error: null } : g)));
    const result = await executeGraphQuery(query, rangeMinutes);
    setAdhocGraphs((prev) => prev.map((g) => (
      g.id === graphId
        ? { ...g, data: result.data, series: result.series, error: result.error, loading: false }
        : g
    )));
  }, [executeGraphQuery]);

  // When the Graphs tab is opened, bootstrap a working default graph from the
  // automatically detected metric instead of presenting an empty board.
  useEffect(() => {
    if (activeTab !== "graphs" || !selectedNode || !detectedMetric) return;
    const bootKey = `${selectedNode.id}:${detectedMetric}`;
    if (graphBootstrapRef.current === bootKey) return;
    graphBootstrapRef.current = bootKey;
    const query = metricQuery(detectedMetric, "$serviceRegex");
    setCustomQuery(query);
    setCustomLoading(true);
    setCustomError(null);
    void executeGraphQuery(query).then((result) => {
      setCustomData(result.data);
      setCustomSeries(result.series);
      setCustomError(result.error);
      setCustomLoading(false);
    });
    if (adhocGraphs.length === 0) {
      const id = `adhoc-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
      setAdhocGraphs([{
        id,
        title: `${detectedMetric} · ${selectedNode.name}`,
        query,
        rangeMinutes: timeRange,
        refreshIntervalSec: 0,
        data: [],
        series: [],
        loading: true,
        error: null,
      }]);
      void refreshAdhocGraph(id, query, timeRange);
    }
  }, [activeTab, selectedNode?.id, detectedMetric, timeRange, executeGraphQuery, refreshAdhocGraph, adhocGraphs.length]);

  const reorderAdhocGraph = useCallback((sourceId: string, targetId: string) => {
    if (!sourceId || !targetId || sourceId === targetId) return;
    setAdhocGraphs((prev) => {
      const from = prev.findIndex((g) => g.id === sourceId);
      const to = prev.findIndex((g) => g.id === targetId);
      if (from < 0 || to < 0) return prev;
      const next = [...prev];
      const [moved] = next.splice(from, 1);
      next.splice(to, 0, moved);
      return next;
    });
  }, []);

  const moveAdhocGraphBy = useCallback((graphId: string, delta: number) => {
    setAdhocGraphs((prev) => {
      const index = prev.findIndex((g) => g.id === graphId);
      if (index < 0) return prev;
      const nextIndex = index + delta;
      if (nextIndex < 0 || nextIndex >= prev.length) return prev;
      const next = [...prev];
      const [moved] = next.splice(index, 1);
      next.splice(nextIndex, 0, moved);
      return next;
    });
  }, []);

  const exportSavedBoards = useCallback(() => {
    const payload: SavedBundle = {
      version: 1,
      exportedAt: new Date().toISOString(),
      savedGraphs,
      savedDashboards,
    };

    try {
      const blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `doctor-graphs-${new Date().toISOString().slice(0, 10)}.json`;
      a.click();
      URL.revokeObjectURL(url);
      setGraphsNotice("Exported saved graphs and dashboards");
    } catch {
      setGraphsNotice("Failed to export dashboards");
    }
  }, [savedGraphs, savedDashboards]);

  const importSavedBoards = useCallback((event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) return;

    const reader = new FileReader();
    reader.onload = (ev) => {
      try {
        const parsed = JSON.parse(String(ev.target?.result || "{}")) as Partial<SavedBundle>;
        if (!Array.isArray(parsed.savedGraphs) || !Array.isArray(parsed.savedDashboards)) {
          throw new Error("Invalid saved dashboard bundle");
        }

        const importedGraphs = parsed.savedGraphs
          .filter((g) => typeof g?.query === "string" && typeof g?.title === "string")
          .map((g) => ({
            id: g.id || `graph-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`,
            title: g.title,
            query: g.query,
            rangeMinutes: Number(g.rangeMinutes) || 60,
            refreshIntervalSec: Number(g.refreshIntervalSec) || 0,
            createdAt: g.createdAt || new Date().toISOString(),
          }));

        const importedDashboards = parsed.savedDashboards
          .filter((d) => typeof d?.name === "string" && Array.isArray(d?.graphs))
          .map((d) => ({
            id: d.id || `dash-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`,
            name: d.name,
            graphs: d.graphs
              .filter((g) => typeof g?.query === "string" && typeof g?.title === "string")
              .map((g) => ({
                title: g.title,
                query: g.query,
                rangeMinutes: Number(g.rangeMinutes) || 60,
                refreshIntervalSec: Number(g.refreshIntervalSec) || 0,
              })),
            createdAt: d.createdAt || new Date().toISOString(),
            updatedAt: d.updatedAt || new Date().toISOString(),
          }));

        setSavedGraphs(importedGraphs);
        setSavedDashboards(importedDashboards);
        setGraphsNotice(`Imported ${importedGraphs.length} graph(s) and ${importedDashboards.length} dashboard(s)`);
      } catch {
        setGraphsNotice("Import failed: invalid JSON bundle");
      }
    };

    reader.onerror = () => setGraphsNotice("Import failed: could not read file");
    reader.readAsText(file);
    event.target.value = "";
  }, []);

  useEffect(() => {
    const timers: number[] = [];
    for (const graph of adhocGraphs) {
      if (!graph.refreshIntervalSec || graph.refreshIntervalSec <= 0) continue;
      const intervalMs = Math.max(5, graph.refreshIntervalSec) * 1000;
      const timerId = window.setInterval(() => {
        void refreshAdhocGraph(graph.id, graph.query, graph.rangeMinutes);
      }, intervalMs);
      timers.push(timerId);
    }

    return () => {
      timers.forEach((id) => window.clearInterval(id));
    };
  }, [adhocGraphs, refreshAdhocGraph]);

  const addAdhocGraphFromCurrent = useCallback(async () => {
    const query = customQuery.trim();
    if (!query) {
      setGraphsNotice("Query is empty");
      return;
    }
    const title = graphTitle.trim() || `Adhoc Graph ${adhocGraphs.length + 1}`;
    const graphId = `adhoc-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
    setAdhocGraphs((prev) => [
      {
        id: graphId,
        title,
        query,
        rangeMinutes: timeRange,
        refreshIntervalSec: 0,
        data: [],
        series: [],
        loading: false,
        error: null,
      },
      ...prev,
    ]);
    setGraphsNotice(`Added ${title}`);
    await refreshAdhocGraph(graphId, query, timeRange);
  }, [customQuery, graphTitle, adhocGraphs.length, refreshAdhocGraph, timeRange]);

  const saveCurrentQueryAsGraph = useCallback(() => {
    const query = customQuery.trim();
    if (!query) {
      setGraphsNotice("Query is empty");
      return;
    }
    const title = graphTitle.trim() || `Saved Graph ${savedGraphs.length + 1}`;
    const entry: SavedGraphDefinition = {
      id: `graph-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`,
      title,
      query,
      rangeMinutes: timeRange,
      refreshIntervalSec: 0,
      createdAt: new Date().toISOString(),
    };
    setSavedGraphs((prev) => [entry, ...prev]);
    setGraphsNotice(`Saved graph ${title}`);
  }, [customQuery, graphTitle, savedGraphs.length, timeRange]);

  const addSavedGraphToBoard = useCallback(async (graph: SavedGraphDefinition) => {
    const graphId = `adhoc-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
    setAdhocGraphs((prev) => [
      {
        id: graphId,
        title: graph.title,
        query: graph.query,
        rangeMinutes: graph.rangeMinutes || timeRange,
        refreshIntervalSec: graph.refreshIntervalSec || 0,
        data: [],
        series: [],
        loading: false,
        error: null,
      },
      ...prev,
    ]);
    await refreshAdhocGraph(graphId, graph.query, graph.rangeMinutes || timeRange);
  }, [refreshAdhocGraph, timeRange]);

  const saveDashboard = useCallback(() => {
    const graphs = adhocGraphs
      .map((g) => ({
        title: g.title.trim(),
        query: g.query.trim(),
        rangeMinutes: g.rangeMinutes,
        refreshIntervalSec: g.refreshIntervalSec,
      }))
      .filter((g) => g.title && g.query);

    if (graphs.length === 0) {
      setGraphsNotice("Add at least one ad-hoc graph before saving dashboard");
      return;
    }

    const name = dashboardName.trim() || `Dashboard ${savedDashboards.length + 1}`;
    const now = new Date().toISOString();
    const dashboard: SavedDashboard = {
      id: `dash-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`,
      name,
      graphs,
      createdAt: now,
      updatedAt: now,
    };
    setSavedDashboards((prev) => [dashboard, ...prev]);
    setGraphsNotice(`Saved dashboard ${name}`);
  }, [adhocGraphs, dashboardName, savedDashboards.length]);

  const loadDashboard = useCallback(async (dashboard: SavedDashboard) => {
    const cards: AdhocGraphCard[] = dashboard.graphs.map((g, idx) => ({
      id: `adhoc-${Date.now()}-${idx}-${Math.random().toString(36).slice(2, 7)}`,
      title: g.title,
      query: g.query,
      rangeMinutes: g.rangeMinutes || timeRange,
      refreshIntervalSec: g.refreshIntervalSec || 0,
      data: [],
      series: [],
      loading: false,
      error: null,
    }));

    setAdhocGraphs(cards);
    setGraphsNotice(`Loaded dashboard ${dashboard.name}`);
    for (const card of cards) {
      await refreshAdhocGraph(card.id, card.query, card.rangeMinutes);
    }
  }, [refreshAdhocGraph, timeRange]);

  const deleteSavedGraph = useCallback((graphId: string) => {
    setSavedGraphs((prev) => prev.filter((g) => g.id !== graphId));
  }, []);

  const deleteDashboard = useCallback((dashboardId: string) => {
    setSavedDashboards((prev) => prev.filter((d) => d.id !== dashboardId));
  }, []);

  const showMetrics = metricsProbing || metricsAvailable;
  const showLogs = logsLoading || logs.length > 0;
  const showGraphs = metricsProbing || metricsAvailable;

  useEffect(() => {
    if (!selectedNode) return;
    if (activeTab === "metrics" && !showMetrics) {
      setActiveTab(showLogs ? "logs" : "traces");
    } else if (activeTab === "logs" && !showLogs) {
      setActiveTab("traces");
    } else if (activeTab === "graphs" && !showGraphs) {
      setActiveTab(showLogs ? "logs" : "traces");
    }
  }, [activeTab, selectedNode?.id, showMetrics, showLogs, showGraphs]);

  if (partitionLoading) {
    return (
      <div className="min-h-screen bg-slate-950 flex items-center justify-center">
        <div className="text-center space-y-4">
          <Loader2 className="w-10 h-10 text-slate-600 mx-auto animate-spin" />
          <p className="text-slate-400 text-sm">Loading partition data…</p>
        </div>
      </div>
    );
  }

  if (!partitionModel) {
    return (
      <div className="min-h-screen bg-slate-950 flex items-center justify-center">
        <div className="text-center space-y-4">
          <Layers className="w-10 h-10 text-slate-600 mx-auto" />
          <p className="text-slate-400 text-sm">No partition data</p>
          {partitionError && <p className="text-amber-400 text-xs">{partitionError}</p>}
          <button onClick={() => navigate("/fleet")} className="px-4 py-2 bg-slate-900 border border-slate-700 text-sm text-slate-200 hover:bg-slate-800">Back</button>
        </div>
      </div>
    );
  }

  const allAssets = partitionModel.intents.flatMap((i) => i.assets);
  const crit = allAssets.filter((a) => a.status === "critical").length;
  const deg = allAssets.filter((a) => a.status === "degraded").length;
  const ok = allAssets.filter((a) => a.status === "healthy").length;
  const pColor = crit > 0 ? "var(--ui-red)" : deg > 0 ? "var(--ui-amber)" : "var(--ui-green)";
  const TIME_OPTIONS = [15, 60, 360, 1440];

  return (
    <div className="min-h-screen bg-slate-950 text-slate-100 relative overflow-hidden">
      {/* Animated background */}
      <div className="fixed inset-0 pointer-events-none z-0">
        <div className="absolute inset-0 opacity-[0.03]">
          <svg width="100%" height="100%">
            <defs>
              <pattern id="tgrid" width="32" height="32" patternUnits="userSpaceOnUse">
                <path d="M 32 0 L 0 0 0 32" fill="none" stroke="currentColor" strokeWidth="0.5" />
              </pattern>
              <radialGradient id="tglow" cx="50%" cy="50%">
                <stop offset="0%" stopColor={pColor} stopOpacity="0.06" />
                <stop offset="100%" stopColor="transparent" />
              </radialGradient>
            </defs>
            <rect width="100%" height="100%" fill="url(#tgrid)" className="text-slate-600" />
            <circle cx="50%" cy="40%" r="50%" fill="url(#tglow)" />
          </svg>
        </div>
      </div>

      {/* Header */}
      <motion.header
        initial={{ opacity: 0, y: -10 }}
        animate={{ opacity: 1, y: 0 }}
        className="relative z-10 sticky top-0 bg-slate-950/90 backdrop-blur-lg border-b border-slate-800/60"
      >
        <div className="px-6 py-3 flex items-center justify-between gap-4">
          <div className="flex items-center gap-3">
            <motion.button whileHover={{ x: -2 }} onClick={() => navigate("/fleet")} className="text-slate-400 hover:text-slate-200 transition-colors flex items-center gap-1.5">
              <ArrowLeft className="w-4 h-4" /><span className="text-xs">Fleet</span>
            </motion.button>
            <div className="w-px h-4 bg-slate-700" />
            <div className="flex items-center gap-2">
              <motion.div
                animate={{ scale: [1, 1.3, 1], opacity: [0.8, 0.4, 0.8] }}
                transition={{ duration: 3, repeat: Infinity }}
                className="w-2.5 h-2.5 rounded-full"
                style={{ backgroundColor: pColor }}
              />
              <h1 className="text-sm font-bold text-slate-100 font-['Space_Grotesk'] tracking-tight">
                {name || partitionModel.region}
              </h1>
            </div>
          </div>

          <div className="flex items-center gap-2 text-[10px] text-slate-500">
            <span className="flex items-center gap-1"><Server className="w-3 h-3" />{allAssets.length} assets</span>
            <span className="w-px h-3 bg-slate-700" />
            <span className="flex items-center gap-1"><Network className="w-3 h-3" />{partitionModel.intents.length} intents</span>
            <span className="w-px h-3 bg-slate-700" />
            <span className="flex items-center gap-1"><Activity className="w-3 h-3" />{filteredEdges.length} connections</span>
          </div>
        </div>
      </motion.header>

      <div className="relative z-10">
        {/* Stat chips + time range + helper */}
        <div className="px-6 py-3 flex items-center gap-4 flex-wrap">
          <div className="flex items-center gap-2">
            <div className="flex items-center gap-1 bg-slate-900 border border-slate-800 px-3 py-1.5">
              <CheckCircle2 className="w-3 h-3 text-emerald-400" />
              <span className="text-[11px] text-emerald-400 font-bold font-mono">{ok}</span>
              <span className="text-[10px] text-slate-500">healthy</span>
            </div>
            <div className="flex items-center gap-1 bg-slate-900 border border-slate-800 px-3 py-1.5">
              <AlertTriangle className="w-3 h-3 text-amber-400" />
              <span className="text-[11px] text-amber-400 font-bold font-mono">{deg}</span>
              <span className="text-[10px] text-slate-500">degraded</span>
            </div>
            <div className="flex items-center gap-1 bg-slate-900 border border-slate-800 px-3 py-1.5">
              <span className="text-[11px] text-red-400 font-bold font-mono">{crit}</span>
              <span className="text-[10px] text-slate-500">critical</span>
            </div>
          </div>

          {/* Time range selector */}
          <div className="h-6 w-px bg-slate-700" />
          <div className="flex items-center gap-1">
            {TIME_OPTIONS.map((m) => (
              <button
                key={m}
                onClick={() => handleRangeChange(m)}
                className={`px-2.5 py-1 text-[11px] rounded border font-mono transition-colors ${
                  timeRange === m
                    ? "bg-sky-500/20 border-sky-400/60 text-sky-300"
                    : "bg-slate-900 border-slate-700 text-slate-400 hover:border-slate-600 hover:text-slate-300"
                }`}
              >
                {minutesLabel(m)}
              </button>
            ))}
          </div>

          <div className="flex-1" />

          <label className="flex items-center gap-1.5 px-3 py-1.5 bg-slate-900 border border-slate-700 text-[11px] text-slate-300 hover:bg-slate-800 cursor-pointer transition-colors relative">
            <Upload className="w-3 h-3" />
            Helper
            <input type="file" accept=".json" onChange={handleFileUpload} className="absolute inset-0 opacity-0 cursor-pointer" />
          </label>
          {helperManifest && (
            <span className="text-[10px] text-emerald-400">{Object.keys(helperManifest.assets).length} configured</span>
          )}
        </div>
        {(uploadError || partitionError) && (
          <div className="px-6 pb-2 text-[11px] space-y-1">
            {uploadError && <div className="text-amber-400">{uploadError}</div>}
            {partitionError && <div className="text-amber-400">{partitionError}</div>}
          </div>
        )}

        {/* Topology graph */}
        <div className="px-6 pb-4">
          <div className="bg-slate-900/60 border border-slate-800 overflow-hidden relative">
            <div className="px-3 py-2 border-b border-slate-800/60 flex items-center justify-between text-[10px] text-slate-500 bg-slate-950/60">
              <span className="uppercase tracking-wider">
                Topology Map
                {selectedNode && (
                  <span className="ml-2 text-slate-400 normal-case tracking-normal">
                    — selected: <span className="text-sky-400 font-mono">{selectedNode.name}</span>
                  </span>
                )}
              </span>
              <div className="flex items-center gap-2">
                <span className="text-[10px] text-slate-600">
                  {hiddenInfrastructureCount > 0 && !showInfrastructure
                    ? `${hiddenInfrastructureCount} infra hidden`
                    : ""}
                </span>
                <button
                  onClick={() => setShowInfrastructure((value) => !value)}
                  className={`px-2 py-0.5 border text-[10px] transition-colors ${
                    showInfrastructure
                      ? "border-amber-500/50 text-amber-300 bg-amber-500/10"
                      : "border-slate-700 text-slate-400 hover:text-slate-200"
                  }`}
                  title="VPCs, subnets, security groups, IAM roles, and other CFN plumbing"
                >
                  {showInfrastructure ? "Hide infrastructure" : "Show infrastructure"}
                </button>
                <button onClick={() => setZoom((z) => Math.min(3, z + 0.15))} className="p-1 hover:bg-slate-800"><ZoomIn className="w-3 h-3" /></button>
                <button onClick={() => setZoom((z) => Math.max(0.3, z - 0.15))} className="p-1 hover:bg-slate-800"><ZoomOut className="w-3 h-3" /></button>
                <button onClick={() => { setZoom(1); setPan({ x: 0, y: 0 }); }} className="p-1 hover:bg-slate-800"><RotateCcw className="w-3 h-3" /></button>
              </div>
            </div>

            <svg
              width="100%"
              height="400"
              viewBox={`0 0 ${svgW} ${svgH}`}
              onWheel={(e) => { e.preventDefault(); setZoom((z) => Math.min(3, Math.max(0.3, z + (e.deltaY > 0 ? -0.1 : 0.1)))); }}
              onMouseDown={(e) => { setDragging(true); dragRef.current = { sx: e.clientX, sy: e.clientY, px: pan.x, py: pan.y }; }}
              onMouseMove={(e) => { if (!dragging || !dragRef.current) return; setPan({ x: dragRef.current.px + (e.clientX - dragRef.current.sx), y: dragRef.current.py + (e.clientY - dragRef.current.sy) }); }}
              onMouseUp={() => setDragging(false)}
              onMouseLeave={() => setDragging(false)}
              className="bg-slate-950/40 cursor-grab"
            >
              <defs>
                <filter id="glow">
                  <feGaussianBlur stdDeviation="2" result="blur" />
                  <feMerge><feMergeNode in="blur" /><feMergeNode in="SourceGraphic" /></feMerge>
                </filter>
                <filter id="selectedGlow">
                  <feGaussianBlur stdDeviation="3" result="blur" />
                  <feMerge><feMergeNode in="blur" /><feMergeNode in="SourceGraphic" /></feMerge>
                </filter>
                <marker id="arrowHead" markerWidth="8" markerHeight="6" refX="8" refY="3" orient="auto">
                  <path d="M0,0 L8,3 L0,6 Z" fill="var(--ui-accent)" />
                </marker>
              </defs>

              <g transform={`translate(${pan.x} ${pan.y}) scale(${zoom})`}>
                {filteredEdges.map((edge, idx) => {
                  const f = nodeById.get(edge.from);
                  const t = nodeById.get(edge.to);
                  if (!f || !t) return null;
                  const fKind = resolveNodeKind(f);
                  const stroke = fKind === "intent" ? "var(--ui-overlay-border)" : "var(--ui-border)";
                  return (
                    <g key={`e-${idx}`}>
                      <line
                        x1={f.x + 55} y1={f.y + 22}
                        x2={t.x + 55} y2={t.y + 22}
                        stroke={stroke}
                        strokeWidth={fKind === "intent" ? 2 : 1}
                        markerEnd="url(#arrowHead)"
                      />
                      <circle r="2.5" fill="var(--ui-accent)" opacity="0.6">
                        <animateMotion
                          dur={`${2 + idx * 0.7}s`}
                          repeatCount="indefinite"
                          path={`M${f.x + 55},${f.y + 22} L${t.x + 55},${t.y + 22}`}
                        />
                      </circle>
                    </g>
                  );
                })}

                {layoutNodes.map((node) => {
                  const kind = resolveNodeKind(node);
                  const selected = selectedNode?.id === node.id;
                  const hColor = healthColor(
                    selected && healthMetricStatus !== "unknown" ? healthMetricStatus : node.health?.level,
                  );
                  const isIntent = kind === "intent";
                  const w = isIntent ? 120 : 100;
                  const h = isIntent ? 44 : 36;
                  const fs = isIntent ? 11 : 10;

                  return (
                    <g
                      key={node.id}
                      transform={`translate(${node.x} ${node.y})`}
                      onClick={(e) => { e.stopPropagation(); handleNodeSelect(node); }}
                      onMouseDown={(e) => e.stopPropagation()}
                      className="cursor-pointer transition-transform"
                    >
                      {selected && (
                        <rect x={-4} y={-4} width={w + 8} height={h + 8} fill="none" stroke="var(--chart-1)" strokeWidth="1.5" opacity="0.5" filter="url(#selectedGlow)" />
                      )}
                      <rect
                        x={0} y={0} width={w} height={h}
                        fill={isIntent ? "var(--ui-panel)" : "var(--ui-card)"}
                        stroke={selected ? "var(--ui-accent)" : `${hColor}40`}
                        strokeWidth={selected ? 1.5 : 1}
                      />
                      <circle cx={isIntent ? 10 : 8} cy={h / 2} r={isIntent ? 5 : 4} fill={hColor} opacity="0.8" />
                      <text x={isIntent ? 20 : 16} y={isIntent ? 16 : 14} fill="var(--ui-faint)" fontSize={fs - 2}>
                        {kind.toUpperCase()}
                      </text>
                      <text x={isIntent ? 20 : 16} y={isIntent ? 32 : 27} fill="var(--ui-text)" fontSize={fs} fontWeight={isIntent ? 600 : 400}>
                        {node.name.length > 14 ? `${node.name.slice(0, 12)}..` : node.name}
                      </text>
                    </g>
                  );
                })}
              </g>
            </svg>
          </div>
        </div>

        {/* Dashboard tabs */}
        <div className="px-6 pb-12" ref={tabsRef}>
          <div className="bg-slate-900/60 border border-slate-800 overflow-hidden">
            {/* Tab bar */}
            <div className="flex items-center border-b border-slate-800/60 bg-slate-950/60">
              {showMetrics && (
                <button
                  onClick={() => handleTabChange("metrics")}
                  className={`flex items-center gap-1.5 px-4 py-2.5 text-xs font-medium transition-colors border-b-2 ${
                    activeTab === "metrics"
                      ? "border-sky-400 text-sky-300 bg-sky-500/5"
                      : "border-transparent text-slate-500 hover:text-slate-400"
                  }`}
                >
                  <BarChart3 className="w-3.5 h-3.5" />
                  Metrics
                </button>
              )}
              <button
                onClick={() => handleTabChange("traces")}
                className={`flex items-center gap-1.5 px-4 py-2.5 text-xs font-medium transition-colors border-b-2 ${
                  activeTab === "traces"
                    ? "border-violet-400 text-violet-300 bg-violet-500/5"
                    : "border-transparent text-slate-500 hover:text-slate-400"
                }`}
              >
                <GanttChartSquare className="w-3.5 h-3.5" />
                Traces
              </button>
              {showLogs && (
                <button
                  onClick={() => handleTabChange("logs")}
                  className={`flex items-center gap-1.5 px-4 py-2.5 text-xs font-medium transition-colors border-b-2 ${
                    activeTab === "logs"
                      ? "border-amber-400 text-amber-300 bg-amber-500/5"
                      : "border-transparent text-slate-500 hover:text-slate-400"
                  }`}
                >
                  <ScrollText className="w-3.5 h-3.5" />
                  Logs
                </button>
              )}
              {showGraphs && (
                <button
                  onClick={() => handleTabChange("graphs")}
                  className={`flex items-center gap-1.5 px-4 py-2.5 text-xs font-medium transition-colors border-b-2 ${
                    activeTab === "graphs"
                      ? "border-cyan-400 text-cyan-300 bg-cyan-500/5"
                      : "border-transparent text-slate-500 hover:text-slate-400"
                  }`}
                >
                  <BarChart3 className="w-3.5 h-3.5" />
                  Graphs
                </button>
              )}

              <div className="flex-1" />

              {/* Copy dashboard link */}
              <button
                onClick={() => setStateEditorOpen((v) => !v)}
                className={`px-3 py-2 text-[10px] transition-colors ${
                  stateEditorOpen ? "text-sky-300" : "text-slate-400 hover:text-slate-200"
                }`}
                title="Open location.state editor"
              >
                state
              </button>
              <button
                onClick={handleCopyLink}
                className="flex items-center gap-1 px-3 py-2 text-[10px] text-slate-400 hover:text-slate-200 transition-colors"
                title="Copy dashboard link"
              >
                {copied ? <Check className="w-3 h-3 text-emerald-400" /> : <Link2 className="w-3 h-3" />}
                {copied ? "Copied!" : "Share"}
              </button>
            </div>

            {/* Panel content */}
            <div className="p-4">
              {stateEditorOpen && (
                <div className="mb-4 bg-slate-950/60 border border-slate-800 rounded-lg p-3 space-y-2">
                  <div className="flex items-center justify-between">
                    <p className="text-xs font-semibold text-slate-300">location.state editor</p>
                    <div className="flex items-center gap-2">
                      <button onClick={hydrateStateEditor} className="px-2 py-1 text-[10px] border border-slate-700 text-slate-300 hover:bg-slate-800">
                        Load current
                      </button>
                      <button onClick={applyStateEditor} className="px-2 py-1 text-[10px] border border-sky-500/60 text-sky-300 hover:bg-sky-500/10">
                        Apply state
                      </button>
                    </div>
                  </div>
                  <p className="text-[10px] text-slate-500">Edit JSON and apply to update this page state in-place.</p>
                  <textarea
                    value={stateEditorText}
                    onChange={(e) => setStateEditorText(e.target.value)}
                    className="w-full h-44 bg-slate-900 border border-slate-700 text-[11px] text-slate-200 font-mono p-2 outline-none focus:border-sky-500/60"
                    spellCheck={false}
                  />
                  {stateEditorError && <p className="text-[10px] text-rose-400">{stateEditorError}</p>}
                </div>
              )}

              {!selectedNode ? (
                <div className="flex flex-col items-center justify-center py-16 text-slate-600 space-y-3">
                  <Layers className="w-8 h-8 opacity-40" />
                  <p className="text-sm">Click a node in the topology map above</p>
                  <p className="text-xs text-slate-700">Metrics, traces, and logs will appear here for the selected asset</p>
                </div>
              ) : (
                <>
                  {/* Selected node header */}
                  <div className="flex items-center gap-3 mb-4 pb-3 border-b border-slate-800/60">
                    <div className="flex items-center gap-2">
                      <div
                        className="w-2 h-2 rounded-full"
                        style={{
                          backgroundColor: healthColor(
                            healthMetricStatus === "unknown" ? selectedNode.health?.level : healthMetricStatus,
                          ),
                        }}
                        title={healthMetricStatus === "unknown" ? "Topology health" : `Health metric: ${healthMetricStatus}`}
                      />
                      <span className="text-sm font-semibold text-slate-200">{selectedNode.name}</span>
                      <span className="text-[10px] text-slate-500 uppercase px-1.5 py-0.5 bg-slate-800 rounded">
                        {resolveNodeKind(selectedNode)}
                      </span>
                    </div>
                    {selectedNode.partition && (
                      <span className="text-[10px] text-slate-500">{selectedNode.partition}</span>
                    )}
                    {selectedNode.intent && (
                      <span className="text-[10px] text-slate-500">/ {selectedNode.intent}</span>
                    )}
                    {healthRule && healthMetricStatus !== "unknown" && (
                      <span
                        className="text-[10px] font-mono px-2 py-0.5 rounded border"
                        style={{
                          color: healthColor(healthMetricStatus),
                          borderColor: `${healthColor(healthMetricStatus)}66`,
                        }}
                        title={`${healthRule.metric}: ${healthRuleText(healthRule)}`}
                      >
                        {healthRule.metric} ={" "}
                        {healthMetricValue !== null
                          ? formatHealthValue(healthMetricValue, healthRule.unit)
                          : "…"}{" "}
                        · {healthMetricStatus}
                      </span>
                    )}
                    <div className="flex-1" />
                    {/* Helper config hint */}
                    {helperManifest && selectedNode.name in helperManifest.assets && (
                      <span className="text-[10px] text-emerald-400 px-2 py-0.5 bg-emerald-500/10 border border-emerald-500/20 rounded">
                        configured
                      </span>
                    )}
                  </div>

                  {/* Metrics tab */}
                  {activeTab === "metrics" && showMetrics && (
                    <div className="space-y-5">
                      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4 space-y-2">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                          <h3 className="text-xs font-semibold text-slate-300 flex items-center gap-2">
                            <Activity className="w-3.5 h-3.5 text-cyan-400" />
                            Key metric (auto-detected)
                          </h3>
                          <span className="text-[10px] text-slate-500">
                            {selectedNode.name} · {selectedNode.intent || selectedNode.partition}
                          </span>
                        </div>
                        {metricsProbing || keyMetricLoading ? (
                          <div className="h-44 flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                        ) : detectedMetric ? (
                          <>
                            <p className="text-[11px] text-slate-400">
                              <span className="font-mono text-slate-300">{detectedMetric}</span>
                              <span className="text-slate-600"> · automatically selected from the node’s available series</span>
                            </p>
                            <div className="h-44">
                              {keyMetricError ? (
                                <div className="h-full flex items-center justify-center text-xs text-slate-600">{keyMetricError}</div>
                              ) : keyMetricData.length === 0 ? (
                                <div className="h-full flex items-center justify-center text-xs text-slate-600">No samples in the selected window</div>
                              ) : (
                                <ResponsiveContainer width="100%" height="100%">
                                  <LineChart data={keyMetricData}>
                                    <XAxis dataKey="time" stroke="var(--chart-text)" fontSize={10} />
                                    <YAxis stroke="var(--chart-text)" fontSize={10} />
                                    <Tooltip contentStyle={{ backgroundColor: "var(--ui-elevated)", borderColor: "var(--ui-border)", color: "var(--ui-text)", fontSize: 12 }} />
                                    <Line type="monotone" dataKey="value" name={detectedMetric} stroke="var(--chart-1)" strokeWidth={1.5} dot={false} />
                                  </LineChart>
                                </ResponsiveContainer>
                              )}
                            </div>
                          </>
                        ) : (
                          <p className="text-[10px] text-slate-500">
                            No metric series found for this node. The Metrics and Graphs tabs are hidden.
                          </p>
                        )}
                      </div>

                      {healthRule && metricsAvailable && (healthMetricLoading || healthMetricStatus !== "unknown") && (
                      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4 space-y-2">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                          <h3 className="text-xs font-semibold text-slate-300 flex items-center gap-2">
                            <Activity className="w-3.5 h-3.5 text-emerald-400" />
                            Health metric (auto-detected)
                          </h3>
                          <span
                            className="text-[10px] px-2 py-0.5 rounded border uppercase tracking-wider"
                            style={{
                              color: healthColor(healthMetricStatus),
                              borderColor: `${healthColor(healthMetricStatus)}66`,
                            }}
                          >
                            {healthMetricStatus === "unknown" ? "unknown" : healthMetricStatus}
                          </span>
                        </div>
                        {healthRule ? (
                          <>
                            <p className="text-[11px] text-slate-400">
                              <span className="font-mono text-slate-300">{healthRule.metric}</span>
                              <span className="text-slate-600"> · {healthRule.label} · source: {healthRule.source}</span>
                            </p>
                            <p className="text-[10px] text-slate-500">
                              Rule: {healthRuleText(healthRule)}.{" "}
                              {healthMetricLoading
                                ? "Querying…"
                                : healthMetricError
                                  ? healthMetricError
                                  : healthMetricValue !== null
                                    ? `Current value: ${formatHealthValue(healthMetricValue, healthRule.unit)}.`
                                    : "No samples in the current window; using topology health."}
                            </p>
                          </>
                        ) : (
                          <p className="text-[10px] text-slate-500">
                            No single health metric is defined for this asset type. Use a helper manifest metric to override it.
                          </p>
                        )}
                      </div>
                      )}

                      {metricsAvailable && (
                      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4 space-y-3">
                        <div className="flex items-center justify-between gap-3">
                          <h3 className="text-xs font-semibold text-slate-300">Custom graph query</h3>
                          <button
                            onClick={() => void runCustomQuery()}
                            disabled={customLoading}
                            className="px-2.5 py-1 text-[11px] border border-sky-500/60 text-sky-300 hover:bg-sky-500/10 disabled:opacity-50"
                          >
                            {customLoading ? "Running..." : "Run query"}
                          </button>
                        </div>
                        <p className="text-[10px] text-slate-500">
                          Macros: <span className="font-mono">$serviceRegex</span>, <span className="font-mono">$asset</span>, <span className="font-mono">$node</span>, <span className="font-mono">$intent</span>, <span className="font-mono">$partition</span>
                        </p>
                        <textarea
                          value={customQuery}
                          onChange={(e) => setCustomQuery(e.target.value)}
                          className="w-full h-20 bg-slate-900 border border-slate-700 text-[11px] text-slate-200 font-mono p-2 outline-none focus:border-sky-500/60"
                          spellCheck={false}
                        />
                        <div className="h-44">
                          {customLoading ? (
                            <div className="h-full flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                          ) : customError ? (
                            <div className="h-full flex items-center justify-center text-xs text-slate-600">{customError}</div>
                          ) : customData.length === 0 || customSeries.length === 0 ? (
                            <div className="h-full flex items-center justify-center text-xs text-slate-600">Run a query to render a separate graph</div>
                          ) : (
                            <ResponsiveContainer width="100%" height="100%">
                              <LineChart data={customData}>
                                <XAxis dataKey="time" stroke="var(--chart-text)" fontSize={10} />
                                <YAxis stroke="var(--chart-text)" fontSize={10} />
                                <Tooltip contentStyle={{ backgroundColor: "var(--ui-elevated)", borderColor: "var(--ui-border)", color: "var(--ui-text)", fontSize: 12 }} />
                                {customSeries.map((series) => (
                                  <Line key={series.key} type="monotone" dataKey={series.key} name={series.label} stroke={series.color} strokeWidth={1.5} dot={false} connectNulls />
                                ))}
                              </LineChart>
                            </ResponsiveContainer>
                          )}
                        </div>
                      </div>
                      )}

                      {selectedNode.type === "asset" && metricData.length > 0 && (
                      <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
                        {/* Latency chart */}
                        <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4">
                          <div className="flex items-center justify-between mb-3">
                            <h3 className="text-xs font-semibold text-slate-300 flex items-center gap-2">
                              <Activity className="w-3.5 h-3.5 text-sky-400" />
                              Latency (p95 & p99 ms)
                            </h3>
                            {metricSource !== "none" && (
                              <span className={`text-[10px] px-2 py-0.5 rounded border ${sourceBadgeClass(metricSource)}`}>
                                {sourceBadgeLabel(metricSource)}
                              </span>
                            )}
                          </div>
                          {metricLoading ? (
                            <div className="h-44 flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                          ) : metricError ? (
                            <div className="h-44 flex items-center justify-center text-xs text-slate-600">{metricError}</div>
                          ) : metricData.length === 0 ? (
                            <div className="h-44 flex items-center justify-center text-xs text-slate-600">No metric samples for the selected time window</div>
                          ) : (
                            <div className="h-44">
                              <ResponsiveContainer width="100%" height="100%">
                                <LineChart data={metricData}>
                                  <XAxis dataKey="time" stroke="var(--chart-text)" fontSize={10} />
                                  <YAxis stroke="var(--chart-text)" fontSize={10} />
                                  <Tooltip contentStyle={{ backgroundColor: "var(--ui-elevated)", borderColor: "var(--ui-border)", color: "var(--ui-text)", fontSize: 12 }} />
                                  <Line type="monotone" dataKey="p95" stroke="var(--chart-1)" strokeWidth={1.5} dot={false} />
                                  <Line type="monotone" dataKey="p99" stroke="var(--chart-5)" strokeWidth={1.5} dot={false} />
                                </LineChart>
                              </ResponsiveContainer>
                            </div>
                          )}
                        </div>

                        {/* RPS chart */}
                        <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4">
                          <h3 className="text-xs font-semibold text-slate-300 flex items-center gap-2 mb-3">
                            <Activity className="w-3.5 h-3.5 text-cyan-400" />
                            Throughput (RPS)
                          </h3>
                          {metricLoading ? (
                            <div className="h-44 flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                          ) : metricError ? (
                            <div className="h-44 flex items-center justify-center text-xs text-slate-600">{metricError}</div>
                          ) : metricData.length === 0 ? (
                            <div className="h-44 flex items-center justify-center text-xs text-slate-600">No throughput data for the selected time window</div>
                          ) : (
                            <div className="h-44">
                              <ResponsiveContainer width="100%" height="100%">
                                <LineChart data={metricData}>
                                  <XAxis dataKey="time" stroke="var(--chart-text)" fontSize={10} />
                                  <YAxis stroke="var(--chart-text)" fontSize={10} />
                                  <Tooltip contentStyle={{ backgroundColor: "var(--ui-elevated)", borderColor: "var(--ui-border)", color: "var(--ui-text)", fontSize: 12 }} />
                                  <Line type="monotone" dataKey="rps" stroke="var(--chart-2)" strokeWidth={1.5} dot={false} />
                                </LineChart>
                              </ResponsiveContainer>
                            </div>
                          )}
                        </div>
                      </div>
                      )}
                    </div>
                  )}

                  {/* Traces tab */}
                  {activeTab === "traces" && (
                    <div className="space-y-4">
                      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-3">
                        <div className="flex items-center justify-between gap-3 mb-2">
                          <h3 className="text-xs font-semibold text-slate-300 flex items-center gap-2">
                            <Network className="w-3.5 h-3.5 text-violet-400" />
                            Trace Flow
                          </h3>
                          {selectedTrace ? (
                            <span className="text-[10px] text-slate-500 font-mono truncate" title={selectedTrace.trace_id}>
                              {selectedTrace.trace_id}
                            </span>
                          ) : null}
                        </div>
                        {tracesLoading ? (
                          <div className="py-8 flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                        ) : traceSpans.length === 0 ? (
                          <p className="text-xs text-slate-600 py-4">Select a trace to view its flow</p>
                        ) : (
                          <TraceFlow spans={traceSpans} />
                        )}
                      </div>
                      <div className="grid grid-cols-1 xl:grid-cols-3 gap-4">
                      <div className="xl:col-span-1 bg-slate-950/60 border border-slate-800 rounded-lg p-3">
                        <h3 className="text-xs font-semibold text-slate-300 flex items-center gap-2 mb-2">
                          <GanttChartSquare className="w-3.5 h-3.5 text-violet-400" />
                          Recent Traces
                        </h3>
                        {tracesFallback && !tracesLoading ? (
                          <p className="text-[10px] text-amber-400/80 mb-2">
                            No direct traces for {selectedNode.name}; showing traces from partition{" "}
                            <code>{selectedNode.partition}</code>.
                          </p>
                        ) : null}
                        {tracesLoading ? (
                          <div className="py-8 flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                        ) : tracesError ? (
                          <p className="text-xs text-slate-600 py-4">{tracesError}</p>
                        ) : traces.length === 0 ? (
                          <p className="text-xs text-slate-600 py-4">No traces available</p>
                        ) : (
                          <div className="space-y-1 max-h-[320px] overflow-y-auto">
                            {traces.map((trace) => (
                              <button
                                key={trace.trace_id}
                                onClick={() => void openTrace(trace)}
                                className={`w-full text-left px-2.5 py-2 rounded border text-xs font-mono transition-colors ${
                                  selectedTrace?.trace_id === trace.trace_id
                                    ? "bg-violet-500/15 border-violet-400/60 text-violet-200"
                                    : "bg-slate-900 border-slate-800 text-slate-400 hover:border-slate-700"
                                }`}
                              >
                                <div className="truncate">{trace.service || "unknown"}</div>
                                <div className="truncate text-[10px] text-slate-500 mt-0.5">{trace.trace_id.slice(0, 16)}</div>
                              </button>
                            ))}
                          </div>
                        )}
                      </div>
                      <div className="xl:col-span-2 bg-slate-950/60 border border-slate-800 rounded-lg p-3">
                        <h3 className="text-xs font-semibold text-slate-300 flex items-center gap-2 mb-2">
                          <Activity className="w-3.5 h-3.5 text-violet-400" />
                          Trace Waterfall
                        </h3>
                        <div className="max-h-[320px] overflow-y-auto">
                          {tracesLoading ? (
                            <div className="py-8 flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                          ) : traceSpans.length === 0 ? (
                            <p className="text-xs text-slate-600 py-4">Select a trace to view its waterfall</p>
                          ) : (
                            <TraceWaterfall spans={traceSpans} />
                          )}
                        </div>
                      </div>
                      </div>
                    </div>
                  )}

                  {/* Logs tab */}
                  {activeTab === "logs" && showLogs && (
                    <div>
                      {logsLoading ? (
                        <div className="py-8 flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                      ) : logsError ? (
                        <div className="py-8 text-center text-xs text-slate-600">{logsError}</div>
                      ) : logs.length === 0 ? (
                        <div className="py-8 text-center text-xs text-slate-600">No logs found for the selected asset and time window</div>
                      ) : (
                        <div className="space-y-1">
                          <div className="flex items-center gap-3 mb-2 pb-2 border-b border-slate-800/60">
                            <h3 className="text-xs font-semibold text-slate-300 flex items-center gap-2">
                              <ScrollText className="w-3.5 h-3.5 text-amber-400" />
                              Log Entries
                            </h3>
                            <span className="text-[10px] text-slate-500">{logs.length} entries</span>
                          </div>
                          <div className="space-y-0.5 max-h-[500px] overflow-y-auto">
                            {logs.map((log, i) => (
                              <div key={i} className="flex items-start gap-2 px-2.5 py-1.5 bg-slate-950/60 border border-slate-800/40 hover:border-slate-700/60 transition-colors group">
                                <span className="text-[10px] text-slate-600 font-mono shrink-0 w-[52px] pt-px">
                                  {new Date(log.timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}
                                </span>
                                <span className={`text-[9px] px-1 py-px rounded border shrink-0 ${logSevBadge(sevKey(log.severity_text ?? ""))}`}>
                                  {sevKey(log.severity_text ?? "")}
                                </span>
                                <span className="text-[10px] text-slate-500 font-mono shrink-0 max-w-[100px] truncate">{log.service || "-"}</span>
                                <span className="text-[11px] text-slate-300 leading-relaxed break-all">{log.body}</span>
                              </div>
                            ))}
                          </div>
                        </div>
                      )}
                    </div>
                  )}

                  {/* Graphs tab */}
                  {activeTab === "graphs" && metricsAvailable && (
                    <div className="space-y-4">
                      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4">
                        <h3 className="text-xs font-semibold text-slate-300 mb-2">Graph Editor</h3>
                        <p className="text-[10px] text-slate-500 mb-3">
                          Build graph queries from metrics, logs, and traces. Drag blocks into the pipeline, then apply the generated query to the Metrics chart.
                        </p>
                        <GraphQueryBuilder
                          selectedNode={selectedNode}
                          onApplyQuery={handleApplyGraphQuery}
                          discoveredMetrics={discoveredMetrics}
                          observability={observabilityConfig}
                        />
                      </div>

                      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4 space-y-3">
                        <div className="flex items-center justify-between gap-2">
                          <h3 className="text-xs font-semibold text-slate-300">Ad-hoc and Persistent Dashboards</h3>
                          {graphsNotice && <span className="text-[10px] text-emerald-300">{graphsNotice}</span>}
                        </div>
                        <div className="grid grid-cols-1 md:grid-cols-3 gap-2">
                          <label className="text-[10px] text-slate-500">
                            Graph title
                            <input
                              value={graphTitle}
                              onChange={(e) => setGraphTitle(e.target.value)}
                              className="mt-1 w-full bg-slate-900 border border-slate-700 text-[11px] text-slate-200 px-2 py-1"
                              placeholder="My latency graph"
                            />
                          </label>
                          <label className="text-[10px] text-slate-500">
                            Dashboard name
                            <input
                              value={dashboardName}
                              onChange={(e) => setDashboardName(e.target.value)}
                              className="mt-1 w-full bg-slate-900 border border-slate-700 text-[11px] text-slate-200 px-2 py-1"
                              placeholder="Ops overview"
                            />
                          </label>
                          <div className="flex items-end gap-2">
                            <button
                              onClick={() => void addAdhocGraphFromCurrent()}
                              className="px-2.5 py-1 text-[11px] border border-cyan-500/60 text-cyan-300 hover:bg-cyan-500/10"
                            >
                              Add ad-hoc graph
                            </button>
                            <button
                              onClick={saveCurrentQueryAsGraph}
                              className="px-2.5 py-1 text-[11px] border border-sky-500/60 text-sky-300 hover:bg-sky-500/10"
                            >
                              Save graph
                            </button>
                            <button
                              onClick={saveDashboard}
                              className="px-2.5 py-1 text-[11px] border border-emerald-500/60 text-emerald-300 hover:bg-emerald-500/10"
                            >
                              Save dashboard
                            </button>
                            <button
                              onClick={exportSavedBoards}
                              className="px-2.5 py-1 text-[11px] border border-indigo-500/60 text-indigo-300 hover:bg-indigo-500/10"
                            >
                              Export JSON
                            </button>
                            <button
                              onClick={() => importInputRef.current?.click()}
                              className="px-2.5 py-1 text-[11px] border border-fuchsia-500/60 text-fuchsia-300 hover:bg-fuchsia-500/10"
                            >
                              Import JSON
                            </button>
                            <input
                              ref={importInputRef}
                              type="file"
                              accept="application/json,.json"
                              onChange={importSavedBoards}
                              className="hidden"
                            />
                          </div>
                        </div>
                      </div>

                      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4 space-y-2">
                        <div className="flex items-center justify-between gap-2">
                          <h3 className="text-xs font-semibold text-slate-300">Current Query Preview</h3>
                          <button
                            onClick={() => void runCustomQuery()}
                            disabled={customLoading || !selectedNode}
                            className="px-2.5 py-1 text-[11px] border border-cyan-500/60 text-cyan-300 hover:bg-cyan-500/10 disabled:opacity-40"
                          >
                            {customLoading ? "Running..." : "Run preview"}
                          </button>
                        </div>
                        <pre className="text-[11px] text-slate-300 bg-slate-900 border border-slate-800 p-2 overflow-x-auto">{customQuery}</pre>
                        <div className="h-44">
                          {customLoading ? (
                            <div className="h-full flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                          ) : customError ? (
                            <div className="h-full flex items-center justify-center text-xs text-slate-600">{customError}</div>
                          ) : customData.length === 0 || customSeries.length === 0 ? (
                            <div className="h-full flex items-center justify-center text-xs text-slate-600">Apply and run a query to preview this graph</div>
                          ) : (
                            <ResponsiveContainer width="100%" height="100%">
                              <LineChart data={customData}>
                                <XAxis dataKey="time" stroke="var(--chart-text)" fontSize={10} />
                                <YAxis stroke="var(--chart-text)" fontSize={10} />
                                <Tooltip contentStyle={{ backgroundColor: "var(--ui-elevated)", borderColor: "var(--ui-border)", color: "var(--ui-text)", fontSize: 12 }} />
                                {customSeries.map((series) => (
                                  <Line key={series.key} type="monotone" dataKey={series.key} name={series.label} stroke={series.color} strokeWidth={1.5} dot={false} connectNulls />
                                ))}
                              </LineChart>
                            </ResponsiveContainer>
                          )}
                        </div>
                      </div>

                      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4 space-y-3">
                        <h3 className="text-xs font-semibold text-slate-300">Ad-hoc Graph Board</h3>
                        {adhocGraphs.length === 0 ? (
                          <p className="text-[11px] text-slate-500">No ad-hoc graphs yet. Add one from the current query.</p>
                        ) : (
                          <div
                            className="grid gap-3"
                            style={{ gridTemplateColumns: "repeat(auto-fit, minmax(360px, 1fr))" }}
                          >
                            {adhocGraphs.map((graph) => (
                              <div
                                key={graph.id}
                                draggable
                                onDragStart={(e) => {
                                  setDraggingGraphId(graph.id);
                                  e.dataTransfer.setData("text/plain", graph.id);
                                  e.dataTransfer.effectAllowed = "move";
                                }}
                                onDragOver={(e) => e.preventDefault()}
                                onDrop={(e) => {
                                  e.preventDefault();
                                  const sourceId = e.dataTransfer.getData("text/plain") || draggingGraphId;
                                  if (sourceId) reorderAdhocGraph(sourceId, graph.id);
                                  setDraggingGraphId(null);
                                }}
                                onDragEnd={() => setDraggingGraphId(null)}
                                className="border border-slate-800 rounded-lg bg-slate-900/60 p-3 space-y-2"
                              >
                                <div className="flex items-center gap-2">
                                  <input
                                    value={graph.title}
                                    onChange={(e) => {
                                      const value = e.target.value;
                                      setAdhocGraphs((prev) => prev.map((g) => (g.id === graph.id ? { ...g, title: value } : g)));
                                    }}
                                    className="flex-1 bg-slate-900 border border-slate-700 text-[11px] text-slate-200 px-2 py-1"
                                  />
                                  <button
                                    onClick={() => moveAdhocGraphBy(graph.id, -1)}
                                    className="px-2 py-1 text-[10px] border border-slate-700 text-slate-300 hover:bg-slate-800"
                                    title="Move up"
                                  >
                                    Up
                                  </button>
                                  <button
                                    onClick={() => moveAdhocGraphBy(graph.id, 1)}
                                    className="px-2 py-1 text-[10px] border border-slate-700 text-slate-300 hover:bg-slate-800"
                                    title="Move down"
                                  >
                                    Down
                                  </button>
                                  <button
                                    onClick={() => void refreshAdhocGraph(graph.id, graph.query, graph.rangeMinutes)}
                                    className="px-2 py-1 text-[10px] border border-sky-500/60 text-sky-300 hover:bg-sky-500/10"
                                  >
                                    Refresh
                                  </button>
                                  <button
                                    onClick={() => setAdhocGraphs((prev) => prev.filter((g) => g.id !== graph.id))}
                                    className="px-2 py-1 text-[10px] border border-rose-500/60 text-rose-300 hover:bg-rose-500/10"
                                  >
                                    Remove
                                  </button>
                                </div>
                                <div className="grid grid-cols-2 gap-2">
                                  <label className="text-[10px] text-slate-500">
                                    Range
                                    <select
                                      value={graph.rangeMinutes}
                                      onChange={(e) => {
                                        const value = Number(e.target.value) || 60;
                                        setAdhocGraphs((prev) => prev.map((g) => (g.id === graph.id ? { ...g, rangeMinutes: value } : g)));
                                      }}
                                      className="mt-1 w-full bg-slate-900 border border-slate-700 text-[11px] text-slate-200 px-2 py-1"
                                    >
                                      {TIME_OPTIONS.map((m) => (
                                        <option key={m} value={m}>{minutesLabel(m)}</option>
                                      ))}
                                    </select>
                                  </label>
                                  <label className="text-[10px] text-slate-500">
                                    Auto-refresh (sec, 0=off)
                                    <input
                                      type="number"
                                      min={0}
                                      step={5}
                                      value={graph.refreshIntervalSec}
                                      onChange={(e) => {
                                        const value = Math.max(0, Number(e.target.value) || 0);
                                        setAdhocGraphs((prev) => prev.map((g) => (g.id === graph.id ? { ...g, refreshIntervalSec: value } : g)));
                                      }}
                                      className="mt-1 w-full bg-slate-900 border border-slate-700 text-[11px] text-slate-200 px-2 py-1"
                                    />
                                  </label>
                                </div>
                                <textarea
                                  value={graph.query}
                                  onChange={(e) => {
                                    const value = e.target.value;
                                    setAdhocGraphs((prev) => prev.map((g) => (g.id === graph.id ? { ...g, query: value } : g)));
                                  }}
                                  className="w-full h-16 bg-slate-900 border border-slate-700 text-[11px] text-slate-200 font-mono p-2 outline-none focus:border-sky-500/60"
                                />
                                <div className="h-44">
                                  {graph.loading ? (
                                    <div className="h-full flex items-center justify-center"><Loader2 className="w-5 h-5 text-slate-600 animate-spin" /></div>
                                  ) : graph.error ? (
                                    <div className="h-full flex items-center justify-center text-xs text-slate-600">{graph.error}</div>
                                  ) : graph.data.length === 0 || graph.series.length === 0 ? (
                                    <div className="h-full flex items-center justify-center text-xs text-slate-600">No data for this graph yet</div>
                                  ) : (
                                    <ResponsiveContainer width="100%" height="100%">
                                      <LineChart data={graph.data}>
                                        <XAxis dataKey="time" stroke="var(--chart-text)" fontSize={10} />
                                        <YAxis stroke="var(--chart-text)" fontSize={10} />
                                        <Tooltip contentStyle={{ backgroundColor: "var(--ui-elevated)", borderColor: "var(--ui-border)", color: "var(--ui-text)", fontSize: 12 }} />
                                        {graph.series.map((series) => (
                                          <Line key={series.key} type="monotone" dataKey={series.key} name={series.label} stroke={series.color} strokeWidth={1.5} dot={false} connectNulls />
                                        ))}
                                      </LineChart>
                                    </ResponsiveContainer>
                                  )}
                                </div>
                              </div>
                            ))}
                          </div>
                        )}
                      </div>

                      <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
                        <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4 space-y-2">
                          <h3 className="text-xs font-semibold text-slate-300">Saved Graphs</h3>
                          {savedGraphs.length === 0 ? (
                            <p className="text-[11px] text-slate-500">No saved graphs yet.</p>
                          ) : (
                            <div className="space-y-2 max-h-64 overflow-y-auto">
                              {savedGraphs.map((graph) => (
                                <div key={graph.id} className="border border-slate-800 rounded bg-slate-900/60 p-2">
                                  <p className="text-[11px] text-slate-200 font-medium">{graph.title}</p>
                                  <p className="text-[10px] text-slate-500 truncate font-mono mt-1">{graph.query}</p>
                                  <div className="mt-2 flex items-center gap-2">
                                    <button
                                      onClick={() => void addSavedGraphToBoard(graph)}
                                      className="px-2 py-1 text-[10px] border border-cyan-500/60 text-cyan-300 hover:bg-cyan-500/10"
                                    >
                                      Add to board
                                    </button>
                                    <button
                                      onClick={() => deleteSavedGraph(graph.id)}
                                      className="px-2 py-1 text-[10px] border border-rose-500/60 text-rose-300 hover:bg-rose-500/10"
                                    >
                                      Delete
                                    </button>
                                  </div>
                                </div>
                              ))}
                            </div>
                          )}
                        </div>

                        <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-4 space-y-2">
                          <h3 className="text-xs font-semibold text-slate-300">Saved Dashboards</h3>
                          {savedDashboards.length === 0 ? (
                            <p className="text-[11px] text-slate-500">No saved dashboards yet.</p>
                          ) : (
                            <div className="space-y-2 max-h-64 overflow-y-auto">
                              {savedDashboards.map((dash) => (
                                <div key={dash.id} className="border border-slate-800 rounded bg-slate-900/60 p-2">
                                  <p className="text-[11px] text-slate-200 font-medium">{dash.name}</p>
                                  <p className="text-[10px] text-slate-500 mt-1">{dash.graphs.length} graph(s)</p>
                                  <div className="mt-2 flex items-center gap-2">
                                    <button
                                      onClick={() => void loadDashboard(dash)}
                                      className="px-2 py-1 text-[10px] border border-emerald-500/60 text-emerald-300 hover:bg-emerald-500/10"
                                    >
                                      Load
                                    </button>
                                    <button
                                      onClick={() => deleteDashboard(dash.id)}
                                      className="px-2 py-1 text-[10px] border border-rose-500/60 text-rose-300 hover:bg-rose-500/10"
                                    >
                                      Delete
                                    </button>
                                  </div>
                                </div>
                              ))}
                            </div>
                          )}
                        </div>
                      </div>
                    </div>
                  )}
                </>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
