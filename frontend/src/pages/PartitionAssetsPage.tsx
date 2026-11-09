import { useEffect, useMemo, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
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
import { useConfig } from "../context/ConfigContext";
import RtoPanel from "../components/fleet/RtoPanel";
import TrafficGraph from "../components/fleet/TrafficGraph";
import { computeRto, formatDurationMs, rtoStatus } from "../components/fleet/rto";
import {
  alertsMatchingNode,
  buildTrafficModel,
  topologyNodeStatus,
} from "../components/fleet/traffic";
import { usePartitionFleet } from "../components/fleet/usePartitionFleet";
import type { AssetGroup, HealthStatus } from "./FleetView";
import HubDetailView from "./HubDetailView";

const RTO_STORAGE_KEY = "doctor.fleet.rtoTargetMs";
const DEFAULT_RTO_MS = 30 * 60_000;

function minutesLabel(minutes: number): string {
  if (minutes < 60) return `${minutes}m`;
  if (minutes < 60 * 24) return `${minutes / 60}h`;
  return `${minutes / (60 * 24)}d`;
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

export default function PartitionAssetsPage() {
  const { name = "" } = useParams<{ name: string }>();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { config } = useConfig();
  const tenant = config.default_tenant || "default";

  const [rangeMinutes, setRangeMinutes] = useState(() => Number(searchParams.get("range")) || 60);
  const [focusId, setFocusId] = useState(() => searchParams.get("node") || "");
  const [selectedHub, setSelectedHub] = useState<AssetGroup | null>(null);
  const [rtoTarget, setRtoTarget] = useState(() => {
    try {
      const stored = Number(localStorage.getItem(RTO_STORAGE_KEY));
      return Number.isFinite(stored) && stored > 0 ? stored : DEFAULT_RTO_MS;
    } catch {
      return DEFAULT_RTO_MS;
    }
  });
  const [now, setNow] = useState(Date.now());

  const fleet = usePartitionFleet(tenant, name, rangeMinutes);

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

  useEffect(() => {
    const next = new URLSearchParams(searchParams);
    next.set("range", String(rangeMinutes));
    if (focusId) next.set("node", focusId);
    else next.delete("node");
    const search = next.toString();
    if (search !== searchParams.toString()) {
      setSearchParams(next, { replace: true });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rangeMinutes, focusId]);

  const assets = useMemo<AssetGroup[]>(
    () =>
      (fleet.topology?.nodes ?? [])
        .filter((node) => node.type === "asset")
        .map((node) => ({
          id: node.id,
          name: node.name,
          partition: node.partition || name,
          intent: node.intent || "Services",
          status: topologyNodeStatus(node),
        }))
        .sort((a, b) => a.intent.localeCompare(b.intent) || a.name.localeCompare(b.name)),
    [fleet.topology, name]
  );

  const nodeById = useMemo(
    () => new Map((fleet.topology?.nodes ?? []).map((node) => [node.id, node])),
    [fleet.topology]
  );

  const effectiveFocus = useMemo(() => {
    if (focusId && nodeById.has(focusId)) return focusId;
    const ranked = [...assets].sort((a, b) => {
      const rank = (status: HealthStatus) => (status === "critical" ? 0 : status === "degraded" ? 1 : 2);
      const byStatus = rank(a.status) - rank(b.status);
      if (byStatus !== 0) return byStatus;
      return (fleet.nodeRps.get(b.id) ?? 0) - (fleet.nodeRps.get(a.id) ?? 0);
    });
    return ranked[0]?.id ?? "";
  }, [focusId, nodeById, assets, fleet.nodeRps]);

  const trafficModel = useMemo(
    () =>
      buildTrafficModel(
        fleet.topology,
        effectiveFocus,
        fleet.nodeRps,
        fleet.edgeRps,
        fleet.alerts,
        fleet.explicitEdges
      ),
    [fleet.topology, effectiveFocus, fleet.nodeRps, fleet.edgeRps, fleet.alerts, fleet.explicitEdges]
  );

  const activeAlerts = useMemo(() => fleet.alerts.filter((alert) => alert.status !== "resolved"), [fleet.alerts]);
  const rto = useMemo(() => computeRto(fleet.events, now, rtoTarget), [fleet.events, now, rtoTarget]);
  const rtoState = rtoStatus(rto, rtoTarget);

  const trafficPaths = useMemo(
    () =>
      trafficModel.edges
        .filter((edge) => edge.relation !== "contains")
        .sort((a, b) => b.rps - a.rps)
        .slice(0, 12),
    [trafficModel.edges]
  );

  const assetsWithState = useMemo(
    () =>
      assets.map((asset) => {
        const node = nodeById.get(asset.id);
        const alerts = alertsMatchingNode(fleet.alerts, node);
        return {
          ...asset,
          rps: fleet.nodeRps.get(asset.id) ?? 0,
          alerts,
        };
      }),
    [assets, nodeById, fleet.alerts, fleet.nodeRps]
  );

  const openAlerts = (fingerprint?: string) => {
    const params = new URLSearchParams({ tenant, partition: name });
    if (fingerprint) params.set("alert", fingerprint);
    navigate(`/alerts?${params.toString()}`);
  };

  if (selectedHub) {
    return (
      <HubDetailView
        asset={selectedHub}
        health={null}
        events={fleet.events}
        tenant={tenant}
        rangeMinutes={rangeMinutes}
        onRangeChange={setRangeMinutes}
        onBack={() => setSelectedHub(null)}
      />
    );
  }

  const counts = trafficModel.nodes.reduce(
    (acc, node) => {
      if (node.status === "critical") acc.critical += 1;
      else if (node.status === "degraded") acc.degraded += 1;
      else acc.healthy += 1;
      return acc;
    },
    { healthy: 0, degraded: 0, critical: 0 }
  );

  return (
    <div className="h-full min-h-0 w-full overflow-y-auto bg-slate-950 p-5 text-slate-100">
      <header className="mb-4 flex flex-wrap items-start justify-between gap-3 border-b border-slate-800 pb-4">
        <div className="flex items-start gap-3">
          <button type="button" onClick={() => navigate("/fleet")} className="btn mt-1 text-xs">
            <ArrowLeft size={14} /> Fleet
          </button>
          <div>
            <p className="text-[10px] font-bold uppercase tracking-[0.18em] text-[var(--strata-accent-text)]">Partition traffic & recovery</p>
            <h1 className="mt-1 text-xl font-semibold">{name || "Partition"}</h1>
            <p className="mt-1 text-xs text-slate-500">
              {assets.length} assets · {trafficPaths.length} traffic paths · {activeAlerts.length} active alerts
              {fleet.lastSync ? ` · synced ${fleet.lastSync.toLocaleTimeString()}` : ""}
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {[15, 60, 360, 1440].map((minutes) => (
            <button
              key={minutes}
              type="button"
              onClick={() => setRangeMinutes(minutes)}
              className={`rounded border px-2 py-1 text-[11px] ${
                rangeMinutes === minutes
                  ? "border-sky-500/50 bg-sky-500/15 text-sky-300"
                  : "border-slate-700 bg-slate-900 text-slate-400 hover:bg-slate-800"
              }`}
            >
              {minutesLabel(minutes)}
            </button>
          ))}
          <button
            type="button"
            onClick={() => navigate(`/partition/${encodeURIComponent(name)}/telemetry?range=${rangeMinutes}`)}
            className="btn text-xs"
            title="Open the full metrics, traces, logs, and graph explorer"
          >
            <ExternalLink size={13} /> Deep telemetry
          </button>
          <button type="button" onClick={() => void fleet.refresh()} className="btn text-xs" disabled={fleet.loading}>
            <RefreshCw size={13} className={fleet.loading ? "animate-spin" : ""} /> Refresh
          </button>
        </div>
      </header>

      {fleet.error && (
        <div className="mb-4 flex items-center gap-2 rounded-lg border border-rose-500/30 bg-rose-500/10 px-4 py-2.5 text-xs text-rose-300">
          <AlertTriangle size={14} /> {fleet.error}
        </div>
      )}

      <div className="mb-4 grid grid-cols-2 gap-2 md:grid-cols-3 xl:grid-cols-6">
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="flex items-center gap-1 text-[9px] uppercase tracking-wide text-slate-500"><Timer size={10} /> RTO</div>
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
          <div className="text-[9px] uppercase tracking-wide text-slate-500">p95 recovery</div>
          <div className="mt-1 text-sm font-bold text-slate-100">{formatDurationMs(rto.p95)}</div>
          <div className="text-[10px] text-slate-500">{rto.completed.length} completed</div>
        </div>
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="flex items-center gap-1 text-[9px] uppercase tracking-wide text-slate-500"><AlertTriangle size={10} /> firing alerts</div>
          <div className={`mt-1 text-sm font-bold ${activeAlerts.length > 0 ? "text-rose-300" : "text-emerald-300"}`}>{activeAlerts.length}</div>
          <button type="button" onClick={() => openAlerts()} className="text-[10px] text-slate-500 hover:text-slate-300">open inbox →</button>
        </div>
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="flex items-center gap-1 text-[9px] uppercase tracking-wide text-slate-500"><Server size={10} /> assets</div>
          <div className="mt-1 text-sm font-bold text-slate-100">{assets.length}</div>
          <div className="text-[10px] text-slate-500">{counts.critical} critical · {counts.degraded} degraded</div>
        </div>
        <div className="rounded-lg border border-slate-800 bg-slate-900/60 p-3">
          <div className="flex items-center gap-1 text-[9px] uppercase tracking-wide text-slate-500"><Waypoints size={10} /> traffic paths</div>
          <div className="mt-1 text-sm font-bold text-sky-300">{trafficPaths.length}</div>
          <div className="text-[10px] text-slate-500">{fleet.explicitEdges ? "measured edges" : "inferred from nodes"}</div>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
        <div className="space-y-4">
          <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
            <div className="flex flex-wrap items-start justify-between gap-2">
              <div>
                <h2 className="flex items-center gap-2 text-sm font-semibold">
                  <Waypoints size={15} className="text-sky-400" /> Traffic between intents and assets
                </h2>
                <p className="mt-1 text-[11px] text-slate-500">
                  Click a node to focus it. Edge width follows request rate; dashed edges are intent membership.
                </p>
              </div>
              <span className="rounded border border-slate-700 px-2 py-0.5 text-[10px] text-slate-400">
                {trafficModel.explicitEdges ? "measured edges" : "inferred edges"}
              </span>
            </div>
            <div className="mt-3">
              <TrafficGraph
                model={trafficModel}
                selectedId={effectiveFocus}
                onSelect={setFocusId}
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
                    return (
                      <tr key={`${edge.from}-${edge.to}`} className="hover:bg-slate-800/40">
                        <td className="px-2 py-1.5">
                          <button type="button" onClick={() => setFocusId(edge.from)} className="text-slate-200 hover:text-slate-50">{from?.name ?? edge.from}</button>
                        </td>
                        <td className="px-2 py-1.5">
                          <button type="button" onClick={() => setFocusId(edge.to)} className="text-slate-300 hover:text-slate-100">{to?.name ?? edge.to}</button>
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

          <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
            <div className="flex items-center justify-between gap-2">
              <div>
                <h2 className="flex items-center gap-2 text-sm font-semibold"><Server size={15} className="text-sky-400" /> Assets</h2>
                <p className="mt-1 text-[11px] text-slate-500">Open an asset for traffic, RTO, telemetry, and alerts.</p>
              </div>
              <span className="text-[10px] text-slate-500">{assetsWithState.length} assets</span>
            </div>
            <div className="mt-3 overflow-x-auto">
              <table className="w-full text-left text-xs">
                <thead className="text-[10px] uppercase tracking-wide text-slate-500">
                  <tr><th className="px-2 py-1.5">Asset</th><th className="px-2 py-1.5">Intent</th><th className="px-2 py-1.5">Status</th><th className="px-2 py-1.5 text-right">RPS</th><th className="px-2 py-1.5">Alerts</th><th className="px-2 py-1.5" /></tr>
                </thead>
                <tbody className="divide-y divide-slate-800">
                  {assetsWithState.map((asset) => (
                    <tr key={asset.id} className="hover:bg-slate-800/40">
                      <td className="px-2 py-1.5">
                        <button type="button" onClick={() => setFocusId(asset.id)} className="text-left text-slate-200 hover:text-slate-50">{asset.name}</button>
                      </td>
                      <td className="px-2 py-1.5 text-slate-500">{asset.intent}</td>
                      <td className="px-2 py-1.5">
                        <span className={`rounded border px-1.5 py-0.5 text-[9px] uppercase ${statusClass(asset.status)}`}>{asset.status}</span>
                      </td>
                      <td className="px-2 py-1.5 text-right font-mono text-slate-300">{asset.rps.toFixed(2)}</td>
                      <td className="px-2 py-1.5">
                        {asset.alerts.length > 0 ? (
                          <button type="button" onClick={() => openAlerts(asset.alerts[0]?.fingerprint)} className="text-rose-300 hover:underline">
                            {asset.alerts.length} firing
                          </button>
                        ) : (
                          <span className="text-emerald-300">none</span>
                        )}
                      </td>
                      <td className="px-2 py-1.5 text-right">
                        <button type="button" onClick={() => setSelectedHub(asset)} className="rounded border border-slate-700 px-2 py-0.5 text-[10px] text-slate-300 hover:bg-slate-800">
                          Open
                        </button>
                      </td>
                    </tr>
                  ))}
                  {assetsWithState.length === 0 && (
                    <tr><td colSpan={6} className="px-2 py-6 text-center text-slate-500">No assets were reported for this partition.</td></tr>
                  )}
                </tbody>
              </table>
            </div>
          </section>
        </div>

        <div className="space-y-4">
          <RtoPanel summary={rto} targetMs={rtoTarget} onTargetChange={setRtoTarget} onOpenAlerts={() => openAlerts()} />

          <section className="rounded-xl border border-slate-800 bg-slate-900/60 p-4">
            <div className="flex items-center justify-between gap-2">
              <h2 className="flex items-center gap-2 text-sm font-semibold"><AlertTriangle size={15} className="text-rose-400" /> Firing alerts</h2>
              <span className="text-[10px] text-slate-500">{activeAlerts.length} active</span>
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
            <h2 className="text-sm font-semibold">Recent Guardian events</h2>
            <div className="mt-2 max-h-64 space-y-1.5 overflow-y-auto pr-1">
              {fleet.events.slice(0, 12).map((event, index) => (
                <div key={`${event.timestamp || "na"}-${index}`} className="rounded border border-slate-800 bg-slate-950/70 px-2 py-1.5">
                  <div className="flex items-center gap-2 text-[10px]">
                    <span className="truncate font-mono text-slate-400">{event.kind || event.status || "event"}</span>
                    <span className="ml-auto shrink-0 text-slate-600">{event.timestamp ? new Date(event.timestamp).toLocaleTimeString() : ""}</span>
                  </div>
                  <div className="truncate text-[10px] text-slate-500">{event.asset || event.intent || "component"} · {event.message || event.status || "—"}</div>
                </div>
              ))}
              {fleet.events.length === 0 && <p className="text-xs text-slate-500">No events for this window.</p>}
            </div>
          </section>
        </div>
      </div>
    </div>
  );
}
