import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import MetricLineChart from "../components/charts/MetricLineChart";
import DataTable from "../components/ui/DataTable";
import type {
  MetricPoint,
  MetricSeries,
  PrometheusMatrixResult,
  PrometheusQueryResponse,
  PrometheusSeriesMetric,
  PrometheusVectorResult,
} from "../types/api";

type ViewMode = "graph" | "table" | "json";
type QueryMode = "range" | "instant";

const METRIC_HELPERS = [
  {
    group: "Guardian runtime",
    items: [
      { name: "guardian_runtime_loads_total", hint: "Partition runtime reloads by source." },
      { name: "guardian_runtime_intent_states_loaded_total", hint: "Intent states materialized during runtime loads." },
      { name: "guardian_dispatcher_writes_total", hint: "Dispatcher writes by partition/intent/result." },
      { name: "guardian_dispatcher_skipped_writes_total", hint: "Writes skipped because output did not change." },
      { name: "guardian_dispatcher_partition_status_current", hint: "Current partition status gauge." },
      { name: "guardian_dispatcher_intent_status_current", hint: "Current intent status gauge." },
    ],
  },
  {
    group: "MonoFS router and storage",
    items: [
      { name: "monofs_ops_total", hint: "Server operations by operation type." },
      { name: "monofs_read_bytes_total", hint: "Bytes read from MonoFS server paths." },
      { name: "monofs_write_bytes_total", hint: "Bytes written through MonoFS server paths." },
      { name: "monofs_guardian_upsert_files_total", hint: "Guardian upsert file count through the router." },
      { name: "monofs_guardian_upsert_bytes_total", hint: "Guardian upsert bytes through the router." },
      { name: "monofs_native_read_ops_total", hint: "Native router read operations." },
      { name: "monofs_query_duration_seconds", hint: "Log engine query latency histogram." },
      { name: "monofs_query_returned_records_total", hint: "Records returned by log engine queries." },
    ],
  },
  {
    group: "KVS",
    items: [
      { name: "kvs_active_keys", hint: "Current live key count." },
      { name: "kvs_write_files_total", hint: "Logical file writes by operation." },
      { name: "kvs_write_bytes_total", hint: "Raw blob bytes written." },
      { name: "kvs_read_ops_total", hint: "Read operations by operation type." },
      { name: "kvs_read_bytes_total", hint: "Bytes returned by reads." },
      { name: "kvs_pebble_batch_commits_total", hint: "Pebble batch commits by operation." },
    ],
  },
  {
    group: "Doctor and OTLP",
    items: [
      { name: "http_requests_total", hint: "Common HTTP request counter if exported by services." },
      { name: "request_duration_seconds", hint: "Common app request duration histogram." },
      { name: "request_size_bytes", hint: "Common request payload size metric." },
      { name: "go_goroutines", hint: "Go runtime goroutine count." },
      { name: "process_resident_memory_bytes", hint: "Resident memory for scraped processes." },
    ],
  },
] as const;

const PROMQL_EXAMPLES = [
  { label: "Request rate", query: "rate(http_requests_total[5m])" },
  { label: "Guardian write rate", query: "sum by (partition) (rate(guardian_dispatcher_writes_total[5m]))" },
  { label: "MonoFS read throughput", query: "sum(rate(monofs_read_bytes_total[5m]))" },
  { label: "KVS write throughput", query: "rate(kvs_write_bytes_total[5m])" },
  { label: "Log query p95", query: "histogram_quantile(0.95, sum by (le) (rate(monofs_query_duration_seconds_bucket[5m])))" },
] as const;

function formatDateTimeLocal(date: Date): string {
  if (Number.isNaN(date.getTime())) return formatDateTimeLocal(new Date());
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function localNow(offsetMinutes: number): string {
  return formatDateTimeLocal(new Date(Date.now() + offsetMinutes * 60_000));
}

function toISOStringOrEmpty(value: string): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toISOString();
}

function metricLabel(metric: PrometheusSeriesMetric): string {
  const name = metric.__name__ || "";
  const labels = Object.entries(metric)
    .filter(([key]) => key !== "__name__")
    .sort(([left], [right]) => left.localeCompare(right));
  if (labels.length === 0) return name || "value";
  const renderedLabels = `{${labels.map(([key, value]) => `${key}="${value}"`).join(",")}}`;
  if (!name) return renderedLabels;
  return `${name}${renderedLabels}`;
}

