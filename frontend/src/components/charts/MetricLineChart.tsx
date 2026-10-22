import { useMemo } from "react";
import { Maximize2 } from "lucide-react";
import TimeSeriesChart, { formatChartValue } from "./TimeSeriesChart";
import SeriesDetails from "./SeriesDetails";
import type { MetricSeries } from "../../types/api";

interface MetricLineChartProps {
  series: MetricSeries[];
  metricName?: string;
  onExpand?: () => void;
  from?: Date;
  to?: Date;
  step?: string;
  description?: string;
}

export default function MetricLineChart({
  series,
  metricName,
  onExpand,
  from,
  to,
  step,
  description,
}: MetricLineChartProps) {
  const labelSummary = useMemo(() => {
    const values = new Map<string, Set<string>>();
    for (const item of series.slice(0, 30)) {
      for (const [key, value] of Object.entries(item.labels ?? {})) {
        if (key === "__name__" || value === "") continue;
        let bucket = values.get(key);
        if (!bucket) {
          bucket = new Set();
          values.set(key, bucket);
        }
        bucket.add(value);
      }
    }
    return [...values.entries()].map(([key, bucket]) => ({
      key,
      values: [...bucket].sort(),
    }));
  }, [series]);

  if (series.length === 0) return null;

  let min = Infinity;
  let max = -Infinity;
  let sum = 0;
  let count = 0;
  for (const item of series) {
    for (const point of item.points) {
      const value = Number(point.value);
      if (!Number.isFinite(value)) continue;
      if (value < min) min = value;
      if (value > max) max = value;
      sum += value;
      count += 1;
    }
  }
  if (count === 0) return null;

  const avg = sum / count;
  const maxSeries = 30;

  return (
    <section className="result-block">
      <div className="metric-chart-header">
        <div>
          <h3>{metricName ? `Metric: ${metricName}` : "Metric values over time"}</h3>
          {from && to && (
            <div className="metric-chart-subtitle">
              {from.toLocaleString()} → {to.toLocaleString()}
              {step ? ` · step ${step}` : ""}
            </div>
          )}
          {description && <div className="metric-chart-subtitle">{description}</div>}
        </div>
        {onExpand && (
          <button
            type="button"
            className="xp-live-btn"
            onClick={onExpand}
            title="Open a larger chart with series details"
          >
            <Maximize2 size={12} /> Expand
          </button>
        )}
      </div>
      <div className="metric-chart-stats">
        <span>series {series.length}</span>
        <span>samples {count}</span>
        <span>min {formatChartValue(min)}</span>
        <span>avg {formatChartValue(avg)}</span>
        <span>max {formatChartValue(max)}</span>
      </div>
      {labelSummary.length > 0 && (
        <div className="series-label-summary" aria-label="Labels across returned series">
          {labelSummary.map(({ key, values }) => {
            const shown = values.slice(0, 4).join(" · ");
            const extra = values.length > 4 ? ` +${values.length - 4}` : "";
            return (
              <span className="label-chip" key={key} title={`${key}: ${values.join(", ")}`}>
                <strong>{key}</strong>
                <span>{shown}{extra}</span>
              </span>
            );
          })}
        </div>
      )}
      {series.length > maxSeries && (
        <p className="metric-chart-note">
          Showing {maxSeries} of {series.length} series. Aggregate with sum/avg/topk or add label filters to see the rest.
        </p>
      )}
      <div style={{ height: 300 }}>
        <TimeSeriesChart
          series={series}
          displayMode="line"
          showLegend
          maxSeries={maxSeries}
          onExpand={onExpand}
        />
      </div>
      <SeriesDetails series={series} maxSeries={maxSeries} />
    </section>
  );
}
