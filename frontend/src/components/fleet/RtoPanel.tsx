import { AlertTriangle, Clock3, HeartPulse, Timer } from "lucide-react";
import type { RtoSummary } from "./rto";
import { formatDurationMs, rtoStatus } from "./rto";

interface RtoPanelProps {
  summary: RtoSummary;
  targetMs: number;
  onTargetChange: (value: number) => void;
  onOpenAlerts: () => void;
}

const TARGETS = [
  { label: "5m", value: 5 * 60_000 },
  { label: "15m", value: 15 * 60_000 },
  { label: "30m", value: 30 * 60_000 },
  { label: "1h", value: 60 * 60_000 },
  { label: "4h", value: 4 * 60 * 60_000 },
];

function statusClass(status: ReturnType<typeof rtoStatus>): string {
  if (status === "breached") return "border-rose-500/40 bg-rose-500/10 text-rose-300";
  if (status === "at-risk") return "border-amber-500/40 bg-amber-500/10 text-amber-300";
  return "border-emerald-500/40 bg-emerald-500/10 text-emerald-300";
}

export default function RtoPanel({ summary, targetMs, onTargetChange, onOpenAlerts }: RtoPanelProps) {
  const status = rtoStatus(summary, targetMs);
  const currentWorst = summary.open.reduce<number>((max, incident) => Math.max(max, Date.now() - incident.startedAt), 0);

  return (
    <div className="space-y-4">
      <div className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <h2 className="flex items-center gap-2 text-sm font-semibold">
              <Timer size={15} className="text-sky-400" /> Recovery time objective
            </h2>
            <p className="mt-1 text-[11px] text-slate-500">Observed recovery from Guardian failure/recovery events.</p>
          </div>
          <span className={`rounded border px-2 py-0.5 text-[10px] font-semibold uppercase ${statusClass(status)}`}>
            {status === "within" ? "within RTO" : status === "at-risk" ? "at risk" : "RTO breached"}
          </span>
        </div>

        <div className="mt-3 flex items-center gap-2">
          <span className="text-[10px] uppercase tracking-wide text-slate-500">Target</span>
          <div className="flex flex-wrap gap-1">
            {TARGETS.map((target) => (
              <button
                key={target.value}
                type="button"
                onClick={() => onTargetChange(target.value)}
                className={`rounded border px-1.5 py-0.5 text-[10px] ${
                  targetMs === target.value
                    ? "border-sky-500/50 bg-sky-500/15 text-sky-300"
                    : "border-slate-700 bg-slate-950 text-slate-400 hover:border-slate-600"
                }`}
              >
                {target.label}
              </button>
            ))}
          </div>
        </div>

        <div className="mt-4 grid grid-cols-2 gap-2">
          <div className="rounded-lg border border-slate-800 bg-slate-950/70 p-2.5">
            <div className="flex items-center gap-1 text-[9px] uppercase tracking-wide text-slate-500"><Clock3 size={10} /> current outage</div>
            <div className={`mt-1 text-lg font-bold tabular-nums ${currentWorst > targetMs ? "text-rose-300" : "text-slate-100"}`}>
              {formatDurationMs(currentWorst)}
            </div>
          </div>
          <div className="rounded-lg border border-slate-800 bg-slate-950/70 p-2.5">
            <div className="flex items-center gap-1 text-[9px] uppercase tracking-wide text-slate-500"><HeartPulse size={10} /> open incidents</div>
            <div className={`mt-1 text-lg font-bold tabular-nums ${summary.open.length > 0 ? "text-rose-300" : "text-emerald-300"}`}>
              {summary.open.length}
            </div>
          </div>
          <div className="rounded-lg border border-slate-800 bg-slate-950/70 p-2.5">
            <div className="text-[9px] uppercase tracking-wide text-slate-500">p50 recovery</div>
            <div className="mt-1 text-lg font-bold tabular-nums text-slate-100">{formatDurationMs(summary.p50)}</div>
          </div>
          <div className="rounded-lg border border-slate-800 bg-slate-950/70 p-2.5">
            <div className="text-[9px] uppercase tracking-wide text-slate-500">p95 recovery</div>
            <div className={`mt-1 text-lg font-bold tabular-nums ${summary.p95 > targetMs ? "text-rose-300" : "text-slate-100"}`}>
              {formatDurationMs(summary.p95)}
            </div>
          </div>
        </div>

        {summary.open.length > 0 && (
          <button type="button" onClick={onOpenAlerts} className="mt-3 flex w-full items-center justify-center gap-1.5 rounded border border-rose-500/30 bg-rose-500/10 px-2 py-1.5 text-[11px] text-rose-300 hover:bg-rose-500/20">
            <AlertTriangle size={12} /> Inspect {summary.open.length} active incident{summary.open.length === 1 ? "" : "s"} and alerts
          </button>
        )}
      </div>

      <div className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
        <h3 className="text-[10px] font-bold uppercase tracking-wide text-slate-500">RTO by intent</h3>
        <div className="mt-2 space-y-1.5">
          {summary.byIntent.slice(0, 8).map((bucket) => (
            <div key={bucket.key} className="flex items-center gap-2 text-[11px]">
              <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${bucket.open.length > 0 ? "bg-rose-400" : bucket.p95 > targetMs ? "bg-amber-400" : "bg-emerald-400"}`} />
              <span className="min-w-0 flex-1 truncate text-slate-300" title={bucket.key}>{bucket.key}</span>
              <span className="shrink-0 tabular-nums text-slate-500">{bucket.open.length > 0 ? `${bucket.open.length} open` : formatDurationMs(bucket.p95)}</span>
            </div>
          ))}
          {summary.byIntent.length === 0 && <p className="text-[11px] text-slate-500">No failure/recovery events in this window.</p>}
        </div>
      </div>

      <div className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
        <h3 className="text-[10px] font-bold uppercase tracking-wide text-slate-500">Recovery timeline</h3>
        <div className="mt-2 max-h-56 space-y-2 overflow-y-auto pr-1">
          {summary.incidents.slice(-10).reverse().map((incident, index) => (
            <div key={`${incident.key}-${incident.startedAt}-${index}`} className="flex items-start gap-2 text-[11px]">
              <span className={`mt-1 h-1.5 w-1.5 shrink-0 rounded-full ${incident.open ? "bg-rose-400" : "bg-emerald-400"}`} />
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <span className="truncate text-slate-300">{incident.asset || incident.intent || incident.key}</span>
                  <span className="ml-auto shrink-0 tabular-nums text-slate-500">
                    {incident.open ? "open" : formatDurationMs(incident.durationMs ?? 0)}
                  </span>
                </div>
                <div className="truncate text-[10px] text-slate-600">
                  {new Date(incident.startedAt).toLocaleString()}
                  {incident.open ? "" : ` → ${new Date(incident.endedAt ?? incident.startedAt).toLocaleTimeString()}`}
                  {incident.message ? ` · ${incident.message}` : ""}
                </div>
              </div>
            </div>
          ))}
          {summary.incidents.length === 0 && <p className="text-[11px] text-slate-500">No incident transitions recorded in the selected window.</p>}
        </div>
      </div>
    </div>
  );
}
