import TimeSeriesChart, { formatChartValue } from "./TimeSeriesChart";
import type { MetricSeries } from "../../types/api";

interface MetricLineChartProps {
  series: MetricSeries[];
  metricName?: string;
}

export default function MetricLineChart({ series, metricName }: MetricLineChartProps) {
  if (series.length === 0) return null;

  const values = series
    .flatMap((item) => item.points.map((point) => Number(point.value)))
    .filter((value) => Number.isFinite(value));
  if (values.length === 0) return null;

  const min = Math.min(...values);
  const max = Math.max(...values);
  const avg = values.reduce((sum, value) => sum + value, 0) / values.length;
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
