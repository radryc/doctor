import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import type { EChartsCoreOption } from "echarts/core";
import {
  Activity,
  AlertTriangle,
  ArrowUpRight,
  BarChart3,
  CheckCircle,
  Copy,
  FileText,
  GitBranch,
  RefreshCw,
  Server,
  ShieldCheck,
  TriangleAlert,
} from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import { copyText } from "../lib/clipboard";
import EChart from "../components/charts/EChart";
import { escapeHtml, useChartTheme } from "../lib/chartTheme";
import type { GuardianOverview } from "../types/api";

const HEALTH_GREEN = "var(--ui-green)";
const HEALTH_AMBER = "var(--ui-amber)";
const HEALTH_RED = "var(--ui-red)";
const HEALTH_GRAY = "var(--ui-faint)";

type Tone = "healthy" | "attention" | "failing";

function partitionTone(health: string | undefined): Tone {
  const value = (health ?? "").toLowerCase();
  if (value.includes("fail") || value.includes("crit") || value.includes("error")) return "failing";
  if (
    value.includes("attention") ||
    value.includes("degrad") ||
    value.includes("drift") ||
    value.includes("pending")
  ) {
    return "attention";
  }
  return "healthy";
}

function toneRank(tone: Tone): number {
  if (tone === "failing") return 0;
  if (tone === "attention") return 1;
  return 2;
}

function healthColor(health: string): string {
  const tone = partitionTone(health);
  if (tone === "healthy") return HEALTH_GREEN;
  if (tone === "attention") return HEALTH_AMBER;
  if (tone === "failing") return HEALTH_RED;
  return HEALTH_GRAY;
}

function StatusBadge({ health }: { health: string }) {
  const color = healthColor(health);
  return (
    <span
      className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium capitalize"
      style={{
        backgroundColor: `color-mix(in oklab, ${color} 14%, transparent)`,
        color,
        border: `1px solid color-mix(in oklab, ${color} 30%, transparent)`,
      }}
    >
      <span className="w-1.5 h-1.5 rounded-full" style={{ backgroundColor: color }} />
      {health || "unknown"}
    </span>
  );
}

function StatCard({
  icon: Icon,
  label,
  value,
  sub,
  color = "text-accent-blue",
}: {
  icon: React.ElementType;
  label: string;
  value: number | string;
  sub?: string;
  color?: string;
}) {
  return (
    <div className="bg-dark-800 border border-dark-600 rounded-xl p-4 flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <Icon size={15} className={color} />
        <span className="text-[10px] text-gray-500 uppercase tracking-[0.12em] font-bold">
          {label}
        </span>
      </div>
      <p className="text-2xl font-bold text-gray-100 tabular-nums">{value}</p>
      {sub && <p className="text-xs text-gray-500">{sub}</p>}
    </div>
  );
}

function appUrl(hash: string): string {
  return `${window.location.origin}${window.location.pathname}#${hash}`;
}

function reconciledLabel(value: string | undefined): string {
  if (!value || value.startsWith("0001-01-01")) return "not reconciled yet";
  return `reconciled ${new Date(value).toLocaleTimeString()}`;
}

