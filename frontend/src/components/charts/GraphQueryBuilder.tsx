import { useMemo, useState } from "react";
import { BarChart3, CircleDot, Logs, MoveRight, Radar, Trash2 } from "lucide-react";
import type { GuardianTopologyNode } from "../../types/api";
import type { ObservabilityConfig } from "../../types/observability";

type BlockType =
  | "metrics_rps"
  | "metrics_latency_p95"
  | "metrics_errors"
  | "logs_rate"
  | "logs_errors"
  | "traces_rate"
  | "traces_latency_p95";

interface GraphBlock {
  id: string;
  type: BlockType;
}

interface PaletteItem {
  type: BlockType;
  label: string;
  source: "metrics" | "logs" | "traces";
  hint: string;
}

interface GraphQueryBuilderProps {
  selectedNode: GuardianTopologyNode | null;
  onApplyQuery: (query: string) => void;
  discoveredMetrics?: string[];
  observability?: ObservabilityConfig | null;
}

interface QuerySuggestion {
  id: string;
  label: string;
  query: string;
  source: "metrics" | "logs" | "traces";
  hint: string;
}

const PALETTE: PaletteItem[] = [
  { type: "metrics_rps", label: "Request Throughput", source: "metrics", hint: "RPS from http request counters" },
  { type: "metrics_latency_p95", label: "Latency p95", source: "metrics", hint: "Histogram quantile over request duration" },
  { type: "metrics_errors", label: "5xx Error Rate", source: "metrics", hint: "Server error ratio from request counters" },
  { type: "logs_rate", label: "Log Volume", source: "logs", hint: "Log records per second" },
  { type: "logs_errors", label: "Error Logs", source: "logs", hint: "ERROR/FATAL log rate" },
  { type: "traces_rate", label: "Span Throughput", source: "traces", hint: "Trace span calls per second" },
  { type: "traces_latency_p95", label: "Span Latency p95", source: "traces", hint: "Trace latency histogram p95" },
];

