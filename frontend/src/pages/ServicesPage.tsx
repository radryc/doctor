import { useCallback, useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { ChevronLeft, BarChart3, FileText, GitBranch } from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import { useInspector } from "../context/InspectorContext";
import { useTimeRange } from "../context/TimeRangeContext";
import type { DiscoveredService, LogRecord } from "../types/api";

// Services are emitted with both short names ("doctor") and canonical names
// ("doctor.query.query"). Match the exact service or any dot-prefixed child.
// Dots in the service name are written as [.] so the expression stays valid in
// both PromQL and TraceQL string literals without backslash escaping.
function escapeServiceRegex(value: string): string {
  return value.replaceAll(".", "[.]");
}

function promServiceMatcher(name: string): string {
  return `${escapeServiceRegex(name)}([.].*)?`;
}

function traceServiceMatcher(name: string): string {
  return `^${escapeServiceRegex(name)}([.].*)?`;
}

// ── APM Service Catalog (/services) ──────────────────────────────────────────

export default function ServicesPage() {
  const { name } = useParams<{ name?: string }>();
  if (name) return <ServiceDetail name={decodeURIComponent(name)} />;
  return <ServiceCatalog />;
}

function ServiceCatalog() {
  const { config } = useConfig();
  const navigate = useNavigate();
  const tr = useTimeRange();
  const { from, to } = tr.resolve();

  const [services, setServices] = useState<DiscoveredService[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [partitionFilter, setPartitionFilter] = useState("");
  const [partitions, setPartitions] = useState<string[]>([]);

  const load = useCallback(() => {
    setLoading(true);
    setError("");
    const range = `${Math.ceil((to.getTime() - from.getTime()) / 60000)}m`;
    const params = new URLSearchParams({
      tenant: config.default_tenant,
      window: range,
    });
    api
      .discoverServices(params)
      .then((r) => {
        const svcs = r.services ?? [];
        setServices(svcs);
        const parts = [...new Set(svcs.map((s) => s.partition ?? "").filter(Boolean))].sort();
        setPartitions(parts);
      })
      .catch((err) => {
        setError(err instanceof Error ? err.message : "Failed to discover services");
      })
      .finally(() => setLoading(false));
  }, [config.default_tenant, from, to]);

  useEffect(() => { load(); }, [load, tr.refreshTick]);

  const serviceName = (s: DiscoveredService) => s.name || s.service || "";

  const filtered = services.filter((s) => {
    if (partitionFilter && s.partition !== partitionFilter) return false;
    if (search) {
      const q = search.toLowerCase();
      return (
        serviceName(s).toLowerCase().includes(q) ||
        (s.full_name ?? "").toLowerCase().includes(q) ||
        (s.partition ?? "").toLowerCase().includes(q) ||
        (s.intent ?? "").toLowerCase().includes(q)
      );
    }
    return true;
  });

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>APM Service Catalog</h2>
          <p>{loading ? "Loading…" : `${filtered.length} service${filtered.length !== 1 ? "s" : ""}`}</p>
        </div>
        <div className="xp-toolbar">
          <input
            type="text"
            className="xp-search"
            placeholder="Search services…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          {partitions.length > 0 && (
            <label className="filter-label">
              Partition
              <select
                className="xp-select"
                value={partitionFilter}
                onChange={(e) => setPartitionFilter(e.target.value)}
              >
                <option value="">All</option>
                {partitions.map((p) => (
                  <option key={p} value={p}>{p}</option>
                ))}
              </select>
            </label>
          )}
        </div>
      </div>

      <div className="panel-body">
        <div className="xp-table-header services-header">
          <span>Service</span>
          <span>Partition</span>
          <span>Signals</span>
          <span>Traces</span>
          <span>Logs</span>
          <span>Error %</span>
        </div>
        <div className="xp-rows">
          {loading ? (
            <div className="xp-empty">Loading…</div>
          ) : error ? (
            <div className="xp-empty">Could not discover services: {error}</div>
          ) : filtered.length === 0 ? (
            <div className="xp-empty">No services discovered in this time range.</div>
          ) : (
            filtered.map((svc) => {
              const name = serviceName(svc);
              return (
                <div
                  key={name}
                  className="xp-row services-row"
                  onClick={() => navigate(`/services/${encodeURIComponent(name)}`)}
                  style={{ cursor: "pointer" }}
                >
                  <span className="services-row__name">
                    {name}
                    {svc.truncated && <span className="signal-badge signal-badge--truncated" title="Name truncated by X-Ray">T</span>}
                  </span>
                  <span className="services-row__partition">
                    {svc.partition ?? "—"}
                    {svc.intent ? <small>{svc.intent}</small> : null}
                  </span>
                  <span className="services-row__signals">
                    {svc.has_metrics && <span className="signal-badge signal-badge--m" title="Metrics">M</span>}
                    {svc.has_traces && <span className="signal-badge signal-badge--t" title="Traces">T</span>}
                    {svc.has_logs && <span className="signal-badge signal-badge--l" title="Logs">L</span>}
                  </span>
                  <span className="services-row__traces">{svc.trace_count ?? "—"}</span>
                  <span className="services-row__logs">{svc.log_count ?? "—"}</span>
                  <span
                    className="services-row__error"
                    style={{
                      color:
                        svc.error_rate != null && svc.error_rate > 0.05
                          ? "var(--err)"
                          : svc.error_rate != null && svc.error_rate > 0.01
                          ? "var(--warn)"
                          : undefined,
                    }}
                  >
                    {svc.error_rate != null ? `${(svc.error_rate * 100).toFixed(2)}%` : "—"}
                  </span>
                </div>
              );
            })
          )}
        </div>
      </div>
    </div>
  );
}

