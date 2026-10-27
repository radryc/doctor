import { useMemo, useState } from "react";
import type { TraceRecord } from "../../types/api";

type SpanHealth = "healthy" | "warning" | "failing" | "unknown";

interface FlowNode {
  span: TraceRecord;
  depth: number;
  x: number;
  y: number;
  health: SpanHealth;
  durationMs: number;
}

interface TraceFlowProps {
  spans: TraceRecord[];
}

const CARD_W = 230;
const CARD_H = 74;
const COL_GAP = 270;
const ROW_GAP = 104;

function parseTime(value?: string): number {
  if (!value) return 0;
  const parsed = new Date(value).getTime();
  return Number.isFinite(parsed) ? parsed : 0;
}

function spanDurationMs(span: TraceRecord): number {
  const start = parseTime(span.start_time);
  const end = parseTime(span.end_time);
  return start > 0 && end >= start ? end - start : 0;
}

function firstAttribute(span: TraceRecord, keys: string[]): string {
  for (const key of keys) {
    const value = span.attributes?.[key];
    if (value) return value;
  }
  return "";
}

function spanHealth(span: TraceRecord, durationMs: number): SpanHealth {
  const status = (span.status_code || "").toUpperCase();
  const httpStatus = Number(
    firstAttribute(span, ["http.response.status_code", "http.status_code", "http.status.code"]) || 0,
  );
  const errorFlag = firstAttribute(span, ["error", "otel.status_code"]).toLowerCase();
  if (status.includes("ERROR") || httpStatus >= 500 || errorFlag === "true" || errorFlag === "error") {
    return "failing";
  }
  if (durationMs >= 1000) return "warning";
  if (durationMs === 0 && !status) return "unknown";
  return "healthy";
}

function healthColor(health: SpanHealth): string {
  switch (health) {
    case "failing":
      return "var(--ui-red)";
    case "warning":
      return "var(--ui-amber)";
    case "healthy":
      return "var(--ui-green)";
    default:
      return "var(--ui-faint)";
  }
}

function healthLabel(health: SpanHealth): string {
  switch (health) {
    case "failing":
      return "error";
    case "warning":
      return "slow";
    case "healthy":
      return "ok";
    default:
      return "unknown";
  }
}

function short(value: string, max: number): string {
  return value.length > max ? `${value.slice(0, max - 1)}…` : value;
}

function buildFlow(spans: TraceRecord[]): { nodes: FlowNode[]; width: number; height: number } {
  const byId = new Map<string, TraceRecord>();
  for (const span of spans) {
    if (span.span_id) byId.set(span.span_id, span);
  }

  const children = new Map<string, TraceRecord[]>();
  const roots: TraceRecord[] = [];
  for (const span of spans) {
    const parent = span.parent_span_id ?? "";
    if (!parent || !byId.has(parent)) {
      roots.push(span);
      continue;
    }
    const list = children.get(parent);
    if (list) list.push(span);
    else children.set(parent, [span]);
  }

  const depthById = new Map<string, number>();
  const queue: Array<{ span: TraceRecord; depth: number }> = roots.map((span) => ({ span, depth: 0 }));
  // A damaged trace can contain a cycle; the depth guard prevents an infinite walk.
  for (let i = 0; i < queue.length && i < spans.length * 4; i++) {
    const item = queue[i];
    if (!item.span.span_id) continue;
    const current = depthById.get(item.span.span_id);
    if (current !== undefined && current <= item.depth) continue;
    depthById.set(item.span.span_id, item.depth);
    for (const child of children.get(item.span.span_id) ?? []) {
      queue.push({ span: child, depth: item.depth + 1 });
    }
  }
  for (const span of spans) {
    if (span.span_id && !depthById.has(span.span_id)) depthById.set(span.span_id, 0);
  }

  const byDepth = new Map<number, TraceRecord[]>();
  for (const span of spans) {
    const depth = span.span_id ? depthById.get(span.span_id) ?? 0 : 0;
    const list = byDepth.get(depth);
    if (list) list.push(span);
    else byDepth.set(depth, [span]);
  }

  const nodes: FlowNode[] = [];
  let maxX = 0;
  let maxY = 0;
  const depths = [...byDepth.keys()].sort((a, b) => a - b);
  for (const depth of depths) {
    const column = (byDepth.get(depth) ?? []).sort(
      (a, b) => parseTime(a.start_time) - parseTime(b.start_time) || a.span_id.localeCompare(b.span_id),
    );
    column.forEach((span, index) => {
      const durationMs = spanDurationMs(span);
      const x = 24 + depth * COL_GAP;
      const y = 24 + index * ROW_GAP;
      maxX = Math.max(maxX, x + CARD_W);
      maxY = Math.max(maxY, y + CARD_H);
      nodes.push({
        span,
        depth,
        x,
        y,
        health: spanHealth(span, durationMs),
        durationMs,
      });
    });
  }

  return {
    nodes,
    width: Math.max(720, maxX + 40),
    height: Math.max(220, maxY + 40),
  };
}