function escapeRegex(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function serviceScopeRegex(node: GuardianTopologyNode | null): string {
  if (!node) return "$serviceRegex";
  const values = [node.name, node.asset || "", node.id, node.intent || "", node.partition || ""]
    .map((v) => v.trim())
    .filter(Boolean);
  return values.map(escapeRegex).join("|") || "$serviceRegex";
}

function uniqueStrings(values: string[]): string[] {
  return [...new Set(values.map((value) => value.trim()).filter(Boolean))];
}

function buildMetricQuery(metricName: string, serviceRegex: string, window: string): string {
  if (metricName.endsWith("_bucket")) {
    return `histogram_quantile(0.95, sum by (le) (rate(${metricName}{service=~"${serviceRegex}"}[${window}])))`;
  }
  return `rate(${metricName}{service=~"${serviceRegex}"}[${window}])`;
}

function buildSuggestions(
  selectedNode: GuardianTopologyNode | null,
  observability: ObservabilityConfig | null | undefined,
  discoveredMetrics: string[] | undefined,
  window: string,
): QuerySuggestion[] {
  const serviceRegex = serviceScopeRegex(selectedNode);
  const suggestions: QuerySuggestion[] = [];
  const metricNames = uniqueStrings([
    ...(observability?.metrics ?? []),
    ...(discoveredMetrics ?? []),
  ]).slice(0, 8);

  for (const metricName of metricNames) {
    suggestions.push({
      id: `metric-${metricName}`,
      label: metricName,
      query: buildMetricQuery(metricName, serviceRegex, window),
      source: "metrics",
      hint: selectedNode ? `for ${selectedNode.name}` : "live metric",
    });
  }

  return suggestions;
}

function sourceBadge(source: PaletteItem["source"]): string {
  if (source === "metrics") return "border-sky-500/40 text-sky-300 bg-sky-500/10";
  if (source === "logs") return "border-amber-500/40 text-amber-300 bg-amber-500/10";
  return "border-violet-500/40 text-violet-300 bg-violet-500/10";
}

function wrapAggregate(aggregation: string, groupBy: string, expr: string): string {
  if (aggregation === "none") return expr;
  if (groupBy === "none") return `${aggregation}(${expr})`;
  return `${aggregation} by (${groupBy}) (${expr})`;
}

function buildQuery(type: BlockType, groupBy: string, window: string, aggregation: string, serviceRegex: string): string {
  const svc = `{service=~"${serviceRegex}"}`;

  switch (type) {
    case "metrics_rps":
      return wrapAggregate(aggregation, groupBy, `rate(http_requests_total${svc}[${window}])`);
    case "metrics_latency_p95": {
      const group = groupBy === "none" ? "le" : `le,${groupBy}`;
      return `histogram_quantile(0.95, sum by (${group}) (rate(http_request_duration_seconds_bucket${svc}[${window}])))`;
    }
    case "metrics_errors":
      return wrapAggregate(aggregation, groupBy, `rate(http_requests_total{service=~"${serviceRegex}",status_code=~"5.."}[${window}])`);
    case "logs_rate":
      return wrapAggregate(aggregation, groupBy, `rate(log_records_total${svc}[${window}])`);
    case "logs_errors":
      return wrapAggregate(aggregation, groupBy, `rate(log_records_total{service=~"${serviceRegex}",severity=~"ERROR|FATAL"}[${window}])`);
    case "traces_rate":
      return wrapAggregate(aggregation, groupBy, `rate(traces_spanmetrics_calls_total${svc}[${window}])`);
    case "traces_latency_p95": {
      const group = groupBy === "none" ? "le" : `le,${groupBy}`;
      return `histogram_quantile(0.95, sum by (${group}) (rate(traces_spanmetrics_latency_bucket${svc}[${window}])))`;
    }
    default:
      return "";
  }
}

export default function GraphQueryBuilder({ selectedNode, onApplyQuery, discoveredMetrics, observability }: GraphQueryBuilderProps) {
  const [blocks, setBlocks] = useState<GraphBlock[]>([]);
  const [activeBlockId, setActiveBlockId] = useState<string>("");
  const [groupBy, setGroupBy] = useState("service");
  const [window, setWindow] = useState("5m");
  const [aggregation, setAggregation] = useState("sum");

  function addBlock(type: BlockType) {
    const id = `${type}-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
    setBlocks((prev) => [...prev, { id, type }]);
    setActiveBlockId(id);
  }

  function removeBlock(id: string) {
    setBlocks((prev) => prev.filter((b) => b.id !== id));
    if (activeBlockId === id) {
      setActiveBlockId("");
    }
  }

  function onDragStart(event: React.DragEvent<HTMLButtonElement>, type: BlockType) {
    event.dataTransfer.setData("application/x-graph-block", type);
  }

  function onDrop(event: React.DragEvent<HTMLDivElement>) {
    event.preventDefault();
    const type = event.dataTransfer.getData("application/x-graph-block") as BlockType;
    if (!type) return;
    addBlock(type);
  }

  const activeBlock = useMemo(
    () => blocks.find((b) => b.id === activeBlockId) || blocks[blocks.length - 1] || null,
    [blocks, activeBlockId]
  );

  const builtQuery = useMemo(() => {
    if (!activeBlock) return "";
    return buildQuery(activeBlock.type, groupBy, window, aggregation, serviceScopeRegex(selectedNode));
  }, [activeBlock, groupBy, window, aggregation, selectedNode]);

  const activeLabel = useMemo(() => {
    if (!activeBlock) return "";
    return PALETTE.find((p) => p.type === activeBlock.type)?.label || activeBlock.type;
  }, [activeBlock]);

  const suggestions = useMemo(
    () => buildSuggestions(selectedNode, observability, discoveredMetrics, window),
    [selectedNode, observability, discoveredMetrics, window]
  );

  const observabilitySummary = useMemo(() => {
    if (!observability) return "";
    const logCount = observability.logs.length;
    const traceCount = observability.traces.length;
    if (logCount === 0 && traceCount === 0) return "";
    return `${logCount} log hint${logCount === 1 ? "" : "s"} · ${traceCount} trace hint${traceCount === 1 ? "" : "s"}`;
  }, [observability]);

  return (
    <div className="grid grid-cols-1 xl:grid-cols-[300px_1fr] gap-4">
      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-3">
        <p className="text-xs font-semibold text-slate-300 mb-2">Data Sources</p>
        <p className="text-[10px] text-slate-500 mb-3">Drag blocks to the builder area</p>
        <div className="space-y-2">
          {PALETTE.map((item) => (
            <button
              key={item.type}
              type="button"
              draggable
              onDragStart={(e) => onDragStart(e, item.type)}
              onClick={() => addBlock(item.type)}
              className="w-full text-left px-2.5 py-2 rounded border border-slate-800 bg-slate-900 hover:border-slate-700 transition-colors"
            >
              <div className="flex items-center justify-between gap-2">
                <span className="text-xs text-slate-200">{item.label}</span>
                <span className={`text-[9px] px-1.5 py-0.5 rounded border ${sourceBadge(item.source)}`}>{item.source}</span>
              </div>
              <p className="text-[10px] text-slate-500 mt-1">{item.hint}</p>
            </button>
          ))}
        </div>

        <div className="mt-4">
          <p className="text-[10px] font-semibold uppercase tracking-wide text-slate-500 mb-2">Live Suggestions</p>
          {observabilitySummary && <p className="mb-2 text-[10px] text-slate-600">{observabilitySummary}</p>}
          {suggestions.length === 0 ? (
            <p className="text-[10px] text-slate-600">Select an asset to discover live metric queries.</p>
          ) : (
            <div className="space-y-2 max-h-[280px] overflow-y-auto pr-1">
              {suggestions.map((item) => (
                <button
                  key={item.id}
                  type="button"
                  onClick={() => onApplyQuery(item.query)}
                  className="w-full text-left px-2.5 py-2 rounded border border-slate-800 bg-slate-900 hover:border-sky-500/40 hover:bg-sky-500/5 transition-colors"
                >
                  <div className="flex items-center justify-between gap-2">
                    <span className="text-xs text-slate-200 truncate">{item.label}</span>
                    <span className={`text-[9px] px-1.5 py-0.5 rounded border ${sourceBadge(item.source)}`}>{item.source}</span>
                  </div>
                  <p className="mt-1 text-[10px] text-slate-500 truncate">{item.hint}</p>
                </button>
              ))}
            </div>
          )}
        </div>
      </div>

      <div className="bg-slate-950/60 border border-slate-800 rounded-lg p-3">
        <div
          onDragOver={(e) => e.preventDefault()}
          onDrop={onDrop}
          className="rounded border border-dashed border-slate-700 p-3 min-h-[120px]"
        >
          <div className="flex items-center justify-between gap-2 mb-2">
            <p className="text-xs font-semibold text-slate-300">Graph Pipeline</p>
            <span className="text-[10px] text-slate-500">Datadog-style quick builder</span>
          </div>

          {blocks.length === 0 ? (
            <div className="text-[11px] text-slate-500 py-6 text-center">Drop a block here to start building a graph query</div>
          ) : (
            <div className="flex flex-wrap items-center gap-2">
              {blocks.map((block, idx) => {
                const meta = PALETTE.find((p) => p.type === block.type);
                const active = block.id === activeBlock?.id;
                const icon = meta?.source === "metrics" ? BarChart3 : meta?.source === "logs" ? Logs : Radar;
                const Icon = icon || CircleDot;
                return (
                  <div
                    key={block.id}
                    className={`flex items-center gap-1.5 border rounded px-2 py-1 ${active ? "border-sky-400/60 bg-sky-500/10" : "border-slate-700 bg-slate-900"}`}
                  >
                    <button type="button" onClick={() => setActiveBlockId(block.id)} className="flex items-center gap-1.5 text-[11px] text-slate-200">
                      <Icon className="w-3.5 h-3.5" />
                      {meta?.label || block.type}
                    </button>
                    <button type="button" onClick={() => removeBlock(block.id)} className="text-slate-500 hover:text-red-300">
                      <Trash2 className="w-3 h-3" />
                    </button>
                    {idx < blocks.length - 1 && <MoveRight className="w-3 h-3 text-slate-600" />}
                  </div>
                );
              })}
            </div>
          )}
        </div>

        <div className="mt-3 grid grid-cols-1 md:grid-cols-3 gap-2">
          <label className="text-[10px] text-slate-500">
            Group by
            <select value={groupBy} onChange={(e) => setGroupBy(e.target.value)} className="mt-1 w-full bg-slate-900 border border-slate-700 text-[11px] text-slate-200 px-2 py-1">
              <option value="service">service</option>
              <option value="asset">asset</option>
              <option value="intent">intent</option>
              <option value="partition">partition</option>
              <option value="none">none</option>
            </select>
          </label>

          <label className="text-[10px] text-slate-500">
            Rate window
            <select value={window} onChange={(e) => setWindow(e.target.value)} className="mt-1 w-full bg-slate-900 border border-slate-700 text-[11px] text-slate-200 px-2 py-1">
              <option value="1m">1m</option>
              <option value="5m">5m</option>
              <option value="15m">15m</option>
            </select>
          </label>

          <label className="text-[10px] text-slate-500">
            Aggregation
            <select value={aggregation} onChange={(e) => setAggregation(e.target.value)} className="mt-1 w-full bg-slate-900 border border-slate-700 text-[11px] text-slate-200 px-2 py-1">
              <option value="sum">sum</option>
              <option value="avg">avg</option>
              <option value="max">max</option>
              <option value="min">min</option>
              <option value="none">none</option>
            </select>
          </label>
        </div>

        <div className="mt-3 rounded border border-slate-800 bg-slate-900 p-3">
          <div className="flex items-center justify-between gap-2">
            <p className="text-xs text-slate-300">Generated Query {activeLabel ? `(${activeLabel})` : ""}</p>
            <button
              type="button"
              disabled={!builtQuery}
              onClick={() => onApplyQuery(builtQuery)}
              className="px-2.5 py-1 text-[11px] border border-sky-500/60 text-sky-300 hover:bg-sky-500/10 disabled:opacity-40"
            >
              Use in chart
            </button>
          </div>
          <pre className="mt-2 text-[11px] text-slate-300 whitespace-pre-wrap break-words">{builtQuery || "No query generated yet"}</pre>
        </div>
      </div>
    </div>
  );
}
