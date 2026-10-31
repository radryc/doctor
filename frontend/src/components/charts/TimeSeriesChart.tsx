import { useEffect, useMemo, useState } from "react";
import { Maximize2 } from "lucide-react";
import type { EChartsCoreOption } from "echarts/core";
import EChart from "./EChart";
import SeriesLegend from "./SeriesLegend";
import { escapeHtml, useChartTheme } from "../../lib/chartTheme";
import type { MetricSeries } from "../../types/api";

export type ChartDisplayMode = "line" | "area" | "bars";

export const CHART_PALETTE = [
  "var(--chart-1)",
  "var(--chart-2)",
  "var(--chart-3)",
  "var(--chart-4)",
  "var(--chart-5)",
  "var(--chart-6)",
  "var(--chart-7)",
  "var(--chart-8)",
];

interface TimeSeriesChartProps {
  series: MetricSeries[];
  height?: number;
  displayMode?: ChartDisplayMode;
  stacked?: boolean;
  showLegend?: boolean;
  onExpand?: () => void;
  maxSeries?: number;
  unit?: string;
}

function formatTime(ts: number): string {
  const d = new Date(ts);
  if (isNaN(d.getTime())) return "";
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}

function formatAxisTime(ts: number, withDate: boolean): string {
  const d = new Date(ts);
  if (isNaN(d.getTime())) return "";
  const time = formatTime(ts);
  if (!withDate) return time;
  return `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")} ${time.slice(0, 5)}`;
}

export function formatChartValue(value: number): string {
  if (!Number.isFinite(value)) return "—";
  const abs = Math.abs(value);
  if (abs >= 1_000_000) return `${(value / 1_000_000).toFixed(2)}M`;
  if (abs >= 1_000) return `${(value / 1_000).toFixed(2)}k`;
  if (abs >= 10) return value.toFixed(1);
  return value.toFixed(3);
}

export function formatDateTimeFull(value: number | Date | string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString(undefined, { timeZoneName: "short" });
}

interface TooltipItem {
  seriesName?: string;
  color?: string;
  value?: unknown;
  axisValue?: unknown;
}

/** Grafana-style tooltip: timestamp first, then values sorted high → low. */
function tooltipFormatter(params: unknown): string {
  const list = (Array.isArray(params) ? params : [params]) as TooltipItem[];
  if (list.length === 0) return "";

  const firstValue = list[0]?.value;
  const firstTimestamp = Array.isArray(firstValue) ? Number(firstValue[0]) : NaN;
  const label = Number.isFinite(firstTimestamp)
    ? formatDateTimeFull(firstTimestamp)
    : String(list[0]?.axisValue ?? "");

  const rows = list
    .map((item) => ({
      name: item.seriesName ?? "",
      color: item.color ?? "transparent",
      value: Array.isArray(item.value) ? Number(item.value[1]) : Number(item.value),
    }))
    .filter((item) => Number.isFinite(item.value))
    .sort((left, right) => right.value - left.value);

  const body = rows
    .map(
      (row) =>
        `<div class="ts-tooltip__row">` +
        `<span class="ts-tooltip__swatch" style="background:${escapeHtml(row.color)}"></span>` +
        `<span class="ts-tooltip__name">${escapeHtml(row.name)}</span>` +
        `<span class="ts-tooltip__value">${escapeHtml(formatChartValue(row.value))}</span>` +
        `</div>`
    )
    .join("");

  return `<div class="ts-tooltip"><div class="ts-tooltip__time">${escapeHtml(label)}</div>${body}</div>`;
}