// ── Service Detail (/services/:name) ─────────────────────────────────────────

function ServiceDetail({ name }: { name: string }) {
  const navigate = useNavigate();
  const { config } = useConfig();
  const promSvc = promServiceMatcher(name);
  const traceSvc = traceServiceMatcher(name);

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left" style={{ display: "flex", alignItems: "center", gap: "0.5rem" }}>
          <button className="icon-btn" onClick={() => navigate("/services")} title="Back to catalog">
            <ChevronLeft size={16} />
          </button>
          <h2>{name}</h2>
        </div>
        <div className="xp-toolbar">
          <button
            className="xp-live-btn"
            onClick={() => navigate(`/logs?service=${encodeURIComponent(name)}`)}
          >
            <FileText size={12} /> Logs
          </button>
          <button
            className="xp-live-btn"
            onClick={() => navigate(`/traces?service=${encodeURIComponent(name)}`)}
          >
            <GitBranch size={12} /> Traces
          </button>
          <button
            className="xp-live-btn"
            onClick={() =>
              navigate(
                `/metrics?query=${encodeURIComponent(`{service=~"${promSvc}"}`)}&tenant=${encodeURIComponent(config.default_tenant)}`
              )
            }
          >
            <BarChart3 size={12} /> Metrics
          </button>
        </div>
      </div>

      <div className="panel-body service-detail">
        <div className="service-detail__red">
          <REDCard
            label="Request Rate"
            expr={`sum(rate(http_server_request_duration_count{service=~"${promSvc}"}[$__rate_interval])) or sum(rate(http_requests_total{service=~"${promSvc}"}[$__rate_interval]))`}
            traceqlExpr={`{resource.service.name=~"${traceSvc}"} | rate()`}
            traceqlAgg="sum"
            unit="reqps"
            tenant={config.default_tenant}
          />
          <REDCard
            label="Error Rate"
            expr={`(sum(rate(http_server_request_duration_count{service=~"${promSvc}",http_response_status_code=~"5.."}[$__rate_interval])) or sum(rate(http_requests_total{service=~"${promSvc}",status_code=~"5.."}[$__rate_interval]))) / (sum(rate(http_server_request_duration_count{service=~"${promSvc}"}[$__rate_interval])) or sum(rate(http_requests_total{service=~"${promSvc}"}[$__rate_interval]))) * 100`}
            traceqlExpr={`{resource.service.name=~"${traceSvc}" && status = error} | rate()`}
            traceqlDenominator={`{resource.service.name=~"${traceSvc}"} | rate()`}
            traceqlAgg="ratio"
            unit="%"
            tenant={config.default_tenant}
          />
          <REDCard
            label="P95 Latency"
            expr={`histogram_quantile(0.95, sum(rate(http_server_request_duration_bucket{service=~"${promSvc}"}[$__rate_interval])) by (le)) * 1000 or histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket{service=~"${promSvc}"}[$__rate_interval])) by (le)) * 1000`}
            traceqlExpr={`{resource.service.name=~"${traceSvc}"} | quantile_over_time(duration, 0.95)`}
            traceqlAgg="max"
            traceqlScale={1e-6}
            unit="ms"
            tenant={config.default_tenant}
          />
          <REDCard
            label="P99 Latency"
            expr={`histogram_quantile(0.99, sum(rate(http_server_request_duration_bucket{service=~"${promSvc}"}[$__rate_interval])) by (le)) * 1000 or histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket{service=~"${promSvc}"}[$__rate_interval])) by (le)) * 1000`}
            traceqlExpr={`{resource.service.name=~"${traceSvc}"} | quantile_over_time(duration, 0.99)`}
            traceqlAgg="max"
            traceqlScale={1e-6}
            unit="ms"
            tenant={config.default_tenant}
          />
        </div>
        <ProbeCards service={name} />
        <div className="service-detail__recent">
          <div>
            <h3 style={{ fontSize: "0.85rem", opacity: 0.7, marginBottom: "0.5rem" }}>Recent Traces</h3>
            <RecentTraces service={name} tenant={config.default_tenant} />
          </div>
          <div>
            <h3 style={{ fontSize: "0.85rem", opacity: 0.7, marginBottom: "0.5rem" }}>Recent Logs</h3>
            <RecentLogs service={name} tenant={config.default_tenant} />
          </div>
        </div>
      </div>
    </div>
  );
}

