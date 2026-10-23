import { type CSSProperties, useEffect, useMemo, useState } from "react";
import { Pencil, Trash2 } from "lucide-react";
import { api } from "../../api/client";
import { interpolate } from "../../lib/variables";
import type {
  DashboardWidget,
  VariableValues,
} from "../../types/dashboard";
import type {
  LogRecord,
  LokiMatrixSeries,
  LokiStream,
  MetricSeries,
  PrometheusMatrixResult,
  ProxyEndpointInfo,
  TempoMetricLabel,
  TempoTimeSeries,
  TraceSummary,
} from "../../types/api";
import { useTimeRange } from "../../context/TimeRangeContext";
import { useInspector } from "../../context/InspectorContext";
import TimeSeriesChart from "../charts/TimeSeriesChart";
import ChartExpandModal from "../charts/ChartExpandModal";

interface Props {
  widget: DashboardWidget;
  tenant: string;
  variableValues: VariableValues;
  editMode?: boolean;
  onEdit?: () => void;
  onRemove?: () => void;
}

interface Macros {
  interval: string;
  range: string;
  tenant: string;
}

export default function DashboardWidgetCard({
  widget,
  tenant,
  variableValues,
  editMode,
  onEdit,
  onRemove,
}: Props) {
  const tr = useTimeRange();
  const { from, to } = tr.resolve();
  const interval = tr.stepInterval();
  const range = tr.rangeString();
  const macros = useMemo<Macros>(
    () => ({ interval, range, tenant }),
    [interval, range, tenant]
  );

  return (
    <div className="dash-widget" style={gridStyle(widget)}>
      <div className="dash-widget__header">
        <span className="dash-widget__title">{widget.title}</span>
        {widget.description && (
          <span className="dash-widget__desc" title={widget.description}>ℹ</span>
        )}
        {editMode && (
          <span className="dash-widget__actions">
            <button className="icon-btn" title="Edit widget" onClick={onEdit}>
              <Pencil size={11} />
            </button>
            <button className="icon-btn icon-btn--danger" title="Remove widget" onClick={onRemove}>
              <Trash2 size={11} />
            </button>
          </span>
        )}
      </div>
      <div className="dash-widget__body">
        <WidgetContent
          widget={widget}
          tenant={tenant}
          variableValues={variableValues}
          macros={macros}
          from={from}
          to={to}
          refreshTick={tr.refreshTick}
        />
      </div>
    </div>
  );
}

function gridStyle(widget: DashboardWidget): CSSProperties {
  const { x, y, w, h } = widget.gridPos;
  return {
    gridColumn: `${x + 1} / span ${w}`,
    gridRow: `${y + 1} / span ${h}`,
  };
}

interface ContentProps {
  widget: DashboardWidget;
  tenant: string;
  variableValues: VariableValues;
  macros: Macros;
  from: Date;
  to: Date;
  refreshTick: number;
}

function WidgetContent({ widget, ...props }: ContentProps) {
  switch (widget.kind) {
    case "markdown":
      return <MarkdownWidget content={widget.visualization?.markdownContent ?? ""} />;
    case "stat":
      return <StatWidget widget={widget} {...props} />;
    case "timeseries":
      return <TimeseriesWidget widget={widget} {...props} />;
    case "toplist":
      return <ToplistWidget widget={widget} {...props} />;
    case "logs":
      return <LogsWidget widget={widget} {...props} />;
    case "traces":
      return <TracesWidget widget={widget} {...props} />;
    case "table":
      return <TableWidget widget={widget} {...props} />;
    case "gauge":
      return <GaugeWidget widget={widget} {...props} />;
    case "topology":
      return <TopologyWidget widget={widget} {...props} />;
    default:
      return <div className="dash-widget__unsupported">Widget type "{widget.kind}" not yet rendered.</div>;
  }
}

function MarkdownWidget({ content }: { content: string }) {
  return (
    <div className="dash-widget__markdown">
      <pre>{content}</pre>
    </div>
  );
}

// ── Stat ────────────────────────────────────────────────────────────────────