export default function TimeSeriesChart({
  series,
  height,
  displayMode = "line",
  stacked = false,
  showLegend = true,
  onExpand,
  maxSeries = 50,
  unit,
}: TimeSeriesChartProps) {
  const theme = useChartTheme();
  // Series hidden with Ctrl/Cmd-click on the legend.
  const [hidden, setHidden] = useState<Set<string>>(() => new Set());
  // Grafana/Thanos behaviour: a plain legend click isolates one series.
  const [isolated, setIsolated] = useState<string | null>(null);

  const visibleSeries = useMemo(
    () => (maxSeries > 0 && series.length > maxSeries ? series.slice(0, maxSeries) : series),
    [series, maxSeries]
  );

  const keys = useMemo(
    () =>
      visibleSeries.map((item, index) => ({
        key: `s${index}`,
        index,
        label: item.label,
        color: theme.palette[index % theme.palette.length],
      })),
    [visibleSeries, theme.palette]
  );

  // Reset visibility when the returned series set changes (new query, new
  // partition). Position-based keys would otherwise hide unrelated series.
  const identity = useMemo(
    () => keys.map((item) => item.label).join("\u0000"),
    [keys]
  );
  useEffect(() => {
    setHidden(new Set());
    setIsolated(null);
  }, [identity]);

  const activeIsolation =
    isolated && keys.some((item) => item.key === isolated) ? isolated : null;

  const visibleKeys = useMemo(
    () =>
      keys.filter(
        (item) =>
          !hidden.has(item.key) && (activeIsolation === null || item.key === activeIsolation)
      ),
    [keys, hidden, activeIsolation]
  );

  const withDate = useMemo(() => {
    let min = Infinity;
    let max = -Infinity;
    for (const item of visibleSeries) {
      for (const point of item.points) {
        const ts = new Date(point.timestamp).getTime();
        if (!Number.isFinite(ts)) continue;
        if (ts < min) min = ts;
        if (ts > max) max = ts;
      }
    }
    return Number.isFinite(min) && Number.isFinite(max) && max - min > 12 * 60 * 60 * 1000;
  }, [visibleSeries]);

  const option = useMemo<EChartsCoreOption>(() => {
    const chartSeries = visibleKeys.map((item) => {
      const source = visibleSeries[item.index];
      const data = (source?.points ?? [])
        .map((point) => {
          const ts = new Date(point.timestamp).getTime();
          const value = Number(point.value);
          return [ts, value] as [number, number];
        })
        .filter(([ts, value]) => Number.isFinite(ts) && Number.isFinite(value))
        .sort((left, right) => left[0] - right[0]);

      const stack = stacked ? "total" : undefined;
      const emphasis = { focus: "series" as const };

      if (displayMode === "bars") {
        return {
          type: "bar" as const,
          name: item.label,
          data,
          stack,
          barMaxWidth: 16,
          itemStyle: { color: item.color, borderRadius: stacked ? 0 : [2, 2, 0, 0] },
          emphasis,
        };
      }

      if (displayMode === "area") {
        return {
          type: "line" as const,
          name: item.label,
          data,
          stack,
          smooth: false,
          showSymbol: false,
          symbol: "circle",
          symbolSize: 6,
          connectNulls: true,
          sampling: "lttb" as const,
          lineStyle: { color: item.color, width: 2 },
          itemStyle: { color: item.color },
          areaStyle: { color: item.color, opacity: stacked ? 0.25 : 0.14 },
          emphasis,
        };
      }

      return {
        type: "line" as const,
        name: item.label,
        data,
        smooth: false,
        showSymbol: false,
        symbol: "circle",
        symbolSize: 6,
        connectNulls: true,
        sampling: "lttb" as const,
        lineStyle: { color: item.color, width: 2 },
        itemStyle: { color: item.color },
        emphasis,
      };
    });

    return {
      animation: false,
      color: theme.palette,
      grid: { left: 4, right: 14, top: 16, bottom: 4, containLabel: true },
      tooltip: {
        trigger: "axis",
        confine: true,
        backgroundColor: theme.elevated,
        borderColor: theme.borderStrong,
        borderWidth: 1,
        padding: [6, 8],
        textStyle: { color: theme.text, fontSize: 11 },
        extraCssText: "max-height:300px;overflow-y:auto;border-radius:6px;",
        axisPointer: {
          type: "line",
          lineStyle: { color: theme.borderStrong, type: "dashed" },
        },
        formatter: tooltipFormatter,
      },
      xAxis: {
        type: "time",
        boundaryGap: displayMode === "bars" ? ["2%", "2%"] : false,
        axisLine: { lineStyle: { color: theme.border } },
        axisTick: { show: false },
        axisLabel: {
          color: theme.text,
          fontSize: 10,
          hideOverlap: true,
          formatter: (value: number) => formatAxisTime(Number(value), withDate),
        },
        splitLine: { show: false },
      },
      yAxis: {
        type: "value",
        scale: displayMode !== "bars",
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel: {
          color: theme.text,
          fontSize: 10,
          formatter: (value: number) => formatChartValue(Number(value)),
        },
        splitLine: { lineStyle: { color: theme.grid, type: "dashed" } },
      },
      series: chartSeries,
    };
  }, [displayMode, stacked, theme, visibleKeys, visibleSeries, withDate]);

  if (keys.length === 0 || visibleSeries.every((item) => item.points.length === 0)) {
    return <div className="dash-empty">No series in range.</div>;
  }

  return (
    <div
      className={"timeseries-chart" + (onExpand ? " timeseries-chart--clickable" : "")}
      style={height ? { height } : { height: "100%", minHeight: 140 }}
      onClick={onExpand}
      title={onExpand ? "Open larger chart" : undefined}
      role={onExpand ? "button" : undefined}
      tabIndex={onExpand ? 0 : undefined}
      onKeyDown={
        onExpand
          ? (event) => {
              if (event.key === "Enter" || event.key === " ") {
                event.preventDefault();
                onExpand();
              }
            }
          : undefined
      }
    >
      {unit && <span className="timeseries-chart__unit">{unit}</span>}
      {onExpand && (
        <button
          type="button"
          className="timeseries-chart__expand"
          title="Expand chart"
          aria-label="Expand chart"
          onClick={(event) => {
            event.stopPropagation();
            onExpand();
          }}
        >
          <Maximize2 size={12} />
          Expand
        </button>
      )}
      <div className="timeseries-chart__plot">
        {visibleKeys.length === 0 ? (
          <div className="dash-empty">All series are hidden. Click a legend item to show it again.</div>
        ) : (
          <EChart
            option={option}
            ariaLabel="Time series chart"
            style={{ cursor: onExpand ? "zoom-in" : undefined }}
          />
        )}
      </div>
      {showLegend && (
        <SeriesLegend
          items={keys}
          hidden={hidden}
          isolated={activeIsolation}
          swatchShape={displayMode === "bars" ? "square" : "circle"}
          onToggle={(key) =>
            setHidden((previous) => {
              const next = new Set(previous);
              if (next.has(key)) next.delete(key);
              else next.add(key);
              return next;
            })
          }
          onIsolate={(key) => {
            if (key !== null) {
              setHidden((previous) => {
                if (!previous.has(key)) return previous;
                const next = new Set(previous);
                next.delete(key);
                return next;
              });
            }
            setIsolated(key);
          }}
        />
      )}
    </div>
  );
}
