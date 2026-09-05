import { useCallback, useEffect, useState } from "react";
import { Activity, BarChart3, Clock, AlertTriangle, Gauge, Loader2, Sparkles } from "lucide-react";
import { api } from "../../api/client";
import MetricLineChart from "./MetricLineChart";
import type { ServiceSuggestion } from "../../types/api";

interface QuickGraphsProps {
  serviceName: string;
  window?: string;
  onApplyQuery?: (query: string, label: string) => void;
}

const typeIcon: Record<string, typeof Activity> = {
  rate: Activity,
  latency: Clock,
  errors: AlertTriangle,
  saturation: Gauge,
};

const typeColor: Record<string, string> = {
  rate: "text-sky-400 bg-sky-500/10 border-sky-500/30",
  latency: "text-violet-400 bg-violet-500/10 border-violet-500/30",
  errors: "text-rose-400 bg-rose-500/10 border-rose-500/30",
  saturation: "text-amber-400 bg-amber-500/10 border-amber-500/30",
};

export default function QuickGraphs({ serviceName, window = "5m", onApplyQuery }: QuickGraphsProps) {
  const [suggestions, setSuggestions] = useState<ServiceSuggestion[]>([]);
  const [loading, setLoading] = useState(true);
  const [expandedIdx, setExpandedIdx] = useState<number | null>(null);
  const [expandedData, setExpandedData] = useState<any[]>([]);
  const [chartLoading, setChartLoading] = useState(false);

  const loadSuggestions = useCallback(async () => {
    setLoading(true);
    try {
      const params = new URLSearchParams({
        service: serviceName,
        window,
        tenant: "default",
      });
      const data = await api.suggestServiceQueries(params);
      setSuggestions(data.suggestions || []);
    } catch {
      setSuggestions([]);
    } finally {
      setLoading(false);
    }
  }, [serviceName, window]);

  useEffect(() => {
    void loadSuggestions();
  }, [loadSuggestions]);

  const expandGraph = async (suggestion: ServiceSuggestion, idx: number) => {
    if (expandedIdx === idx) {
      setExpandedIdx(null);
      setExpandedData([]);
      return;
    }
    setChartLoading(true);
    setExpandedIdx(idx);
    try {
      const end = new Date();
      const start = new Date(end.getTime() - 60 * 60_000);
      const params = new URLSearchParams({
        tenant: "default",
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
        setExpandedData(series);
      } else {
        setExpandedData([]);
      }
    } catch {
      setExpandedData([]);
    } finally {
      setChartLoading(false);
    }
  };

  if (loading) {
    return (
      <div className="flex items-center gap-2 text-xs text-slate-400 py-4">
        <Loader2 className="w-3.5 h-3.5 animate-spin" />
        Generating AI suggestions for {serviceName}...
      </div>
    );
  }

  if (suggestions.length === 0) {
    return (
      <div className="text-xs text-slate-500 py-4">
        No auto-generated suggestions for {serviceName}. Try the Metrics page to build custom queries.
      </div>
    );
  }

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-1.5 text-xs font-semibold text-slate-300 mb-2">
        <Sparkles className="w-3.5 h-3.5 text-amber-400" />
        Auto-generated graphs ({suggestions.length})
      </div>

      {suggestions.map((s, idx) => {
        const Icon = typeIcon[s.type] || BarChart3;
        const colorClass = typeColor[s.type] || "text-slate-400 bg-slate-500/10 border-slate-500/30";
        const isExpanded = expandedIdx === idx;

        return (
          <div key={idx} className="border border-slate-800 rounded-lg overflow-hidden bg-slate-900/50">
            <button
              type="button"
              onClick={() => void expandGraph(s, idx)}
              className="w-full text-left px-3 py-2 flex items-center gap-2 hover:bg-slate-800/50 transition-colors"
            >
              <div className={`p-1.5 rounded border ${colorClass}`}>
                <Icon className="w-3.5 h-3.5" />
              </div>
              <div className="flex-1 min-w-0">
                <div className="text-xs text-slate-200 font-medium">{s.label}</div>
                <div className="text-[10px] text-slate-500 truncate">{s.description}</div>
              </div>
              <div className="text-[10px] text-slate-500">{s.window}</div>
            </button>

            {isExpanded && (
              <div className="px-3 pb-3 border-t border-slate-800">
                <pre className="mt-2 text-[10px] text-slate-400 bg-slate-950 rounded p-2 overflow-x-auto">
                  {s.query}
                </pre>
                {chartLoading ? (
                  <div className="flex items-center gap-2 text-xs text-slate-400 py-4 justify-center">
                    <Loader2 className="w-3.5 h-3.5 animate-spin" />
                    Loading data...
                  </div>
                ) : expandedData.length > 0 ? (
                  <div className="mt-2">
                    <MetricLineChart series={expandedData} metricName={s.label} />
                  </div>
                ) : (
                  <div className="text-[10px] text-slate-500 py-3 text-center">
                    No data available for this query in the last hour.
                  </div>
                )}
                {onApplyQuery && (
                  <button
                    type="button"
                    onClick={() => onApplyQuery(s.query, s.label)}
                    className="mt-2 text-[10px] text-sky-400 hover:text-sky-300 border border-sky-500/30 px-2 py-1 rounded hover:bg-sky-500/10"
                  >
                    Use in dashboard
                  </button>
                )}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
