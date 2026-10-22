import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Activity, CheckCircle2, RefreshCw, TriangleAlert, XCircle } from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import type { ProbeResult } from "../types/api";

export default function ProbesPage() {
  const navigate = useNavigate();
  const { config } = useConfig();
  const [probes, setProbes] = useState<ProbeResult[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [partitionFilter, setPartitionFilter] = useState("");
  const [typeFilter, setTypeFilter] = useState("");

  const load = useCallback(() => {
    setLoading(true);
    setError("");
    api
      .getProbes()
      .then((r) => setProbes(r.probes ?? []))
      .catch((err) => setError(err instanceof Error ? err.message : "Failed to load probes"))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => { load(); }, [load]);

  useEffect(() => {
    const id = window.setInterval(load, 30_000);
    return () => window.clearInterval(id);
  }, [load]);

  const { partitions, types } = useMemo(() => {
    const p = new Set<string>();
    const t = new Set<string>();
    for (const probe of probes) {
      if (probe.partition) p.add(probe.partition);
      if (probe.type) t.add(probe.type);
    }
    return {
      partitions: [...p].sort(),
      types: [...t].sort(),
    };
  }, [probes]);

  const filtered = probes.filter((probe) => {
    if (partitionFilter && probe.partition !== partitionFilter) return false;
    if (typeFilter && probe.type !== typeFilter) return false;
    if (statusFilter && probeStatus(probe) !== statusFilter) return false;
    if (search) {
      const q = search.toLowerCase();
      return (
        probe.service.toLowerCase().includes(q) ||
        probe.name.toLowerCase().includes(q) ||
        probe.target.toLowerCase().includes(q) ||
        probe.asset.toLowerCase().includes(q) ||
        (probe.fullName ?? "").toLowerCase().includes(q)
      );
    }
    return true;
  });

  const passing = probes.filter((p) => probeStatus(p) === "passing").length;
  const failing = probes.filter((p) => probeStatus(p) === "failing").length;
  const avgDuration =
    probes.length > 0
      ? Math.round(probes.reduce((sum, p) => sum + p.durationMs, 0) / probes.length)
      : 0;

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Synthetic Probes</h2>
          <p>
            {loading
              ? "Loading…"
              : `${probes.length} probe${probes.length === 1 ? "" : "s"} · ${passing} passing · ${failing} failing`}
          </p>
        </div>
        <div className="xp-toolbar">
          <input
            type="text"
            className="xp-search"
            placeholder="Search probes…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          <label className="filter-label">
            Partition
            <select className="xp-select" value={partitionFilter} onChange={(e) => setPartitionFilter(e.target.value)}>
              <option value="">All</option>
              {partitions.map((partition) => <option key={partition} value={partition}>{partition}</option>)}
            </select>
          </label>
          <label className="filter-label">
            Type
            <select className="xp-select" value={typeFilter} onChange={(e) => setTypeFilter(e.target.value)}>
              <option value="">All</option>
              {types.map((type) => <option key={type} value={type}>{type}</option>)}
            </select>
          </label>
          <label className="filter-label">
            Status
            <select className="xp-select" value={statusFilter} onChange={(e) => setStatusFilter(e.target.value)}>
              <option value="">All</option>
              <option value="passing">Passing</option>
              <option value="failing">Failing</option>
            </select>
          </label>
          <button className="xp-live-btn" onClick={load} disabled={loading}>
            <RefreshCw size={12} className={loading ? "animate-spin" : ""} /> Refresh
          </button>
          <button
            className="xp-live-btn"
            onClick={() =>
              navigate(`/metrics?query=${encodeURIComponent("guardian_probe_up")}&tenant=${encodeURIComponent(config.default_tenant)}`)
            }
            title="Open default probe availability metric"
          >
            <Activity size={12} /> Probe metrics
          </button>
        </div>
      </div>

      <div className="panel-body probes-body">
        <div className="probe-stats">
          <ProbeStat icon={<Activity size={14} />} label="Total probes" value={probes.length} />
          <ProbeStat icon={<CheckCircle2 size={14} />} label="Passing" value={passing} tone="ok" />
          <ProbeStat icon={<XCircle size={14} />} label="Failing" value={failing} tone="err" />
          <ProbeStat icon={<TriangleAlert size={14} />} label="Avg latency" value={`${avgDuration}ms`} />
        </div>

        {error ? (
          <div className="dash-error">{error}</div>
        ) : loading && probes.length === 0 ? (
          <div className="xp-empty">Loading probes…</div>
        ) : filtered.length === 0 ? (
          <div className="xp-empty">
            No synthetic probes found{probes.length > 0 ? " matching the current filters" : ""}.
          </div>
        ) : (
          <div className="probe-table">
            <div className="probe-table__head">
              <span>Status</span>
              <span>Probe / Service</span>
              <span>Target</span>
              <span>Partition / Asset</span>
              <span>Latency</span>
              <span>Failures</span>
              <span>Checked</span>
              <span />
            </div>
            {filtered.map((probe) => {
              const status = probeStatus(probe);
              return (
                <div key={probe.probeID} className={`probe-row probe-row--${status}`}>
                  <span className="probe-row__status">
                    <span className={`probe-dot probe-dot--${status}`} />
                    {status === "passing" ? "Up" : "Down"}
                  </span>
                  <span className="probe-row__identity">
                    <strong>{probe.name || probe.service}</strong>
                    <small>{probe.service}{probe.fullName && probe.fullName !== probe.service ? ` · ${probe.fullName}` : ""}</small>
                  </span>
                  <span className="probe-row__target" title={probe.target}>
                    <span className="probe-kind">{probe.type?.toUpperCase() || "HTTP"}</span>
                    {probe.target}
                  </span>
                  <span className="probe-row__placement">
                    {probe.partition || "—"}
                    <small>{[probe.intent, probe.asset].filter(Boolean).join(" / ") || "—"}</small>
                  </span>
                  <span className={`probe-row__latency${probe.durationMs > 1000 ? " is-slow" : ""}`}>
                    {probe.durationMs}ms
                  </span>
                  <span className="probe-row__failures">
                    {probe.consecutiveFailures > 0 ? probe.consecutiveFailures : "—"}
                  </span>
                  <span className="probe-row__checked">{formatAge(probe.checkedAt)}</span>
                  <span className="probe-row__actions">
                    <button
                      className="xp-live-btn"
                      onClick={() => navigate(`/services/${encodeURIComponent(probe.service)}`)}
                    >
                      Service
                    </button>
                    <button
                      className="xp-live-btn"
                      onClick={() => navigate(`/logs?q=${encodeURIComponent(probe.service)}`)}
                    >
                      Logs
                    </button>
                    <button
                      className="xp-live-btn"
                      onClick={() =>
                        navigate(
                          `/metrics?query=${encodeURIComponent(
                            `guardian_probe_up{service="${probe.service}"}`
                          )}&tenant=${encodeURIComponent(config.default_tenant)}`
                        )
                      }
                    >
                      Metrics
                    </button>
                  </span>
                  {probe.error && (
                    <div className="probe-row__error" title={probe.error}>
                      {probe.error}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}

function ProbeStat({
  icon,
  label,
  value,
  tone,
}: {
  icon: React.ReactNode;
  label: string;
  value: number | string;
  tone?: "ok" | "err";
}) {
  return (
    <div className={`probe-stat${tone ? ` probe-stat--${tone}` : ""}`}>
      <span className="probe-stat__icon">{icon}</span>
      <span className="probe-stat__value">{value}</span>
      <span className="probe-stat__label">{label}</span>
    </div>
  );
}

function probeStatus(probe: ProbeResult): "passing" | "failing" {
  const status = (probe.status ?? "").toLowerCase();
  if (probe.success || status === "up" || status === "passing" || status === "ok" || status === "healthy") {
    return "passing";
  }
  return "failing";
}

function formatAge(value: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (isNaN(date.getTime())) return "—";
  const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m ago`;
  if (seconds < 86_400) return `${Math.round(seconds / 3600)}h ago`;
  return date.toLocaleDateString();
}
