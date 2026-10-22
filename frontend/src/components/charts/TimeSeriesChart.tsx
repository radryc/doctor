import { useMemo } from "react";
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Legend,
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
}

function formatTime(ts: number): string {
  const d = new Date(ts);
  if (isNaN(d.getTime())) return "";
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}

export function formatChartValue(value: number): string {
  if (!Number.isFinite(value)) return "—";
  const abs = Math.abs(value);
  if (abs >= 1_000_000) return `${(value / 1_000_000).toFixed(2)}M`;
  if (abs >= 1_000) return `${(value / 1_000).toFixed(2)}k`;
  if (abs >= 10) return value.toFixed(1);
  return value.toFixed(3);
}

export default function TimeSeriesChart({
  series,
  height,
  displayMode = "line",
  stacked = false,
  showLegend = true,
  onExpand,
  maxSeries = 50,
}: TimeSeriesChartProps) {
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

  const common = {
    data,
    margin: { top: 8, right: 12, bottom: 4, left: 0 },
  };
  const axes = (
    <>
      <CartesianGrid stroke="var(--chart-grid)" strokeDasharray="3 3" vertical={false} />
      <XAxis
        dataKey="ts"
        type="number"
        domain={["dataMin", "dataMax"]}
        tickFormatter={(value) => formatTime(Number(value))}
        minTickGap={28}
        tick={{ fill: "var(--chart-text)", fontSize: 10 }}
        stroke="var(--ui-border)"
      />
      <YAxis
        width={48}
        tickFormatter={(value) => formatChartValue(Number(value))}
        tick={{ fill: "var(--chart-text)", fontSize: 10 }}
        stroke="var(--ui-border)"
      />
      <Tooltip
        labelFormatter={(label) => formatTime(Number(label))}
        formatter={(value) => formatChartValue(Number(value))}
        contentStyle={{
          background: "var(--ui-elevated)",
          border: "1px solid var(--ui-border-strong)",
          borderRadius: 6,
          color: "var(--ui-text)",
          fontSize: 11,
        }}
        labelStyle={{ color: "var(--ui-muted)" }}
        itemStyle={{ color: "var(--ui-text)" }}
        cursor={{ stroke: "var(--ui-border-strong)", strokeDasharray: "3 3" }}
      />
      {showLegend && (
        <Legend
          verticalAlign="bottom"
          height={28}
          iconType={displayMode === "bars" ? "square" : "line"}
          wrapperStyle={{ color: "var(--ui-muted)", fontSize: 10 }}
        />
      )}
    </>
  );

  let chart;
  if (displayMode === "bars") {
    chart = (
      <BarChart {...common}>
        {axes}
        {keys.map((item) => (
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
        {axes}
        {keys.map((item) => (
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
        {axes}
        {keys.map((item) => (
          <Line
            key={item.key}
            type="monotone"
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
      <ResponsiveContainer width="100%" height="100%">
        {chart}
      </ResponsiveContainer>
    </div>
  );
}