export default function TraceFlow({ spans }: TraceFlowProps) {
  const { nodes, width, height } = useMemo(() => buildFlow(spans), [spans]);
  const [selectedSpanId, setSelectedSpanId] = useState("");

  const nodeById = useMemo(() => new Map(nodes.map((node) => [node.span.span_id, node])), [nodes]);
  const selected = nodeById.get(selectedSpanId) ?? null;

  if (spans.length === 0) {
    return <p className="text-xs text-slate-600 py-4">Select a trace to view its flow</p>;
  }

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-3 text-[10px] text-slate-500">
        <span className="inline-flex items-center gap-1.5">
          <span className="w-2 h-2 rounded-full" style={{ backgroundColor: healthColor("healthy") }} />
          OK
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="w-2 h-2 rounded-full" style={{ backgroundColor: healthColor("warning") }} />
          Slow &gt; 1s
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="w-2 h-2 rounded-full" style={{ backgroundColor: healthColor("failing") }} />
          Error
        </span>
        <span className="text-slate-600">{spans.length} spans · click a card for details · scroll to pan</span>
      </div>

      <div
        className="overflow-auto overscroll-contain border border-slate-800/70 bg-slate-950/40"
        style={{ maxHeight: 520, width: "100%", touchAction: "pan-x pan-y" }}
      >
        <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} role="img" aria-label="Trace flow" style={{ display: "block" }}>
          <defs>
            <marker id="trace-flow-arrow" markerWidth="7" markerHeight="6" refX="7" refY="3" orient="auto">
              <path d="M0,0 L7,3 L0,6 Z" fill="var(--ui-border-strong)" />
            </marker>
          </defs>

          {nodes.map((node) => {
            const parentId = node.span.parent_span_id ?? "";
            const parent = nodeById.get(parentId);
            if (!parent) return null;
            const sx = parent.x + CARD_W;
            const sy = parent.y + CARD_H / 2;
            const ex = node.x;
            const ey = node.y + CARD_H / 2;
            const curve = Math.max(50, Math.abs(ex - sx) / 2);
            return (
              <path
                key={`edge-${node.span.span_id}`}
                d={`M ${sx} ${sy} C ${sx + curve} ${sy}, ${ex - curve} ${ey}, ${ex} ${ey}`}
                fill="none"
                stroke="var(--ui-border-strong)"
                strokeWidth={1.2}
                markerEnd="url(#trace-flow-arrow)"
              />
            );
          })}

          {nodes.map((node) => {
            const selectedNode = node.span.span_id === selectedSpanId;
            const color = healthColor(node.health);
            const service = node.span.service || node.span.name || "span";
            const operation = node.span.name || node.span.kind || "span";
            return (
              <g
                key={node.span.span_id}
                transform={`translate(${node.x} ${node.y})`}
                onClick={() => setSelectedSpanId(node.span.span_id)}
                style={{ cursor: "pointer" }}
              >
                <rect
                  width={CARD_W}
                  height={CARD_H}
                  rx={10}
                  fill="var(--ui-card)"
                  stroke={selectedNode ? "var(--ui-accent)" : `${color}66`}
                  strokeWidth={selectedNode ? 2 : 1}
                />
                <rect width={4} height={CARD_H} rx={4} fill={color} />
                <circle cx={18} cy={18} r={5} fill={color} />
                <text x={30} y={18} fill="var(--ui-muted)" fontSize={10} fontFamily="monospace">
                  {short(service, 30)}
                </text>
                <text x={30} y={36} fill="var(--ui-text)" fontSize={12} fontWeight={600}>
                  {short(operation, 30)}
                </text>
                <text x={30} y={56} fill="var(--ui-muted)" fontSize={10}>
                  {node.durationMs >= 1000 ? `${(node.durationMs / 1000).toFixed(2)}s` : `${Math.round(node.durationMs)}ms`} · {healthLabel(node.health)}
                </text>
                {node.span.status_code ? (
                  <text x={CARD_W - 10} y={18} fill={color} fontSize={9} textAnchor="end">
                    {short(node.span.status_code.replace("STATUS_CODE_", ""), 12)}
                  </text>
                ) : null}
              </g>
            );
          })}
        </svg>
      </div>

      {selected ? (
        <div className="border border-slate-800 bg-slate-950/60 p-3 text-[11px]">
          <div className="flex flex-wrap items-center gap-2 mb-2">
            <span className="w-2 h-2 rounded-full" style={{ backgroundColor: healthColor(selected.health) }} />
            <span className="font-semibold text-slate-200">{selected.span.service || "span"}</span>
            <span className="text-slate-500">{selected.span.name}</span>
            <span className="text-slate-500 ml-auto font-mono">{selected.durationMs}ms</span>
          </div>
          <div className="grid grid-cols-1 md:grid-cols-2 gap-x-6 gap-y-1 text-slate-500">
            <div><span className="text-slate-600">span</span> <code className="text-slate-400">{selected.span.span_id}</code></div>
            <div><span className="text-slate-600">parent</span> <code className="text-slate-400">{selected.span.parent_span_id || "—"}</code></div>
            <div><span className="text-slate-600">status</span> {selected.span.status_code || "—"}</div>
            <div><span className="text-slate-600">start</span> {selected.span.start_time || "—"}</div>
          </div>
          {selected.span.attributes && Object.keys(selected.span.attributes).length > 0 ? (
            <div className="mt-2 grid grid-cols-1 md:grid-cols-2 gap-x-6 gap-y-0.5 max-h-32 overflow-y-auto font-mono text-[10px]">
              {Object.entries(selected.span.attributes)
                .filter(([key]) => !key.startsWith("process.") && !key.startsWith("host.") && key !== "service")
                .slice(0, 16)
                .map(([key, value]) => (
                  <div key={key} className="truncate" title={`${key}=${value}`}>
                    <span className="text-slate-600">{key}</span>
                    <span className="text-slate-400">={value}</span>
                  </div>
                ))}
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
