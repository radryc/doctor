import { useMemo, useState } from "react";
import { Maximize2 } from "lucide-react";
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
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
  const [hidden, setHidden] = useState<Set<string>>(() => new Set());
  const visibleSeries = maxSeries > 0 && series.length > maxSeries ? series.slice(0, maxSeries) : series;
  const keys = useMemo(
    () =>
      visibleSeries.map((item, index) => ({
        key: `s${index}`,
        label: item.label,
        color: CHART_PALETTE[index % CHART_PALETTE.length],
      })),
    [visibleSeries]
  );
  const renderedKeys = keys.filter((item) => !hidden.has(item.key));

  const data = useMemo(() => {
    const byTime = new Map<number, Record<string, number>>();
    visibleSeries.forEach((item, index) => {
      const key = `s${index}`;
      for (const point of item.points) {
        const ts = new Date(point.timestamp).getTime();
        const value = Number(point.value);
        if (!Number.isFinite(ts) || !Number.isFinite(value)) continue;
        let row = byTime.get(ts);
        if (!row) {
          row = { ts };
          byTime.set(ts, row);
        }
        row[key] = value;
      }
    });
    return [...byTime.values()].sort((a, b) => a.ts - b.ts);
  }, [visibleSeries]);

  if (keys.length === 0 || data.length === 0) {
    return <div className="dash-empty">No series in range.</div>;
  }

  const withDate = data.length > 1 && data[data.length - 1].ts - data[0].ts > 12 * 60 * 60 * 1000;
  const toggleSeries = (key: string) => {
    setHidden((previous) => {
      const next = new Set(previous);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const common = {
    data,
    margin: { top: 16, right: 12, bottom: 4, left: 0 },
  };
  const grid = <CartesianGrid stroke="var(--chart-grid)" strokeDasharray="3 3" vertical={false} />;
  const xAxis = (
    <XAxis
      dataKey="ts"
      type="number"
      domain={["dataMin", "dataMax"]}
      tickFormatter={(value) => formatAxisTime(Number(value), withDate)}
      minTickGap={28}
      tick={{ fill: "var(--chart-text)", fontSize: 10 }}
      stroke="var(--ui-border)"
    />
  );
  const yAxis = (
    <YAxis
      width={48}
      tickFormatter={(value) => formatChartValue(Number(value))}
      tick={{ fill: "var(--chart-text)", fontSize: 10 }}
      stroke="var(--ui-border)"
    />
  );
  const tooltip = (
    <Tooltip
      labelFormatter={(label) => formatDateTimeFull(Number(label))}
      formatter={(value, name) => [formatChartValue(Number(value)), name]}
      contentStyle={{
        background: "var(--ui-elevated)",
        border: "1px solid var(--ui-border-strong)",
        borderRadius: 6,
        color: "var(--ui-text)",
        fontSize: 11,
        maxHeight: 280,
        overflowY: "auto",
      }}
      labelStyle={{ color: "var(--ui-muted)" }}
      itemStyle={{ color: "var(--ui-text)" }}
      cursor={{ stroke: "var(--ui-border-strong)", strokeDasharray: "3 3" }}
    />
  );

  let chart;
  if (displayMode === "bars") {
    chart = (
      <BarChart {...common}>
        {grid}
        {xAxis}
        {yAxis}
        {tooltip}
        {renderedKeys.map((item) => (
          <Bar
            key={item.key}
            dataKey={item.key}
            name={item.label}
            fill={item.color}
            stackId={stacked ? "stack" : undefined}
            radius={[2, 2, 0, 0]}
            isAnimationActive={false}
          />
        ))}
      </BarChart>
    );
  } else if (displayMode === "area") {
    chart = (
      <AreaChart {...common}>
        {grid}
        {xAxis}
        {yAxis}
        {tooltip}
        {renderedKeys.map((item) => (
          <Area
            key={item.key}
            dataKey={item.key}
            name={item.label}
            stroke={item.color}
            fill={item.color}
            fillOpacity={stacked ? 0.2 : 0.12}
            strokeWidth={2}
            stackId={stacked ? "stack" : undefined}
            isAnimationActive={false}
          />
        ))}
      </AreaChart>
    );
  } else {
    chart = (
      <LineChart {...common}>
        {grid}
        {xAxis}
        {yAxis}
        {tooltip}
        {renderedKeys.map((item) => (
          <Line
            key={item.key}
            type="linear"
            dataKey={item.key}
            name={item.label}
            stroke={item.color}
            strokeWidth={2}
            dot={false}
            activeDot={{ r: 3 }}
            connectNulls
            isAnimationActive={false}
          />
        ))}
      </LineChart>
    );
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
        {renderedKeys.length === 0 ? (
          <div className="dash-empty">All series are hidden. Click a legend item to show it again.</div>
        ) : (
          <ResponsiveContainer width="100%" height="100%">
            {chart}
          </ResponsiveContainer>
        )}
      </div>
      {showLegend && (
        <div className="timeseries-chart__legend">
          {keys.map((item) => (
            <button
              type="button"
              key={item.key}
              className={"timeseries-chart__legend-item" + (hidden.has(item.key) ? " is-hidden" : "")}
              title={hidden.has(item.key) ? `Show ${item.label}` : `Hide ${item.label}`}
              onClick={(event) => {
                event.stopPropagation();
                toggleSeries(item.key);
              }}
            >
              <span
                className="timeseries-chart__swatch"
                style={{
                  backgroundColor: item.color,
                  borderRadius: displayMode === "bars" ? 2 : 999,
                }}
              />
              <span className="timeseries-chart__legend-label">{item.label}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