function StatWidget({ widget, tenant, variableValues, macros, from, to, refreshTick }: ContentProps) {
  const [value, setValue] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let active = true;
    setLoading(true);
    const query = widget.queries[0];
    if (!query) {
      setValue("—");
      setLoading(false);
      return;
    }
    const expr = interpolate(query.expr, variableValues, macros);

    if (query.datasource === "tempo") {
      const params = new URLSearchParams({
        q: expr,
        start: (from.getTime() / 1000).toFixed(0),
        end: (to.getTime() / 1000).toFixed(0),
        step: macros.interval,
        tenant,
      });
      api
        .queryTempoMetricsRange(params)
        .then((r) => {
          if (!active) return;
          const v = aggregateTempoSeries(r.series ?? [], expr);
          setValue(v == null ? "—" : formatStat(v, widget.visualization?.unit));
        })
        .catch(() => { if (active) setValue("Err"); })
        .finally(() => { if (active) setLoading(false); });
      return () => { active = false; };
    }

    if (query.datasource !== "prometheus") {
      setLoading(false);
      setValue("—");
      return;
    }
    const params = new URLSearchParams({
      query: expr,
      time: (to.getTime() / 1000).toFixed(0),
    });
    api
      .queryPrometheus(params)
      .then((r) => {
        if (!active) return;
        if (r.data.resultType === "vector") {
          const v = r.data.result[0]?.value?.[1];
          setValue(v != null ? formatStat(parseFloat(v), widget.visualization?.unit) : "—");
        } else {
          setValue("—");
        }
      })
      .catch(() => { if (active) setValue("Err"); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [widget, variableValues, macros, from, to, tenant, refreshTick]);

  const thresholdColor = getThresholdColor(
    value != null && !isNaN(parseFloat(value)) ? parseFloat(value) : null,
    widget.visualization?.thresholds
  );

  return (
    <div className="dash-stat" style={{ color: thresholdColor }}>
      {loading ? <span className="dash-stat__loading">…</span> : (
        <span className="dash-stat__value">{value ?? "—"}</span>
      )}
    </div>
  );
}

// ── Timeseries ──────────────────────────────────────────────────────────────

function TimeseriesWidget({ widget, tenant, variableValues, macros, from, to, refreshTick }: ContentProps) {
  const [series, setSeries] = useState<MetricSeries[]>([]);
  const [error, setError] = useState("");
  const [expanded, setExpanded] = useState(false);

  useEffect(() => {
    let active = true;
    setError("");
    const queries = widget.queries.filter((q) => !q.disabled);

    if (queries.every((q) => q.datasource === "tempo") && queries.length > 0) {
      Promise.all(
        queries.map((q) => {
          const expr = interpolate(q.expr, variableValues, macros);
          const params = new URLSearchParams({
            q: expr,
            start: (from.getTime() / 1000).toFixed(0),
            end: (to.getTime() / 1000).toFixed(0),
            step: macros.interval,
            tenant,
          });
          return api.queryTempoMetricsRange(params).then((r) =>
            (r.series ?? []).map((s) => ({
              label: tempoLegend(s.labels, q.legendFormat) || q.refId || expr,
              points: s.samples.map((sample) => ({
                timestamp: new Date(sample.timestampMs).toISOString(),
                value: sample.value,
              })),
            }))
          );
        })
      )
        .then((results) => { if (active) setSeries(results.flat()); })
        .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); });
      return () => { active = false; };
    }

    const promises = queries
      .map((q) => {
        const expr = interpolate(q.expr, variableValues, macros);
        if (q.datasource === "loki") {
          const params = new URLSearchParams({
            query: expr,
            start: (from.getTime() / 1000).toFixed(0),
            end: (to.getTime() / 1000).toFixed(0),
            step: macros.interval,
            tenant,
          });
          return api.queryLokiRange(params).then((r) => {
            if (r.data.resultType !== "matrix") return [];
            return (r.data.result as LokiMatrixSeries[]).map((s) => ({
              label: formatLegend(q.legendFormat ?? "", s.metric) || q.refId || "A",
              points: s.values.map(([ts, v]) => ({
                timestamp: new Date(ts * 1000).toISOString(),
                value: parseFloat(v),
              })),
            }));
          });
        }
        if (q.datasource !== "prometheus") return Promise.resolve([]);
        const params = new URLSearchParams({
          query: expr,
          start: (from.getTime() / 1000).toFixed(0),
          end: (to.getTime() / 1000).toFixed(0),
          step: macros.interval,
        });
        return api.queryPrometheusRange(params).then((r) => {
          if (r.data.resultType !== "matrix") return [];
          return (r.data.result as PrometheusMatrixResult[]).map((s) => ({
            label: q.legendFormat
              ? formatLegend(q.legendFormat, s.metric)
              : Object.entries(s.metric)
                  .filter(([k]) => k !== "__name__")
                  .map(([k, v]) => `${k}="${v}"`)
                  .join(", ") || (q.refId ?? "A"),
            points: s.values.map(([ts, v]) => ({
              timestamp: new Date(ts * 1000).toISOString(),
              value: parseFloat(v),
            })),
          }));
        });
      });

    Promise.all(promises)
      .then((results) => { if (active) setSeries(results.flat()); })
      .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); });
    return () => { active = false; };
  }, [widget, variableValues, macros, from, to, tenant, refreshTick]);

  if (error) return <div className="dash-widget__error">{error}</div>;

  const displayMode: "line" | "area" | "bars" =
    widget.visualization?.displayMode === "bars"
      ? "bars"
      : widget.visualization?.displayMode === "area"
      ? "area"
      : "line";
  const chartQueries = widget.queries
    .filter((query) => !query.disabled)
    .map((query) => interpolate(query.expr, variableValues, macros));

  return (
    <>
      <div style={{ height: "100%", minHeight: "120px" }}>
        <TimeSeriesChart
          series={series}
          displayMode={displayMode}
          stacked={widget.visualization?.stacked}
          unit={widget.visualization?.unit}
          onExpand={() => setExpanded(true)}
        />
      </div>
      <ChartExpandModal
        open={expanded}
        title={widget.title}
        description={widget.description}
        series={series}
        displayMode={displayMode}
        stacked={widget.visualization?.stacked}
        queries={chartQueries}
        from={from}
        to={to}
        unit={widget.visualization?.unit}
        onClose={() => setExpanded(false)}
      />
    </>
  );
}

