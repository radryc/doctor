import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import StatCard from "../components/ui/StatCard";
import type { GuardianHealthResponse } from "../types/api";

interface DashboardState {
  health: "loading" | "ok" | "error";
  version: string;
  buildDate: string;
  metricCount: number | null;
  serviceCount: number | null;
  traceCount: number | null;
  logCount: number | null;
  guardianHealth: GuardianHealthResponse | null;
  lastRefresh: Date | null;
  error: string;
}

const emptyState: DashboardState = {
  health: "loading",
  version: "-",
  buildDate: "-",
  metricCount: null,
  serviceCount: null,
  traceCount: null,
  logCount: null,
  guardianHealth: null,
  lastRefresh: null,
  error: "",
};

function formatCount(value: number | null): string {
  if (value === null) return "...";
  return new Intl.NumberFormat().format(value);
}

function formatBuild(value: string): string {
  if (!value || value === "-") return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString();
}

function levelVariant(level?: string): "default" | "err" | "warn" {
  if (level === "unhealthy" || level === "failing") return "err";
  if (level === "degraded" || level === "unknown" || level === "attention" || level === "pending") return "warn";
  return "default";
}

function guardianSummaryText(health: GuardianHealthResponse | null): string {
  const reason = health?.overall_health?.reason?.trim();
  const recommendation = health?.recommendation?.trim();
  const score = health?.overall_health?.score;

  if (reason && recommendation) {
    return `${reason} · ${recommendation}`;
  }
  if (reason) {
    return reason;
  }
  if (score === undefined) {
    return recommendation || "No status returned";
  }
  return `${Math.round(score * 100)}%${recommendation ? ` · ${recommendation}` : ""}`;
}

