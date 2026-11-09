import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  ArrowLeft,
  CheckCircle2,
  Clock3,
  ExternalLink,
  RefreshCw,
  Server,
  Timer,
  Waypoints,
} from "lucide-react";
import { api } from "../api/client";
import MiniLineChart from "../components/charts/MiniLineChart";
import TraceWaterfall from "../components/charts/TraceWaterfall";
import RtoPanel from "../components/fleet/RtoPanel";
import TrafficGraph from "../components/fleet/TrafficGraph";
import { computeRto, formatDurationMs, rtoStatus } from "../components/fleet/rto";
import {
  buildTrafficModel,
  edgeRpsFromVector,
  nodeRpsFromVector,
  topologyNodeStatus,
  alertsMatchingNode,
} from "../components/fleet/traffic";
import type { AssetGroup, HealthStatus } from "./FleetView";
import type { Alert } from "../types/alerts";
import type {
  GuardianEventRecord,
  GuardianHealthResponse,
  GuardianTopologyResponse,
  PrometheusMatrixResult,
  PrometheusVectorResult,
  TraceRecord,
  TraceSummary,
} from "../types/api";

interface HubDetailViewProps {
  asset: AssetGroup;
  health: GuardianHealthResponse | null;
  events: GuardianEventRecord[];
  tenant: string;
  rangeMinutes: number;
  onRangeChange: (minutes: number) => void;
  onBack: () => void;
}

interface TelemetryPoint {
  time: string;
  p95: number;
  p99: number;
  rps: number;
}

interface SourceQuery {
  query: string;
  source: string;
}

const RTO_STORAGE_KEY = "doctor.fleet.rtoTargetMs";
const DEFAULT_RTO_MS = 30 * 60_000;

function minutesLabel(minutes: number): string {
  if (minutes < 60) return `${minutes}m`;
  if (minutes < 60 * 24) return `${minutes / 60}h`;
  return `${minutes / (60 * 24)}d`;
}