// ── Top list ────────────────────────────────────────────────────────────────

function ToplistWidget({ widget, tenant, variableValues, macros, from, to, refreshTick }: ContentProps) {
  const [rows, setRows] = useState<Array<{ label: string; value: number }>>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setError("");
    const query = widget.queries[0];
    if (!query) return;
    const expr = interpolate(query.expr, variableValues, macros);

    if (query.datasource === "tempo") {
      const params = new URLSearchParams({
        q: expr,
        start: (from.getTime() / 1000).toFixed(0),
        end: (to.getTime() / 1000).toFixed(0),
        step: macros.interval,
        tenant,
      });
      api
        .queryTempoMetricsRange(params)
        .then((r) => {
          if (!active) return;
          setRows(
            (r.series ?? [])
              .map((s) => ({
                label: tempoLegend(s.labels, query.legendFormat) || query.refId || "series",
                value: s.samples.length > 0 ? s.samples[s.samples.length - 1].value : 0,
              }))
              .sort((a, b) => b.value - a.value)
              .slice(0, 10)
          );
        })
        .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); });
      return () => { active = false; };
    }

    if (query.datasource !== "prometheus") return;
    const params = new URLSearchParams({
      query: expr,
      time: (to.getTime() / 1000).toFixed(0),
    });
    api.queryPrometheus(params).then((r) => {
      if (!active) return;
      if (r.data.resultType !== "vector") return;
      const sorted = r.data.result
        .map((s) => ({
          label: formatLegend(query.legendFormat ?? "", s.metric) || JSON.stringify(s.metric),
          value: parseFloat(s.value[1]),
        }))
        .sort((a, b) => b.value - a.value)
        .slice(0, 10);
      setRows(sorted);
    }).catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); });
    return () => { active = false; };
  }, [widget, variableValues, macros, from, to, tenant, refreshTick]);

  if (error) return <div className="dash-widget__error">{error}</div>;
  const max = rows[0]?.value ?? 1;
  return (
    <div className="dash-toplist">
      {rows.length === 0 && <div className="dash-empty">No series in range.</div>}
      {rows.map((row, i) => (
        <div key={i} className="dash-toplist__row">
          <span className="dash-toplist__label" title={row.label}>{row.label}</span>
          <div className="dash-toplist__bar-wrap">
            <div
              className="dash-toplist__bar"
              style={{ width: `${Math.max(2, (row.value / Math.max(max, 0.000001)) * 100)}%` }}
            />
          </div>
          <span className="dash-toplist__value">{formatStat(row.value, widget.visualization?.unit)}</span>
        </div>
      ))}
    </div>
  );
}

