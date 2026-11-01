import { useMemo } from "react";
import type { EChartsCoreOption } from "echarts/core";
import EChart from "./EChart";
import { escapeHtml, useChartTheme } from "../../lib/chartTheme";
import type { LogRecord } from "../../types/api";

interface LogHistogramProps {
  records: LogRecord[];
}

const BUCKETS = 20;

type SevKey = "error" | "warn" | "info" | "debug";

const SEV_ORDER: SevKey[] = ["error", "warn", "info", "debug"];
const SEV_LABELS: Record<SevKey, string> = {
  error: "ERROR",
  warn: "WARN",
  info: "INFO",
  debug: "DEBUG",
};

function sevKey(text: string): SevKey {
  const s = text.toUpperCase();
  if (s.includes("FATAL") || s.includes("ERROR")) return "error";
  if (s.includes("WARN")) return "warn";
  if (s.includes("DEBUG") || s.includes("TRACE")) return "debug";
  return "info";
}

function fmt(t: number): string {
  const d = new Date(t);
  return (
    String(d.getHours()).padStart(2, "0") +
    ":" +
    String(d.getMinutes()).padStart(2, "0")
  );
}

interface HistogramData {
  categories: string[];
  starts: number[];
  ends: number[];
  buckets: Array<Record<SevKey, number>>;
}

function buildHistogram(records: LogRecord[]): HistogramData | null {
  const timestamps = records
    .map((record) => new Date(record.timestamp).getTime())
    .filter((timestamp) => !isNaN(timestamp));
  if (timestamps.length === 0) return null;

  const minT = Math.min(...timestamps);
  const maxT = Math.max(...timestamps, minT + 1);
  const bucketMs = (maxT - minT) / BUCKETS || 60000;

  const buckets: Array<Record<SevKey, number>> = Array.from({ length: BUCKETS }, () => ({
    error: 0,
    warn: 0,
    info: 0,
    debug: 0,
  }));

  for (const record of records) {
    const timestamp = new Date(record.timestamp).getTime();
    if (isNaN(timestamp)) continue;
    const index = Math.min(Math.floor((timestamp - minT) / bucketMs), BUCKETS - 1);
    buckets[index][sevKey(record.severity_text ?? "")] += 1;
  }

  return {
    categories: Array.from({ length: BUCKETS }, (_, index) => fmt(minT + index * bucketMs)),
    starts: Array.from({ length: BUCKETS }, (_, index) => minT + index * bucketMs),
    ends: Array.from({ length: BUCKETS }, (_, index) => minT + (index + 1) * bucketMs),
    buckets,
  };
}

export default function LogHistogram({ records }: LogHistogramProps) {
  const theme = useChartTheme();
  const histogram = useMemo(() => buildHistogram(records), [records]);

  const option = useMemo<EChartsCoreOption>(() => {
    if (!histogram) return {};
    const colors: Record<SevKey, string> = {
      error: theme.palette[4],
      warn: theme.palette[3],
      info: theme.palette[1],
      debug: theme.text,
    };

    return {
      animation: false,
      color: SEV_ORDER.map((severity) => colors[severity]),
      grid: { left: 4, right: 8, top: 22, bottom: 4, containLabel: true },
      legend: {
        top: 0,
        left: 0,
        icon: "roundRect",
        itemWidth: 8,
        itemHeight: 8,
        itemGap: 10,
        textStyle: { color: theme.text, fontSize: 9 },
      },
      tooltip: {
        trigger: "axis",
        confine: true,
        backgroundColor: theme.elevated,
        borderColor: theme.borderStrong,
        borderWidth: 1,
        padding: [5, 7],
        textStyle: { color: theme.text, fontSize: 11 },
        extraCssText: "border-radius:6px;",
        axisPointer: { type: "shadow", shadowStyle: { color: theme.grid } },
        formatter: (params: unknown) => {
          const list = (Array.isArray(params) ? params : [params]) as Array<{
            dataIndex?: number;
            seriesName?: string;
            value?: number;
          }>;
          if (list.length === 0) return "";
          const index = list[0]?.dataIndex ?? 0;
          const start = histogram.starts[index] ?? 0;
          const end = histogram.ends[index] ?? start;
          const rows = list
            .filter((item) => Number(item.value) > 0)
            .map(
              (item) =>
                `<div class="ts-tooltip__row">` +
                `<span class="ts-tooltip__name">${escapeHtml(item.seriesName ?? "")}</span>` +
                `<span class="ts-tooltip__value">${Number(item.value)}</span>` +
                `</div>`
            )
            .join("");
          return (
            `<div class="ts-tooltip"><div class="ts-tooltip__time">${fmt(start)} – ${fmt(end)}</div>` +
            `${rows || '<div class="ts-tooltip__row">No logs</div>'}</div>`
          );
        },
      },
      xAxis: {
        type: "category",
        data: histogram.categories,
        axisLine: { lineStyle: { color: theme.border } },
        axisTick: { show: false },
        axisLabel: { color: theme.text, fontSize: 9, hideOverlap: true },
      },
      yAxis: {
        type: "value",
        minInterval: 1,
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel: { color: theme.text, fontSize: 9 },
        splitLine: { lineStyle: { color: theme.grid, type: "dashed" } },
      },
      series: SEV_ORDER.map((severity) => ({
        type: "bar" as const,
        name: SEV_LABELS[severity],
        stack: "severity",
        data: histogram.buckets.map((bucket) => bucket[severity]),
        barMaxWidth: 14,
        itemStyle: { color: colors[severity] },
        emphasis: { focus: "series" as const },
      })),
    };
  }, [histogram, theme]);

  if (!histogram) return null;

  return (
    <div style={{ height: 108 }}>
      <EChart option={option} ariaLabel="Log volume by severity over time" />
    </div>
  );
}