function formatNumber(value: number): string {
  if (!Number.isFinite(value)) return "-";
  if (value === 0) return "0";
  if (Math.abs(value) >= 1000 || Math.abs(value) < 0.01) return value.toExponential(2);
  if (Math.abs(value) >= 100) return value.toFixed(1);
  return value.toFixed(3).replace(/\.?0+$/, "");
}

function matrixToSeries(result?: PrometheusMatrixResult[]): MetricSeries[] {
  if (!result) return [];
  return result
    .map((series) => ({
      label: metricLabel(series.metric),
      labels: series.metric,
      points: series.values.map(([timestamp, value]) => ({
        timestamp: new Date(timestamp * 1000).toISOString(),
        value: Number(value),
        labels: series.metric,
      } satisfies MetricPoint)),
    }))
    .sort((left, right) => left.label.localeCompare(right.label));
}

function resultStats(response: PrometheusQueryResponse | null) {
  if (!response) return { series: 0, samples: 0 };
  const data = response.data;
  if (data.resultType === "matrix") {
    return {
      series: data.result.length,
      samples: data.result.reduce((total, series) => total + series.values.length, 0),
    };
  }
  if (data.resultType === "vector") return { series: data.result.length, samples: data.result.length };
  return { series: 1, samples: 1 };
}

function tableRows(response: PrometheusQueryResponse | null): Record<string, unknown>[] {
  if (!response) return [];
  const data = response.data;
  if (data.resultType === "matrix") {
    return data.result.flatMap((series) =>
      series.values.map(([timestamp, value]) => ({
        time: new Date(timestamp * 1000).toLocaleString(),
        series: metricLabel(series.metric),
        value: formatNumber(Number(value)),
      }))
    );
  }
  if (data.resultType === "vector") {
    return data.result.map((sample: PrometheusVectorResult) => ({
      time: new Date(sample.value[0] * 1000).toLocaleString(),
      series: metricLabel(sample.metric),
      value: formatNumber(Number(sample.value[1])),
    }));
  }
  return [{ time: new Date(data.result[0] * 1000).toLocaleString(), series: data.resultType, value: data.result[1] }];
}

