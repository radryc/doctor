import { useMemo } from "react";
import { Activity, ArrowRight, AlertTriangle, Server } from "lucide-react";
import type { TrafficModel, TrafficNode } from "./traffic";

interface TrafficGraphProps {
  model: TrafficModel;
  selectedId: string;
  onSelect: (id: string) => void;
  onOpenAlerts: () => void;
}

function statusColor(status: TrafficNode["status"]): string {
  if (status === "critical") return "var(--ui-red)";
  if (status === "degraded") return "var(--ui-amber)";
  return "var(--ui-green)";
}

function statusLabel(status: TrafficNode["status"]): string {
  if (status === "critical") return "critical";
  if (status === "degraded") return "degraded";
  return "healthy";
}

export default function TrafficGraph({ model, selectedId, onSelect, onOpenAlerts }: TrafficGraphProps) {
  const selected = useMemo(
    () => model.nodes.find((node) => node.id === selectedId) || model.nodes.find((node) => node.id === model.focusId) || null,
    [model, selectedId]
  );
  const maxRps = Math.max(model.maxRps, 1);
  const trafficEdges = model.edges.filter((edge) => edge.relation !== "contains");
  const structuralEdges = model.edges.filter((edge) => edge.relation === "contains");

  if (model.nodes.length === 0) {
    return (
      <div className="flex h-[420px] items-center justify-center rounded-lg border border-dashed border-slate-700 text-xs text-slate-500">
        No intent or asset topology is available for this partition.
      </div>
    );
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 text-[10px] text-slate-500">
        <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full bg-emerald-400" /> healthy</span>
        <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full bg-amber-400" /> degraded</span>
        <span className="inline-flex items-center gap-1"><span className="h-2 w-2 rounded-full bg-rose-400" /> critical</span>
        <span className="inline-flex items-center gap-1 border-l border-slate-700 pl-2"><ArrowRight size={10} /> dependency traffic</span>
        <span className="inline-flex items-center gap-1 border-l border-slate-700 pl-2"><span className="h-px w-4 border-t border-dashed border-slate-500" /> intent contains asset</span>
        <span className="ml-auto rounded border border-slate-700 px-1.5 py-0.5">
          {model.explicitEdges ? "edge traffic: measured" : "edge traffic: inferred from source RPS"}
        </span>
      </div>

      <div className="relative h-[420px] overflow-hidden rounded-xl border border-slate-800 bg-slate-950">
        <div
          className="absolute inset-0 opacity-50"
          style={{
            backgroundImage:
              "radial-gradient(circle at 18% 22%, color-mix(in oklab, var(--ui-accent) 15%, transparent), transparent 28%), radial-gradient(circle at 82% 76%, color-mix(in oklab, var(--ui-red) 10%, transparent), transparent 30%), linear-gradient(var(--chart-grid) 1px, transparent 1px), linear-gradient(90deg, var(--chart-grid) 1px, transparent 1px)",
            backgroundSize: "auto, auto, 24px 24px, 24px 24px",
          }}
        />
        <svg viewBox="0 0 100 100" className="relative z-10 h-full w-full" role="img" aria-label="Traffic between intents and assets">
          <defs>
            <marker id="fleet-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="4" markerHeight="4" orient="auto-start-reverse">
              <path d="M 0 0 L 10 5 L 0 10 z" fill="var(--ui-border-strong)" />
            </marker>
            <filter id="fleet-glow" x="-50%" y="-50%" width="200%" height="200%">
              <feGaussianBlur stdDeviation="1.6" result="blur" />
              <feMerge>
                <feMergeNode in="blur" />
                <feMergeNode in="SourceGraphic" />
              </feMerge>
            </filter>
          </defs>

          {structuralEdges.map((edge, index) => {
            const from = model.nodes.find((node) => node.id === edge.from);
            const to = model.nodes.find((node) => node.id === edge.to);
            if (!from || !to) return null;
            return (
              <line
                key={`struct-${edge.from}-${edge.to}-${index}`}
                x1={from.x}
                y1={from.y}
                x2={to.x}
                y2={to.y}
                stroke="var(--ui-border)"
                strokeWidth="0.2"
                strokeDasharray="1 1"
              />
            );
          })}

          {trafficEdges.map((edge, index) => {
            const from = model.nodes.find((node) => node.id === edge.from);
            const to = model.nodes.find((node) => node.id === edge.to);
            if (!from || !to) return null;
            const width = 0.22 + Math.min(1, edge.rps / maxRps) * 1.35;
            const midX = (from.x + to.x) / 2;
            const midY = (from.y + to.y) / 2;
            const dx = to.x - from.x;
            const dy = to.y - from.y;
            const length = Math.hypot(dx, dy) || 1;
            const curve = 2.4;
            const controlX = midX - (dy / length) * curve;
            const controlY = midY + (dx / length) * curve;
            return (
              <g key={`traffic-${edge.from}-${edge.to}-${index}`}>
                <path
                  d={`M ${from.x} ${from.y} Q ${controlX} ${controlY} ${to.x} ${to.y}`}
                  stroke={edge.inferred ? "var(--ui-border-strong)" : "color-mix(in oklab, var(--ui-accent) 70%, transparent)"}
                  strokeWidth={width}
                  fill="none"
                  markerEnd="url(#fleet-arrow)"
                  opacity={0.8}
                >
                  <title>{`${from.name} → ${to.name} · ${edge.rps.toFixed(2)} rps${edge.inferred ? " (inferred)" : ""}`}</title>
                </path>
                {edge.rps > 0 && (
                  <text
                    x={controlX}
                    y={controlY - 1}
                    fontSize="1.6"
                    textAnchor="middle"
                    fill="var(--ui-faint)"
                    style={{ paintOrder: "stroke", stroke: "var(--ui-bg)", strokeWidth: 0.5 }}
                  >
                    {edge.rps.toFixed(1)} rps
                  </text>
                )}
              </g>
            );
          })}

          {model.nodes.map((node) => {
            const focused = node.id === selected?.id;
            const color = statusColor(node.status);
            const radius = (focused ? 2.7 : 1.7) + Math.min(1.1, node.rps / maxRps) + Math.min(0.6, node.degree * 0.04);
            return (
              <g key={node.id} onClick={() => onSelect(node.id)} className="cursor-pointer" role="button" tabIndex={0}>
                <title>{`${node.name} · ${statusLabel(node.status)} · ${node.rps.toFixed(2)} rps${node.alertCount ? ` · ${node.alertCount} alert(s)` : ""}`}</title>
                {node.type === "intent" && (
                  <circle cx={node.x} cy={node.y} r={radius + 1.2} fill="none" stroke={color} strokeWidth="0.18" strokeDasharray="0.8 0.8" opacity="0.7" />
                )}
                <circle cx={node.x} cy={node.y} r={radius + 0.7} fill={color} opacity="0.15" filter="url(#fleet-glow)" />
                <circle
                  cx={node.x}
                  cy={node.y}
                  r={radius}
                  fill={color}
                  stroke={focused ? "var(--ui-text)" : "var(--ui-border-strong)"}
                  strokeWidth={focused ? 0.5 : 0.22}
                />
                {node.alertCount > 0 && (
                  <g>
                    <circle cx={node.x + radius * 0.8} cy={node.y - radius * 0.8} r={1.35} fill={node.criticalAlerts > 0 ? "var(--ui-red)" : "var(--ui-amber)"} />
                    <text x={node.x + radius * 0.8} y={node.y - radius * 0.8 + 0.55} fontSize="1.5" textAnchor="middle" fill="var(--ui-accent-contrast)" fontWeight="700">
                      {node.alertCount}
                    </text>
                  </g>
                )}
                {(focused || node.degree > 2 || node.rps > 0) && (
                  <text
                    x={node.x + 1.9}
                    y={node.y - 1.4}
                    fontSize="2"
                    fill="var(--ui-text)"
                    style={{ paintOrder: "stroke", stroke: "var(--ui-bg)", strokeWidth: 0.7 }}
                  >
                    {node.name}
                  </text>
                )}
              </g>
            );
          })}
        </svg>
      </div>

      {selected && (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
          <div className="rounded-lg border border-slate-800 bg-slate-950/70 p-3">
            <div className="flex items-center gap-2 text-[10px] uppercase tracking-wide text-slate-500">
              {selected.type === "intent" ? <Server size={12} /> : <Activity size={12} />} selected {selected.type}
            </div>
            <div className="mt-1 truncate text-sm font-semibold text-slate-100">{selected.name}</div>
            <div className="truncate text-[10px] text-slate-500">{selected.intent} · {selected.id}</div>
          </div>
          <div className="rounded-lg border border-slate-800 bg-slate-950/70 p-3">
            <div className="text-[10px] uppercase tracking-wide text-slate-500">Traffic through node</div>
            <div className="mt-1 text-lg font-bold tabular-nums text-sky-300">{selected.rps.toFixed(2)} <span className="text-xs font-normal text-slate-500">rps</span></div>
            <div className="text-[10px] text-slate-500">{selected.degree} topology connection(s)</div>
          </div>
          <div className="rounded-lg border border-slate-800 bg-slate-950/70 p-3">
            <div className="flex items-center justify-between">
              <div className="text-[10px] uppercase tracking-wide text-slate-500">Alert state</div>
              {selected.alertCount > 0 && (
                <button type="button" onClick={onOpenAlerts} className="text-[10px] text-rose-300 hover:underline">
                  open alerts →
                </button>
              )}
            </div>
            <div className={`mt-1 flex items-center gap-2 text-sm font-semibold ${selected.criticalAlerts > 0 ? "text-rose-300" : selected.alertCount > 0 ? "text-amber-300" : "text-emerald-300"}`}>
              {selected.alertCount > 0 ? <AlertTriangle size={14} /> : null}
              {selected.alertCount === 0 ? "No firing alerts" : `${selected.alertCount} firing · ${selected.criticalAlerts} critical`}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
