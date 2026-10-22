import { useMemo } from "react";
import { CHART_PALETTE, formatChartValue } from "./TimeSeriesChart";
import type { MetricSeries } from "../../types/api";

interface SeriesDetailsProps {
  series: MetricSeries[];
  maxSeries?: number;
  showPoints?: boolean;
}

interface SeriesStats {
  latest: number | null;
  latestAt: string | null;
  min: number | null;
  max: number | null;
  avg: number | null;
  points: number;
}

function computeStats(series: MetricSeries): SeriesStats {
  let latest: number | null = null;
  let latestTs = -Infinity;
  let latestAt: string | null = null;
  let min = Infinity;
  let max = -Infinity;
  let sum = 0;
  let count = 0;
  for (const point of series.points) {
    const value = Number(point.value);
    if (!Number.isFinite(value)) continue;
    const ts = new Date(point.timestamp).getTime();
    if (Number.isFinite(ts) && ts >= latestTs) {
      latestTs = ts;
      latest = value;
      latestAt = point.timestamp;
    }
    if (value < min) min = value;
    if (value > max) max = value;
    sum += value;
    count += 1;
  }
  return {
    latest,
    latestAt,
    min: count > 0 ? min : null,
    max: count > 0 ? max : null,
    avg: count > 0 ? sum / count : null,
    points: count,
  };
}

export default function SeriesDetails({ series, maxSeries = 50, showPoints = true }: SeriesDetailsProps) {
  const visible = useMemo(
    () => (maxSeries > 0 && series.length > maxSeries ? series.slice(0, maxSeries) : series),
    [series, maxSeries]
  );
  const rows = useMemo(
    () =>
      visible.map((item, index) => ({
        item,
        index,
        stats: computeStats(item),
      })),
    [visible]
  );

  if (rows.length === 0) return null;

  return (
    <div className="series-details">
      <div className="series-details__head">
        <span>Series / labels</span>
        <span>Latest</span>
        <span>Min</span>
        <span>Avg</span>
        <span>Max</span>
        {showPoints && <span>Points</span>}
      </div>
      <div className="series-details__body">
        {rows.map(({ item, index, stats }) => (
          <div className="series-details__row" key={`${item.label}-${index}`}>
            <span className="series-details__identity" title={item.label}>
              <span
                className="series-details__swatch"
                style={{ backgroundColor: CHART_PALETTE[index % CHART_PALETTE.length] }}
              />
              <code>{item.label}</code>
            </span>
            <span title={stats.latestAt ? new Date(stats.latestAt).toLocaleString() : undefined}>
              {stats.latest == null ? "—" : formatChartValue(stats.latest)}
            </span>
            <span>{stats.min == null ? "—" : formatChartValue(stats.min)}</span>
            <span>{stats.avg == null ? "—" : formatChartValue(stats.avg)}</span>
            <span>{stats.max == null ? "—" : formatChartValue(stats.max)}</span>
            {showPoints && <span>{stats.points}</span>}
          </div>
        ))}
      </div>
      {series.length > visible.length && (
        <p className="series-details__note">
          Showing {visible.length} of {series.length} series. Add a label filter or aggregate the query to see the rest.
        </p>
      )}
    </div>
  );
}