export default function MetricsPage() {
  const { config, loaded } = useConfig();
  const [searchParams, setSearchParams] = useSearchParams();
  const initialized = useRef(false);

  const [tenant, setTenant] = useState(searchParams.get("tenant") ?? config.default_tenant ?? "default");
  const [metricNames, setMetricNames] = useState<string[]>([]);
  const [metricFilter, setMetricFilter] = useState("");
  const [selectedMetric, setSelectedMetric] = useState(searchParams.get("metric") ?? "");
  const [query, setQuery] = useState(searchParams.get("query") ?? searchParams.get("metric") ?? "");
  const [from, setFrom] = useState(searchParams.get("from") ? formatDateTimeLocal(new Date(searchParams.get("from") || "")) : localNow(-60));
  const [to, setTo] = useState(searchParams.get("to") ? formatDateTimeLocal(new Date(searchParams.get("to") || "")) : localNow(0));
  const [step, setStep] = useState(searchParams.get("step") ?? "30s");
  const [mode, setMode] = useState<QueryMode>((searchParams.get("mode") as QueryMode) || "range");
  const [view, setView] = useState<ViewMode>("graph");
  const [response, setResponse] = useState<PrometheusQueryResponse | null>(null);
  const [requestUrl, setRequestUrl] = useState("");
  const [rawJson, setRawJson] = useState("{}");
  const [statusKind, setStatusKind] = useState<"idle" | "loading" | "ok" | "error">("idle");
  const [statusMsg, setStatusMsg] = useState("Pick a metric or run a PromQL expression.");

  const loadMetricNames = useCallback(async () => {
    const end = new Date();
    const start = new Date(end.getTime() - 24 * 60 * 60_000);
    const params = new URLSearchParams({ tenant, start: start.toISOString(), end: end.toISOString() });
    try {
      const data = await api.getMetricNames(params);
      setMetricNames(data.data ?? []);
      if (!selectedMetric && data.data?.length) {
        setSelectedMetric(data.data[0]);
        setQuery(data.data[0]);
      }
    } catch (err) {
      setStatusKind("error");
      setStatusMsg(err instanceof Error ? err.message : "Metric discovery failed");
    }
  }, [tenant, selectedMetric]);

  useEffect(() => {
    if (loaded && !initialized.current) {
      initialized.current = true;
      if (!searchParams.get("tenant") && config.default_tenant) setTenant(config.default_tenant);
    }
  }, [loaded, config.default_tenant, searchParams]);

  useEffect(() => {
    void loadMetricNames();
  }, [loadMetricNames]);

  useEffect(() => {
    const next = new URLSearchParams();
    if (tenant) next.set("tenant", tenant);
    if (selectedMetric) next.set("metric", selectedMetric);
    if (query) next.set("query", query);
    const fromISO = toISOStringOrEmpty(from);
    const toISO = toISOStringOrEmpty(to);
    if (fromISO) next.set("from", fromISO);
    if (toISO) next.set("to", toISO);
    if (step) next.set("step", step);
    next.set("mode", mode);
    if (next.toString() !== searchParams.toString()) setSearchParams(next, { replace: true });
  }, [tenant, selectedMetric, query, from, to, step, mode, searchParams, setSearchParams]);

  const filteredMetricNames = useMemo(() => {
    const needle = metricFilter.trim().toLowerCase();
    if (!needle) return metricNames.slice(0, 200);
    return metricNames.filter((name) => name.toLowerCase().includes(needle)).slice(0, 200);
  }, [metricNames, metricFilter]);

  const stats = resultStats(response);
  const graphSeries = response?.data.resultType === "matrix" ? matrixToSeries(response.data.result) : [];
  const hasGraphSeries = graphSeries.some((series) => series.points.length >= 2);
  const rows = tableRows(response);

  const chooseMetric = (metricName: string) => {
    setSelectedMetric(metricName);
    setQuery(metricName);
    setResponse(null);
    setRawJson("{}");
    setStatusKind("idle");
    setStatusMsg(`Ready to query ${metricName}.`);
  };

  const chooseExpression = (expression: string) => {
    setSelectedMetric("");
    setQuery(expression);
    setResponse(null);
    setRawJson("{}");
    setStatusKind("idle");
    setStatusMsg("Ready to run helper expression.");
  };

  const setQuickRange = (minutes: number) => {
    setFrom(localNow(-minutes));
    setTo(localNow(0));
  };

  const run = async (event?: React.FormEvent) => {
    event?.preventDefault();
    const expr = query.trim() || selectedMetric.trim();
    if (!expr) return;

    const params = new URLSearchParams({ tenant, query: expr });
    let url = "/api/v1/query";
    if (mode === "range") {
      const fromISO = toISOStringOrEmpty(from);
      const toISO = toISOStringOrEmpty(to);
      if (fromISO) params.set("start", fromISO);
      if (toISO) params.set("end", toISO);
      params.set("step", step.trim() || "30s");
      url = "/api/v1/query_range";
    } else {
      const toISO = toISOStringOrEmpty(to);
      if (toISO) params.set("time", toISO);
    }

    setRequestUrl(`${url}?${params}`);
    setStatusKind("loading");
    setStatusMsg("Executing PromQL query...");
    try {
      const data = mode === "range" ? await api.queryPrometheusRange(params) : await api.queryPrometheus(params);
      setResponse(data);
      setRawJson(JSON.stringify(data, null, 2));
      const nextStats = resultStats(data);
      setStatusKind("ok");
      setStatusMsg(`${data.data.resultType} result: ${nextStats.series} series, ${nextStats.samples} samples.`);
    } catch (err) {
      setResponse(null);
      const message = err instanceof Error ? err.message : "Query failed";
      setRawJson(JSON.stringify({ error: message }, null, 2));
      setStatusKind("error");
      setStatusMsg(message);
    }
  };

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Metrics</h2>
          <p>Prometheus-compatible metric discovery, query, graph, and table view.</p>
        </div>
        <span className="panel-badge">/api/v1/query_range</span>
      </div>

      <div className="panel-body">
        <div className="prom-layout">
          <aside className="prom-sidebar">
            <div className="prom-sidebar__head">
              <strong>Metrics</strong>
              <span>{metricNames.length} discovered</span>
            </div>
            <input
              className="xp-search prom-metric-filter"
              value={metricFilter}
              onChange={(event) => setMetricFilter(event.target.value)}
              placeholder="Filter metric names..."
            />
            <div className="prom-metric-list">
              {filteredMetricNames.length === 0 ? (
                <div className="prom-helper">
                  <div className="prom-helper__empty">No live metric names found for the last 24h.</div>
                  <div className="prom-helper__section">
                    <strong>Known useful metrics</strong>
                    {METRIC_HELPERS.map((group) => (
                      <div className="prom-helper__group" key={group.group}>
                        <span>{group.group}</span>
                        {group.items.map((item) => (
                          <button
                            type="button"
                            key={item.name}
                            className="prom-helper__metric"
                            onClick={() => chooseMetric(item.name)}
                            title={item.hint}
                          >
                            <code>{item.name}</code>
                            <small>{item.hint}</small>
                          </button>
                        ))}
                      </div>
                    ))}
                  </div>
                  <div className="prom-helper__section">
                    <strong>PromQL starters</strong>
                    {PROMQL_EXAMPLES.map((example) => (
                      <button
                        type="button"
                        key={example.label}
                        className="prom-helper__metric"
                        onClick={() => chooseExpression(example.query)}
                      >
                        <code>{example.label}</code>
                        <small>{example.query}</small>
                      </button>
                    ))}
                  </div>
                </div>
              ) : (
                filteredMetricNames.map((metricName) => (
                  <button
                    type="button"
                    key={metricName}
                    className={"prom-metric-item" + (selectedMetric === metricName ? " is-selected" : "")}
                    onClick={() => chooseMetric(metricName)}
                    title={metricName}
                  >
                    {metricName}
                  </button>
                ))
              )}
            </div>
          </aside>

          <section className="prom-console">
            <form className="prom-query-bar" onSubmit={(event) => void run(event)}>
              <div className="prom-query-main">
                <label>
                  Tenant
                  <input value={tenant} onChange={(event) => setTenant(event.target.value)} />
                </label>
                <label className="prom-expression-label">
                  Expression
                  <textarea
                    className="prom-expression"
                    value={query}
                    onChange={(event) => setQuery(event.target.value)}
                    spellCheck={false}
                    rows={2}
                  />
                </label>
              </div>

              <div className="prom-controls">
                <div className="xp-toolbar" role="group" aria-label="Query mode">
                  <button type="button" className={"xp-preset" + (mode === "range" ? " xp-active" : "")} onClick={() => setMode("range")}>Range</button>
                  <button type="button" className={"xp-preset" + (mode === "instant" ? " xp-active" : "")} onClick={() => setMode("instant")}>Instant</button>
                </div>
                <div className="xp-toolbar">
                  {[15, 60, 360, 1440].map((minutes) => (
                    <button key={minutes} type="button" className="xp-preset" onClick={() => setQuickRange(minutes)}>
                      {minutes < 60 ? `${minutes}m` : `${minutes / 60}h`}
                    </button>
                  ))}
                </div>
                <label>
                  From
                  <input type="datetime-local" value={from} onChange={(event) => setFrom(event.target.value)} disabled={mode === "instant"} />
                </label>
                <label>
                  To
                  <input type="datetime-local" value={to} onChange={(event) => setTo(event.target.value)} />
                </label>
                <label>
                  Step
                  <input value={step} onChange={(event) => setStep(event.target.value)} disabled={mode === "instant"} />
                </label>
                <button type="submit" className="run-btn">Execute</button>
              </div>
            </form>

            {requestUrl && (
              <div className="request">
                GET <a href={requestUrl} target="_blank" rel="noreferrer">{requestUrl}</a>
              </div>
            )}
            <div className={`status status-${statusKind}`}>{statusMsg}</div>

            <div className="prom-tabs" role="tablist" aria-label="Metric result view">
              {(["graph", "table", "json"] as ViewMode[]).map((item) => (
                <button
                  type="button"
                  key={item}
                  className={"prom-tab" + (view === item ? " is-active" : "")}
                  onClick={() => setView(item)}
                >
                  {item}
                </button>
              ))}
            </div>

            <div className="summary">
              <span className="pill">series: {stats.series}</span>
              <span className="pill">samples: {stats.samples}</span>
              {response?.data.resultType && <span className="pill">type: {response.data.resultType}</span>}
              {graphSeries.length > 0 && (
                <span className="pill">graph: {graphSeries.length === 1 ? graphSeries[0].label : `${graphSeries.length} series`}</span>
              )}
            </div>

            {view === "graph" && (
              <div className="results">
                {hasGraphSeries ? (
                  <MetricLineChart series={graphSeries} metricName={query} />
                ) : (
                  <div className="result-block">
                    <h3>Graph</h3>
                    <p className="empty">Run a range query with at least two samples to render a graph.</p>
                  </div>
                )}
              </div>
            )}

            {view === "table" && (
              <DataTable
                title="Samples"
                columns={[
                  { label: "Time", key: "time" },
                  { label: "Series", key: "series" },
                  { label: "Value", key: "value" },
                ]}
                rows={rows}
              />
            )}

            {view === "json" && (
              <div className="result-block">
                <h3>Prometheus response</h3>
                <pre>{rawJson}</pre>
              </div>
            )}
          </section>
        </div>
      </div>
    </div>
  );
}