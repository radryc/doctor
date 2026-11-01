import { useEffect, useMemo, useState } from "react";
import type { EChartsCoreOption } from "echarts/core";
import EChart from "./EChart";
import SeriesLegend from "./SeriesLegend";
import { useChartTheme } from "../../lib/chartTheme";

export interface MiniLineSeries {
  key: string;
  label?: string;
  color?: string;
}

interface MiniLineChartProps {
  data: ReadonlyArray<object>;
  series: MiniLineSeries[];
  xKey?: string;
  ariaLabel?: string;
}

function field(row: object, key: string): unknown {
  return (row as Record<string, unknown>)[key];
}

function resolveColor(color: string | undefined, fallback: string): string {
  if (!color) return fallback;
  const match = /^var\((--[^),]+)\)$/.exec(color.trim());
  if (!match || typeof document === "undefined") return color;
  const resolved = getComputedStyle(document.documentElement)
    .getPropertyValue(match[1])
    .trim();
  return resolved || fallback;
}

/**
 * Compact line chart for the fleet/partition detail views. Keeps the same
 * isolation legend semantics as the main metrics chart while accepting the
 * pre-formatted time buckets those pages already produce.
 */
export default function MiniLineChart({
  data,
  series,
  xKey = "time",
  ariaLabel = "Time series chart",
}: MiniLineChartProps) {
  const theme = useChartTheme();
  const [hidden, setHidden] = useState<Set<string>>(() => new Set());
  const [isolated, setIsolated] = useState<string | null>(null);

  const items = useMemo(
    () =>
      series.map((item, index) => ({
        key: item.key,
        label: item.label ?? item.key,
        color: resolveColor(item.color, theme.palette[index % theme.palette.length]),
      })),
    [series, theme.palette]
  );

  const identity = useMemo(
    () => items.map((item) => `${item.key}:${item.label}`).join("\u0000"),
    [items]
  );
  useEffect(() => {
    setHidden(new Set());
    setIsolated(null);
  }, [identity]);

  const activeIsolation =
    isolated && items.some((item) => item.key === isolated) ? isolated : null;

  const categories = useMemo(
    () => data.map((row) => String(field(row, xKey) ?? "")),
    [data, xKey]
  );

  const option = useMemo<EChartsCoreOption>(() => {
    const chartSeries = items
      .filter(
        (item) =>
          !hidden.has(item.key) &&
          (activeIsolation === null || item.key === activeIsolation)
      )
      .map((item) => ({
        type: "line" as const,
        name: item.label,
        data: data.map((row) => {
          const value = Number(field(row, item.key));
          return Number.isFinite(value) ? value : null;
        }),
        smooth: 0.15,
        showSymbol: false,
        symbol: "circle",
        symbolSize: 5,
        connectNulls: true,
        lineStyle: { color: item.color, width: 1.5 },
        itemStyle: { color: item.color },
        emphasis: { focus: "series" as const },
      }));

    return {
      animation: false,
      color: items.map((item) => item.color),
      grid: { left: 4, right: 10, top: 8, bottom: 4, containLabel: true },
      tooltip: {
        trigger: "axis",
        confine: true,
        backgroundColor: theme.elevated,
        borderColor: theme.borderStrong,
        borderWidth: 1,
        padding: [5, 7],
        textStyle: { color: theme.text, fontSize: 11 },
        extraCssText: "border-radius:6px;",
        axisPointer: {
          type: "line",
          lineStyle: { color: theme.borderStrong, type: "dashed" },
        },
      },
      xAxis: {
        type: "category",
        boundaryGap: false,
        data: categories,
        axisLine: { lineStyle: { color: theme.border } },
        axisTick: { show: false },
        axisLabel: {
          color: theme.text,
          fontSize: 10,
          hideOverlap: true,
        },
        splitLine: { show: false },
      },
      yAxis: {
        type: "value",
        scale: true,
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel: { color: theme.text, fontSize: 10 },
        splitLine: { lineStyle: { color: theme.grid, type: "dashed" } },
      },
      series: chartSeries,
    };
  }, [activeIsolation, categories, data, hidden, items, theme]);

  if (data.length === 0 || items.length === 0) {
    return <div className="dash-empty">No samples in range.</div>;
  }

  const visibleCount = items.filter(
    (item) =>
      !hidden.has(item.key) &&
      (activeIsolation === null || item.key === activeIsolation)
  ).length;

  return (
    <div className="mini-line-chart">
      <div className="mini-line-chart__plot">
        {visibleCount === 0 ? (
          <div className="dash-empty">All series are hidden. Click a legend item to show it again.</div>
        ) : (
          <EChart option={option} ariaLabel={ariaLabel} />
        )}
      </div>
      {items.length > 1 && (
        <SeriesLegend
          items={items}
          hidden={hidden}
          isolated={activeIsolation}
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