// ── Logs ────────────────────────────────────────────────────────────────────

function LogsWidget({ widget, tenant, variableValues, macros, from, to, refreshTick }: ContentProps) {
  const { open } = useInspector();
  const [logs, setLogs] = useState<LogRecord[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setError("");
    const query = widget.queries[0];
    if (!query) return;
    const expr = interpolate(query.expr, variableValues, macros);

    if (query.datasource === "loki") {
      const params = new URLSearchParams({
        query: expr,
        start: (from.getTime() / 1000).toFixed(0),
        end: (to.getTime() / 1000).toFixed(0),
        limit: "50",
        tenant,
      });
      api
        .queryLokiRange(params)
        .then((r) => {
          if (!active) return;
          const rows: LogRecord[] = [];
          for (const stream of (r.data.result as LokiStream[]) ?? []) {
            const labels = stream.stream ?? {};
            for (const [ts, line] of stream.values ?? []) {
              rows.push({
                tenant,
                service: labels.service ?? "",
                partition: labels.guardian_partition || labels.partition || "",
                intent: labels.guardian_intent || labels.intent || "",
                asset: labels.guardian_asset || labels.asset || "",
                timestamp: lokiTimestamp(ts),
                severity_text: labels.level || labels.severity || severityFromLine(line),
                body: line,
                trace_id: labels.trace_id || undefined,
                span_id: labels.span_id || undefined,
                resource_attributes: labels,
              });
            }
          }
          rows.sort((a, b) => b.timestamp.localeCompare(a.timestamp));
          setLogs(rows);
        })
        .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); });
      return () => { active = false; };
    }

    const params = new URLSearchParams({
      tenant,
      q: expr,
      from: from.toISOString(),
      to: to.toISOString(),
      limit: "50",
    });
    api.searchLogs(params).then((r) => {
      if (active) setLogs(r.results ?? []);
    }).catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); });
    return () => { active = false; };
  }, [widget, variableValues, macros, from, to, tenant, refreshTick]);

  if (error) return <div className="dash-widget__error">{error}</div>;

  const openLog = (log: LogRecord) =>
    open({
      kind: "log",
      id: log.trace_id || `${log.service || "log"}@${log.timestamp}`,
      tenant,
      log,
    });

  return (
    <div className="dash-logs">
      {logs.slice(0, 50).map((log, i) => (
        <div
          key={i}
          className="dash-log-row dash-log-row--clickable"
          role="button"
          tabIndex={0}
          title="Open full log details"
          onClick={() => openLog(log)}
          onKeyDown={(event) => {
            if (event.key === "Enter" || event.key === " ") {
              event.preventDefault();
              openLog(log);
            }
          }}
        >
          <span className="dash-log-ts">{formatLogTime(log.timestamp)}</span>
          <span className={`dash-log-sev dash-log-sev--${(log.severity_text ?? "INFO").toLowerCase()}`}>
            {(log.severity_text ?? "INFO").slice(0, 4)}
          </span>
          <span className="dash-log-body">{log.body}</span>
        </div>
      ))}
      {logs.length === 0 && <div className="dash-empty">No logs in range.</div>}
    </div>
  );
}

// ── Traces ──────────────────────────────────────────────────────────────────

