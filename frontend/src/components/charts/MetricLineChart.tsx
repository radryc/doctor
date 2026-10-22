import TimeSeriesChart, { formatChartValue } from "./TimeSeriesChart";
import type { MetricSeries } from "../../types/api";

interface MetricLineChartProps {
  series: MetricSeries[];
  metricName?: string;
}

export default function MetricLineChart({ series, metricName }: MetricLineChartProps) {
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
      <h3>{metricName ? `Metric: ${metricName}` : "Metric values over time"}</h3>
      <div className="metric-chart-stats">
        <span>series {series.length}</span>
        <span>min {formatChartValue(min)}</span>
        <span>avg {formatChartValue(avg)}</span>
        <span>max {formatChartValue(max)}</span>
      </div>
      {series.length > maxSeries && (
        <p className="metric-chart-note">
          Showing {maxSeries} of {series.length} series. Aggregate with sum/avg/topk or add label filters to see the rest.
        </p>
      )}
      <div style={{ height: 220 }}>
        <TimeSeriesChart series={series} displayMode="line" showLegend maxSeries={maxSeries} />
      </div>
    </section>
  );
}