export default function DashboardPage() {
  const { config } = useConfig();
  const theme = useChartTheme();
  const [overview, setOverview] = useState<GuardianOverview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [refreshedAt, setRefreshedAt] = useState<Date>(new Date());
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [copiedPartition, setCopiedPartition] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.guardianOverview();
      setOverview(data);
      setRefreshedAt(new Date());
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to load overview");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
    if (!autoRefresh) return;
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, [autoRefresh, load]);

  const s = overview?.summary;

  const healthPct =
    s && s.assets > 0
      ? Math.round((s.healthyAssets / s.assets) * 100)
      : null;

  const partitions = useMemo(() => overview?.partitions ?? [], [overview]);

  const triage = useMemo(
    () =>
      [...partitions]
        .filter((partition) => partitionTone(partition.health) !== "healthy")
        .sort((a, b) => {
          const tone = toneRank(partitionTone(a.health)) - toneRank(partitionTone(b.health));
          if (tone !== 0) return tone;
          const failing = (b.failingAssets ?? 0) - (a.failingAssets ?? 0);
          if (failing !== 0) return failing;
          return (b.attentionAssets ?? 0) - (a.attentionAssets ?? 0);
        }),
    [partitions]
  );

  const failingPartitions = triage.filter((p) => partitionTone(p.health) === "failing").length;
  const attentionPartitions = triage.length - failingPartitions;
  const overallTone: Tone =
    failingPartitions > 0 ? "failing" : attentionPartitions > 0 ? "attention" : "healthy";

  const pieData = useMemo(
    () =>
      s
        ? [
            { name: "Healthy", value: s.healthyAssets },
            { name: "Attention", value: s.attentionAssets },
            { name: "Failing", value: s.failingAssets },
          ].filter((d) => d.value > 0)
        : [],
    [s]
  );

  const barData = useMemo(
    () =>
      [...partitions]
        .sort((a, b) => {
          const tone = toneRank(partitionTone(b.health)) - toneRank(partitionTone(a.health));
          if (tone !== 0) return tone;
          return b.assetCount - a.assetCount;
        })
        .map((p) => ({
          fullName: p.name,
          Healthy: p.healthyAssets,
          Attention: p.attentionAssets,
          Failing: p.failingAssets,
        })),
    [partitions]
  );

  const pieOption = useMemo<EChartsCoreOption>(() => {
    const toneColor: Record<string, string> = {
      Healthy: theme.green,
      Attention: theme.amber,
      Failing: theme.red,
    };
    return {
      animation: false,
      color: [theme.green, theme.amber, theme.red],
      tooltip: {
        trigger: "item",
        confine: true,
        backgroundColor: theme.elevated,
        borderColor: theme.borderStrong,
        borderWidth: 1,
        padding: [6, 8],
        textStyle: { color: theme.text, fontSize: 11 },
        extraCssText: "border-radius:6px;",
        formatter: (params: unknown) => {
          const item = params as { name?: string; value?: number; percent?: number; color?: string };
          return (
            `<div class="ts-tooltip"><div class="ts-tooltip__row">` +
            `<span class="ts-tooltip__swatch" style="background:${escapeHtml(item.color ?? theme.accent)}"></span>` +
            `<span class="ts-tooltip__name">${escapeHtml(item.name ?? "")}</span>` +
            `<span class="ts-tooltip__value">${item.value ?? 0} (${item.percent ?? 0}%)</span>` +
            `</div></div>`
          );
        },
      },
      legend: {
        bottom: 0,
        icon: "circle",
        itemWidth: 8,
        itemHeight: 8,
        itemGap: 12,
        textStyle: { color: theme.muted, fontSize: 11 },
      },
      series: [
        {
          type: "pie",
          radius: ["52%", "78%"],
          center: ["50%", "43%"],
          avoidLabelOverlap: true,
          padAngle: 3,
          itemStyle: { borderColor: theme.panel, borderWidth: 1 },
          label: { show: false },
          emphasis: { scale: true, label: { show: false } },
          data: pieData.map((entry) => ({
            ...entry,
            itemStyle: { color: toneColor[entry.name] ?? theme.accent },
          })),
        },
      ],
    };
  }, [pieData, theme]);

  const barOption = useMemo<EChartsCoreOption>(() => {
    const axisLabel = {
      color: theme.text,
      fontSize: 10,
      hideOverlap: true,
      interval: 0,
      rotate: barData.length > 6 ? 30 : 0,
      formatter: (value: string) => (value.length > 14 ? `${value.slice(0, 12)}…` : value),
    };
    return {
      animation: false,
      color: [theme.green, theme.amber, theme.red],
      tooltip: {
        trigger: "axis",
        confine: true,
        backgroundColor: theme.elevated,
        borderColor: theme.borderStrong,
        borderWidth: 1,
        padding: [6, 8],
        textStyle: { color: theme.text, fontSize: 11 },
        extraCssText: "border-radius:6px;",
        axisPointer: { type: "shadow", shadowStyle: { color: theme.grid } },
      },
      legend: {
        top: 0,
        right: 0,
        icon: "roundRect",
        itemWidth: 8,
        itemHeight: 8,
        itemGap: 10,
        textStyle: { color: theme.muted, fontSize: 10 },
      },
      grid: { left: 4, right: 8, top: 26, bottom: 4, containLabel: true },
      xAxis: {
        type: "category",
        data: barData.map((entry) => entry.fullName),
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel,
      },
      yAxis: {
        type: "value",
        minInterval: 1,
        axisLine: { show: false },
        axisTick: { show: false },
        axisLabel: { color: theme.text, fontSize: 10 },
        splitLine: { lineStyle: { color: theme.grid, type: "dashed" } },
      },
      series: [
        { key: "Healthy", color: theme.green },
        { key: "Attention", color: theme.amber },
        { key: "Failing", color: theme.red },
      ].map((entry) => ({
        type: "bar" as const,
        name: entry.key,
        data: barData.map((row) => Number(row[entry.key as "Healthy" | "Attention" | "Failing"])),
        barMaxWidth: 14,
        itemStyle: { color: entry.color, borderRadius: [3, 3, 0, 0] },
        emphasis: { focus: "series" as const },
      })),
    };
  }, [barData, theme]);

  const copyTriage = useCallback(async (partition: GuardianOverview["partitions"][number]) => {
    const encoded = encodeURIComponent(partition.name);
    const lines = [
      `Doctor triage — ${partition.name}`,
      `Health: ${partition.health || "unknown"} (${partition.displayStatus || "unknown"})`,
      `Assets: ${partition.assetCount} total · ${partition.healthyAssets} healthy · ${partition.attentionAssets} attention · ${partition.failingAssets} failing`,
      `Intents: ${partition.intentCount} total · ${partition.healthyIntents} healthy · ${partition.driftedIntents} drifted · ${partition.failedIntents} failed`,
      `Last reconciled: ${!partition.lastReconciledAt || partition.lastReconciledAt.startsWith("0001-01-01") ? "unknown" : new Date(partition.lastReconciledAt).toLocaleString()}`,
      "",
      `Assets:   ${appUrl(`/partition/${encoded}/assets`)}`,
      `Logs:     ${appUrl(`/logs?partition=${encoded}&range=15&sev=ERR`)}`,
      `Traces:   ${appUrl(`/traces?partition=${encoded}&range=15`)}`,
      `Metrics:  ${appUrl(`/metrics?partition=${encoded}`)}`,
    ];
    const ok = await copyText(lines.join("\n"));
    setCopiedPartition(ok ? partition.name : "");
    window.setTimeout(() => setCopiedPartition(""), 1800);
  }, []);

  return (
    <div className="h-full flex flex-col overflow-hidden bg-dark-900">
      <header className="shrink-0 border-b border-dark-600 app-header-glow">
        <div className="px-6 py-4 flex items-center justify-between gap-4 flex-wrap">
          <div>
            <p className="text-[11px] uppercase tracking-[0.22em] text-accent-cyan/80 font-semibold">
              On-call workspace
            </p>
            <h1 className="text-2xl font-bold text-gray-100 mt-1">Doctor Triage</h1>
            <p className="text-xs text-gray-500 mt-1">
              {loading && !overview
                ? "Synchronizing telemetry…"
                : `Live telemetry · tenant ${config.default_tenant || "default"} · synced ${refreshedAt.toLocaleTimeString()}`}
            </p>
          </div>

          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => setAutoRefresh((value) => !value)}
              className={`flex items-center gap-1.5 px-3 py-1.5 rounded-lg border text-xs font-semibold transition-colors ${
                autoRefresh
                  ? "border-emerald-500/40 bg-emerald-500/10 text-emerald-400"
                  : "border-dark-500 bg-dark-800 text-gray-400"
              }`}
              title="Toggle 30 second auto-refresh"
            >
              <span className={`w-1.5 h-1.5 rounded-full ${autoRefresh ? "bg-emerald-400" : "bg-gray-500"}`} />
              {autoRefresh ? "Live · 30s" : "Paused"}
            </button>
            <button
              onClick={load}
              disabled={loading}
              className="flex items-center gap-2 px-3 py-1.5 rounded-lg bg-accent-blue/85 hover:bg-accent-blue border border-blue-300/20 text-sm text-white transition-colors disabled:opacity-50"
            >
              <RefreshCw size={14} className={loading ? "animate-spin" : ""} />
              Refresh
            </button>
          </div>
        </div>
      </header>

      <div className="flex-1 overflow-y-auto p-6 space-y-5">
        {error && (
          <div className="rounded-lg bg-red-500/10 border border-red-500/30 px-4 py-3 text-sm text-red-400">
            {error} — Guardian may be unreachable.
          </div>
        )}

        <section className={`oncall-banner oncall-banner--${overallTone}`}>
          <span className="oncall-banner__dot" />
          <div className="oncall-banner__copy">
            <strong>
              {overallTone === "healthy"
                ? "All partitions healthy"
                : overallTone === "failing"
                  ? `${failingPartitions} partition${failingPartitions === 1 ? "" : "s"} failing`
                  : `${attentionPartitions} partition${attentionPartitions === 1 ? "" : "s"} need attention`}
            </strong>
            <span>
              {s
                ? `${s.assets} assets across ${s.partitions} partitions · ${healthPct ?? 0}% healthy · ${s.failingAssets} failing assets`
                : "Waiting for Guardian overview…"}
            </span>
          </div>
          <Link className="triage-action triage-action--primary" to="/fleet">
            Open fleet <ArrowUpRight size={12} />
          </Link>
        </section>

        <div className="grid grid-cols-1 xl:grid-cols-3 gap-4">
          <section className="xl:col-span-2 glass rounded-xl p-5">
            <div className="flex items-center justify-between gap-3 mb-3">
              <h2 className="text-sm font-semibold text-gray-200 flex items-center gap-2">
                <TriangleAlert size={15} className="text-accent-cyan" />
                Needs attention
                {triage.length > 0 && (
                  <span className="text-[10px] px-2 py-0.5 rounded-full bg-amber-500/10 border border-amber-500/30 text-amber-400 font-bold">
                    {triage.length}
                  </span>
                )}
              </h2>
              <span className="text-[11px] text-gray-500">Sorted by severity</span>
            </div>

            {triage.length === 0 ? (
              <div className="py-10 text-center">
                <CheckCircle size={24} className="mx-auto text-green-500/70" />
                <p className="mt-2 text-sm text-gray-300">Nothing needs attention right now.</p>
                <p className="text-xs text-gray-500 mt-1">
                  Doctor will keep watching; failures and drift will surface here first.
                </p>
              </div>
            ) : (
              <div className="triage-list">
                {triage.map((partition) => {
                  const tone = partitionTone(partition.health);
                  const encoded = encodeURIComponent(partition.name);
                  return (
                    <article key={partition.name} className={`triage-row triage-row--${tone}`}>
                      <div className="triage-row__head">
                        <strong>{partition.name}</strong>
                        <StatusBadge health={partition.health || partition.displayStatus} />
                        <span className="text-[11px] text-gray-500 ml-auto">
                          {reconciledLabel(partition.lastReconciledAt)}
                        </span>
                      </div>
                      <div className="triage-row__meta">
                        <span><b>{partition.failingAssets}</b> failing</span>
                        <span><b>{partition.attentionAssets}</b> attention</span>
                        <span><b>{partition.healthyAssets}</b> healthy</span>
                        <span><b>{partition.assetCount}</b> assets</span>
                        <span><b>{partition.intentCount}</b> intents</span>
                        {partition.driftedIntents > 0 && <span><b>{partition.driftedIntents}</b> drifted</span>}
                      </div>
                      <div className="triage-row__actions">
                        <Link className="triage-action triage-action--primary" to={`/partition/${encoded}/assets`}>
                          Assets
                        </Link>
                        <Link className="triage-action" to={`/logs?partition=${encoded}&range=15&sev=ERR`}>
                          <FileText size={11} /> Error logs
                        </Link>
                        <Link className="triage-action" to={`/traces?partition=${encoded}&range=15`}>
                          <GitBranch size={11} /> Traces
                        </Link>
                        <Link className="triage-action" to={`/metrics?partition=${encoded}`}>
                          <BarChart3 size={11} /> Metrics
                        </Link>
                        <button
                          type="button"
                          className={"triage-action" + (copiedPartition === partition.name ? " triage-action--primary" : "")}
                          onClick={() => void copyTriage(partition)}
                        >
                          <Copy size={11} /> {copiedPartition === partition.name ? "Copied" : "Copy triage"}
                        </button>
                      </div>
                    </article>
                  );
                })}
              </div>
            )}
          </section>

          <section className="glass rounded-xl p-5">
            <h2 className="text-sm font-semibold text-gray-200 flex items-center gap-2 mb-3">
              <ShieldCheck size={15} className="text-accent-blue" />
              Investigation shortcuts
            </h2>
            <div className="quick-actions">
              <Link className="quick-action" to="/logs?range=15&sev=ERR">
                <TriangleAlert size={15} />
                <span>Errors in the last 15m</span>
                <small>logs</small>
              </Link>
              <Link className="quick-action" to="/logs?range=60">
                <FileText size={15} />
                <span>Live log tail (1h)</span>
                <small>logs</small>
              </Link>
              <Link className="quick-action" to="/traces?range=15">
                <Activity size={15} />
                <span>Recent traces</span>
                <small>15m</small>
              </Link>
              <Link className="quick-action" to="/fleet">
                <Server size={15} />
                <span>Fleet topology</span>
                <small>assets</small>
              </Link>
              <Link className="quick-action" to="/metrics">
                <BarChart3 size={15} />
                <span>Metrics explorer</span>
                <small>PromQL</small>
              </Link>
              {config.guardian_url && (
                <a className="quick-action" href={config.guardian_url} target="_blank" rel="noreferrer">
                  <ShieldCheck size={15} />
                  <span>Guardian control plane</span>
                  <small>↗</small>
                </a>
              )}
            </div>
            <p className="text-[11px] text-gray-500 mt-3 leading-relaxed">
              Press <kbd className="text-gray-400">⌘K</kbd> anywhere to jump to a partition or investigation.
            </p>
          </section>
        </div>

        <div className="oncall-stats">
          <StatCard icon={Server} label="Partitions" value={s?.partitions ?? "\u2014"} color="text-accent-blue" />
          <StatCard
            icon={Activity}
            label="Assets"
            value={s?.assets ?? "\u2014"}
            sub={healthPct !== null ? `${healthPct}% healthy` : undefined}
            color="text-accent-cyan"
          />
          <StatCard
            icon={AlertTriangle}
            label="Failing assets"
            value={s?.failingAssets ?? "\u2014"}
            sub={`${s?.failedIntents ?? 0} failed intents`}
            color="text-red-500"
          />
          <StatCard
            icon={GitBranch}
            label="Drifted intents"
            value={s?.driftedIntents ?? "\u2014"}
            sub={`${s?.healthyIntents ?? 0} healthy intents`}
            color="text-amber-500"
          />
        </div>

        <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
          <div className="glass rounded-xl p-5 border border-green-500/20">
            <h2 className="text-sm font-semibold text-gray-200 mb-4 flex items-center gap-2">
              <CheckCircle size={14} className="text-green-500" />
              Asset health
            </h2>
            {pieData.length > 0 ? (
              <div style={{ height: 200 }}>
                <EChart option={pieOption} ariaLabel="Asset health distribution" />
              </div>
            ) : (
              <div className="h-[200px] flex items-center justify-center text-gray-600 text-sm">
                No data
              </div>
            )}
          </div>

          <div className="lg:col-span-2 glass rounded-xl p-5 border border-cyan-500/20">
            <h2 className="text-sm font-semibold text-gray-200 mb-4 flex items-center gap-2">
              <Server size={14} className="text-accent-blue" />
              Assets per partition
            </h2>
            {barData.length > 0 ? (
              <div style={{ height: 200 }}>
                <EChart option={barOption} ariaLabel="Assets per partition by health" />
              </div>
            ) : (
              <div className="h-[200px] flex items-center justify-center text-gray-600 text-sm">
                No data
              </div>
            )}
          </div>
        </div>

        <div className="glass border border-dark-600 rounded-xl overflow-hidden">
          <div className="px-5 py-3 border-b border-dark-600 flex items-center gap-2">
            <Server size={14} className="text-accent-blue" />
            <h2 className="text-sm font-semibold text-gray-200">Partition health feed</h2>
            <span className="text-[11px] text-gray-500 ml-auto">
              {partitions.length} partitions
            </span>
          </div>
          {loading && !overview ? (
            <div className="py-12 text-center text-gray-600 text-sm">
              Loading…
            </div>
          ) : partitions.length === 0 ? (
            <div className="py-12 text-center text-gray-600 text-sm">
              No partitions found
            </div>
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="text-left text-gray-500 text-xs uppercase tracking-wide">
                  <th className="px-5 py-2.5">Partition</th>
                  <th className="px-4 py-2.5">Health</th>
                  <th className="px-4 py-2.5 text-right">Assets</th>
                  <th className="px-4 py-2.5 text-right">Failing</th>
                  <th className="px-4 py-2.5 text-right">Attention</th>
                  <th className="px-4 py-2.5 hidden lg:table-cell text-right">
                    Last reconciled
                  </th>
                  <th className="px-4 py-2.5 hidden md:table-cell">Investigate</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-dark-600">
                {[...partitions]
                  .sort((a, b) => {
                    const tone = toneRank(partitionTone(a.health)) - toneRank(partitionTone(b.health));
                    if (tone !== 0) return tone;
                    return b.failingAssets - a.failingAssets;
                  })
                  .map((p) => {
                    const encoded = encodeURIComponent(p.name);
                    return (
                      <tr key={p.name} className="hover:bg-dark-700 transition-colors">
                        <td className="px-5 py-2.5 font-medium text-gray-200">
                          <Link to={`/partition/${encoded}/assets`} className="hover:text-accent-blue">
                            {p.name}
                          </Link>
                        </td>
                        <td className="px-4 py-2.5">
                          <StatusBadge health={p.health || p.displayStatus} />
                        </td>
                        <td className="px-4 py-2.5 text-right tabular-nums text-gray-300">
                          {p.assetCount}
                        </td>
                        <td className="px-4 py-2.5 text-right tabular-nums text-red-500">
                          {p.failingAssets}
                        </td>
                        <td className="px-4 py-2.5 text-right tabular-nums text-amber-500">
                          {p.attentionAssets}
                        </td>
                        <td className="px-4 py-2.5 hidden lg:table-cell text-right text-gray-600 text-xs">
                          {!p.lastReconciledAt || p.lastReconciledAt.startsWith("0001-01-01")
                            ? "\u2014"
                            : new Date(p.lastReconciledAt).toLocaleString()}
                        </td>
                        <td className="px-4 py-2.5 hidden md:table-cell">
                          <div className="flex items-center gap-2 text-xs">
                            <Link className="text-sky-400 hover:text-sky-300" to={`/logs?partition=${encoded}&range=15&sev=ERR`}>
                              Logs
                            </Link>
                            <Link className="text-sky-400 hover:text-sky-300" to={`/traces?partition=${encoded}&range=15`}>
                              Traces
                            </Link>
                          </div>
                        </td>
                      </tr>
                    );
                  })}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </div>
  );
}