function TracesWidget({ widget, tenant, variableValues, macros, from, to, refreshTick }: ContentProps) {
  const { open } = useInspector();
  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setError("");
    const query = widget.queries[0];
    if (!query) return;
    const expr = interpolate(query.expr, variableValues, macros);

    if (query.datasource === "native_traces") {
      const params = new URLSearchParams({
        tenant,
        from: from.toISOString(),
        to: to.toISOString(),
        limit: "20",
      });
      const service = extractService(expr);
      if (service) params.set("service", service);
      api
        .getRecentTraces(params)
        .then((r) => { if (active) setTraces(r.traces ?? []); })
        .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); });
      return () => { active = false; };
    }

    const params = new URLSearchParams({
      q: expr,
      start: (from.getTime() / 1000).toFixed(0),
      end: (to.getTime() / 1000).toFixed(0),
      limit: "20",
      tenant,
    });
    api
      .searchTraces(params)
      .then((r) => {
        if (!active) return;
        setTraces(
          (r.traces ?? []).map((t) => ({
            trace_id: t.traceID,
            service: t.rootServiceName ?? "",
            span_count: 0,
            min_time: t.startTimeUnixNano
              ? new Date(Number(BigInt(t.startTimeUnixNano) / 1_000_000n)).toISOString()
              : "",
            max_time: "",
            duration_ms: t.durationMs,
          }))
        );
      })
      .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); });
    return () => { active = false; };
  }, [widget, variableValues, macros, from, to, tenant, refreshTick]);

  if (error) return <div className="dash-widget__error">{error}</div>;

  return (
    <div className="dash-traces">
      {traces.length === 0 && <div className="dash-empty">No traces in range.</div>}
      {traces.map((trace) => (
        <button
          key={trace.trace_id}
          className="dash-trace-row"
          onClick={() => open({ kind: "trace", id: trace.trace_id, tenant })}
          title={trace.trace_id}
        >
          <code>{trace.trace_id.slice(0, 16)}…</code>
          <span className="dash-trace-row__service">{trace.service || "unknown"}</span>
          <span className="dash-trace-row__duration">
            {trace.duration_ms != null ? `${trace.duration_ms}ms` : `${trace.span_count} spans`}
          </span>
          <span className="dash-trace-row__time">{formatLogTime(trace.min_time)}</span>
        </button>
      ))}
    </div>
  );
}

// ── Table ───────────────────────────────────────────────────────────────────

function nativeTableRows(payload: unknown): Array<{ label: string; value: string }> {
  if (payload == null) return [];
  if (Array.isArray(payload)) {
    return payload.map((item, index) => ({ label: `[${index}]`, value: formatNativeValue(item) }));
  }
  if (typeof payload !== "object") {
    return [{ label: "value", value: formatNativeValue(payload) }];
  }
  return Object.entries(payload as Record<string, unknown>).map(([label, value]) => ({
    label,
    value: formatNativeValue(value),
  }));
}

function formatNativeValue(value: unknown): string {
  if (value == null) return "—";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return JSON.stringify(value);
}

