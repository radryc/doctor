import { useCallback, useEffect, useMemo, useState } from "react";
import { Activity, AlertTriangle, Clock, Gauge, Search, Sparkles, BarChart3 } from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import QuickGraphs from "../components/charts/QuickGraphs";
import MetricLineChart from "../components/charts/MetricLineChart";
import type { DiscoveredService, ServiceSuggestion } from "../types/api";

const signalBadge = (hasSignal: boolean, label: string) => (
  <span
    className={`text-[10px] px-1.5 py-0.5 rounded border ${
      hasSignal
        ? "border-emerald-500/30 text-emerald-300 bg-emerald-500/10"
        : "border-slate-700 text-slate-500 bg-slate-800/50"
    }`}
  >
    {label}
  </span>
);

export default function ServicesPage() {
  const { config } = useConfig();
  const tenant = config.default_tenant || "default";
  const [services, setServices] = useState<DiscoveredService[]>([]);
  const [loading, setLoading] = useState(true);
  const [search, setSearch] = useState("");
  const [selectedService, setSelectedService] = useState<DiscoveredService | null>(null);
  const [suggestions, setSuggestions] = useState<ServiceSuggestion[]>([]);
  const [expandedGraphIdx, setExpandedGraphIdx] = useState<number | null>(null);
  const [graphData, setGraphData] = useState<any[]>([]);
  const [graphLoading, setGraphLoading] = useState(false);

  const loadServices = useCallback(async () => {
    setLoading(true);
    try {
      const params = new URLSearchParams({ tenant, window: "24h" });
      const data = await api.discoverServices(params);
      setServices(data.services || []);
    } catch {
      setServices([]);
    } finally {
      setLoading(false);
    }
  }, [tenant]);

  useEffect(() => {
    void loadServices();
    const interval = setInterval(loadServices, 60_000);
    return () => clearInterval(interval);
  }, [loadServices]);

  const filteredServices = useMemo(() => {
    const needle = search.trim().toLowerCase();
    if (!needle) return services;
    return services.filter((s) =>
      [s.name, s.partition, s.intent].filter(Boolean).join(" ").toLowerCase().includes(needle)
    );
  }, [services, search]);

  const selectService = async (svc: DiscoveredService) => {
    setSelectedService(svc);
    setExpandedGraphIdx(null);
    setGraphData([]);
    try {
      const params = new URLSearchParams({
        service: svc.name,
        window: "5m",
        tenant,
      });
      const data = await api.suggestServiceQueries(params);
      setSuggestions(data.suggestions || []);
    } catch {
      setSuggestions([]);
    }
  };

  const expandGraph = async (suggestion: ServiceSuggestion, idx: number) => {
    if (expandedGraphIdx === idx) {
      setExpandedGraphIdx(null);
      setGraphData([]);
      return;
    }
    setGraphLoading(true);
    setExpandedGraphIdx(idx);
    try {
      const end = new Date();
      const start = new Date(end.getTime() - 60 * 60_000);
      const params = new URLSearchParams({
        tenant,
        query: suggestion.query,
        start: start.toISOString(),
        end: end.toISOString(),
        step: "30s",
      });
      const data = await api.queryPrometheusRange(params);
      if (data.data?.resultType === "matrix" && data.data.result?.length > 0) {
        const series = data.data.result.map((s: any) => ({
          label: Object.entries(s.metric || {})
            .filter(([k]) => k !== "__name__")
            .map(([k, v]) => `${k}="${v}"`)
            .join(",") || suggestion.label,
          points: s.values.map((v: [number, string]) => ({
            timestamp: new Date(v[0] * 1000).toISOString(),
            value: Number(v[1]),
          })),
        }));
        setGraphData(series);
      } else {
        setGraphData([]);
      }
    } catch {
      setGraphData([]);
    } finally {
      setGraphLoading(false);
    }
  };

  if (selectedService) {
    return (
      <div className="h-full w-full overflow-auto bg-slate-950">
        <div className="px-6 pt-4">
          <button
            type="button"
            onClick={() => setSelectedService(null)}
            className="text-xs text-slate-400 hover:text-slate-200 mb-3"
          >
            ← Back to services
          </button>
          <div className="flex items-center gap-2 mb-4">
            <h2 className="text-lg font-semibold text-slate-100">{selectedService.name}</h2>
            <div className="flex gap-1">
              {signalBadge(selectedService.has_traces, "traces")}
              {signalBadge(selectedService.has_logs, "logs")}
              {signalBadge(selectedService.has_metrics, "metrics")}
            </div>
          </div>

          {selectedService.partition && (
            <div className="text-xs text-slate-500 mb-4">
              Partition: {selectedService.partition}
              {selectedService.intent && ` · Intent: ${selectedService.intent}`}
              {selectedService.error_rate !== undefined && selectedService.error_rate > 0 && (
                <span className="ml-2 text-rose-400">
                  Error rate: {(selectedService.error_rate * 100).toFixed(1)}%
                </span>
              )}
            </div>
          )}

          <div className="grid grid-cols-1 xl:grid-cols-[320px_1fr] gap-4">
            {/* Sidebar: suggestions */}
            <div className="bg-slate-900/50 border border-slate-800 rounded-lg p-3">
              <div className="flex items-center gap-1.5 text-xs font-semibold text-slate-300 mb-3">
                <Sparkles className="w-3.5 h-3.5 text-amber-400" />
                Auto-generated queries ({suggestions.length})
              </div>
              {suggestions.map((s, idx) => {
                const typeColors: Record<string, string> = {
                  rate: "border-sky-500/30 text-sky-300 bg-sky-500/10",
                  latency: "border-violet-500/30 text-violet-300 bg-violet-500/10",
                  errors: "border-rose-500/30 text-rose-300 bg-rose-500/10",
                  saturation: "border-amber-500/30 text-amber-300 bg-amber-500/10",
                };
                const typeIcons: Record<string, typeof Activity> = {
                  rate: Activity,
                  latency: Clock,
                  errors: AlertTriangle,
                  saturation: Gauge,
                };
                const Icon = typeIcons[s.type] || BarChart3;
                const colorClass = typeColors[s.type] || "border-slate-700 text-slate-400 bg-slate-800/50";
                const isExpanded = expandedGraphIdx === idx;

                return (
                  <div key={idx} className={`mb-2 border rounded-lg overflow-hidden ${isExpanded ? "border-sky-500/40" : "border-slate-800"}`}>
                    <button
                      type="button"
                      onClick={() => void expandGraph(s, idx)}
                      className="w-full text-left px-2.5 py-2 flex items-center gap-2 hover:bg-slate-800/50"
                    >
                      <div className={`p-1 rounded border ${colorClass}`}>
                        <Icon className="w-3 h-3" />
                      </div>
                      <div className="flex-1 min-w-0">
                        <div className="text-[11px] text-slate-200">{s.label}</div>
                        <div className="text-[10px] text-slate-500 truncate">{s.description}</div>
                      </div>
                    </button>
                    {isExpanded && (
                      <div className="px-2.5 pb-2 border-t border-slate-800">
                        <pre className="mt-1.5 text-[9px] text-slate-500 bg-slate-950 rounded p-1.5 overflow-x-auto">
                          {s.query}
                        </pre>
                        {graphLoading ? (
                          <div className="text-[10px] text-slate-500 py-2 text-center">Loading...</div>
                        ) : graphData.length > 0 ? (
                          <MetricLineChart series={graphData} metricName={s.label} />
                        ) : (
                          <div className="text-[10px] text-slate-500 py-2 text-center">No data in last hour</div>
                        )}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>

            {/* Main: quick graphs dashboard */}
            <div className="bg-slate-900/50 border border-slate-800 rounded-lg p-3">
              <h3 className="text-xs font-semibold text-slate-300 mb-3">
                RED Dashboard — {selectedService.name}
              </h3>
              <QuickGraphs
                serviceName={selectedService.name}
                onApplyQuery={() => {}}
              />
            </div>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="h-full w-full overflow-auto bg-slate-950">
      <div className="px-6 pt-4">
        <div className="flex items-center justify-between mb-4">
          <div>
            <h2 className="text-lg font-semibold text-slate-100">Services</h2>
            <p className="text-xs text-slate-500">
              {loading ? "Discovering services..." : `${services.length} services detected in the last 24h`}
            </p>
          </div>
          <div className="relative">
            <Search className="w-3.5 h-3.5 absolute left-2.5 top-1/2 -translate-y-1/2 text-slate-500" />
            <input
              type="text"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Filter services..."
              className="pl-8 pr-3 py-1.5 text-xs bg-slate-900 border border-slate-700 rounded-lg text-slate-200 placeholder:text-slate-500 focus:outline-none focus:ring-1 focus:ring-sky-400 w-56"
            />
          </div>
        </div>

        {loading ? (
          <div className="text-xs text-slate-500 py-8 text-center">Scanning telemetry signals...</div>
        ) : filteredServices.length === 0 ? (
          <div className="text-xs text-slate-500 py-8 text-center">
            No services detected. Ensure OpenTelemetry exporters are configured and sending to Doctor.
          </div>
        ) : (
          <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-3">
            {filteredServices.map((svc) => (
              <button
                key={svc.name}
                type="button"
                onClick={() => void selectService(svc)}
                className="text-left p-3 rounded-lg border border-slate-800 bg-slate-900/50 hover:border-sky-500/40 hover:bg-sky-500/5 transition-colors"
              >
                <div className="flex items-center justify-between mb-2">
                  <span className="text-xs font-semibold text-slate-200 truncate">{svc.name}</span>
                  <Sparkles className="w-3 h-3 text-amber-400/60" />
                </div>
                <div className="flex gap-1 mb-2">
                  {signalBadge(svc.has_traces, "traces")}
                  {signalBadge(svc.has_logs, "logs")}
                  {signalBadge(svc.has_metrics, "metrics")}
                </div>
                <div className="text-[10px] text-slate-500">
                  {svc.trace_count > 0 && `${svc.trace_count} traces`}
                  {svc.trace_count > 0 && svc.log_count > 0 && " · "}
                  {svc.log_count > 0 && `${svc.log_count} logs`}
                  {svc.metric_names && svc.metric_names.length > 0 && ` · ${svc.metric_names.length} metrics`}
                </div>
                {svc.error_rate !== undefined && svc.error_rate > 0 && (
                  <div className="text-[10px] text-rose-400 mt-1">
                    Error rate: {(svc.error_rate * 100).toFixed(1)}%
                  </div>
                )}
                {svc.suggested_dashboards && svc.suggested_dashboards.length > 0 && (
                  <div className="text-[10px] text-sky-400 mt-1">
                    {svc.suggested_dashboards.map((d: string) => d.toUpperCase()).join(" + ")} dashboards available
                  </div>
                )}
              </button>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
