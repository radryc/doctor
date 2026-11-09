import { useEffect, useMemo, useState } from "react";
import { Activity, AlertTriangle, Gauge, Play, Plus, Server, Trash2, WifiOff, X } from "lucide-react";
import { api } from "../../api/client";
import type {
  AlertPreviewResponse,
  AlertRule,
  AlertSpec,
  Matcher,
  Receiver,
  SignalQuery,
  SignalType,
} from "../../types/alerts";
import {
  SIGNALS,
  defaultSpec,
  describeSpec,
  renderSummary,
  signalDefinition,
} from "./signals";

const SIGNAL_ICONS = {
  errors: AlertTriangle,
  latency: Activity,
  traffic: Server,
  saturation: Gauge,
  availability: WifiOff,
} as const;

interface AlertBuilderProps {
  tenant: string;
  partition: string;
  rule: AlertRule;
  endpoints: Receiver[];
  onChange: (rule: AlertRule) => void;
}

export default function AlertBuilder({ tenant, partition, rule, endpoints, onChange }: AlertBuilderProps) {
  const spec = useMemo(() => rule.spec ?? defaultSpec("errors", partition), [rule.spec, partition]);
  const [metricNames, setMetricNames] = useState<string[]>([]);
  const [metricType, setMetricType] = useState("");
  const [labelNames, setLabelNames] = useState<string[]>([]);
  const [labelValues, setLabelValues] = useState<Record<string, string[]>>({});
  const [preview, setPreview] = useState<AlertPreviewResponse | null>(null);
  const [previewError, setPreviewError] = useState("");
  const [previewing, setPreviewing] = useState(false);

  useEffect(() => {
    api
      .getMetricNames()
      .then((res) => setMetricNames((res.data ?? []).slice().sort()))
      .catch(() => {});
    api
      .getMetricLabels()
      .then((res) => setLabelNames((res.data ?? []).slice().sort()))
      .catch(() => {});
  }, []);

  useEffect(() => {
    if (!spec.primary.metric) {
      setMetricType("");
      return;
    }
    let cancelled = false;
    api
      .getMetricMetadata(spec.primary.metric)
      .then((res) => {
        if (cancelled) return;
        const meta = res.data?.[spec.primary.metric];
        setMetricType(meta?.type ?? "");
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [spec.primary.metric]);

  const updateSpec = (next: AlertSpec, extra: Partial<AlertRule> = {}) => {
    const summary = next.summary || renderSummary(next);
    onChange({
      ...rule,
      ...extra,
      mode: "guided",
      spec: { ...next, summary },
      for: next.for,
      labels: { ...(rule.labels ?? {}), severity: next.severity || "warning" },
      annotations: {
        ...(rule.annotations ?? {}),
        summary,
        "sentinel.signal": next.signal,
      },
    });
  };

  const updatePrimary = (patch: Partial<SignalQuery>) => {
    const primary = { ...spec.primary, ...patch };
    const next = { ...spec, primary };
    if (!spec.summary) next.summary = renderSummary(next);
    updateSpec(next);
  };

  const chooseSignal = (signal: SignalType) => {
    const next = defaultSpec(signal, partition);
    next.summary = renderSummary(next);
    next.destinations = spec.destinations;
    updateSpec(next, { notify: rule.notify });
  };

  const setFilter = (index: number, patch: Partial<Matcher>) => {
    const filters = (spec.primary.filters ?? []).map((filter, i) => (i === index ? { ...filter, ...patch } : filter));
    updatePrimary({ filters });
    const label = patch.name;
    if (label && !labelValues[label]) {
      api
        .getMetricLabelValues(label)
        .then((res) => setLabelValues((prev) => ({ ...prev, [label]: res.data ?? [] })))
        .catch(() => {});
    }
  };

  const addFilter = () => updatePrimary({ filters: [...(spec.primary.filters ?? []), { name: "service", operator: "=", value: "" }] });
  const removeFilter = (index: number) =>
    updatePrimary({ filters: (spec.primary.filters ?? []).filter((_, i) => i !== index) });

  const enableDenominator = () => {
    const denominator: SignalQuery = {
      metric: spec.primary.metric,
      filters: (spec.primary.filters ?? []).filter((filter) => !/status|error|result/i.test(filter.name)),
      function: spec.primary.function,
      reduce: spec.primary.reduce,
      by: spec.primary.by,
    };
    updateSpec({ ...spec, denominator });
  };

  const updateDenominator = (patch: Partial<SignalQuery>) =>
    updateSpec({ ...spec, denominator: { ...(spec.denominator ?? { metric: "" }), ...patch } });

  const runPreview = async () => {
    setPreviewing(true);
    setPreviewError("");
    try {
      const result = await api.previewAlertRule(tenant, partition, { spec });
      setPreview(result);
    } catch (error) {
      setPreview(null);
      setPreviewError(error instanceof Error ? error.message : "preview failed");
    } finally {
      setPreviewing(false);
    }
  };

  const definition = signalDefinition(spec.signal);
  const groupBy = (spec.primary.by ?? []).join(", ");
  const destination = rule.notify?.[0] || "";

  return (
    <div className="space-y-5">
      <div>
        <p className="text-[10px] font-bold uppercase tracking-[0.18em] text-slate-500">1 · Choose the signal</p>
        <div className="mt-2 grid grid-cols-2 gap-2 xl:grid-cols-5">
          {SIGNALS.map((signal) => {
            const Icon = SIGNAL_ICONS[signal.id];
            const active = spec.signal === signal.id;
            return (
              <button
                key={signal.id}
                type="button"
                onClick={() => chooseSignal(signal.id)}
                className={`rounded-lg border p-3 text-left transition ${
                  active
                    ? "border-[var(--strata-accent)] bg-slate-800/80"
                    : "border-slate-800 bg-slate-950/60 hover:border-slate-600"
                }`}
              >
                <Icon size={15} className={active ? "text-[var(--strata-accent-text)]" : "text-slate-500"} />
                <div className="mt-2 text-xs font-semibold text-slate-200">{signal.label}</div>
                <div className="mt-0.5 text-[10px] leading-snug text-slate-500">{signal.question}</div>
              </button>
            );
          })}
        </div>
        <p className="mt-2 text-[11px] text-slate-500">{definition.description}</p>
      </div>

      <div className="rounded-lg border border-slate-800 bg-slate-950/40 p-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <p className="text-[10px] font-bold uppercase tracking-[0.18em] text-slate-500">2 · What to watch</p>
            <p className="mt-1 text-[11px] text-slate-500">Search metrics by name. Filters narrow the data to your service or partition.</p>
          </div>
          <button type="button" onClick={() => void runPreview()} className="btn text-xs" disabled={previewing}>
            <Play size={12} /> {previewing ? "Checking…" : "Preview"}
          </button>
        </div>

        <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-2">
          <label className="block text-[11px] text-slate-400">
            Metric
            <input
              list={`metrics-${rule.alert}`}
              value={spec.primary.metric}
              onChange={(event) => updatePrimary({ metric: event.target.value })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 font-mono text-xs"
              placeholder="start typing, for example http_requests_total"
            />
            <datalist id={`metrics-${rule.alert}`}>
              {metricNames.slice(0, 500).map((name) => (
                <option key={name} value={name} />
              ))}
            </datalist>
            {metricType && <span className="mt-1 block text-[10px] text-slate-500">Detected type: {metricType}</span>}
          </label>
          <label className="block text-[11px] text-slate-400">
            Group by (one alert per value)
            <input
              value={groupBy}
              onChange={(event) => updatePrimary({ by: event.target.value.split(",").map((item) => item.trim()).filter(Boolean) })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 font-mono text-xs"
              placeholder="service, guardian_partition"
            />
          </label>
        </div>

        <div className="mt-3">
          <div className="flex items-center justify-between">
            <span className="text-[11px] text-slate-400">Label filters</span>
            <button type="button" onClick={addFilter} className="text-[11px] text-[var(--strata-accent-text)] hover:underline">
              <Plus size={11} className="inline" /> Add filter
            </button>
          </div>
          <div className="mt-2 space-y-2">
            {(spec.primary.filters ?? []).map((filter, index) => (
              <div key={`${filter.name}-${index}`} className="grid grid-cols-[minmax(0,1fr)_90px_minmax(0,1fr)_28px] items-center gap-2">
                <input
                  list={`labels-${rule.alert}`}
                  value={filter.name}
                  onChange={(event) => setFilter(index, { name: event.target.value })}
                  className="rounded border border-slate-700 bg-slate-950 px-2 py-1 font-mono text-[11px]"
                  placeholder="label"
                />
                <select
                  value={filter.operator || "="}
                  onChange={(event) => setFilter(index, { operator: event.target.value as Matcher["operator"] })}
                  className="rounded border border-slate-700 bg-slate-950 px-1 py-1 text-[11px]"
                >
                  <option value="=">=</option>
                  <option value="!=">!=</option>
                  <option value="=~">=~</option>
                  <option value="!~">!~</option>
                </select>
                <input
                  list={`values-${rule.alert}-${filter.name}`}
                  value={filter.value}
                  onChange={(event) => setFilter(index, { value: event.target.value })}
                  className="rounded border border-slate-700 bg-slate-950 px-2 py-1 font-mono text-[11px]"
                  placeholder="value"
                />
                <datalist id={`values-${rule.alert}-${filter.name}`}>
                  {(labelValues[filter.name] ?? []).map((value) => (
                    <option key={value} value={value} />
                  ))}
                </datalist>
                <button type="button" onClick={() => removeFilter(index)} className="text-slate-500 hover:text-rose-300">
                  <X size={13} />
                </button>
              </div>
            ))}
            <datalist id={`labels-${rule.alert}`}>
              {labelNames.map((name) => (
                <option key={name} value={name} />
              ))}
            </datalist>
            {(spec.primary.filters ?? []).length === 0 && (
              <p className="text-[10px] text-slate-600">No filters. This may match series from every partition.</p>
            )}
          </div>
        </div>

        <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-3">
          <label className="block text-[11px] text-slate-400">
            Counter or gauge handling
            <select
              value={spec.primary.function || "none"}
              onChange={(event) => updatePrimary({ function: event.target.value })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            >
              <option value="rate">Rate (counter, per second)</option>
              <option value="increase">Increase (counter, total)</option>
              <option value="avg_over_time">Average over window</option>
              <option value="max_over_time">Maximum over window</option>
              <option value="min_over_time">Minimum over window</option>
              <option value="none">Raw value (gauge)</option>
            </select>
          </label>
          <label className="block text-[11px] text-slate-400">
            Aggregate
            <select
              value={spec.primary.reduce || "sum"}
              onChange={(event) => updatePrimary({ reduce: event.target.value })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            >
              <option value="sum">Sum</option>
              <option value="avg">Average</option>
              <option value="max">Maximum</option>
              <option value="min">Minimum</option>
              <option value="count">Count</option>
              <option value="none">None</option>
            </select>
          </label>
          {spec.signal === "latency" && (
            <label className="block text-[11px] text-slate-400">
              Percentile
              <select
                value={String(spec.primary.quantile ?? 0.95)}
                onChange={(event) => updatePrimary({ quantile: Number(event.target.value) })}
                className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
              >
                <option value="0.5">p50</option>
                <option value="0.9">p90</option>
                <option value="0.95">p95</option>
                <option value="0.99">p99</option>
              </select>
            </label>
          )}
        </div>

        {(spec.signal === "errors" || spec.signal === "saturation" || spec.signal === "availability") && (
          <div className="mt-3 rounded border border-slate-800 bg-slate-900/40 p-3">
            <div className="flex items-center justify-between">
              <div>
                <div className="text-[11px] font-medium text-slate-300">Compare against a second metric (ratio)</div>
                <div className="text-[10px] text-slate-500">
                  {spec.signal === "errors" ? "Errors divided by total requests." : "Usage divided by limit or capacity."}
                </div>
              </div>
              {spec.denominator ? (
                <button
                  type="button"
                  onClick={() => updateSpec({ ...spec, denominator: null })}
                  className="text-slate-500 hover:text-rose-300"
                >
                  <Trash2 size={13} />
                </button>
              ) : (
                <button type="button" onClick={enableDenominator} className="btn text-xs">
                  <Plus size={12} /> Add ratio
                </button>
              )}
            </div>
            {spec.denominator && (
              <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-2">
                <label className="block text-[11px] text-slate-400">
                  Denominator metric
                  <input
                    list={`metrics-${rule.alert}`}
                    value={spec.denominator.metric}
                    onChange={(event) => updateDenominator({ metric: event.target.value })}
                    className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 font-mono text-xs"
                  />
                </label>
                <label className="block text-[11px] text-slate-400">
                  Denominator group by
                  <input
                    value={(spec.denominator.by ?? []).join(", ")}
                    onChange={(event) => updateDenominator({ by: event.target.value.split(",").map((item) => item.trim()).filter(Boolean) })}
                    className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 font-mono text-xs"
                  />
                </label>
              </div>
            )}
          </div>
        )}
      </div>

      <div className="rounded-lg border border-slate-800 bg-slate-950/40 p-4">
        <p className="text-[10px] font-bold uppercase tracking-[0.18em] text-slate-500">3 · When should it alert?</p>
        <div className="mt-3 grid grid-cols-2 gap-3 md:grid-cols-4">
          <label className="block text-[11px] text-slate-400">
            Condition
            <select
              value={spec.condition.op}
              onChange={(event) => updateSpec({ ...spec, condition: { ...spec.condition, op: event.target.value } })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            >
              <option value="gt">is above</option>
              <option value="gte">is at or above</option>
              <option value="lt">is below</option>
              <option value="lte">is at or below</option>
              <option value="eq">equals</option>
              <option value="neq">is not equal to</option>
              {spec.signal === "availability" && <option value="absent">has no data</option>}
            </select>
          </label>
          <label className="block text-[11px] text-slate-400">
            Threshold
            <input
              type="number"
              value={spec.condition.value}
              onChange={(event) => updateSpec({ ...spec, condition: { ...spec.condition, value: Number(event.target.value) } })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            />
          </label>
          <label className="block text-[11px] text-slate-400">
            Unit
            <select
              value={spec.condition.unit || ""}
              onChange={(event) => updateSpec({ ...spec, condition: { ...spec.condition, unit: event.target.value } })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            >
              <option value="">none</option>
              <option value="percent">percent</option>
              <option value="ms">milliseconds</option>
              <option value="s">seconds</option>
              <option value="rps">requests/second</option>
              <option value="millicores">millicores</option>
              <option value="bytes">bytes</option>
            </select>
          </label>
          <label className="block text-[11px] text-slate-400">
            For
            <select
              value={spec.for || "5m"}
              onChange={(event) => updateSpec({ ...spec, for: event.target.value })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            >
              {["1m", "5m", "10m", "15m", "30m", "1h"].map((value) => (
                <option key={value} value={value}>{value}</option>
              ))}
            </select>
          </label>
          <label className="block text-[11px] text-slate-400">
            Look-back window
            <select
              value={spec.window || "5m"}
              onChange={(event) => updateSpec({ ...spec, window: event.target.value })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            >
              {["1m", "5m", "10m", "15m", "30m"].map((value) => (
                <option key={value} value={value}>{value}</option>
              ))}
            </select>
          </label>
          <label className="block text-[11px] text-slate-400">
            Severity
            <select
              value={spec.severity || "warning"}
              onChange={(event) => updateSpec({ ...spec, severity: event.target.value })}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            >
              <option value="info">Info</option>
              <option value="warning">Warning</option>
              <option value="critical">Critical</option>
            </select>
          </label>
          <label className="block text-[11px] text-slate-400 md:col-span-2">
            Destination
            <select
              value={destination}
              onChange={(event) => {
                const value = event.target.value;
                onChange({
                  ...rule,
                  notify: value ? [value] : [],
                  spec: rule.spec ? { ...rule.spec, destinations: value ? [value] : [] } : rule.spec,
                });
              }}
              className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            >
              <option value="">Use partition/default routing</option>
              {endpoints.map((endpoint) => (
                <option key={endpoint.name} value={endpoint.name}>
                  {endpoint.name} · {endpoint.type}
                </option>
              ))}
            </select>
          </label>
        </div>

        <label className="mt-3 block text-[11px] text-slate-400">
          Summary shown in the alert
          <input
            value={spec.summary ?? ""}
            onChange={(event) => updateSpec({ ...spec, summary: event.target.value })}
            className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
            placeholder={renderSummary(spec)}
          />
        </label>

        <div className="mt-3 rounded border border-slate-800 bg-slate-900/50 p-3 text-[11px] text-slate-400">
          <span className="text-slate-500">In plain language: </span>
          {describeSpec(spec)}
        </div>

        {(previewError || preview) && (
          <div className="mt-3 rounded border border-slate-800 bg-slate-950 p-3 text-[11px]">
            {previewError ? (
              <span className="text-rose-300">{previewError}</span>
            ) : preview ? (
              <div className="space-y-2">
                <div className="flex flex-wrap items-center gap-2">
                  <span className={`rounded border px-1.5 py-0.5 text-[10px] uppercase ${
                    preview.wouldFire ? "border-rose-500/40 bg-rose-500/10 text-rose-300" : "border-emerald-500/40 bg-emerald-500/10 text-emerald-300"
                  }`}>
                    {preview.wouldFire ? "would fire now" : "not firing right now"}
                  </span>
                  <span className="text-slate-500">{preview.instances?.length ?? 0} matching series</span>
                </div>
                <code className="block overflow-x-auto rounded bg-slate-900 p-2 font-mono text-[10px] text-slate-300">{preview.expr}</code>
                {(preview.instances ?? []).slice(0, 6).map((instance, index) => (
                  <div key={index} className="flex items-center justify-between gap-2 text-[10px] text-slate-400">
                    <span className="truncate font-mono">
                      {Object.entries(instance.labels).filter(([key]) => key !== "__name__").map(([key, value]) => `${key}=${value}`).join(", ") || "aggregate"}
                    </span>
                    <span className="text-slate-200">{instance.value}</span>
                  </div>
                ))}
              </div>
            ) : null}
          </div>
        )}
      </div>
    </div>
  );
}
