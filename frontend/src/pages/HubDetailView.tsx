import { useEffect, useMemo, useState } from "react";
import { ArrowLeft, Server, Activity, Database } from "lucide-react";
import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  Tooltip,
  ResponsiveContainer,
} from "recharts";
import type { AssetGroup, HealthStatus } from "./FleetView";
import { api } from "../api/client";
import TraceWaterfall from "../components/charts/TraceWaterfall";
import type {
  GuardianEventRecord,
  GuardianHealthResponse,
  GuardianTopologyNode,
  GuardianTopologyResponse,
  PrometheusMatrixResult,
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

interface HubMetricPoint {
  time: string;
  p95: number;
  p99: number;
  rps: number;
}

interface SourceQuery {
  query: string;
  source: string;
}

interface TopologyVisualNode {
  id: string;
  name: string;
  status: HealthStatus;
  intent: string;
  degree: number;
  x: number;
  y: number;
  raw?: GuardianTopologyNode;
}

interface TopologyVisualEdge {
  from: string;
  to: string;
  relation?: string;
}

function minutesLabel(minutes: number): string {
  if (minutes < 60) return `${minutes}m`;
  if (minutes < 60 * 24) return `${minutes / 60}h`;
  return `${minutes / (60 * 24)}d`;
}

function escapeRegex(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
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
): HubMetricPoint[] {
  const byTs = new Map<number, HubMetricPoint>();

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

async function runRangeQuery(query: string, tenant: string, from: string, to: string, step: string) {
  const params = new URLSearchParams({ query, start: from, end: to, step, tenant });
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
      // Try next fallback query.
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
  if (source === "none") return "source: no-data";
  return `source: ${source}`;
}

function nodeStatus(node: GuardianTopologyNode): HealthStatus {
  const health = (node.health?.level || "").toLowerCase();
  const status = (node.status || "").toLowerCase();
  const flow = (node.flow_state || "").toLowerCase();

  if ([health, status, flow].some((v) => v.includes("critical") || v.includes("failed") || v.includes("error"))) {
    return "critical";
  }
  if ([health, status, flow].some((v) => v.includes("degraded") || v.includes("attention") || v.includes("drift"))) {
    return "degraded";
  }
  return "healthy";
}

function statusGlow(status: HealthStatus): string {
  if (status === "healthy") return "#34d399";
  if (status === "degraded") return "#f59e0b";
  return "#f43f5e";
}

function buildTopologyGraph(
  topology: GuardianTopologyResponse | null,
  focusHint: string,
  search: string
): { nodes: TopologyVisualNode[]; edges: TopologyVisualEdge[]; focusId: string | null } {
  const nodes = (topology?.nodes || []).filter((n) => n.type === "asset");
  if (nodes.length === 0) return { nodes: [], edges: [], focusId: null };

  const allEdges = (topology?.edges || []).filter((e) => {
    const from = nodes.some((n) => n.id === e.from);
    const to = nodes.some((n) => n.id === e.to);
    return from && to;
  });

  const normalizedSearch = search.trim().toLowerCase();
  let visibleNodes = nodes;
  let visibleEdges = allEdges;

  if (normalizedSearch) {
    const hitIds = new Set(
      nodes
        .filter((n) => {
          const text = [n.name, n.id, n.intent || "", n.partition || ""].join(" ").toLowerCase();
          return text.includes(normalizedSearch);
        })
        .map((n) => n.id)
    );

    if (hitIds.size > 0) {
      for (const edge of allEdges) {
        if (hitIds.has(edge.from) || hitIds.has(edge.to)) {
          hitIds.add(edge.from);
          hitIds.add(edge.to);
        }
      }
      visibleNodes = nodes.filter((n) => hitIds.has(n.id));
      visibleEdges = allEdges.filter((e) => hitIds.has(e.from) && hitIds.has(e.to));
    }
  }

  const nodeById = new Map(visibleNodes.map((n) => [n.id, n]));
  const degree = new Map<string, number>(visibleNodes.map((n) => [n.id, 0]));
  const adjacency = new Map<string, Set<string>>(visibleNodes.map((n) => [n.id, new Set()]));

  for (const edge of visibleEdges) {
    if (!nodeById.has(edge.from) || !nodeById.has(edge.to)) continue;
    degree.set(edge.from, (degree.get(edge.from) || 0) + 1);
    degree.set(edge.to, (degree.get(edge.to) || 0) + 1);
    adjacency.get(edge.from)?.add(edge.to);
    adjacency.get(edge.to)?.add(edge.from);
  }

  const fallbackFocus = [...degree.entries()].sort((a, b) => b[1] - a[1])[0]?.[0] || visibleNodes[0]?.id || null;
  const focusId = nodeById.has(focusHint) ? focusHint : fallbackFocus;
  if (!focusId) return { nodes: [], edges: [], focusId: null };

  const levels = new Map<string, number>();
  levels.set(focusId, 0);
  const queue = [focusId];

  while (queue.length > 0) {
    const current = queue.shift();
    if (!current) continue;
    const currentLevel = levels.get(current) || 0;
    const nextNodes = adjacency.get(current) || new Set();
    for (const next of nextNodes) {
      if (!levels.has(next)) {
        levels.set(next, currentLevel + 1);
        queue.push(next);
      }
    }
  }

  const maxKnown = Math.max(...levels.values());
  for (const node of visibleNodes) {
    if (!levels.has(node.id)) levels.set(node.id, maxKnown + 1);
  }

  const grouped = new Map<number, string[]>();
  for (const [id, lvl] of levels.entries()) {
    if (!grouped.has(lvl)) grouped.set(lvl, []);
    grouped.get(lvl)?.push(id);
  }

  const placed = new Map<string, { x: number; y: number }>();
  placed.set(focusId, { x: 50, y: 50 });

  const ringLevels = [...grouped.keys()].filter((lvl) => lvl > 0).sort((a, b) => a - b);
  for (const lvl of ringLevels) {
    const ids = grouped.get(lvl) || [];
    const radius = Math.min(42, 12 + (lvl - 1) * 12);
    const angleStep = (Math.PI * 2) / Math.max(ids.length, 1);
    const offset = (lvl * Math.PI) / 9;
    ids.forEach((id, idx) => {
      const angle = offset + idx * angleStep;
      const x = 50 + radius * Math.cos(angle);
      const y = 50 + radius * Math.sin(angle);
      placed.set(id, { x, y });
    });
  }

  const visualNodes: TopologyVisualNode[] = visibleNodes
    .map((n) => {
      const pos = placed.get(n.id) || { x: 50, y: 50 };
      return {
        id: n.id,
        name: n.name,
        status: nodeStatus(n),
        intent: n.intent || "unknown",
        degree: degree.get(n.id) || 0,
        x: pos.x,
        y: pos.y,
        raw: n,
      };
    })
    .sort((a, b) => b.degree - a.degree);

  return {
    nodes: visualNodes,
    edges: visibleEdges.map((e) => ({ from: e.from, to: e.to, relation: e.relation })),
    focusId,
  };
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
  const [metricData, setMetricData] = useState<HubMetricPoint[]>([]);
  const [metricStatus, setMetricStatus] = useState("Loading metrics...");
  const [metricSource, setMetricSource] = useState("none");
  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [selectedTrace, setSelectedTrace] = useState<TraceSummary | null>(null);
  const [traceSpans, setTraceSpans] = useState<TraceRecord[]>([]);
  const [traceStatus, setTraceStatus] = useState("Loading traces...");
  const [traceSource, setTraceSource] = useState("none");
  const [topology, setTopology] = useState<GuardianTopologyResponse | null>(null);
  const [topologyStatus, setTopologyStatus] = useState("Loading topology...");
  const [focusNodeId, setFocusNodeId] = useState("");
  const [topologySearch, setTopologySearch] = useState("");
  const [topologyZoom, setTopologyZoom] = useState(1);

  const tableRows = events.slice(0, 8);

  const topologyGraph = useMemo(
    () => buildTopologyGraph(topology, focusNodeId || asset.id || asset.name, topologySearch),
    [topology, focusNodeId, asset.id, asset.name, topologySearch]
  );

  useEffect(() => {
    if (topologyGraph.focusId && topologyGraph.focusId !== focusNodeId) {
      setFocusNodeId(topologyGraph.focusId);
    }
  }, [topologyGraph.focusId, focusNodeId]);

  const counts = useMemo(() => {
    if (topologyGraph.nodes.length > 0) {
      const healthy = topologyGraph.nodes.filter((n) => n.status === "healthy").length;
      const degraded = topologyGraph.nodes.filter((n) => n.status === "degraded").length;
      const critical = topologyGraph.nodes.filter((n) => n.status === "critical").length;
      return { healthy, degraded, critical };
    }

    const healthy = asset.nodes.filter((n) => n === "healthy").length;
    const degraded = asset.nodes.filter((n) => n === "degraded").length;
    const critical = asset.nodes.filter((n) => n === "critical").length;
    return { healthy, degraded, critical };
  }, [topologyGraph.nodes, asset.nodes]);

  const focusedNode = useMemo(
    () => topologyGraph.nodes.find((n) => n.id === focusNodeId) || null,
    [topologyGraph.nodes, focusNodeId]
  );

  useEffect(() => {
    let cancelled = false;

    async function loadTopology() {
      setTopologyStatus("Loading topology...");
      try {
        const params = new URLSearchParams({
          tenant,
          partition: asset.partition,
        });
        const data = await api.getGuardianTopology(params);
        if (cancelled) return;
        setTopology(data);
        const assetCount = (data.nodes || []).filter((n) => n.type === "asset").length;
        setTopologyStatus(assetCount > 0 ? `Live topology: ${assetCount} assets` : "No asset topology nodes found");
      } catch {
        if (!cancelled) {
          setTopology(null);
          setTopologyStatus("Failed to load topology");
        }
      }
    }

    void loadTopology();
    return () => {
      cancelled = true;
    };
  }, [asset.partition, tenant]);

  useEffect(() => {
    let cancelled = false;

    async function loadMetricsAndTraces() {
      setMetricStatus("Loading metrics...");
      setMetricSource("none");
      setTraceStatus("Loading traces...");
      setTraceSource("none");
      setSelectedTrace(null);
      setTraceSpans([]);

      const { from, to } = toIsoRange(rangeMinutes);
      const step = stepForMinutes(rangeMinutes);

      const tokenSet = [asset.name, asset.id, asset.intent, asset.partition]
        .map((value) => value.trim())
        .filter(Boolean);
      const serviceRegex = tokenSet.length > 0 ? tokenSet.map(escapeRegex).join("|") : ".*";

      const p95Queries: SourceQuery[] = [
        {
          query: `histogram_quantile(0.95, sum by (le) (rate(request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`,
          source: `asset:${asset.name}`,
        },
        {
          query: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`,
          source: `asset:${asset.name}`,
        },
        {
          query: `histogram_quantile(0.95, sum by (le) (rate(request_duration_seconds_bucket[5m])))`,
          source: `fleet:latency`,
        },
        {
          query: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
          source: `fleet:latency`,
        },
      ];

      const p99Queries: SourceQuery[] = [
        {
          query: `histogram_quantile(0.99, sum by (le) (rate(request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`,
          source: `asset:${asset.name}`,
        },
        {
          query: `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{service=~"${serviceRegex}"}[5m])))`,
          source: `asset:${asset.name}`,
        },
        {
          query: `histogram_quantile(0.99, sum by (le) (rate(request_duration_seconds_bucket[5m])))`,
          source: `fleet:latency`,
        },
        {
          query: `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
          source: `fleet:latency`,
        },
      ];

      const rpsQueries: SourceQuery[] = [
        {
          query: `sum(rate(http_requests_total{service=~"${serviceRegex}"}[5m]))`,
          source: `asset:${asset.name}`,
        },
        {
          query: `sum(rate(http_server_requests_seconds_count{service=~"${serviceRegex}"}[5m]))`,
          source: `asset:${asset.name}`,
        },
        {
          query: `sum(rate(http_requests_total[5m]))`,
          source: "fleet:rps",
        },
        {
          query: `sum(rate(http_server_requests_seconds_count[5m]))`,
          source: "fleet:rps",
        },
      ];

      const [p95, p99, rps] = await Promise.all([
        firstNonEmptyRangeQuery(p95Queries, tenant, from, to, step),
        firstNonEmptyRangeQuery(p99Queries, tenant, from, to, step),
        firstNonEmptyRangeQuery(rpsQueries, tenant, from, to, step),
      ]);

      const merged = mergeMetricPoints(p95.points, p99.points, rps.points);
      if (!cancelled) {
        setMetricData(merged);
        const source = p95.source !== "none" ? p95.source : p99.source !== "none" ? p99.source : rps.source;
        setMetricSource(source);
        setMetricStatus(
          merged.length > 0
            ? `Live metrics loaded (${minutesLabel(rangeMinutes)})`
            : "No matching time-series found for this window"
        );
      }

      const traceParams = new URLSearchParams({
        tenant,
        from,
        to,
        limit: "60",
      });

      try {
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
            setTraceSource(`asset:${serviceName}`);
            break;
          }
        }

        if (traceResults.length === 0) {
          const fallback = await api.getRecentTraces(traceParams);
          traceResults = fallback.traces || [];
          setTraceSource("fleet:recent-traces");
        }

        if (cancelled) return;

        const top = traceResults.slice(0, 10);
        setTraces(top);
        if (top.length === 0) {
          setTraceStatus("No traces in selected time window");
          return;
        }

        setTraceStatus(`Loaded ${top.length} traces (${minutesLabel(rangeMinutes)})`);
        const firstTrace = top[0];
        setSelectedTrace(firstTrace);
        const traceData = await api.getTrace(firstTrace.trace_id, tenant);
        if (!cancelled) {
          setTraceSpans(traceData.spans || []);
        }
      } catch {
        if (!cancelled) {
          setTraces([]);
          setTraceSpans([]);
          setTraceStatus("Failed to load traces");
          setTraceSource("none");
        }
      }
    }

    void loadMetricsAndTraces();
    return () => {
      cancelled = true;
    };
  }, [asset, tenant, rangeMinutes]);

  const openTrace = async (trace: TraceSummary) => {
    setSelectedTrace(trace);
    setTraceStatus(`Loading trace ${trace.trace_id.slice(0, 12)}...`);
    try {
      const traceData = await api.getTrace(trace.trace_id, tenant);
      setTraceSpans(traceData.spans || []);
      setTraceStatus(`Loaded trace ${trace.trace_id.slice(0, 12)}`);
    } catch {
      setTraceSpans([]);
      setTraceStatus("Failed to load selected trace");
    }
  };

  return (
    <div className="w-full min-h-screen bg-slate-950 text-slate-100 p-6 font-sans overflow-auto">
      <div className="flex items-center justify-between mb-6 pb-4 border-b border-slate-800">
        <div className="flex items-center gap-4">
          <button
            onClick={onBack}
            className="flex items-center gap-2 px-3 py-1.5 bg-slate-900 border border-slate-700 hover:bg-slate-800 rounded text-xs font-semibold transition"
          >
            <ArrowLeft className="w-4 h-4" /> Back to Fleet
          </button>
          <div>
            <h1 className="text-xl font-bold text-slate-100">
              {asset.partition} / {asset.intent} / {asset.name}
            </h1>
            <p className="text-xs text-slate-400">Hub Instance Group Details & Real-Time Telemetry</p>
          </div>
        </div>

        <div className="flex items-center gap-3 text-xs font-mono">
          {[15, 60, 360, 1440].map((minutes) => (
            <button
              key={minutes}
              onClick={() => onRangeChange(minutes)}
              className={`px-2 py-1 rounded border ${
                rangeMinutes === minutes
                  ? "bg-sky-500/20 border-sky-400/60 text-sky-300"
                  : "bg-slate-900 border-slate-700 text-slate-300 hover:bg-slate-800"
              }`}
            >
              {minutesLabel(minutes)}
            </button>
          ))}
          <span className="px-2.5 py-1 bg-emerald-500/10 text-emerald-400 border border-emerald-500/30 rounded">
            Healthy: {counts.healthy}
          </span>
          <span className="px-2.5 py-1 bg-amber-500/10 text-amber-400 border border-amber-500/30 rounded">
            Degraded: {counts.degraded}
          </span>
          <span className="px-2.5 py-1 bg-rose-500/10 text-rose-400 border border-rose-500/30 rounded">
            Critical: {counts.critical}
          </span>
        </div>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-12 gap-6">
        <div className="lg:col-span-6 bg-slate-900 border border-slate-800 rounded-xl p-4">
          <div className="flex items-start justify-between gap-3 mb-3">
            <div>
              <h2 className="text-sm font-semibold flex items-center gap-2">
                <Server className="w-4 h-4 text-sky-400" /> Asset Topology Canvas
              </h2>
              <p className="text-[11px] text-slate-400 mt-1">{topologyStatus}</p>
            </div>
            <div className="flex items-center gap-2">
              <button
                onClick={() => setTopologyZoom((z) => Math.max(0.8, Number((z - 0.1).toFixed(2))))}
                className="px-2 py-1 text-xs rounded border border-slate-700 bg-slate-900 hover:bg-slate-800"
              >
                -
              </button>
              <button
                onClick={() => setTopologyZoom((z) => Math.min(1.8, Number((z + 0.1).toFixed(2))))}
                className="px-2 py-1 text-xs rounded border border-slate-700 bg-slate-900 hover:bg-slate-800"
              >
                +
              </button>
              <button
                onClick={() => {
                  setTopologyZoom(1);
                  setTopologySearch("");
                }}
                className="px-2 py-1 text-xs rounded border border-slate-700 bg-slate-900 hover:bg-slate-800"
              >
                reset
              </button>
            </div>
          </div>

          <div className="mb-3 flex items-center gap-2">
            <input
              type="text"
              value={topologySearch}
              onChange={(e) => setTopologySearch(e.target.value)}
              placeholder="Filter assets by name, id, intent"
              className="w-full rounded-lg border border-slate-700 bg-slate-950 px-3 py-1.5 text-xs text-slate-200 placeholder:text-slate-500 focus:outline-none focus:ring-1 focus:ring-sky-400"
            />
            <span className="text-[10px] px-2 py-1 rounded border border-slate-700 bg-slate-950 text-slate-400 whitespace-nowrap">
              zoom {topologyZoom.toFixed(1)}x
            </span>
          </div>

          <div className="relative h-[440px] rounded-xl overflow-hidden border border-slate-800 bg-gradient-to-br from-slate-950 via-[#0b1323] to-[#140d1f]">
            <div className="absolute inset-0 opacity-40" style={{
              backgroundImage:
                "radial-gradient(circle at 20% 20%, rgba(56,189,248,0.18), transparent 30%), radial-gradient(circle at 80% 75%, rgba(244,63,94,0.14), transparent 28%), linear-gradient(rgba(148,163,184,0.06) 1px, transparent 1px), linear-gradient(90deg, rgba(148,163,184,0.06) 1px, transparent 1px)",
              backgroundSize: "auto, auto, 22px 22px, 22px 22px",
            }} />
            {topologyGraph.nodes.length === 0 ? (
              <div className="relative z-10 h-full flex items-center justify-center text-sm text-slate-400">
                No asset connections to display for this partition.
              </div>
            ) : (
              <svg viewBox="0 0 100 100" className="relative z-10 w-full h-full">
                <defs>
                  <filter id="node-glow" x="-50%" y="-50%" width="200%" height="200%">
                    <feGaussianBlur stdDeviation="1.4" result="blur" />
                    <feMerge>
                      <feMergeNode in="blur" />
                      <feMergeNode in="SourceGraphic" />
                    </feMerge>
                  </filter>
                </defs>
                <g transform={`translate(${50 - 50 * topologyZoom} ${50 - 50 * topologyZoom}) scale(${topologyZoom})`}>
                  {topologyGraph.edges.map((edge, index) => {
                    const from = topologyGraph.nodes.find((n) => n.id === edge.from);
                    const to = topologyGraph.nodes.find((n) => n.id === edge.to);
                    if (!from || !to) return null;
                    const mx = (from.x + to.x) / 2;
                    const my = (from.y + to.y) / 2;
                    const curve = 4;
                    return (
                      <path
                        key={`${edge.from}-${edge.to}-${index}`}
                        d={`M ${from.x} ${from.y} Q ${mx} ${my - curve} ${to.x} ${to.y}`}
                        stroke="rgba(125,211,252,0.35)"
                        strokeWidth="0.35"
                        fill="none"
                      >
                        <title>{edge.relation || "connected"}</title>
                      </path>
                    );
                  })}

                  {topologyGraph.nodes.map((node) => {
                    const focused = node.id === topologyGraph.focusId;
                    const color = statusGlow(node.status);
                    const radius = focused ? 2.6 : 1.8 + Math.min(1.4, node.degree * 0.15);
                    return (
                      <g key={node.id} onClick={() => setFocusNodeId(node.id)} className="cursor-pointer">
                        <circle cx={node.x} cy={node.y} r={radius + 0.8} fill={color} opacity={0.16} filter="url(#node-glow)" />
                        <circle
                          cx={node.x}
                          cy={node.y}
                          r={radius}
                          fill={color}
                          stroke={focused ? "#e2e8f0" : "rgba(226,232,240,0.28)"}
                          strokeWidth={focused ? 0.45 : 0.2}
                        >
                          <title>{`${node.name} (${node.status})`}</title>
                        </circle>
                        {(focused || node.degree > 2) && (
                          <text
                            x={node.x + 1.9}
                            y={node.y - 1.2}
                            fontSize="2.1"
                            fill="rgba(226,232,240,0.92)"
                            style={{ paintOrder: "stroke", stroke: "rgba(2,6,23,0.9)", strokeWidth: 0.7 }}
                          >
                            {node.name}
                          </text>
                        )}
                      </g>
                    );
                  })}
                </g>
              </svg>
            )}
          </div>

          <div className="mt-3 grid grid-cols-1 md:grid-cols-2 gap-3 text-xs">
            <div className="rounded-lg border border-slate-800 bg-slate-950/70 p-3">
              <div className="text-slate-400 mb-1">Focused asset</div>
              <div className="font-semibold text-slate-200 truncate">{focusedNode?.name || "-"}</div>
              <div className="text-slate-500 mt-1 truncate">{focusedNode?.id || ""}</div>
            </div>
            <div className="rounded-lg border border-slate-800 bg-slate-950/70 p-3">
              <div className="text-slate-400 mb-1">Legend</div>
              <div className="flex items-center gap-3">
                <span className="inline-flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-emerald-400" /> healthy</span>
                <span className="inline-flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-amber-400" /> degraded</span>
                <span className="inline-flex items-center gap-1"><span className="w-2 h-2 rounded-full bg-rose-400" /> critical</span>
              </div>
            </div>
          </div>
        </div>

        <div className="lg:col-span-6 space-y-6">
          <div className="bg-slate-900 border border-slate-800 rounded-xl p-4">
            <h2 className="text-sm font-semibold mb-4 flex items-center gap-2">
              <Activity className="w-4 h-4 text-sky-400" /> Latency (p95 & p99 ms)
            </h2>
            <div className="flex items-center justify-between gap-3 mb-2">
              <div className="text-[11px] text-slate-400">{metricStatus}</div>
              <span className={`text-[10px] px-2 py-0.5 rounded border ${sourceBadgeClass(metricSource)}`}>
                {sourceBadgeLabel(metricSource)}
              </span>
            </div>
            <div className="h-48">
              {metricData.length === 0 ? (
                <div className="h-full flex items-center justify-center text-xs text-slate-500">
                  No latency metric samples for current filter window
                </div>
              ) : (
                <ResponsiveContainer width="100%" height="100%">
                  <LineChart data={metricData}>
                    <XAxis dataKey="time" stroke="#64748b" fontSize={11} />
                    <YAxis stroke="#64748b" fontSize={11} />
                    <Tooltip
                      contentStyle={{ backgroundColor: "#0f172a", borderColor: "#334155", color: "#f8fafc" }}
                    />
                    <Line type="monotone" dataKey="p95" stroke="#38bdf8" strokeWidth={2} dot={false} />
                    <Line type="monotone" dataKey="p99" stroke="#f43f5e" strokeWidth={2} dot={false} />
                  </LineChart>
                </ResponsiveContainer>
              )}
            </div>
          </div>

          <div className="bg-slate-900 border border-slate-800 rounded-xl p-4">
            <h2 className="text-sm font-semibold mb-4 flex items-center gap-2">
              <Activity className="w-4 h-4 text-sky-400" /> Throughput (RPS)
            </h2>
            <div className="h-36">
              {metricData.length === 0 ? (
                <div className="h-full flex items-center justify-center text-xs text-slate-500">
                  No throughput metric samples for current filter window
                </div>
              ) : (
                <ResponsiveContainer width="100%" height="100%">
                  <LineChart data={metricData}>
                    <XAxis dataKey="time" stroke="#64748b" fontSize={11} />
                    <YAxis stroke="#64748b" fontSize={11} />
                    <Tooltip
                      contentStyle={{ backgroundColor: "#0f172a", borderColor: "#334155", color: "#f8fafc" }}
                    />
                    <Line type="monotone" dataKey="rps" stroke="#22d3ee" strokeWidth={2} dot={false} />
                  </LineChart>
                </ResponsiveContainer>
              )}
            </div>
          </div>

          <div className="bg-slate-900 border border-slate-800 rounded-xl p-4">
            <h2 className="text-sm font-semibold mb-2">Recent Traces</h2>
            <div className="flex items-center justify-between gap-3 mb-3">
              <div className="text-[11px] text-slate-400">{traceStatus}</div>
              <span className={`text-[10px] px-2 py-0.5 rounded border ${sourceBadgeClass(traceSource)}`}>
                {sourceBadgeLabel(traceSource)}
              </span>
            </div>
            <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
              <div className="lg:col-span-1 space-y-2 max-h-56 overflow-y-auto pr-1">
                {traces.length === 0 ? (
                  <div className="text-xs text-slate-500">No traces available for selected window.</div>
                ) : (
                  traces.map((trace) => (
                    <button
                      key={trace.trace_id}
                      onClick={() => void openTrace(trace)}
                      className={`w-full text-left px-3 py-2 rounded border text-xs font-mono transition ${
                        selectedTrace?.trace_id === trace.trace_id
                          ? "bg-sky-500/15 border-sky-400/60 text-sky-200"
                          : "bg-slate-950 border-slate-700 text-slate-300 hover:bg-slate-900"
                      }`}
                    >
                      <div className="truncate">{trace.service || "unknown-service"}</div>
                      <div className="truncate text-[10px] text-slate-400">{trace.trace_id}</div>
                    </button>
                  ))
                )}
              </div>
              <div className="lg:col-span-2 border border-slate-800 rounded p-3 bg-slate-950/70 max-h-56 overflow-y-auto">
                {traceSpans.length === 0 ? (
                  <div className="text-xs text-slate-500">Select a trace to render the waterfall.</div>
                ) : (
                  <TraceWaterfall spans={traceSpans} />
                )}
              </div>
            </div>
          </div>

          <div className="bg-slate-900 border border-slate-800 rounded-xl p-4">
            <h2 className="text-sm font-semibold mb-3">Internal Partitions Status</h2>
            <div className="overflow-x-auto">
              <table className="w-full text-left text-xs">
                <thead className="bg-slate-950 text-slate-400 border-b border-slate-800">
                  <tr>
                    <th className="p-2">Partition</th>
                    <th className="p-2">Status</th>
                    <th className="p-2">Health</th>
                    <th className="p-2">Message</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-800">
                  {tableRows.length === 0 ? (
                    <tr>
                      <td className="p-2 text-slate-500" colSpan={4}>No recent events for this partition.</td>
                    </tr>
                  ) : (
                    tableRows.map((row, idx) => (
                      <tr key={`${row.timestamp || "na"}-${idx}`}>
                        <td className="p-2 font-mono">{row.asset || row.intent || "component"}</td>
                        <td className="p-2 text-slate-300">{row.kind || "event"}</td>
                        <td className="p-2 text-slate-200">{row.status || health?.overall_health?.level || "unknown"}</td>
                        <td className="p-2 text-slate-300">{row.message || "-"}</td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      </div>

      <div className="mt-6 bg-slate-900 border border-slate-800 rounded-xl p-4">
        <h2 className="text-sm font-semibold mb-4 flex items-center gap-2">
          <Database className="w-4 h-4 text-sky-400" /> Internal Dependency Topology Map
        </h2>
        <div className="flex items-center justify-between bg-slate-950 p-6 rounded-lg border border-slate-800/80">
          <div className="p-3 bg-slate-900 border border-slate-700 rounded text-center">
            <span className="text-xs text-slate-400 block">Ingress</span>
            <span className="text-sm font-bold text-slate-200">API Gateway</span>
          </div>
          <div className="h-0.5 w-16 bg-sky-500/50 relative">
            <div className="absolute -top-3 left-1/2 -translate-x-1/2 text-[10px] text-sky-400">HTTP</div>
          </div>
          <div className="p-3 bg-slate-900 border border-amber-500/50 rounded text-center">
            <span className="text-xs text-slate-400 block">Cache</span>
            <span className="text-sm font-bold text-amber-400">Redis Cluster</span>
          </div>
          <div className="h-0.5 w-16 bg-sky-500/50 relative">
            <div className="absolute -top-3 left-1/2 -translate-x-1/2 text-[10px] text-sky-400">gRPC</div>
          </div>
          <div className="p-3 bg-slate-900 border border-slate-700 rounded text-center">
            <span className="text-xs text-slate-400 block">Storage</span>
            <span className="text-sm font-bold text-slate-200">Primary DB</span>
          </div>
        </div>
      </div>
    </div>
  );
}
