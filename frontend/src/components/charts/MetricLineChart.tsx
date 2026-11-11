import { useMemo } from "react";
import { Maximize2 } from "lucide-react";
import TimeSeriesChart, {
  CHART_DISPLAY_MODES,
  formatChartValue,
  formatDateTimeFull,
  type ChartDisplayMode,
} from "./TimeSeriesChart";
import SeriesDetails from "./SeriesDetails";
import type { MetricLabelFilter, MetricSeries } from "../../types/api";

interface MetricLineChartProps {
  series: MetricSeries[];
  metricName?: string;
  onExpand?: () => void;
  from?: Date;
  to?: Date;
  step?: string;
  description?: string;
  displayMode?: ChartDisplayMode;
  onDisplayModeChange?: (mode: ChartDisplayMode) => void;
  labelFilters?: MetricLabelFilter[];
  onToggleLabel?: (key: string, value: string) => void;
  totalSeries?: number;
  labelSeries?: MetricSeries[];
}

export default function MetricLineChart({
  series,
  metricName,
  onExpand,
  from,
  to,
  step,
  description,
  displayMode = "line",
  onDisplayModeChange,
  labelFilters = [],
  onToggleLabel,
  totalSeries,
  labelSeries,
}: MetricLineChartProps) {
  const labelSummary = useMemo(() => {
    const values = new Map<string, Map<string, number>>();
    for (const item of (labelSeries ?? series).slice(0, 30)) {
      for (const [key, value] of Object.entries(item.labels ?? {})) {
        if (key === "__name__" || value === "") continue;
        let bucket = values.get(key);
        if (!bucket) {
          bucket = new Map();
          values.set(key, bucket);
        }
        bucket.set(value, (bucket.get(value) ?? 0) + 1);
      }
    }
    return [...values.entries()].map(([key, bucket]) => ({
      key,
      values: [...bucket.entries()]
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([value, count]) => ({ value, count })),
    }));
  }, [labelSeries, series]);

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
  const isActive = (key: string, value: string) =>
    labelFilters.some((filter) => filter.key === key && filter.value === value);

  return (
    <section className="result-block">
      <div className="metric-chart-header">
        <div>
          <h3>{metricName ? `Metric: ${metricName}` : "Metric values over time"}</h3>
          {from && to && (
            <div className="metric-chart-subtitle">
              {formatDateTimeFull(from)} → {formatDateTimeFull(to)}
              {step ? ` · step ${step}` : ""}
            </div>
          )}
          {description && <div className="metric-chart-subtitle">{description}</div>}
        </div>
        <div className="metric-chart-header__actions">
          {onDisplayModeChange && (
            <div className="metric-chart-modes" role="group" aria-label="Chart type">
              {CHART_DISPLAY_MODES.map((mode) => (
                <button
                  key={mode.value}
                  type="button"
                  className={"metric-chart-mode" + (displayMode === mode.value ? " is-active" : "")}
                  onClick={() => onDisplayModeChange(mode.value)}
                  title={`Show the result as ${mode.label.toLowerCase()}`}
                  aria-pressed={displayMode === mode.value}
                >
                  {mode.label}
                </button>
              ))}
            </div>
          )}
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
      </div>
      <div className="metric-chart-stats">
        <span>series {series.length}</span>
        {totalSeries !== undefined && totalSeries !== series.length && (
          <span>of {totalSeries}</span>
        )}
        <span>samples {count}</span>
        <span>min {formatChartValue(min)}</span>
        <span>avg {formatChartValue(avg)}</span>
        <span>max {formatChartValue(max)}</span>
      </div>
      {series.length > maxSeries && (
        <p className="metric-chart-note">
          Showing {maxSeries} of {series.length} series. Aggregate with sum/avg/topk or add label filters to see the rest.
        </p>
      )}
      <div style={{ height: displayMode === "gauge" ? 320 : 300 }}>
        <TimeSeriesChart
          series={series}
          displayMode={displayMode}
          showLegend
          maxSeries={maxSeries}
          onExpand={onExpand}
        />
      </div>
      <SeriesDetails
        series={series}
        maxSeries={maxSeries}
        labelFilters={labelFilters}
        onToggleLabel={onToggleLabel}
      />
      {labelSummary.length > 0 && (
        <details className="metric-label-details">
          <summary>Label values across returned series ({labelSummary.length})</summary>
          <p className="metric-label-hint">
            Click a value to show only series carrying that label. Click again to remove the filter.
          </p>
          <div className="series-label-summary" aria-label="Labels across returned series">
            {labelSummary.map(({ key, values }) => (
              <div className="label-filter-group" key={key}>
                <span className="label-filter-group__key">{key}</span>
                <div className="label-filter-group__values">
                  {values.map(({ value, count }) => {
                    const active = isActive(key, value);
                    return (
                      <button
                        key={value}
                        type="button"
                        className={"label-chip label-chip--button" + (active ? " is-active" : "")}
                        title={`${key}="${value}" (${count} series) — click to ${active ? "remove" : "apply"} filter`}
                        aria-pressed={active}
                        onClick={() => onToggleLabel?.(key, value)}
                      >
                        <span className="label-chip__value">{value}</span>
                        <span className="label-chip__count">{count}</span>
                      </button>
                    );
                  })}
                </div>
              </div>
            ))}
          </div>
        </details>
      )}
    </section>
  );
}
