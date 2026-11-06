import { useMemo, useState } from "react";
import { Check, Copy } from "lucide-react";
import { copyText } from "../../lib/clipboard";
import { CHART_PALETTE, formatChartValue, formatDateTimeFull } from "./TimeSeriesChart";
import type { MetricLabelFilter, MetricSeries } from "../../types/api";

interface SeriesDetailsProps {
  series: MetricSeries[];
  maxSeries?: number;
  showPoints?: boolean;
  labelFilters?: MetricLabelFilter[];
  onToggleLabel?: (key: string, value: string) => void;
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

export default function SeriesDetails({
  series,
  maxSeries = 50,
  showPoints = true,
  labelFilters = [],
  onToggleLabel,
}: SeriesDetailsProps) {
  const [copied, setCopied] = useState("");
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

  const copySeries = (key: string, label: string) => {
    void copyText(label).then((ok) => {
      if (!ok) return;
      setCopied(key);
      window.setTimeout(() => setCopied(""), 1400);
    });
  };

  const isActive = (key: string, value: string) =>
    labelFilters.some((filter) => filter.key === key && filter.value === value);

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
        {rows.map(({ item, index, stats }) => {
          const metricName = item.labels?.__name__ ?? "";
          const labelEntries = Object.entries(item.labels ?? {})
            .filter(([key]) => key !== "__name__")
            .sort(([left], [right]) => left.localeCompare(right));
          const rowKey = `${item.label}-${index}`;
          return (
            <div className="series-details__row" key={rowKey}>
              <span className="series-details__identity" title={item.label}>
                <span
                  className="series-details__swatch"
                  style={{ backgroundColor: CHART_PALETTE[index % CHART_PALETTE.length] }}
                />
                <span className="series-details__identity-main">
                  {metricName ? (
                    <code className="series-details__metric-name">{metricName}</code>
                  ) : null}
                  {labelEntries.length > 0 ? (
                    <span className="series-details__labels">
                      {labelEntries.map(([key, value]) => {
                        const active = isActive(key, value);
                        return (
                          <button
                            type="button"
                            key={key}
                            className={"series-label-token" + (active ? " is-active" : "")}
                            title={`${key}="${value}"${onToggleLabel ? " — click to filter by this label" : ""}`}
                            aria-pressed={active}
                            disabled={!onToggleLabel}
                            onClick={(event) => {
                              event.stopPropagation();
                              onToggleLabel?.(key, value);
                            }}
                          >
                            <span className="series-label-token__key">{key}</span>
                            <span className="series-label-token__eq">=</span>
                            <span className="series-label-token__value">{value}</span>
                          </button>
                        );
                      })}
                    </span>
                  ) : !metricName ? (
                    <code className="series-details__metric-name">{item.label}</code>
                  ) : null}
                </span>
                <button
                  type="button"
                  className="series-details__copy"
                  title="Copy the full series name and labels"
                  aria-label="Copy series labels"
                  onClick={() => copySeries(rowKey, item.label)}
                >
                  {copied === rowKey ? <Check size={11} /> : <Copy size={11} />}
                </button>
              </span>
              <span title={stats.latestAt ? formatDateTimeFull(stats.latestAt) : undefined}>
                {stats.latest == null ? "—" : formatChartValue(stats.latest)}
              </span>
              <span>{stats.min == null ? "—" : formatChartValue(stats.min)}</span>
              <span>{stats.avg == null ? "—" : formatChartValue(stats.avg)}</span>
              <span>{stats.max == null ? "—" : formatChartValue(stats.max)}</span>
              {showPoints && <span>{stats.points}</span>}
            </div>
          );
        })}
      </div>
      {series.length > visible.length && (
        <p className="series-details__note">
          Showing {visible.length} of {series.length} series. Add a label filter or aggregate the query to see the rest.
        </p>
      )}
    </div>
  );
}
