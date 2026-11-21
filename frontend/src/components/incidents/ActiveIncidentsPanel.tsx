import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { AlertTriangle, ArrowUpRight, CheckCircle2, ShieldCheck } from "lucide-react";
import { api } from "../../api/client";
import { useConfig } from "../../context/ConfigContext";
import type { Incident } from "../../types/incidents";

function age(value?: string): string {
  if (!value) return "now";
  const ms = Date.now() - new Date(value).getTime();
  if (!Number.isFinite(ms) || ms < 0) return "now";
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return hours < 48 ? `${hours}h` : `${Math.floor(hours / 24)}d`;
}

export default function ActiveIncidentsPanel() {
  const { config } = useConfig();
  const tenant = config.default_tenant || "default";
  const partition = config.default_partition || "default";
  const [incidents, setIncidents] = useState<Incident[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    const load = () => {
      const params = new URLSearchParams({ tenant, partition });
      api
        .getIncidents(params)
        .then((response) => {
          if (cancelled) return;
          setIncidents((response.incidents ?? []).filter((incident) => incident.state !== "resolved").slice(0, 6));
          setError("");
        })
        .catch((e) => {
          if (!cancelled) setError(e instanceof Error ? e.message : "Failed to load incidents");
        });
    };
    load();
    const timer = window.setInterval(load, 15_000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [tenant, partition]);

  return (
    <section className="glass rounded-xl p-5">
      <div className="flex flex-wrap items-center justify-between gap-3 mb-3">
        <h2 className="text-sm font-semibold text-gray-200 flex items-center gap-2">
          <AlertTriangle size={15} className="text-amber-400" />
          Active incidents
          {incidents.length > 0 && (
            <span className="text-[10px] px-2 py-0.5 rounded-full bg-amber-500/10 border border-amber-500/30 text-amber-400 font-bold">
              {incidents.length}
            </span>
          )}
        </h2>
        <Link className="triage-action triage-action--primary" to="/incidents">
          Open incident board <ArrowUpRight size={12} />
        </Link>
      </div>
      {error ? (
        <p className="text-xs text-red-400">{error}</p>
      ) : incidents.length === 0 ? (
        <p className="text-xs text-gray-500 py-3">
          No active incidents for {partition}. Alert sets on the incident board turn critical alerts into trackable incidents.
        </p>
      ) : (
        <div className="divide-y divide-dark-600">
          {incidents.map((incident) => (
            <Link
              key={incident.id}
              to={`/incidents/${encodeURIComponent(incident.id)}?partition=${encodeURIComponent(incident.partition)}`}
              className="flex items-center gap-3 py-2.5 text-sm"
            >
              {incident.state === "mitigated" ? (
                <ShieldCheck size={14} className="text-emerald-400" />
              ) : incident.state === "acknowledged" ? (
                <CheckCircle2 size={14} className="text-amber-400" />
              ) : (
                <AlertTriangle size={14} className="text-red-400" />
              )}
              <div className="min-w-0 flex-1">
                <strong className="block truncate text-gray-200 font-medium">{incident.title || incident.set}</strong>
                <span className="text-[11px] text-gray-500">
                  {incident.set} · {incident.groupKey} · {incident.memberAlerts?.length ?? 0} alert(s)
                </span>
              </div>
              <span className="text-[10px] uppercase tracking-wide text-gray-500">{incident.state}</span>
              <span className="text-[11px] tabular-nums text-gray-500">{age(incident.openedAt)}</span>
              <ArrowUpRight size={12} className="text-gray-600" />
            </Link>
          ))}
        </div>
      )}
    </section>
  );
}