interface REDCardProps {
  label: string;
  expr: string;
  traceqlExpr?: string;
  traceqlDenominator?: string;
  traceqlAgg?: "sum" | "max" | "ratio";
  traceqlScale?: number;
  unit: string;
  tenant: string;
}

function REDCard({
  label,
  expr,
  traceqlExpr,
  traceqlDenominator,
  traceqlAgg = "sum",
  traceqlScale = 1,
  unit,
  tenant,
}: REDCardProps) {
  const tr = useTimeRange();
  const { from, to } = tr.resolve();
  const [value, setValue] = useState<string>("…");

  useEffect(() => {
    let active = true;
    const step = tr.stepInterval();
    const resolvedExpr = expr.replace(/\[\$__rate_interval\]/g, `[${step}]`);
    const params = new URLSearchParams({
      query: resolvedExpr,
      time: (to.getTime() / 1000).toFixed(0),
    });

    const set = (v: number | null) => {
      if (!active) return;
      setValue(v == null || !Number.isFinite(v) ? "—" : `${v.toFixed(unit === "ms" ? 1 : 2)}${unit === "reqps" ? "/s" : unit}`);
    };

    const traceMetrics = (q: string): Promise<number | null> => {
      const queryParams = new URLSearchParams({
        q,
        start: (from.getTime() / 1000).toFixed(0),
        end: (to.getTime() / 1000).toFixed(0),
        step,
        tenant,
      });
      return api.queryTempoMetricsRange(queryParams).then((r) => {
        const values: number[] = [];
        for (const series of r.series ?? []) {
          const last = series.samples[series.samples.length - 1];
          if (last && Number.isFinite(last.value)) values.push(last.value);
        }
        if (values.length === 0) return null;
        const raw = traceqlAgg === "max" ? Math.max(...values) : values.reduce((sum, v) => sum + v, 0);
        return raw * traceqlScale;
      });
    };

    const fallback = () => {
      if (!traceqlExpr) {
        set(null);
        return;
      }
      if (traceqlAgg === "ratio" && traceqlDenominator) {
        Promise.all([traceMetrics(traceqlExpr), traceMetrics(traceqlDenominator)])
          .then(([num, den]) => {
            if (!active) return;
            set(num != null && den != null && den > 0 ? (num / den) * 100 : null);
          })
          .catch(() => set(null));
        return;
      }
      traceMetrics(traceqlExpr)
        .then((v) => set(v))
        .catch(() => set(null));
    };

    api
      .queryPrometheus(params)
      .then((r) => {
        if (!active) return;
        if (r.data.resultType === "vector" && r.data.result.length > 0) {
          const parsed = parseFloat(r.data.result[0].value[1]);
          if (Number.isFinite(parsed)) {
            set(parsed);
            return;
          }
        }
        fallback();
      })
      .catch(fallback);

    return () => { active = false; };
  }, [expr, traceqlExpr, traceqlDenominator, traceqlAgg, traceqlScale, unit, tenant, tr.refreshTick, from, to, tr]);

  return (
    <div className="red-card">
      <div className="red-card__label">{label}</div>
      <div className="red-card__value">{value}</div>
    </div>
  );
}