function escapeRegex(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function escapePromQL(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
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

function mergeTelemetry(
  p95Points: Array<{ ts: number; value: number }>,
  p99Points: Array<{ ts: number; value: number }>,
  rpsPoints: Array<{ ts: number; value: number }>
): TelemetryPoint[] {
  const byTs = new Map<number, TelemetryPoint>();
  const ensure = (ts: number) => {
    let point = byTs.get(ts);
    if (!point) {
      point = { time: new Date(ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }), p95: 0, p99: 0, rps: 0 };
      byTs.set(ts, point);
    }
    return point;
  };
  for (const row of p95Points) ensure(row.ts).p95 = row.value;
  for (const row of p99Points) ensure(row.ts).p99 = row.value;
  for (const row of rpsPoints) ensure(row.ts).rps = row.value;
  return [...byTs.entries()].sort((a, b) => a[0] - b[0]).map(([, point]) => point);
}

async function runRangeQuery(query: string, tenant: string, from: string, to: string, step: string) {
  const params = new URLSearchParams({ query, start: from, end: to, step, tenant });
  const response = await api.queryPrometheusRange(params);
  if (response.status !== "success" || response.data.resultType !== "matrix") return [];
  return matrixPoints(response.data.result);
}

async function firstNonEmptyRangeQuery(
  queries: SourceQuery[],
  tenant: string,
  from: string,
  to: string,
  step: string
): Promise<{ points: Array<{ ts: number; value: number }>; source: string }> {
  for (const query of queries) {
    try {
      const points = await runRangeQuery(query.query, tenant, from, to, step);
      if (points.length > 0) return { points, source: query.source };
    } catch {
      // Try the next fallback.
    }
  }
  return { points: [], source: "none" };
}

async function instantVector(query: string, tenant: string): Promise<PrometheusVectorResult[]> {
  try {
    const response = await api.queryPrometheus(new URLSearchParams({ query, tenant }));
    if (response.status !== "success" || response.data.resultType !== "vector") return [];
    return response.data.result;
  } catch {
    return [];
  }
}

async function firstNonEmptyVector(queries: string[], tenant: string): Promise<PrometheusVectorResult[]> {
  for (const query of queries) {
    const result = await instantVector(query, tenant);
    if (result.length > 0) return result;
  }
  return [];
}

function statusClass(status: HealthStatus): string {
  if (status === "critical") return "border-rose-500/40 bg-rose-500/10 text-rose-300";
  if (status === "degraded") return "border-amber-500/40 bg-amber-500/10 text-amber-300";
  return "border-emerald-500/40 bg-emerald-500/10 text-emerald-300";
}

function alertSeverityClass(severity: string): string {
  if (severity === "critical") return "border-rose-500/40 bg-rose-500/10 text-rose-300";
  if (severity === "warning") return "border-amber-500/40 bg-amber-500/10 text-amber-300";
  return "border-sky-500/40 bg-sky-500/10 text-sky-300";
}

export default function HubDetailView({
  asset,
  health,
  events,
  tenant,
  rangeMinutes,
  onRangeChange,
  onBack,
}: HubDetailViewProps) {
  const navigate = useNavigate();
  const [topology, setTopology] = useState<GuardianTopologyResponse | null>(null);
  const [topologyStatus, setTopologyStatus] = useState("Loading topology…");
  const [nodeRps, setNodeRps] = useState<Map<string, number>>(new Map());
  const [edgeRps, setEdgeRps] = useState<Map<string, number>>(new Map());
  const [explicitEdges, setExplicitEdges] = useState(false);
  const [trafficStatus, setTrafficStatus] = useState("Loading traffic…");
  const [selectedNodeId, setSelectedNodeId] = useState(asset.id || asset.name);
  const [alerts, setAlerts] = useState<Alert[]>([]);
  const [alertsStatus, setAlertsStatus] = useState("Loading alerts…");
  const [telemetry, setTelemetry] = useState<TelemetryPoint[]>([]);
  const [telemetryStatus, setTelemetryStatus] = useState("Loading telemetry…");
  const [metricSource, setMetricSource] = useState("none");
  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [traceStatus, setTraceStatus] = useState("Loading traces…");
  const [selectedTrace, setSelectedTrace] = useState<TraceSummary | null>(null);
  const [traceSpans, setTraceSpans] = useState<TraceRecord[]>([]);
  const [rtoTarget, setRtoTarget] = useState(() => {
    try {
      const stored = Number(localStorage.getItem(RTO_STORAGE_KEY));
      return Number.isFinite(stored) && stored > 0 ? stored : DEFAULT_RTO_MS;
    } catch {
      return DEFAULT_RTO_MS;
    }
  });
  const [now, setNow] = useState(Date.now());
  const [refreshing, setRefreshing] = useState(false);

  useEffect(() => {
    try {
      localStorage.setItem(RTO_STORAGE_KEY, String(rtoTarget));
    } catch {
      // Ignore storage failures.
    }
  }, [rtoTarget]);

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, []);

  const focusNode = useMemo(
    () => (topology?.nodes ?? []).find((node) => node.id === (selectedNodeId || asset.id || asset.name)),
    [topology, selectedNodeId, asset.id, asset.name]
  );

  const rto = useMemo(() => computeRto(events, now, rtoTarget), [events, now, rtoTarget]);

  const trafficModel = useMemo(
    () => buildTrafficModel(topology, selectedNodeId || asset.id || asset.name, nodeRps, edgeRps, alerts, explicitEdges),
    [topology, selectedNodeId, asset.id, asset.name, nodeRps, edgeRps, alerts, explicitEdges]
  );

  useEffect(() => {
    if (trafficModel.focusId && !trafficModel.nodes.some((node) => node.id === selectedNodeId)) {
      setSelectedNodeId(trafficModel.focusId);
    }
  }, [trafficModel, selectedNodeId]);

  const activeAlerts = useMemo(() => alerts.filter((alert) => alert.status !== "resolved"), [alerts]);
  const selectedNodeAlerts = useMemo(
    () => alertsMatchingNode(alerts, focusNode),
    [alerts, focusNode]
  );
  const focusStatus = focusNode ? topologyNodeStatus(focusNode) : "healthy";

  const loadTopology = async () => {
    setTopologyStatus("Loading topology…");
    try {
      const data = await api.getGuardianTopology(new URLSearchParams({ tenant, partition: asset.partition }));
      setTopology(data);
      const assets = (data.nodes || []).filter((node) => node.type === "asset").length;
      setTopologyStatus(assets > 0 ? `${assets} assets · ${(data.edges || []).length} connections` : "No asset topology for this partition");
    } catch {
      setTopology(null);
      setTopologyStatus("Failed to load topology");
    }
  };

  const loadTraffic = async () => {
    setTrafficStatus("Loading traffic…");
    const partition = escapePromQL(asset.partition);
    const nodeQueries = [
      `sum by (service) (rate(http_server_request_duration_count{guardian_partition="${partition}"}[5m]))`,
      `sum by (service) (rate(http_requests_total{guardian_partition="${partition}"}[5m]))`,
      `sum by (service) (rate(http_server_requests_seconds_count{guardian_partition="${partition}"}[5m]))`,
      `sum by (service) (rate(http_server_request_duration_count[5m]))`,
      `sum by (service) (rate(http_requests_total[5m]))`,
    ];
    const edgeQueries = [
      `sum by (source_service, destination_service) (rate(http_requests_total{guardian_partition="${partition}"}[5m]))`,
      `sum by (source_service, destination_service) (rate(http_server_request_duration_count{guardian_partition="${partition}"}[5m]))`,
      `sum by (source, destination) (rate(http_requests_total{guardian_partition="${partition}"}[5m]))`,
      `sum by (client, server) (rate(http_requests_total{guardian_partition="${partition}"}[5m]))`,
      `sum by (service, peer_service) (rate(http_requests_total{guardian_partition="${partition}"}[5m]))`,
      `sum by (service, upstream_service) (rate(http_requests_total{guardian_partition="${partition}"}[5m]))`,
      `sum by (caller, callee) (rate(http_requests_total{guardian_partition="${partition}"}[5m]))`,
    ];
    const [nodeItems, edgeItems] = await Promise.all([
      firstNonEmptyVector(nodeQueries, tenant),
      firstNonEmptyVector(edgeQueries, tenant),
    ]);
    const nodes = (topology?.nodes ?? []).filter((node) => node.type === "asset" || node.type === "intent");
    setNodeRps(nodeRpsFromVector(nodeItems, nodes));
    const edgeValues = edgeRpsFromVector(edgeItems, nodes);
    setEdgeRps(edgeValues);
    setExplicitEdges(edgeItems.length > 0 && edgeValues.size > 0);
    setTrafficStatus(
      nodeItems.length > 0
        ? `Traffic loaded from ${nodeItems.length} service series${edgeValues.size > 0 ? ` and ${edgeValues.size} measured edges` : " (edges inferred from source RPS)"}`
        : "No traffic metrics found; showing topology only"
    );
  };

  const loadAlerts = async () => {
    setAlertsStatus("Loading alerts…");
    try {
      const data = await api.getAlerts(new URLSearchParams({ tenant, partition: asset.partition }));
      setAlerts(data.alerts ?? []);
      setAlertsStatus(`${(data.alerts ?? []).filter((alert) => alert.status !== "resolved").length} active`);
    } catch {
      setAlerts([]);
      setAlertsStatus("Failed to load alerts");
    }
  };

  const loadTelemetry = async () => {
    setTelemetryStatus("Loading telemetry…");
    setMetricSource("none");
    const { from, to } = toIsoRange(rangeMinutes);
    const step = stepForMinutes(rangeMinutes);
    const tokens = [asset.name, asset.id, asset.intent, asset.partition].map((value) => value.trim()).filter(Boolean);
    const serviceRegex = tokens.length > 0 ? tokens.map(escapeRegex).join("|") : ".*";
    const p95Queries: SourceQuery[] = [
      { query: `histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_bucket{service=~"${serviceRegex}"}[5m])))`, source: `asset:${asset.name}` },
      { query: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`, source: `asset:${asset.name}` },
      { query: `histogram_quantile(0.95, sum by (le) (rate(request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`, source: `asset:${asset.name}` },
    ];
    const p99Queries: SourceQuery[] = p95Queries.map((query) => ({ ...query, query: query.query.replace("0.95", "0.99") }));
    const rpsQueries: SourceQuery[] = [
      { query: `sum(rate(http_server_request_duration_count{service=~"${serviceRegex}"}[5m]))`, source: `asset:${asset.name}` },
      { query: `sum(rate(http_requests_total{service=~"${serviceRegex}"}[5m]))`, source: `asset:${asset.name}` },
      { query: `sum(rate(http_server_requests_seconds_count{service=~"${serviceRegex}"}[5m]))`, source: `asset:${asset.name}` },
    ];
    const [p95, p99, rps] = await Promise.all([
      firstNonEmptyRangeQuery(p95Queries, tenant, from, to, step),
      firstNonEmptyRangeQuery(p99Queries, tenant, from, to, step),
      firstNonEmptyRangeQuery(rpsQueries, tenant, from, to, step),
    ]);
    const merged = mergeTelemetry(p95.points, p99.points, rps.points);
    setTelemetry(merged);
    setMetricSource(p95.source !== "none" ? p95.source : p99.source !== "none" ? p99.source : rps.source);
    setTelemetryStatus(merged.length > 0 ? `Live telemetry (${minutesLabel(rangeMinutes)})` : "No matching telemetry series for this window");
  };

  const loadTraces = async () => {
    setTraceStatus("Loading traces…");
    const { from, to } = toIsoRange(rangeMinutes);
    const params = new URLSearchParams({ tenant, from, to, limit: "40" });
    try {
      const services = await api.getLogServices(tenant);
      const tokens = [asset.name, asset.id, asset.intent];
      const matches = (services.services || [])
        .filter((service) => tokens.some((token) => service.toLowerCase().includes(token.toLowerCase())))
        .slice(0, 3);
      let results: TraceSummary[] = [];
      for (const service of matches) {
        const scoped = new URLSearchParams(params);
        scoped.set("service", service);
        const response = await api.getRecentTraces(scoped);
        if ((response.traces || []).length > 0) {
          results = response.traces || [];
          break;
        }
      }
      if (results.length === 0) {
        const response = await api.getRecentTraces(params);
        results = response.traces || [];
      }
      const top = results.slice(0, 10);
      setTraces(top);
      setSelectedTrace(top[0] ?? null);
      setTraceStatus(top.length > 0 ? `${top.length} traces (${minutesLabel(rangeMinutes)})` : "No traces in this window");
      if (top[0]) {
        const data = await api.getTrace(top[0].trace_id, tenant);
        setTraceSpans(data.spans || []);
      } else {
        setTraceSpans([]);
      }
    } catch {
      setTraces([]);
      setSelectedTrace(null);
      setTraceSpans([]);
      setTraceStatus("Failed to load traces");
    }
  };

  const refresh = async () => {
    setRefreshing(true);
    await Promise.all([loadTopology(), loadAlerts(), loadTelemetry(), loadTraces()]);
    setRefreshing(false);
  };

  useEffect(() => {
    void loadTopology();
    void loadAlerts();
    void loadTelemetry();
    void loadTraces();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [asset.partition, asset.id, asset.name, tenant, rangeMinutes]);

  useEffect(() => {
    if (topology) void loadTraffic();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [topology, tenant, asset.partition]);

  const openTrace = async (trace: TraceSummary) => {
    setSelectedTrace(trace);
    setTraceStatus(`Loading ${trace.trace_id.slice(0, 12)}…`);
    try {
      const data = await api.getTrace(trace.trace_id, tenant);
      setTraceSpans(data.spans || []);
      setTraceStatus(`Loaded ${trace.trace_id.slice(0, 12)}`);
    } catch {
      setTraceSpans([]);
      setTraceStatus("Failed to load trace");
    }
  };

  const openAlerts = (fingerprint?: string) => {
    const params = new URLSearchParams({ tenant, partition: asset.partition });
    if (fingerprint) params.set("alert", fingerprint);
    navigate(`/alerts?${params.toString()}`);
  };

  const trafficPaths = useMemo(
    () =>
      trafficModel.edges
        .filter((edge) => edge.relation !== "contains")
        .sort((a, b) => b.rps - a.rps)
        .slice(0, 12),
    [trafficModel.edges]
  );
  const counts = useMemo(() => {
    const healthy = trafficModel.nodes.filter((node) => node.status === "healthy").length;
    const degraded = trafficModel.nodes.filter((node) => node.status === "degraded").length;
    const critical = trafficModel.nodes.filter((node) => node.status === "critical").length;
    return { healthy, degraded, critical };
  }, [trafficModel.nodes]);
  const rtoState = rtoStatus(rto, rtoTarget);

  return (
    <div className="h-full min-h-0 w-full overflow-y-auto bg-slate-950 p-5 text-slate-100">
      <header className="mb-4 flex flex-wrap items-start justify-between gap-3 border-b border-slate-800 pb-4">
        <div className="flex items-start gap-3">
          <button type="button" onClick={onBack} className="btn mt-1 text-xs">
            <ArrowLeft size={14} /> Fleet
          </button>
          <div>
            <p className="text-[10px] font-bold uppercase tracking-[0.18em] text-[var(--strata-accent-text)]">Traffic & recovery</p>
            <h1 className="mt-1 text-xl font-semibold">
              {asset.partition} <span className="text-slate-600">/</span> {asset.intent} <span className="text-slate-600">/</span> {asset.name}
            </h1>
            <p className="mt-1 text-xs text-slate-500">
              {topologyStatus} · {trafficStatus}
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {[15, 60, 360, 1440].map((minutes) => (
            <button
              key={minutes}
              type="button"
              onClick={() => onRangeChange(minutes)}
              className={`rounded border px-2 py-1 text-[11px] ${
                rangeMinutes === minutes
                  ? "border-sky-500/50 bg-sky-500/15 text-sky-300"
                  : "border-slate-700 bg-slate-900 text-slate-400 hover:bg-slate-800"
              }`}
            >
              {minutesLabel(minutes)}
            </button>
          ))}
          <button type="button" onClick={() => void refresh()} className="btn text-xs" disabled={refreshing}>
            <RefreshCw size={13} className={refreshing ? "animate-spin" : ""} /> Refresh
          </button>
        </div>
      </header>

      <div className="mb-4 grid grid-cols-2 gap-2 md:grid-cols-4 xl:grid-cols-6">
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="text-[9px] uppercase tracking-wide text-slate-500">RTO</div>
          <div className={`mt-1 text-sm font-bold uppercase ${rtoState === "breached" ? "text-rose-300" : rtoState === "at-risk" ? "text-amber-300" : "text-emerald-300"}`}>
            {rtoState === "within" ? "within target" : rtoState === "at-risk" ? "at risk" : "breached"}
          </div>
          <div className="text-[10px] text-slate-500">target {formatDurationMs(rtoTarget)}</div>
        </div>
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="flex items-center gap-1 text-[9px] uppercase tracking-wide text-slate-500"><Clock3 size={10} /> current outage</div>
          <div className={`mt-1 text-sm font-bold ${rto.currentOutageMs > 0 ? "text-rose-300" : "text-slate-100"}`}>{formatDurationMs(rto.currentOutageMs)}</div>
          <div className="text-[10px] text-slate-500">{rto.open.length} open</div>
        </div>
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="flex items-center gap-1 text-[9px] uppercase tracking-wide text-slate-500"><Timer size={10} /> p95 recovery</div>
          <div className="mt-1 text-sm font-bold text-slate-100">{formatDurationMs(rto.p95)}</div>
          <div className="text-[10px] text-slate-500">{rto.completed.length} completed</div>
        </div>
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="flex items-center gap-1 text-[9px] uppercase tracking-wide text-slate-500"><AlertTriangle size={10} /> firing alerts</div>
          <div className={`mt-1 text-sm font-bold ${activeAlerts.length > 0 ? "text-rose-300" : "text-emerald-300"}`}>{activeAlerts.length}</div>
          <button type="button" onClick={() => openAlerts()} className="text-[10px] text-slate-500 hover:text-slate-300">open inbox →</button>
        </div>
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="text-[9px] uppercase tracking-wide text-slate-500">assets</div>
          <div className="mt-1 text-sm font-bold text-slate-100">{counts.healthy + counts.degraded + counts.critical}</div>
          <div className="text-[10px] text-slate-500">{counts.critical} critical · {counts.degraded} degraded</div>
        </div>
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="text-[9px] uppercase tracking-wide text-slate-500">traffic paths</div>
          <div className="mt-1 text-sm font-bold text-sky-300">{trafficPaths.length}</div>
          <div className="text-[10px] text-slate-500">{explicitEdges ? "measured edges" : "inferred from nodes"}</div>
        </div>
      </div>

      {rto.open.length > 0 && (
        <div className="mb-4 flex flex-wrap items-center gap-3 rounded-lg border border-rose-500/30 bg-rose-500/10 px-4 py-2.5 text-xs text-rose-300">
          <AlertTriangle size={14} />
          <span className="font-medium">{rto.open.length} active incident{rto.open.length === 1 ? "" : "s"}</span>
          <span className="text-rose-200/80">
            longest open: {rto.open.map((incident) => incident.asset || incident.intent || incident.key).join(", ")}
          </span>
          <button type="button" onClick={() => openAlerts()} className="ml-auto rounded border border-rose-400/40 px-2 py-0.5 text-[11px] hover:bg-rose-500/15">
            Open alerts
          </button>
        </div>
      )}

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
        <div className="space-y-4">
          <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
            <div className="flex flex-wrap items-start justify-between gap-2">
              <div>
                <h2 className="flex items-center gap-2 text-sm font-semibold">
                  <Waypoints size={15} className="text-sky-400" /> Traffic between intents and assets
                </h2>
                <p className="mt-1 text-[11px] text-slate-500">
                  Click a node to inspect it. Edge width follows request rate; dashed edges are intent membership.
                </p>
              </div>
              <span className="rounded border border-slate-700 px-2 py-0.5 text-[10px] text-slate-400">{trafficStatus}</span>
            </div>
            <div className="mt-3">
              <TrafficGraph
                model={trafficModel}
                selectedId={selectedNodeId}
                onSelect={setSelectedNodeId}
                onOpenAlerts={() => openAlerts()}
              />
            </div>
          </section>

          <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
            <div className="flex items-center justify-between gap-2">
              <div>
                <h2 className="flex items-center gap-2 text-sm font-semibold"><Activity size={15} className="text-sky-400" /> Traffic paths</h2>
                <p className="mt-1 text-[11px] text-slate-500">Top dependency edges by request rate in the last five minutes.</p>
              </div>
            </div>
            <div className="mt-3 overflow-x-auto">
              <table className="w-full text-left text-xs">
                <thead className="text-[10px] uppercase tracking-wide text-slate-500">
                  <tr><th className="px-2 py-1.5">From</th><th className="px-2 py-1.5">To</th><th className="px-2 py-1.5">Relation</th><th className="px-2 py-1.5 text-right">RPS</th><th className="px-2 py-1.5">Source</th></tr>
                </thead>
                <tbody className="divide-y divide-slate-800">
                  {trafficPaths.map((edge) => {
                    const from = trafficModel.nodes.find((node) => node.id === edge.from);
                    const to = trafficModel.nodes.find((node) => node.id === edge.to);
                    const fromAlerted = selectedNodeAlerts.length > 0 && from?.id === focusNode?.id;
                    return (
                      <tr key={`${edge.from}-${edge.to}`} className="hover:bg-slate-800/40">
                        <td className="px-2 py-1.5">
                          <button type="button" onClick={() => setSelectedNodeId(edge.from)} className={`inline-flex items-center gap-1.5 ${fromAlerted ? "text-rose-300" : "text-slate-200"}`}>
                            {fromAlerted && <AlertTriangle size={11} />} {from?.name ?? edge.from}
                          </button>
                        </td>
                        <td className="px-2 py-1.5">
                          <button type="button" onClick={() => setSelectedNodeId(edge.to)} className="text-slate-300 hover:text-slate-100">{to?.name ?? edge.to}</button>
                        </td>
                        <td className="px-2 py-1.5 text-slate-500">{edge.relation || "depends_on"}</td>
                        <td className="px-2 py-1.5 text-right font-mono text-sky-300">{edge.rps.toFixed(2)}</td>
                        <td className="px-2 py-1.5 text-[10px] text-slate-500">{edge.inferred ? "inferred" : "measured"}</td>
                      </tr>
                    );
                  })}
                  {trafficPaths.length === 0 && (
                    <tr><td colSpan={5} className="px-2 py-6 text-center text-slate-500">No dependency edges were reported for this partition.</td></tr>
                  )}
                </tbody>
              </table>
            </div>
          </section>

          <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
            <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
              <div className="flex items-center justify-between gap-2">
                <h2 className="text-sm font-semibold">Latency (p95 / p99)</h2>
                <span className="text-[10px] text-slate-500">{telemetryStatus}</span>
              </div>
              <div className="mt-3 h-44">
                {telemetry.length === 0 ? (
                  <div className="flex h-full items-center justify-center text-xs text-slate-500">No latency samples for this window</div>
                ) : (
                  <MiniLineChart
                    data={telemetry}
                    series={[{ key: "p95", label: "p95", color: "var(--chart-1)" }, { key: "p99", label: "p99", color: "var(--chart-5)" }]}
                    ariaLabel="Latency p95 and p99"
                  />
                )}
              </div>
              <div className="mt-1 text-[10px] text-slate-600">source: {metricSource}</div>
            </section>
            <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
              <div className="flex items-center justify-between gap-2">
                <h2 className="text-sm font-semibold">Throughput (RPS)</h2>
                <span className="text-[10px] text-slate-500">{minutesLabel(rangeMinutes)}</span>
              </div>
              <div className="mt-3 h-44">
                {telemetry.length === 0 ? (
                  <div className="flex h-full items-center justify-center text-xs text-slate-500">No throughput samples for this window</div>
                ) : (
                  <MiniLineChart
                    data={telemetry}
                    series={[{ key: "rps", label: "rps", color: "var(--chart-2)" }]}
                    ariaLabel="Throughput in requests per second"
                  />
                )}
              </div>
            </section>
          </div>

          <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
            <div className="flex items-center justify-between gap-2">
              <h2 className="text-sm font-semibold">Recent traces</h2>
              <span className="text-[10px] text-slate-500">{traceStatus}</span>
            </div>
            <div className="mt-3 grid grid-cols-1 gap-3 lg:grid-cols-[220px_minmax(0,1fr)]">
              <div className="max-h-64 space-y-1.5 overflow-y-auto pr-1">
                {traces.length === 0 ? (
                  <p className="text-xs text-slate-500">No traces in this window.</p>
                ) : (
                  traces.map((trace) => (
                    <button
                      key={trace.trace_id}
                      type="button"
                      onClick={() => void openTrace(trace)}
                      className={`w-full rounded border px-2 py-1.5 text-left text-[11px] ${
                        selectedTrace?.trace_id === trace.trace_id
                          ? "border-sky-400/60 bg-sky-500/15 text-sky-200"
                          : "border-slate-700 bg-slate-950 text-slate-300 hover:bg-slate-900"
                      }`}
                    >
                      <div className="truncate">{trace.service || "unknown-service"}</div>
                      <div className="truncate font-mono text-[9px] text-slate-500">{trace.trace_id}</div>
                    </button>
                  ))
                )}
              </div>
              <div className="max-h-64 overflow-y-auto rounded border border-slate-800 bg-slate-950/70 p-2">
                {traceSpans.length === 0 ? (
                  <p className="text-xs text-slate-500">Select a trace to render its waterfall.</p>
                ) : (
                  <TraceWaterfall spans={traceSpans} />
                )}
              </div>
            </div>
          </section>
        </div>

        <div className="space-y-4">
          <RtoPanel summary={rto} targetMs={rtoTarget} onTargetChange={setRtoTarget} onOpenAlerts={() => openAlerts()} />

          <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
            <div className="flex items-center justify-between gap-2">
              <h2 className="flex items-center gap-2 text-sm font-semibold"><AlertTriangle size={15} className="text-rose-400" /> Firing alerts</h2>
              <span className="text-[10px] text-slate-500">{alertsStatus}</span>
            </div>
            <div className="mt-2 max-h-64 space-y-1.5 overflow-y-auto pr-1">
              {activeAlerts.slice(0, 12).map((alert) => (
                <button
                  key={alert.fingerprint}
                  type="button"
                  onClick={() => openAlerts(alert.fingerprint)}
                  className="flex w-full items-start gap-2 rounded border border-slate-800 bg-slate-950/70 px-2 py-1.5 text-left hover:border-rose-500/40"
                >
                  <span className={`mt-0.5 rounded border px-1 text-[9px] uppercase ${alertSeverityClass(alert.severity)}`}>{alert.severity}</span>
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-[11px] text-slate-200">{alert.labels.alertname || alert.fingerprint.slice(0, 10)}</span>
                    <span className="block truncate text-[10px] text-slate-500">{alert.annotations?.summary || alert.status}</span>
                  </span>
                  <ExternalLink size={11} className="mt-0.5 shrink-0 text-slate-500" />
                </button>
              ))}
              {activeAlerts.length === 0 && (
                <p className="flex items-center gap-2 text-xs text-emerald-300"><CheckCircle2 size={13} /> No firing alerts for this partition.</p>
              )}
            </div>
            <button type="button" onClick={() => openAlerts()} className="mt-2 w-full rounded border border-slate-700 px-2 py-1 text-[11px] text-slate-400 hover:bg-slate-800">
              Open alert inbox
            </button>
          </section>

          <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
            <div className="flex items-center justify-between gap-2">
              <h2 className="flex items-center gap-2 text-sm font-semibold"><Server size={15} className="text-sky-400" /> Selected node</h2>
              {focusNode && (
                <span className={`rounded border px-1.5 py-0.5 text-[9px] uppercase ${statusClass(focusStatus)}`}>
                  {focusStatus}
                </span>
              )}
            </div>
            {focusNode ? (
              <div className="mt-2 space-y-1 text-[11px]">
                <div className="text-sm font-semibold text-slate-100">{focusNode.name}</div>
                <div className="truncate text-slate-500">{focusNode.id}</div>
                <div className="text-slate-500">intent: {focusNode.intent || "unknown"}</div>
                <div className="text-slate-500">rps: {(nodeRps.get(focusNode.id) ?? 0).toFixed(2)}</div>
                <div className={selectedNodeAlerts.length > 0 ? "text-rose-300" : "text-emerald-300"}>
                  {selectedNodeAlerts.length > 0 ? `${selectedNodeAlerts.length} firing alert(s)` : "no firing alerts"}
                </div>
                {selectedNodeAlerts.length > 0 && (
                  <button type="button" onClick={() => openAlerts(selectedNodeAlerts[0]?.fingerprint)} className="mt-1 rounded border border-rose-500/40 px-2 py-0.5 text-[10px] text-rose-300 hover:bg-rose-500/10">
                    Inspect alert
                  </button>
                )}
              </div>
            ) : (
              <p className="mt-2 text-xs text-slate-500">Select a node in the traffic map.</p>
            )}
          </section>

          <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
            <h2 className="text-sm font-semibold">Recent Guardian events</h2>
            <div className="mt-2 max-h-56 space-y-1.5 overflow-y-auto pr-1">
              {events.slice(0, 10).map((event, index) => (
                <div key={`${event.timestamp || "na"}-${index}`} className="rounded border border-slate-800 bg-slate-950/70 px-2 py-1.5">
                  <div className="flex items-center gap-2 text-[10px]">
                    <span className="truncate font-mono text-slate-400">{event.kind || event.status || "event"}</span>
                    <span className="ml-auto shrink-0 text-slate-600">{event.timestamp ? new Date(event.timestamp).toLocaleTimeString() : ""}</span>
                  </div>
                  <div className="truncate text-[10px] text-slate-500">{event.asset || event.intent || "component"} · {event.message || event.status || "—"}</div>
                </div>
              ))}
              {events.length === 0 && <p className="text-xs text-slate-500">No events for this window.</p>}
            </div>
          </section>

          {health?.overall_health?.level && (
            <div className="rounded-xl border border-slate-800 bg-slate-900/60 p-3 text-[11px] text-slate-500">
              Guardian health: <span className="font-semibold text-slate-300">{health.overall_health.level}</span>
              {health.overall_health.reason ? ` · ${health.overall_health.reason}` : ""}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
