import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Activity, CheckCircle2, RefreshCw, TriangleAlert, XCircle } from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import type { ProbeResult } from "../types/api";
import "./probes.css";

const PROBE_STALE_AFTER_MS = 3 * 60_000;

interface ProbesResponseWithSummary {
  probes: ProbeResult[];
  pusherSummary?: {
    total: number;
    available: number;
    down: number;
  };
}

export default function ProbesPage() {
  const navigate = useNavigate();
  const { config } = useConfig();
  const [probes, setProbes] = useState<ProbeResult[]>([]);
  const [pusherSummary, setPusherSummary] = useState<{ total: number; available: number; down: number } | null>(null);
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
      .then((response) => {
        const r = response as ProbesResponseWithSummary;
        setProbes(r.probes ?? []);
        setPusherSummary(r.pusherSummary ?? null);
      })
      .catch((err) => setError(err instanceof Error ? err.message : "Failed to load probes"))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => { load(); }, [load]);

  useEffect(() => {
    const id = window.setInterval(load, 30_000);
    return () => window.clearInterval(id);
  }, [load]);

  const { partitions, types, downPushers, pusherTotal } = useMemo(() => {
    const p = new Set<string>();
    const t = new Set<string>();
    const totalByPusher = new Map<string, number>();
    const staleByPusher = new Map<string, number>();
    for (const probe of probes) {
      if (probe.partition) p.add(probe.partition);
      if (probe.type) t.add(probe.type);
      const pusher = probe.pusher || "unknown";
      totalByPusher.set(pusher, (totalByPusher.get(pusher) ?? 0) + 1);
      if (probeStatus(probe) === "stale") {
        staleByPusher.set(pusher, (staleByPusher.get(pusher) ?? 0) + 1);
      }
    }
    return {
      partitions: [...p].sort(),
      types: [...t].sort(),
      downPushers: new Set(
        [...totalByPusher.keys()].filter((pusher) => (staleByPusher.get(pusher) ?? 0) === totalByPusher.get(pusher)),
      ),
      pusherTotal: totalByPusher.size,
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
  const stale = probes.filter((p) => probeStatus(p) === "stale").length;
  const pusherStats = pusherSummary ?? {
    total: pusherTotal,
    available: Math.max(0, pusherTotal - downPushers.size),
    down: downPushers.size,
  };
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
              : `${probes.length} probe${probes.length === 1 ? "" : "s"} · ${passing} passing · ${failing} failing · ${stale} stale`}
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
              <option value="stale">Stale / pusher down</option>
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
          <ProbeStat icon={<TriangleAlert size={14} />} label="Stale" value={stale} tone="warn" />
          <ProbeStat icon={<CheckCircle2 size={14} />} label="Pushers available" value={pusherStats.available} tone="ok" />
          <ProbeStat icon={<XCircle size={14} />} label="Pushers down" value={pusherStats.down} tone="err" />
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
              const pusherDown =
                status === "stale" && downPushers.has(probe.pusher || "unknown");
              const statusLabel =
                status === "passing" ? "Up" : status === "failing" ? "Down" : pusherDown ? "Pusher down" : "Stale";
              return (
                <div key={probe.probeID} className={`probe-row probe-row--${status}`}>
                  <span
                    className="probe-row__status"
                    title={
                      status === "stale"
                        ? `No fresh probe result in the last ${PROBE_STALE_AFTER_MS / 60_000} minutes`
                        : undefined
                    }
                  >
                    <span className={`probe-dot probe-dot--${status}`} />
                    {statusLabel}
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
                  <span className="probe-row__checked">
                    {formatAge(probe.checkedAt)}
                    {status === "stale" ? " · stale" : ""}
                  </span>
                  <span className="probe-row__actions">
                    <button
                      className="xp-live-btn"
                      onClick={() => navigate(`/services/${encodeURIComponent(probe.service)}`)}
                    >
                      Service
                    </button>
                    <button
                      className="xp-live-btn"
                      onClick={() => navigate(`/logs?service=${encodeURIComponent(probe.service)}`)}
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
  tone?: "ok" | "err" | "warn";
}) {
  return (
    <div className={`probe-stat${tone ? ` probe-stat--${tone}` : ""}`}>
      <span className="probe-stat__icon">{icon}</span>
      <span className="probe-stat__value">{value}</span>
      <span className="probe-stat__label">{label}</span>
    </div>
  );
}

function probeStatus(probe: ProbeResult): "passing" | "failing" | "stale" {
  const checked = new Date(probe.checkedAt).getTime();
  if (!probe.checkedAt || !Number.isFinite(checked) || Date.now() - checked > PROBE_STALE_AFTER_MS) {
    return "stale";
  }
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