function formatProbeAge(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  if (seconds < 60) return `${Math.round(seconds)}s ago`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m ago`;
  return `${Math.round(seconds / 3600)}h ago`;
}

function ProbeCards({ service }: { service: string }) {
  const tr = useTimeRange();
  const [values, setValues] = useState<{
    up: boolean | null;
    latencyMs: number | null;
    failures: number | null;
    lastSuccessAge: number | null;
  } | null>(null);

  useEffect(() => {
    let active = true;
    const matcher = promServiceMatcher(service);
    const queryValue = (expr: string): Promise<number | null> => {
      const params = new URLSearchParams({
        query: expr,
        time: (Date.now() / 1000).toFixed(0),
      });
      return api
        .queryPrometheus(params)
        .then((r) => {
          if (r.data.resultType !== "vector" || r.data.result.length === 0) return null;
          const value = Number(r.data.result[0].value[1]);
          return Number.isFinite(value) ? value : null;
        })
        .catch(() => null);
    };

    Promise.all([
      queryValue(`max(guardian_probe_up{service=~"${matcher}"})`),
      queryValue(`max(guardian_probe_duration_seconds{service=~"${matcher}"})`),
      queryValue(`max(guardian_probe_consecutive_failures{service=~"${matcher}"})`),
      queryValue(`time() - max(guardian_probe_last_success_timestamp_seconds{service=~"${matcher}"})`),
    ]).then(([up, latency, failures, lastSuccessAge]) => {
      if (!active) return;
      if (up == null && latency == null && failures == null && lastSuccessAge == null) {
        setValues(null);
        return;
      }
      setValues({
        up: up == null ? null : up > 0,
        latencyMs: latency == null ? null : latency * 1000,
        failures,
        lastSuccessAge,
      });
    });

    return () => {
      active = false;
    };
  }, [service, tr.refreshTick]);

  if (!values) return null;

  return (
    <div className="service-detail__probes">
      <h3 style={{ fontSize: "0.85rem", opacity: 0.7, marginBottom: "0.5rem" }}>
        Synthetic Probe Health
      </h3>
      <div className="service-detail__red">
        <div className="red-card">
          <div className="red-card__label">Probe Status</div>
          <div
            className="red-card__value"
            style={{ color: values.up == null ? undefined : values.up ? "var(--ui-green)" : "var(--ui-red)" }}
          >
            {values.up == null ? "—" : values.up ? "Up" : "Down"}
          </div>
        </div>
        <div className="red-card">
          <div className="red-card__label">Probe Latency</div>
          <div className="red-card__value">
            {values.latencyMs == null ? "—" : `${values.latencyMs.toFixed(0)}ms`}
          </div>
        </div>
        <div className="red-card">
          <div className="red-card__label">Consecutive Failures</div>
          <div className="red-card__value">{values.failures == null ? "—" : values.failures.toFixed(0)}</div>
        </div>
        <div className="red-card">
          <div className="red-card__label">Last Success</div>
          <div className="red-card__value">
            {values.lastSuccessAge == null ? "—" : formatProbeAge(values.lastSuccessAge)}
          </div>
        </div>
      </div>
    </div>
  );
}

function RecentTraces({ service, tenant }: { service: string; tenant: string }) {
  const tr = useTimeRange();
  const { from, to } = tr.resolve();
  const { open } = useInspector();
  const [traces, setTraces] = useState<Array<{ trace_id: string; span_count: number; min_time: string; duration_ms?: number }>>([]);

  useEffect(() => {
    const params = new URLSearchParams({
      tenant,
      service,
      from: from.toISOString(),
      to: to.toISOString(),
      limit: "20",
    });
    api
      .getRecentTraces(params)
      .then((r) => setTraces(r.traces ?? []))
      .catch(() => setTraces([]));
  }, [service, tenant, from, to, tr.refreshTick]);

  if (traces.length === 0)
    return <div className="xp-empty">No recent traces. Services covered only by synthetic probes have probe health above but no spans.</div>;

  const openTrace = (traceId: string) => open({ kind: "trace", id: traceId, tenant });

  return (
    <div>
      {traces.map((t) => (
        <div
          key={t.trace_id}
          className="trace-row"
          role="button"
          tabIndex={0}
          title="Open trace"
          onClick={() => openTrace(t.trace_id)}
          onKeyDown={(event) => {
            if (event.key === "Enter" || event.key === " ") {
              event.preventDefault();
              openTrace(t.trace_id);
            }
          }}
        >
          <code className="trace-row__id">{t.trace_id.slice(0, 16)}…</code>
          <span>{t.duration_ms != null ? `${t.duration_ms}ms` : `${t.span_count} spans`}</span>
          <span>{new Date(t.min_time).toLocaleTimeString()}</span>
        </div>
      ))}
    </div>
  );
}

function RecentLogs({ service, tenant }: { service: string; tenant: string }) {
  const tr = useTimeRange();
  const { from, to } = tr.resolve();
  const { open } = useInspector();
  const [logs, setLogs] = useState<LogRecord[]>([]);

  useEffect(() => {
    const params = new URLSearchParams({
      tenant,
      service,
      from: from.toISOString(),
      to: to.toISOString(),
      limit: "50",
    });
    api
      .searchLogs(params)
      .then((r) => setLogs(r.results ?? []))
      .catch(() => setLogs([]));
  }, [service, tenant, from, to, tr.refreshTick]);

  if (logs.length === 0)
    return <div className="xp-empty">No recent logs. Synthetic probes do not emit application logs; use probe health above.</div>;

  return (
    <div className="dash-logs">
      {logs.slice(0, 50).map((log, i) => (
        <div
          key={i}
          className="dash-log-row dash-log-row--clickable"
          role="button"
          tabIndex={0}
          title="Open full log details"
          onClick={() =>
            open({
              kind: "log",
              id: log.trace_id || `${log.service || "log"}@${log.timestamp}`,
              tenant,
              log,
            })
          }
          onKeyDown={(event) => {
            if (event.key === "Enter" || event.key === " ") {
              event.preventDefault();
              open({
                kind: "log",
                id: log.trace_id || `${log.service || "log"}@${log.timestamp}`,
                tenant,
                log,
              });
            }
          }}
        >
          <span className="dash-log-ts">{new Date(log.timestamp).toLocaleTimeString()}</span>
          <span className={`dash-log-sev dash-log-sev--${(log.severity_text ?? "INFO").toLowerCase()}`}>
            {(log.severity_text ?? "INFO").slice(0, 4)}
          </span>
          <span className="dash-log-body">{log.body}</span>
        </div>
      ))}
    </div>
  );
}