export default function DashboardPage() {
  const { config, loaded } = useConfig();
  const [state, setState] = useState<DashboardState>(emptyState);
  const [refreshing, setRefreshing] = useState(false);

  const tenant = config.default_tenant || "default";

  const load = useCallback(async () => {
    if (!loaded) return;

    setRefreshing(true);
    const to = new Date();
    const from = new Date(to.getTime() - 15 * 60_000);
    const rangeParams = new URLSearchParams({
      tenant,
      start: from.toISOString(),
      end: to.toISOString(),
    });
    const traceParams = new URLSearchParams({
      tenant,
      from: from.toISOString(),
      to: to.toISOString(),
      limit: "500",
    });
    const logParams = new URLSearchParams({
      tenant,
      from: from.toISOString(),
      to: to.toISOString(),
      limit: "500",
    });
    const guardianParams = new URLSearchParams({
      tenant,
      window: "1h",
    });
    if (config.default_partition) {
      guardianParams.set("partition", config.default_partition);
    }

    const [health, build, metrics, services, traces, logs, guardianHealth] =
      await Promise.allSettled([
        api.getHealth(),
        api.getBuildInfo(),
        api.getMetricNames(rangeParams),
        api.getLogServices(tenant),
        api.getRecentTraces(traceParams),
        api.searchLogs(logParams),
        api.getGuardianHealth(guardianParams),
      ]);

    const failures = [health, build, metrics, services, traces, logs, guardianHealth]
      .filter((result) => result.status === "rejected")
      .map((result) => (result as PromiseRejectedResult).reason)
      .map((reason) => (reason instanceof Error ? reason.message : String(reason)));

    const discoveredServices = new Set<string>();
    if (services.status === "fulfilled") {
      services.value.services.forEach((service) => service && discoveredServices.add(service));
    }
    if (traces.status === "fulfilled") {
      traces.value.traces.forEach((trace) => trace.service && discoveredServices.add(trace.service));
    }
    if (logs.status === "fulfilled") {
      logs.value.results.forEach((record) => record.service && discoveredServices.add(record.service));
    }

    setState({
      health: health.status === "fulfilled" && health.value.status === "ok" ? "ok" : "error",
      version:
        build.status === "fulfilled"
          ? build.value.data.version || build.value.data.revision?.slice(0, 12) || "-"
          : "-",
      buildDate:
        build.status === "fulfilled" ? formatBuild(build.value.data.buildDate || "") : "-",
      metricCount: metrics.status === "fulfilled" ? metrics.value.data.length : null,
      serviceCount: discoveredServices.size,
      traceCount: traces.status === "fulfilled" ? traces.value.traces.length : null,
      logCount: logs.status === "fulfilled" ? logs.value.results.length : null,
      guardianHealth: guardianHealth.status === "fulfilled" ? guardianHealth.value : null,
      lastRefresh: new Date(),
      error: failures[0] || "",
    });
    setRefreshing(false);
  }, [loaded, tenant, config.default_partition]);

  useEffect(() => {
    void load();
  }, [load]);

  const guardianLevel = state.guardianHealth?.overall_health?.level ?? "unknown";
  const statusLabel = state.health === "ok" ? "Healthy" : state.health === "error" ? "Error" : "Loading";
  const signals = useMemo(
    () => [
      { label: "Recent traces", value: formatCount(state.traceCount), to: "/traces" },
      { label: "Recent logs", value: formatCount(state.logCount), to: "/logs" },
      { label: "Metric names", value: formatCount(state.metricCount), to: "/metrics" },
      { label: "Log services", value: formatCount(state.serviceCount), to: "/logs" },
    ],
    [state.traceCount, state.logCount, state.metricCount, state.serviceCount]
  );

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Dashboard</h2>
          <p>Query-plane health and recent observability inventory.</p>
        </div>
        <div className="xp-toolbar">
          {state.lastRefresh && (
            <span className="panel-badge">{state.lastRefresh.toLocaleTimeString()}</span>
          )}
          <button
            type="button"
            className={"refresh-btn" + (refreshing ? " spinning" : "")}
            onClick={() => void load()}
          >
            <span className="spin-icon">↻</span>
            Refresh
          </button>
        </div>
      </div>

      <div className="dashboard-body">
        {state.error && <div className="dash-error">{state.error}</div>}

        <div className="stat-grid stat-grid--wide">
          <StatCard
            label="Doctor API"
            value={statusLabel}
            sub="GET /healthz"
            variant={state.health === "error" ? "err" : "default"}
          />
          <StatCard label="Build" value={state.version} sub={state.buildDate} />
          <StatCard
            label="Guardian health"
            value={guardianLevel}
            sub={guardianSummaryText(state.guardianHealth)}
            variant={levelVariant(guardianLevel)}
          />
          <StatCard label="Tenant" value={tenant} sub={config.default_partition || "all partitions"} />
        </div>

        <div className="dashboard-links dashboard-links--compact">
          {signals.map((signal) => (
            <Link key={signal.label} to={signal.to} className="dashboard-link-card">
              <span className="dashboard-link-card__eyebrow">Last 15m</span>
              <strong className="dashboard-link-card__title">{signal.value}</strong>
              <span className="dashboard-link-card__text">{signal.label}</span>
            </Link>
          ))}
        </div>

        <div className="dashboard-grid">
          <section className="chart-card">
            <div className="chart-card__title">Health Signals</div>
            <div className="health-list">
              <div className="health-row">
                <span className={`status-dot status-dot-${state.health === "ok" ? "healthy" : "failing"}`} />
                <span>Doctor query plane</span>
                <strong>{statusLabel}</strong>
              </div>
              <div className="health-row">
                <span className={`status-dot status-dot-${guardianLevel}`} />
                <span>Guardian rollout feedback</span>
                <strong>{guardianLevel}</strong>
              </div>
              <div className="health-row">
                <span className="status-dot status-dot-pending" />
                <span>Metric discovery</span>
                <strong>{formatCount(state.metricCount)}</strong>
              </div>
              <div className="health-row">
                <span className="status-dot status-dot-pending" />
                <span>Observed services</span>
                <strong>{formatCount(state.serviceCount)}</strong>
              </div>
            </div>
          </section>

          <section className="chart-card">
            <div className="chart-card__title">Investigation Surfaces</div>
            <div className="surface-actions">
              <Link to="/metrics" className="surface-action">
                <strong>Metrics</strong>
                <span>Prometheus query and graph console</span>
              </Link>
              <Link to="/traces" className="surface-action">
                <strong>Traces</strong>
                <span>Recent traces and waterfall payloads</span>
              </Link>
              <Link to="/logs" className="surface-action">
                <strong>Logs</strong>
                <span>Service, severity, and text filters</span>
              </Link>
            </div>
          </section>
        </div>
      </div>
    </div>
  );
}