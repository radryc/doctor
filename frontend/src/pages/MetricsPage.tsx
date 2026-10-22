import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  Check,
  Clock,
  Copy,
  Gauge,
  Loader2,
  RefreshCw,
  Search,
  Sparkles,
} from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import { copyText } from "../lib/clipboard";
import MetricLineChart from "../components/charts/MetricLineChart";
import ChartExpandModal from "../components/charts/ChartExpandModal";
import { formatDateTimeFull } from "../components/charts/TimeSeriesChart";
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
type StatusKind = "idle" | "loading" | "ok" | "error";

const METRIC_HELPERS = [
  {
    group: "Doctor and HTTP",
    items: [
      { name: "doctor_component_up", hint: "1 when a Doctor component is reporting. Labels identify component, instance, partition, and build." },
      { name: "doctor_ingest_requests_accepted", hint: "Ingest requests accepted by source and result." },
      { name: "http_server_request_duration_count", hint: "HTTP request count by service, method, and status code." },
      { name: "http_server_request_duration_bucket", hint: "HTTP latency histogram buckets. Use histogram_quantile with sum by (le) for p95/p99." },
      { name: "http_server_request_body_size_bucket", hint: "Request body size histogram buckets. Aggregate with sum by (le) instead of plotting every label set." },
    ],
  },
  {
    group: "Guardian runtime",
    items: [
      { name: "guardian_runtime_partition_loads_total", hint: "Partition runtime reloads by source." },
      { name: "guardian_runtime_partition_intent_states_loaded_total", hint: "Intent states materialized during runtime loads." },
      { name: "guardian_dispatcher_writes_total", hint: "Dispatcher writes by partition/intent/result." },
      { name: "guardian_dispatcher_skipped_writes_total", hint: "Writes skipped because output did not change." },
      { name: "guardian_dispatcher_partition_status_current", hint: "Current partition status gauge." },
      { name: "guardian_dispatcher_intent_status_current", hint: "Current intent status gauge." },
    ],
  },
  {
    group: "MonoFS router and storage",
    items: [
      { name: "monofs_server_ops_total", hint: "Server operations by operation type." },
      { name: "monofs_server_read_bytes_total", hint: "Bytes read from MonoFS server paths." },
      { name: "monofs_server_ingest_bytes_total", hint: "Bytes written through MonoFS server paths." },
      { name: "monofs_router_guardian_upsert_files_total", hint: "Guardian upsert file count through the router." },
      { name: "monofs_router_guardian_upsert_bytes_total", hint: "Guardian upsert bytes through the router." },
      { name: "monofs_router_native_read_ops_total", hint: "Native router read operations." },
      { name: "monofs_logengine_query_duration_seconds", hint: "Log engine query latency histogram." },
      { name: "monofs_logengine_query_returned_records_total", hint: "Records returned by log engine queries." },
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
] as const;

const PREFERRED_METRIC_NAMES: string[] = [
  // High-frequency metrics first: they are the most likely to produce a
  // graphable series in a short default window.
  "guardian_results_executions",
  "guardian_reconciler_partition_executions",
  "guardian_pusher_task_executions",
  "guardian_dispatcher_writes_total",
  "monofs_server_ops_total",
  "monofs_router_guardian_upsert_bytes_total",
  "k8s_top_pod_cpu_usage_millicores",
  "doctor_ingest_requests_accepted",
  ...METRIC_HELPERS.flatMap((group) => group.items.map((item) => item.name)),
].filter((name, index, all) => all.indexOf(name) === index);

const QUICK_METRIC_ICONS = [Activity, Gauge, Clock, Sparkles, AlertTriangle] as const;
const QUICK_METRIC_TONES = ["sky", "emerald", "violet", "amber", "rose"] as const;

function preferredMetricName(names: string[]): string {
  const available = new Set(names);
  for (const candidate of PREFERRED_METRIC_NAMES) {
    if (available.has(candidate)) return candidate;
  }
  return names[0] ?? "";
}

function metricHint(name: string): string {
  for (const group of METRIC_HELPERS) {
    const item = group.items.find((candidate) => candidate.name === name);
    if (item) return item.hint;
  }
  return "Live metric in the selected partition.";
}

const TONE_CLASS: Record<string, string> = {
  sky: "border-sky-500/30 bg-sky-500/10 text-sky-300",
  rose: "border-rose-500/30 bg-rose-500/10 text-rose-300",
  violet: "border-violet-500/30 bg-violet-500/10 text-violet-300",
  amber: "border-amber-500/30 bg-amber-500/10 text-amber-300",
  emerald: "border-emerald-500/30 bg-emerald-500/10 text-emerald-300",
};

const RANGE_PRESETS = [15, 60, 360, 1440] as const;

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

function histogramScalar(histogram?: { count?: string; sum?: string }): number | null {
  if (!histogram) return null;
  const count = Number(histogram.count);
  const sum = Number(histogram.sum);
  if (Number.isFinite(sum) && Number.isFinite(count) && count > 0) return sum / count;
  if (Number.isFinite(sum)) return sum;
  if (Number.isFinite(count)) return count;
  return null;
}

function histogramSummary(histogram?: { count?: string; sum?: string }): string {
  if (!histogram) return "histogram";
  const parts: string[] = [];
  if (histogram.count !== undefined) parts.push(`count=${histogram.count}`);
  if (histogram.sum !== undefined) parts.push(`sum=${histogram.sum}`);
  return parts.length > 0 ? `histogram(${parts.join(", ")})` : "histogram";
}

function matrixToSeries(result?: PrometheusMatrixResult[]): MetricSeries[] {
  if (!result) return [];
  return result
    .map((series) => {
      const points: MetricPoint[] = (series.values ?? []).map(([timestamp, value]) => ({
        timestamp: new Date(timestamp * 1000).toISOString(),
        value: Number(value),
        labels: series.metric,
      }));
      for (const [timestamp, histogram] of series.histograms ?? []) {
        const value = histogramScalar(histogram);
        if (value === null) continue;
        points.push({
          timestamp: new Date(timestamp * 1000).toISOString(),
          value,
          labels: series.metric,
        });
      }
      return {
        label: metricLabel(series.metric),
        labels: series.metric,
        points,
      };
    })
    .sort((left, right) => left.label.localeCompare(right.label));
}

function resultStats(response: PrometheusQueryResponse | null) {
  if (!response) return { series: 0, samples: 0 };
  const data = response.data;
  if (data.resultType === "matrix") {
    return {
      series: data.result.length,
      samples: data.result.reduce(
        (total, series) => total + (series.values?.length ?? 0) + (series.histograms?.length ?? 0),
        0
      ),
    };
  }
  if (data.resultType === "vector") return { series: data.result.length, samples: data.result.length };
  return { series: 1, samples: 1 };
}

function metricLabelFields(metric: PrometheusSeriesMetric): Record<string, string> {
  const fields: Record<string, string> = {};
  for (const [key, value] of Object.entries(metric)) {
    if (key === "__name__") continue;
    fields[`label_${key}`] = value;
  }
  return fields;
}

function tableRows(response: PrometheusQueryResponse | null): Record<string, unknown>[] {
  if (!response) return [];
  const data = response.data;
  if (data.resultType === "matrix") {
    return data.result.flatMap((series) => {
      const labels = metricLabelFields(series.metric);
      const rows = (series.values ?? []).map(([timestamp, value]) => ({
        time: new Date(timestamp * 1000).toLocaleString(),
        series: metricLabel(series.metric),
        value: formatNumber(Number(value)),
        ...labels,
      }));
      for (const [timestamp, histogram] of series.histograms ?? []) {
        rows.push({
          time: new Date(timestamp * 1000).toLocaleString(),
          series: metricLabel(series.metric),
          value: histogramSummary(histogram),
          ...labels,
        });
      }
      return rows;
    });
  }
  if (data.resultType === "vector") {
    return data.result.map((sample: PrometheusVectorResult) => ({
      time: new Date((sample.value?.[0] ?? sample.histogram?.[0] ?? 0) * 1000).toLocaleString(),
      series: metricLabel(sample.metric),
      value: sample.value
        ? formatNumber(Number(sample.value[1]))
        : histogramSummary(sample.histogram?.[1]),
      ...metricLabelFields(sample.metric),
    }));
  }
  return [
    {
      time: new Date(data.result[0] * 1000).toLocaleString(),
      series: data.resultType,
      value: data.result[1],
    },
  ];
}

function tableColumns(response: PrometheusQueryResponse | null): { label: string; key: string }[] {
  const columns: { label: string; key: string }[] = [{ label: "Time", key: "time" }];
  const data = response?.data;
  const labelKeys = new Set<string>();
  if (data && (data.resultType === "matrix" || data.resultType === "vector")) {
    for (const item of data.result) {
      for (const key of Object.keys(item.metric ?? {})) {
        if (key !== "__name__") labelKeys.add(key);
      }
    }
  }
  for (const key of [...labelKeys].sort()) {
    columns.push({ label: key, key: `label_${key}` });
  }
  columns.push({ label: "Value", key: "value" });
  columns.push({ label: "Series", key: "series" });
  return columns;
}

function rangeLabel(from: string, to: string): string {
  const start = new Date(from).getTime();
  const end = new Date(to).getTime();
  if (Number.isNaN(start) || Number.isNaN(end) || end <= start) return "custom range";
  const minutes = Math.round((end - start) / 60_000);
  if (minutes < 60) return `last ${minutes}m`;
  if (minutes < 60 * 24) return `last ${minutes / 60}h`;
  return `last ${minutes / (60 * 24)}d`;
}

export default function MetricsPage() {
  const { config, loaded } = useConfig();
  const [searchParams, setSearchParams] = useSearchParams();
  const initialized = useRef(false);
  const bootstrapped = useRef(false);
  const autoRan = useRef("");

  const [tenant, setTenant] = useState(searchParams.get("tenant") ?? config.default_tenant ?? "default");
  const [partition, setPartition] = useState(searchParams.get("partition") ?? "");
  const [partitions, setPartitions] = useState<string[]>([]);
  const [metricNames, setMetricNames] = useState<string[]>([]);
  const [metricLoading, setMetricLoading] = useState(true);
  const [metricError, setMetricError] = useState("");
  const [metricFilter, setMetricFilter] = useState("");
  const [selectedMetric, setSelectedMetric] = useState(searchParams.get("metric") ?? "");
  const [query, setQuery] = useState(searchParams.get("query") ?? searchParams.get("metric") ?? "");
  const [from, setFrom] = useState(
    searchParams.get("from") ? formatDateTimeLocal(new Date(searchParams.get("from") || "")) : localNow(-60)
  );
  const [to, setTo] = useState(
    searchParams.get("to") ? formatDateTimeLocal(new Date(searchParams.get("to") || "")) : localNow(0)
  );
  const [step, setStep] = useState(searchParams.get("step") ?? "30s");
  const [mode, setMode] = useState<QueryMode>((searchParams.get("mode") as QueryMode) || "range");
  const [view, setView] = useState<ViewMode>("graph");
  const [chartExpanded, setChartExpanded] = useState(false);
  const [response, setResponse] = useState<PrometheusQueryResponse | null>(null);
  const [requestUrl, setRequestUrl] = useState("");
  const [rawJson, setRawJson] = useState("{}");
  const [statusKind, setStatusKind] = useState<StatusKind>("idle");
  const [statusMsg, setStatusMsg] = useState("Choose a quick start, a metric, or write a PromQL expression.");
  const [copiedQuery, setCopiedQuery] = useState(false);
  const [showAdvanced, setShowAdvanced] = useState(false);

  const loadMetricNames = useCallback(async (
    targetTenant = tenant,
    targetPartition = partition,
    rangeStart = from,
    rangeEnd = to
  ) => {
    let end = new Date(rangeEnd);
    let start = new Date(rangeStart);
    if (Number.isNaN(end.getTime()) || Number.isNaN(start.getTime()) || end <= start) {
      end = new Date();
      start = new Date(end.getTime() - 60 * 60_000);
    }
    const params = new URLSearchParams({
      tenant: targetTenant,
      start: start.toISOString(),
      end: end.toISOString(),
    });
    if (targetPartition) params.set("partition", targetPartition);

    setMetricLoading(true);
    setMetricError("");
    try {
      const data = await api.getMetricNames(params);
      setMetricNames(data.data ?? []);
    } catch (err) {
      setMetricNames([]);
      setMetricError(err instanceof Error ? err.message : "Metric discovery failed");
    } finally {
      setMetricLoading(false);
    }
  }, [tenant, partition, from, to]);

  useEffect(() => {
    if (loaded && !initialized.current) {
      initialized.current = true;
      if (!searchParams.get("tenant") && config.default_tenant) setTenant(config.default_tenant);
    }
  }, [loaded, config.default_tenant, searchParams]);

  useEffect(() => {
    let cancelled = false;
    api
      .guardianOverview()
      .then((overview) => {
        if (cancelled) return;
        const names = overview.partitions?.map((p) => p.name).filter(Boolean) ?? [];
        setPartitions(names);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    void loadMetricNames(tenant, partition, from, to);
  }, [tenant, partition, from, to, loadMetricNames]);

  // Augment the Guardian-derived partition list with partitions that actually
  // appear in telemetry, so the metrics picker works even when Guardian is not
  // reachable.
  useEffect(() => {
    api
      .getLogServices(config.default_tenant)
      .then((d) => {
        setPartitions((prev) => [...new Set([...prev, ...(d.partitions ?? [])])].sort());
      })
      .catch(() => {});
  }, [config.default_tenant]);

  useEffect(() => {
    const next = new URLSearchParams();
    if (tenant) next.set("tenant", tenant);
    if (partition) next.set("partition", partition);
    if (selectedMetric) next.set("metric", selectedMetric);
    if (query) next.set("query", query);
    const fromISO = toISOStringOrEmpty(from);
    const toISO = toISOStringOrEmpty(to);
    if (fromISO) next.set("from", fromISO);
    if (toISO) next.set("to", toISO);
    if (step) next.set("step", step);
    next.set("mode", mode);
    if (next.toString() !== searchParams.toString()) setSearchParams(next, { replace: true });
  }, [tenant, partition, selectedMetric, query, from, to, step, mode, searchParams, setSearchParams]);

  const execute = useCallback(
    async (
      expression: string,
      options?: {
        mode?: QueryMode;
        tenant?: string;
        partition?: string;
        from?: string;
        to?: string;
        step?: string;
      }
    ) => {
      const expr = expression.trim();
      if (!expr) {
        setStatusKind("idle");
        setStatusMsg("Enter a PromQL expression first.");
        return;
      }

      const nextMode = options?.mode ?? mode;
      const nextTenant = options?.tenant ?? tenant;
      const nextPartition = options?.partition ?? partition;
      const nextFrom = options?.from ?? from;
      const nextTo = options?.to ?? to;
      const nextStep = options?.step ?? step;

      const params = new URLSearchParams({ tenant: nextTenant, query: expr });
      if (nextPartition) params.set("partition", nextPartition);
      let url = "/api/v1/query";
      if (nextMode === "range") {
        const fromISO = toISOStringOrEmpty(nextFrom);
        const toISO = toISOStringOrEmpty(nextTo);
        if (fromISO) params.set("start", fromISO);
        if (toISO) params.set("end", toISO);
        params.set("step", nextStep.trim() || "30s");
        url = "/api/v1/query_range";
      } else {
        const toISO = toISOStringOrEmpty(nextTo);
        if (toISO) params.set("time", toISO);
      }

      setRequestUrl(`${url}?${params}`);
      setStatusKind("loading");
      setStatusMsg("Executing PromQL query…");
      if (nextMode === "instant") setView("table");

      try {
        const data =
          nextMode === "range"
            ? await api.queryPrometheusRange(params)
            : await api.queryPrometheus(params);
        setResponse(data);
        setRawJson(JSON.stringify(data, null, 2));
        const nextStats = resultStats(data);
        setStatusKind("ok");
        setStatusMsg(
          `${data.data.resultType} result: ${nextStats.series} series, ${nextStats.samples} samples.`
        );
      } catch (err) {
        setResponse(null);
        const message = err instanceof Error ? err.message : "Query failed";
        setRawJson(JSON.stringify({ error: message }, null, 2));
        setStatusKind("error");
        setStatusMsg(message);
      }
    },
    [mode, tenant, partition, from, to, step]
  );

  // If the page was opened from a dashboard link with a query/metric, run it
  // once so the graph is populated without an extra click.
  useEffect(() => {
    if (bootstrapped.current) return;
    const initial = searchParams.get("query") ?? searchParams.get("metric") ?? "";
    if (!initial.trim()) return;
    bootstrapped.current = true;
    void execute(initial);
  }, [execute, searchParams]);

  const filteredMetricNames = useMemo(() => {
    const needle = metricFilter.trim().toLowerCase();
    const names = needle
      ? metricNames.filter((name) => name.toLowerCase().includes(needle))
      : metricNames;
    return names.slice(0, 250);
  }, [metricNames, metricFilter]);

  const quickMetrics = useMemo(() => {
    const available = new Set(metricNames);
    const preferred = PREFERRED_METRIC_NAMES.filter((name) => available.has(name));
    const preferredSet = new Set(preferred);
    const rest = metricNames.filter((name) => !preferredSet.has(name));
    return [...preferred, ...rest].slice(0, 5);
  }, [metricNames]);

  // Populate the main graph from a metric that actually exists in the selected
  // partition instead of leaving the page on an empty "Nothing plotted yet".
  useEffect(() => {
    if (metricLoading || metricNames.length === 0) return;
    const key = `${tenant}|${partition}`;
    if (autoRan.current === key) return;
    autoRan.current = key;
    if (query.trim() || selectedMetric.trim()) return;
    const pick = preferredMetricName(metricNames);
    if (!pick) return;
    setSelectedMetric(pick);
    setQuery(pick);
    void execute(pick, { partition });
  }, [execute, metricLoading, metricNames, partition, query, selectedMetric, tenant]);

  const stats = resultStats(response);
  const graphSeries = response?.data.resultType === "matrix" ? matrixToSeries(response.data.result) : [];
  const hasGraphSeries = graphSeries.some((series) => series.points.length >= 2);
  const rows = tableRows(response);
  const activeExpression = query.trim() || selectedMetric.trim();
  const currentRange = rangeLabel(from, to);
  const parsedFrom = new Date(from);
  const parsedTo = new Date(to);
  const chartFrom = Number.isNaN(parsedFrom.getTime()) ? new Date(Date.now() - 60 * 60_000) : parsedFrom;
  const chartTo = Number.isNaN(parsedTo.getTime()) ? new Date() : parsedTo;
  const activeDescription = selectedMetric
    ? metricHint(selectedMetric)
    : "Custom PromQL expression. Hover the chart for values and click a legend item to hide or show a series.";

  const chooseMetric = (metricName: string) => {
    setSelectedMetric(metricName);
    setQuery(metricName);
    void execute(metricName);
  };

  const changePartition = (nextPartition: string) => {
    setPartition(nextPartition);
    autoRan.current = "";
    if (query.trim()) void execute(query.trim(), { partition: nextPartition });
  };

  const changeMode = (nextMode: QueryMode) => {
    setMode(nextMode);
    if (query.trim()) void execute(query.trim(), { mode: nextMode });
  };

  const setQuickRange = (minutes: number) => {
    const nextFrom = localNow(-minutes);
    const nextTo = localNow(0);
    setFrom(nextFrom);
    setTo(nextTo);
    if (query.trim()) void execute(query.trim(), { from: nextFrom, to: nextTo });
  };

  const copyQuery = () => {
    if (!activeExpression) return;
    void copyText(activeExpression).then((ok) => {
      if (!ok) return;
      setCopiedQuery(true);
      window.setTimeout(() => setCopiedQuery(false), 1600);
    });
  };

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Metrics</h2>
          <p>
            {query
              ? `${partition || "all partitions"} · ${currentRange} · ${mode === "range" ? "range query" : "instant query"}`
              : "Explore live Prometheus-compatible metrics. Pick a partition to narrow the metric catalog and graphs."}
          </p>
        </div>
        <div className="xp-toolbar">
          <div className="xp-presets" role="group" aria-label="Time range">
            {RANGE_PRESETS.map((minutes) => (
              <button
                key={minutes}
                type="button"
                className="xp-preset"
                onClick={() => setQuickRange(minutes)}
                title={`Set the query window to the last ${minutes < 60 ? `${minutes} minutes` : `${minutes / 60} hours`}`}
              >
                {minutes < 60 ? `${minutes}m` : `${minutes / 60}h`}
              </button>
            ))}
          </div>
          <label className="filter-label">
            Partition
            <select
              className="xp-select"
              value={partition}
              onChange={(event) => changePartition(event.target.value)}
            >
              <option value="">All partitions</option>
              {partitions.map((name) => (
                <option key={name} value={name}>{name}</option>
              ))}
            </select>
          </label>
          <button
            type="button"
            className="xp-live-btn"
            onClick={() => void loadMetricNames(tenant, partition)}
            disabled={metricLoading}
            title="Reload the metric catalog from Doctor"
          >
            {metricLoading ? <Loader2 size={12} className="animate-spin" /> : <RefreshCw size={12} />}
            {metricLoading ? "Loading…" : "Refresh"}
          </button>
        </div>
      </div>

      <div className="flex flex-1 min-h-0 flex-col gap-5 overflow-y-auto p-5">
        {metricError && (
          <div className="flex items-center gap-3 rounded-xl border border-rose-500/30 bg-rose-500/10 px-4 py-3 text-sm text-rose-300">
            <AlertTriangle size={16} className="shrink-0" />
            <span className="min-w-0 flex-1">Could not load metric names: {metricError}</span>
            <button
              type="button"
              className="shrink-0 rounded-md border border-rose-400/40 px-2.5 py-1 text-xs font-semibold hover:bg-rose-500/10"
              onClick={() => void loadMetricNames(tenant, partition)}
            >
              Retry
            </button>
          </div>
        )}

        <section>
          <div className="mb-2 flex items-center gap-2">
            <Sparkles size={14} className="text-amber-300" />
            <h3 className="text-[11px] font-bold uppercase tracking-[0.14em] text-gray-500">
              Live metrics in this partition
            </h3>
            <span className="text-[10px] text-gray-600">
              {partition || "all partitions"} · {metricNames.length} metric{metricNames.length === 1 ? "" : "s"} with data
            </span>
          </div>
          {quickMetrics.length === 0 ? (
            <div className="rounded-xl border border-dark-600 bg-dark-800 px-4 py-3 text-xs text-gray-500">
              {metricLoading ? "Discovering metrics with data…" : "No metrics with data found for this partition in the last 24h."}
            </div>
          ) : (
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-5">
              {quickMetrics.map((metricName, index) => {
                const Icon = QUICK_METRIC_ICONS[index % QUICK_METRIC_ICONS.length];
                const tone = QUICK_METRIC_TONES[index % QUICK_METRIC_TONES.length];
                return (
                  <button
                    key={metricName}
                    type="button"
                    onClick={() => chooseMetric(metricName)}
                    className="group rounded-xl border border-dark-600 bg-dark-800 p-3 text-left transition-colors hover:border-accent-blue/50 hover:bg-dark-700"
                    title={`Query ${metricName}`}
                  >
                    <span className={`inline-flex h-7 w-7 items-center justify-center rounded-lg border ${TONE_CLASS[tone]}`}>
                      <Icon size={14} />
                    </span>
                    <span className="mt-2 block truncate font-mono text-[11px] font-semibold text-gray-100">{metricName}</span>
                    <span className="mt-0.5 block truncate text-[11px] text-gray-500">{metricHint(metricName)}</span>
                  </button>
                );
              })}
            </div>
          )}
        </section>

        <div className="grid grid-cols-1 gap-4 xl:grid-cols-[320px_minmax(0,1fr)]">
          <aside className="flex min-h-[320px] flex-col overflow-hidden rounded-xl border border-dark-600 bg-dark-800">
            <div className="flex items-center justify-between gap-2 border-b border-dark-600 px-4 py-3">
              <div>
                <h3 className="text-sm font-semibold text-gray-100">Metric catalog</h3>
                <p className="mt-0.5 text-[11px] text-gray-500">
                  {metricLoading
                    ? "Discovering metrics…"
                    : `${metricNames.length} metric${metricNames.length === 1 ? "" : "s"} with data in the selected range`}
                </p>
              </div>
              {metricLoading && <Loader2 size={15} className="animate-spin text-gray-500" />}
            </div>
            <div className="relative border-b border-dark-600 p-3">
              <Search size={13} className="pointer-events-none absolute left-5 top-1/2 -translate-y-1/2 text-gray-500" />
              <input
                type="text"
                value={metricFilter}
                onChange={(event) => setMetricFilter(event.target.value)}
                placeholder="Search metric names…"
                className="w-full rounded-lg border border-dark-600 bg-dark-900 py-1.5 pl-8 pr-3 text-xs text-gray-200 placeholder:text-gray-600 focus:border-accent-blue focus:outline-none focus:ring-1 focus:ring-accent-blue/40"
              />
            </div>

            <div className="min-h-0 flex-1 overflow-y-auto p-2">
              {metricLoading && metricNames.length === 0 ? (
                <div className="flex items-center gap-2 px-2 py-6 text-xs text-gray-500">
                  <Loader2 size={13} className="animate-spin" /> Loading metrics…
                </div>
              ) : filteredMetricNames.length === 0 ? (
                <div className="space-y-3 px-1 py-2">
                  <p className="text-xs leading-relaxed text-gray-500">
                    {metricNames.length === 0
                      ? "No live metric names found in the last 24h."
                      : "No metric names match this filter."}
                  </p>
                  <details className="rounded-lg border border-dark-600 bg-dark-900/60 p-2" open={metricNames.length === 0}>
                    <summary className="cursor-pointer text-[11px] font-bold uppercase tracking-wide text-gray-500">
                      Known useful metrics
                    </summary>
                    <div className="mt-2 space-y-2">
                      {METRIC_HELPERS.map((group) => (
                        <div key={group.group}>
                          <div className="mb-1 text-[10px] font-bold uppercase tracking-wide text-gray-600">
                            {group.group}
                          </div>
                          {group.items.map((item) => (
                            <button
                              key={item.name}
                              type="button"
                              className="block w-full rounded-md px-2 py-1.5 text-left hover:bg-dark-700"
                              onClick={() => chooseMetric(item.name)}
                              title={item.hint}
                            >
                              <code className="block truncate text-[11px] text-gray-300">{item.name}</code>
                              <span className="block truncate text-[10px] text-gray-600">{item.hint}</span>
                            </button>
                          ))}
                        </div>
                      ))}
                    </div>
                  </details>
                </div>
              ) : (
                filteredMetricNames.map((metricName) => (
                  <button
                    key={metricName}
                    type="button"
                    onClick={() => chooseMetric(metricName)}
                    title={`Query ${metricName}`}
                    className={`block w-full truncate rounded-md px-2.5 py-2 text-left font-mono text-[11px] transition-colors ${
                      selectedMetric === metricName
                        ? "bg-accent-blue/15 text-sky-200"
                        : "text-gray-400 hover:bg-dark-700 hover:text-gray-200"
                    }`}
                  >
                    {metricName}
                  </button>
                ))
              )}
            </div>
          </aside>

          <section className="min-w-0 space-y-4">
            <form
              className="rounded-xl border border-dark-600 bg-dark-800 p-4"
              onSubmit={(event) => {
                event.preventDefault();
                void execute(activeExpression);
              }}
            >
              <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
                <label htmlFor="metrics-expression" className="text-sm font-semibold text-gray-100">
                  PromQL expression
                </label>
                <div className="flex items-center gap-2">
                  <button
                    type="button"
                    className="xp-live-btn"
                    onClick={copyQuery}
                    disabled={!activeExpression}
                    title="Copy the current expression"
                  >
                    {copiedQuery ? <Check size={12} /> : <Copy size={12} />}
                    {copiedQuery ? "Copied" : "Copy"}
                  </button>
                  <button
                    type="button"
                    className="xp-live-btn"
                    onClick={() => setShowAdvanced((value) => !value)}
                  >
                    {showAdvanced ? "Hide options" : "Options"}
                  </button>
                  <button type="submit" className="rounded-lg bg-accent-blue px-3.5 py-1.5 text-xs font-bold text-white transition-colors hover:bg-accent-blue/85">
                    Run query
                  </button>
                </div>
              </div>
              <textarea
                id="metrics-expression"
                className="min-h-[72px] w-full resize-y rounded-lg border border-dark-600 bg-dark-900 px-3 py-2 font-mono text-xs leading-relaxed text-gray-200 placeholder:text-gray-600 focus:border-accent-blue focus:outline-none focus:ring-1 focus:ring-accent-blue/40"
                value={query}
                onChange={(event) => {
                  setQuery(event.target.value);
                  setSelectedMetric("");
                }}
                placeholder='e.g. sum(rate(http_requests_total[5m]))'
                spellCheck={false}
              />

              {showAdvanced && (
                <div className="mt-3 flex flex-wrap items-end gap-3 border-t border-dark-600 pt-3">
                  <div className="flex items-center gap-1" role="group" aria-label="Query mode">
                    <button
                      type="button"
                      className={"xp-preset" + (mode === "range" ? " xp-active" : "")}
                      onClick={() => changeMode("range")}
                    >
                      Range
                    </button>
                    <button
                      type="button"
                      className={"xp-preset" + (mode === "instant" ? " xp-active" : "")}
                      onClick={() => changeMode("instant")}
                    >
                      Instant
                    </button>
                  </div>
                  <label className="filter-label">
                    From
                    <input
                      type="datetime-local"
                      value={from}
                      onChange={(event) => setFrom(event.target.value)}
                      disabled={mode === "instant"}
                    />
                  </label>
                  <label className="filter-label">
                    To
                    <input type="datetime-local" value={to} onChange={(event) => setTo(event.target.value)} />
                  </label>
                  <label className="filter-label">
                    Step
                    <input value={step} onChange={(event) => setStep(event.target.value)} disabled={mode === "instant"} />
                  </label>
                </div>
              )}

              <div className={`mt-3 text-xs ${statusKind === "error" ? "text-rose-300" : statusKind === "ok" ? "text-emerald-300" : "text-gray-500"}`}>
                {statusMsg}
              </div>
            </form>

            <div className="overflow-hidden rounded-xl border border-dark-600 bg-dark-800">
              <div className="flex flex-wrap items-center gap-3 border-b border-dark-600 px-4 py-2.5">
                <div className="prom-tabs m-0 border-0" role="tablist" aria-label="Metric result view">
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
                <div className="flex flex-1 flex-wrap items-center justify-end gap-1.5">
                  <span className="pill">series: {stats.series}</span>
                  <span className="pill">samples: {stats.samples}</span>
                  {response?.data.resultType && <span className="pill">type: {response.data.resultType}</span>}
                  <span className="pill">step: {step}</span>
                  <span className="pill">{formatDateTimeFull(chartFrom)} → {formatDateTimeFull(chartTo)}</span>
                </div>
              </div>

              <div className="min-h-[340px] p-4">
                {statusKind === "loading" ? (
                  <div className="flex h-[300px] items-center justify-center gap-2 text-xs text-gray-500">
                    <Loader2 size={15} className="animate-spin" /> Running query…
                  </div>
                ) : !response ? (
                  <div className="flex h-[300px] flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-dark-600 bg-dark-900/40 px-6 text-center">
                    <Activity size={22} className="text-gray-600" />
                    <p className="text-sm font-medium text-gray-300">Nothing plotted yet</p>
                    <p className="max-w-md text-xs leading-relaxed text-gray-500">
                      Pick one of the quick starts above, choose a metric from the catalog, or type a PromQL expression.
                      Selecting a metric now runs it automatically.
                    </p>
                  </div>
                ) : view === "graph" ? (
                  hasGraphSeries ? (
                    <MetricLineChart
                      series={graphSeries}
                      metricName={activeExpression}
                      onExpand={() => setChartExpanded(true)}
                      from={chartFrom}
                      to={chartTo}
                      step={step}
                      description={activeDescription}
                    />
                  ) : (
                    <div className="flex h-[300px] flex-col items-center justify-center gap-2 text-center">
                      <p className="text-sm font-medium text-gray-300">No graphable series</p>
                      <p className="max-w-md text-xs leading-relaxed text-gray-500">
                        The query returned {stats.series} series. Use the table view for instant queries, or widen the
                        time range and run it again.
                      </p>
                    </div>
                  )
                ) : view === "table" ? (
                  <DataTable
                    title="Samples"
                    columns={tableColumns(response)}
                    rows={rows}
                  />
                ) : (
                  <pre className="max-h-[520px] overflow-auto rounded-lg bg-dark-900 p-3 text-[11px] leading-relaxed text-gray-300">
                    {rawJson}
                  </pre>
                )}
              </div>

              {requestUrl && (
                <details className="border-t border-dark-600 px-4 py-2 text-[11px] text-gray-500">
                  <summary className="cursor-pointer select-none hover:text-gray-300">Request details</summary>
                  <a className="mt-1 block break-all font-mono text-sky-300 hover:text-sky-200" href={requestUrl} target="_blank" rel="noreferrer">
                    GET {requestUrl}
                  </a>
                </details>
              )}
            </div>
            <ChartExpandModal
              open={chartExpanded}
              title={activeExpression || "Metric chart"}
              description={activeDescription}
              series={graphSeries}
              queries={activeExpression ? [activeExpression] : []}
              from={chartFrom}
              to={chartTo}
              maxSeries={30}
              onClose={() => setChartExpanded(false)}
            />
          </section>
        </div>
      </div>
    </div>
  );
}