function TableWidget({ widget, tenant, variableValues, macros, to, refreshTick }: ContentProps) {
  const [rows, setRows] = useState<Array<{ label: string; value: string }>>([]);
  const [endpoints, setEndpoints] = useState<ProxyEndpointInfo[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let active = true;
    setError("");
    setLoading(true);
    setRows([]);
    setEndpoints([]);
    const query = widget.queries[0];
    if (!query) {
      setLoading(false);
      return;
    }
    const expr = interpolate(query.expr, variableValues, macros);

    if (query.datasource === "native_traces" && expr.startsWith("/")) {
      if (expr.split("?")[0] === "/v1/proxy/endpoints") {
        api
          .getProxyEndpoints(new URLSearchParams({ limit: "100" }))
          .then((r) => { if (active) setEndpoints(r.endpoints ?? []); })
          .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); })
          .finally(() => { if (active) setLoading(false); });
      } else {
        api
          .getNativeJSON<unknown>(expr)
          .then((payload) => { if (active) setRows(nativeTableRows(payload)); })
          .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); })
          .finally(() => { if (active) setLoading(false); });
      }
      return () => { active = false; };
    }

    if (query.datasource !== "prometheus") {
      setLoading(false);
      return;
    }
    api
      .queryPrometheus(new URLSearchParams({ query: expr, time: (to.getTime() / 1000).toFixed(0) }))
      .then((r) => {
        if (!active) return;
        if (r.data.resultType !== "vector") return;
        setRows(
          r.data.result.map((s) => ({
            label: formatLegend(query.legendFormat ?? "", s.metric) || JSON.stringify(s.metric),
            value: s.value[1],
          }))
        );
      })
      .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [widget, variableValues, macros, to, tenant, refreshTick]);

  if (error) return <div className="dash-widget__error">{error}</div>;
  if (loading) return <div className="dash-empty">Loading…</div>;

  if (endpoints.length > 0) {
    return (
      <div className="dash-table-wrap">
        <table className="dash-table">
          <thead>
            <tr><th>Name</th><th>Kind</th><th>Status</th><th>Signals</th></tr>
          </thead>
          <tbody>
            {endpoints.map((endpoint) => (
              <tr key={endpoint.name}>
                <td title={endpoint.name}>{endpoint.name}</td>
                <td>{endpoint.kind || "—"}</td>
                <td>{endpoint.healthy === false ? "unhealthy" : endpoint.status || "healthy"}</td>
                <td>{(endpoint.signals ?? []).join(", ") || "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    );
  }

  return (
    <div className="dash-table-wrap">
      {rows.length === 0 ? (
        <div className="dash-empty">No rows returned.</div>
      ) : (
        <table className="dash-table">
          <thead><tr><th>Field</th><th>Value</th></tr></thead>
          <tbody>
            {rows.map((row, i) => (
              <tr key={i}>
                <td title={row.label}>{row.label}</td>
                <td>{row.value}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

// ── Gauge ───────────────────────────────────────────────────────────────────

function GaugeWidget({ widget, tenant, variableValues, macros, to, refreshTick }: ContentProps) {
  const [value, setValue] = useState<number | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setError("");
    const query = widget.queries[0];
    if (!query || query.datasource !== "prometheus") return;
    const expr = interpolate(query.expr, variableValues, macros);
    api
      .queryPrometheus(new URLSearchParams({ query: expr, time: (to.getTime() / 1000).toFixed(0) }))
      .then((r) => {
        if (!active) return;
        if (r.data.resultType === "vector" && r.data.result[0]) {
          setValue(parseFloat(r.data.result[0].value[1]));
        } else {
          setValue(null);
        }
      })
      .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Query failed"); });
    return () => { active = false; };
  }, [widget, variableValues, macros, to, tenant, refreshTick]);

  if (error) return <div className="dash-widget__error">{error}</div>;
  const pct = Math.max(0, Math.min(100, value ?? 0));
  return (
    <div className="dash-gauge">
      <div className="dash-gauge__value">{value == null ? "—" : formatStat(value, widget.visualization?.unit)}</div>
      <div className="dash-gauge__track"><div className="dash-gauge__fill" style={{ width: `${pct}%` }} /></div>
      <div className="dash-gauge__scale"><span>0</span><span>100</span></div>
    </div>
  );
}

// ── Topology ────────────────────────────────────────────────────────────────

function TopologyWidget({ widget, variableValues, macros, refreshTick }: ContentProps) {
  const [nodes, setNodes] = useState<Array<{ id: string; name: string; type: string; status?: string }>>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setError("");
    const partition = variableValues.partition && variableValues.partition !== "$__all"
      ? interpolate("$partition", variableValues, macros)
      : "";
    const params = new URLSearchParams({ tenant: macros.tenant });
    if (partition) params.set("partition", partition);
    api
      .getGuardianTopology(params)
      .then((r) => {
        if (active) setNodes((r.nodes ?? []).slice(0, 40));
      })
      .catch((err) => { if (active) setError(err instanceof Error ? err.message : "Failed to load topology"); });
    return () => { active = false; };
  }, [widget, variableValues, macros, refreshTick]);

  if (error) return <div className="dash-widget__error">{error}</div>;
  if (nodes.length === 0) return <div className="dash-empty">No topology nodes.</div>;
  return (
    <div className="dash-topology">
      {nodes.map((node) => (
        <div key={node.id} className="dash-topology__node">
          <span className={`dash-topology__dot dash-topology__dot--${(node.status ?? "healthy").toLowerCase()}`} />
          <span className="dash-topology__name" title={node.name}>{node.name}</span>
          <span className="dash-topology__type">{node.type}</span>
        </div>
      ))}
    </div>
  );
}

// ── Helpers ─────────────────────────────────────────────────────────────────

function tempoLabelValue(label: TempoMetricLabel): string {
  const value = label.value;
  if (value.stringValue != null) return value.stringValue;
  if (value.intValue != null) return value.intValue;
  if (value.doubleValue != null) return String(value.doubleValue);
  if (value.boolValue != null) return String(value.boolValue);
  return "";
}

function tempoLegend(labels: TempoMetricLabel[] | undefined, template?: string): string {
  const record: Record<string, string> = {};
  for (const label of labels ?? []) record[label.key] = tempoLabelValue(label);
  if (template) return formatLegend(template, record);
  return Object.entries(record).map(([k, v]) => `${k}="${v}"`).join(", ");
}

function aggregateTempoSeries(series: TempoTimeSeries[], expr: string): number | null {
  const values = series
    .map((s) => (s.samples.length > 0 ? s.samples[s.samples.length - 1].value : null))
    .filter((v): v is number => v != null && Number.isFinite(v));
  if (values.length === 0) return null;
  if (/\brate\(\)|count_over_time/.test(expr)) {
    return values.reduce((sum, v) => sum + v, 0);
  }
  return Math.max(...values);
}

function formatLegend(template: string, labels: Record<string, string>): string {
  if (!template) return Object.entries(labels).filter(([k]) => k !== "__name__").map(([k, v]) => `${k}="${v}"`).join(", ");
  return template.replace(/\{\{([^}]+)\}\}/g, (_, rawKey) => {
    const key = String(rawKey).trim();
    return labels[key] ?? key;
  });
}

function formatStat(value: number, unit?: string): string {
  if (isNaN(value)) return "—";
  switch (unit) {
    case "percent": return `${value.toFixed(2)}%`;
    case "ms": return `${value.toFixed(1)}ms`;
    case "seconds": return `${value.toFixed(2)}s`;
    case "bytes": return formatBytes(value);
    case "reqps": return `${value.toFixed(1)}/s`;
    case "bps": return `${formatBytes(value)}/s`;
    default: return value >= 1000 ? `${(value / 1000).toFixed(1)}k` : value.toFixed(2);
  }
}

function formatBytes(b: number): string {
  if (b < 1024) return `${b}B`;
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)}KB`;
  if (b < 1024 * 1024 * 1024) return `${(b / 1024 / 1024).toFixed(1)}MB`;
  return `${(b / 1024 / 1024 / 1024).toFixed(2)}GB`;
}

function getThresholdColor(value: number | null, thresholds?: Array<{ value: number; color: string }>): string | undefined {
  if (value == null || !thresholds || thresholds.length === 0) return undefined;
  let color: string | undefined;
  for (const t of thresholds) {
    if (value >= t.value) color = t.color === "amber" ? "var(--ui-amber)" : t.color === "red" ? "var(--ui-red)" : "var(--ui-green)";
  }
  return color;
}

function extractService(expr: string): string {
  const match = expr.match(/(?:resource\.)?service(?:\.name)?\s*=\s*"([^"]+)"/);
  return match ? match[1] : "";
}

function lokiTimestamp(raw: string): string {
  try {
    const value = BigInt(raw);
    const ms = value > 1_000_000_000_000_000n ? value / 1_000_000n : value * 1000n;
    return new Date(Number(ms)).toISOString();
  } catch {
    return raw;
  }
}

function formatLogTime(raw: string): string {
  if (!raw) return "—";
  const date = new Date(raw);
  return isNaN(date.getTime()) ? "—" : date.toLocaleTimeString();
}

function severityFromLine(line: string): string {
  const value = line.toLowerCase();
  if (value.includes("error") || value.includes("fatal")) return "ERROR";
  if (value.includes("warn")) return "WARN";
  if (value.includes("debug")) return "DEBUG";
  return "INFO";
}
