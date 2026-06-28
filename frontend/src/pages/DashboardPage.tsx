import { useEffect, useState, useCallback } from "react";
import {
  BarChart,
  Bar,
  XAxis,
  YAxis,
  Tooltip,
  ResponsiveContainer,
  PieChart,
  Pie,
  Cell,
  Legend,
} from "recharts";
import {
  Server,
  Boxes,
  Activity,
  CheckCircle,
  AlertTriangle,
  XCircle,
  RefreshCw,
  GitBranch,
  Sparkles,
  ShieldCheck,
  Wrench,
  BrainCircuit,
  Image,
} from "lucide-react";
import { api } from "../api/client";
import type { GuardianOverview } from "../types/api";

const HEALTH_GREEN = "#22c55e";
const HEALTH_AMBER = "#f59e0b";
const HEALTH_RED = "#ef4444";
const HEALTH_GRAY = "#475569";

function healthColor(health: string): string {
  const h = health.toLowerCase();
  if (h === "healthy") return HEALTH_GREEN;
  if (h === "attention" || h === "degraded") return HEALTH_AMBER;
  if (h === "failing" || h === "failed" || h === "error") return HEALTH_RED;
  return HEALTH_GRAY;
}

function StatusBadge({ health }: { health: string }) {
  const color = healthColor(health);
  return (
    <span
      className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-medium"
      style={{ backgroundColor: `${color}22`, color }}
    >
      <span
        className="w-1.5 h-1.5 rounded-full"
        style={{ backgroundColor: color }}
      />
      {health}
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
    <div className="bg-dark-800 border border-dark-600 rounded-xl p-5 flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <Icon size={16} className={color} />
        <span className="text-xs text-gray-500 uppercase tracking-wide font-medium">
          {label}
        </span>
      </div>
      <p className="text-3xl font-bold text-gray-100">{value}</p>
      {sub && <p className="text-xs text-gray-500">{sub}</p>}
    </div>
  );
}

const PIE_COLORS = [HEALTH_GREEN, HEALTH_AMBER, HEALTH_RED];

export default function DashboardPage() {
  const [overview, setOverview] = useState<GuardianOverview | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [refreshedAt, setRefreshedAt] = useState<Date>(new Date());

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
    load();
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, [load]);

  const s = overview?.summary;

  const healthPct =
    s && s.assets > 0
      ? Math.round((s.healthyAssets / s.assets) * 100)
      : null;

  const pieData = s
    ? [
        { name: "Healthy", value: s.healthyAssets },
        { name: "Attention", value: s.attentionAssets },
        { name: "Failing", value: s.failingAssets },
      ].filter((d) => d.value > 0)
    : [];

  const barData =
    overview?.partitions.map((p) => ({
      name: p.name.length > 14 ? p.name.slice(0, 12) + "\u2026" : p.name,
      fullName: p.name,
      Healthy: p.healthyAssets,
      Attention: p.attentionAssets,
      Failing: p.failingAssets,
    })) ?? [];

  return (
    <div className="h-full flex flex-col overflow-hidden bg-dark-900">
      <header className="shrink-0 border-b border-dark-600 bg-[radial-gradient(circle_at_15%_20%,rgba(6,182,212,0.12),transparent_40%),radial-gradient(circle_at_85%_15%,rgba(59,130,246,0.16),transparent_35%)]">
        <div className="px-6 py-4 flex items-center justify-between gap-4">
          <div>
            <p className="text-[11px] uppercase tracking-[0.22em] text-accent-cyan/80 font-semibold">
              Observability Dashboard
            </p>
            <h1 className="text-2xl font-bold text-gray-100 mt-1">Doctor Query Plane</h1>
            <p className="text-xs text-gray-500 mt-1">
              {refreshedAt && !loading
                ? `Live telemetry synced at ${refreshedAt.toLocaleTimeString()}`
                : "Synchronizing telemetry..."}
            </p>
          </div>

          <button
            onClick={load}
            disabled={loading}
            className="flex items-center gap-2 px-3 py-1.5 rounded-lg bg-accent-blue/85 hover:bg-accent-blue border border-blue-300/20 text-sm text-white transition-colors disabled:opacity-50"
          >
            <RefreshCw size={14} className={loading ? "animate-spin" : ""} />
            Refresh
          </button>
        </div>
      </header>

      <div className="flex-1 overflow-y-auto p-6 space-y-6">
        {error && (
          <div className="rounded-lg bg-red-500/10 border border-red-500/30 px-4 py-3 text-sm text-red-400">
            {error} — Guardian may be unreachable.
          </div>
        )}

        <div className="grid grid-cols-1 xl:grid-cols-3 gap-4">
          <div className="xl:col-span-2 glass rounded-xl p-5 border border-cyan-500/20">
            <div className="flex items-center justify-between mb-4">
              <h2 className="text-sm font-semibold text-gray-200 flex items-center gap-2">
                <BrainCircuit size={14} className="text-accent-cyan" />
                Doctor Runtime Snapshot
              </h2>
              <StatusBadge health={healthPct !== null && healthPct >= 80 ? "healthy" : "attention"} />
            </div>

            <div className="grid grid-cols-2 lg:grid-cols-4 gap-4">
              <StatCard
                icon={Server}
                label="Partitions"
                value={s?.partitions ?? "\u2014"}
                color="text-accent-blue"
              />
              <StatCard
                icon={Boxes}
                label="Total Assets"
                value={s?.assets ?? "\u2014"}
                sub={healthPct !== null ? `${healthPct}% healthy` : undefined}
                color="text-accent-purple"
              />
              <StatCard
                icon={GitBranch}
                label="Intents"
                value={s?.intents ?? "\u2014"}
                sub={
                  s
                    ? `${s.healthyIntents} ok \u00b7 ${s.driftedIntents} drifted \u00b7 ${s.failedIntents} failed`
                    : undefined
                }
                color="text-accent-cyan"
              />
              <StatCard
                icon={Activity}
                label="Services"
                value={s ? `${s.servicesHealthy + s.servicesAttention}` : "\u2014"}
                sub={
                  s
                    ? `${s.servicesHealthy} healthy \u00b7 ${s.servicesAttention} degraded`
                    : undefined
                }
                color="text-green-500"
              />
            </div>

            <div className="grid grid-cols-1 md:grid-cols-3 gap-3 mt-4">
              <div className="rounded-lg border border-dark-500 bg-dark-800/70 px-3 py-2 text-xs text-gray-300 flex items-center gap-2">
                <ShieldCheck size={13} className="text-green-400" />
                Guardrail status: Active
              </div>
              <div className="rounded-lg border border-dark-500 bg-dark-800/70 px-3 py-2 text-xs text-gray-300 flex items-center gap-2">
                <Wrench size={13} className="text-accent-purple" />
                Tool loop: up to 5 rounds
              </div>
              <div className="rounded-lg border border-dark-500 bg-dark-800/70 px-3 py-2 text-xs text-gray-300 flex items-center gap-2">
                <Sparkles size={13} className="text-accent-cyan" />
                Streaming mode: enabled
              </div>
            </div>
          </div>

          <section className="glass rounded-xl p-5 border border-blue-500/20 flex flex-col">
            <h2 className="text-sm font-semibold text-gray-200 flex items-center gap-2 mb-3">
              <Image size={14} className="text-accent-blue" />
              Reference Image
            </h2>
            <p className="text-xs text-gray-500 mb-3">
              Prompt text hidden. Waiting for your image.
            </p>
            <div className="w-full h-[230px] rounded-lg border border-dashed border-dark-500 bg-dark-800/70 flex flex-col items-center justify-center gap-2">
              <Image size={20} className="text-gray-500" />
              <p className="text-xs text-gray-500">Share an image and I will wire it in.</p>
            </div>
          </section>
        </div>

        <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
          <div className="glass rounded-xl p-5 border border-green-500/20">
            <h2 className="text-sm font-semibold text-gray-200 mb-4 flex items-center gap-2">
              <CheckCircle size={14} className="text-green-500" />
              Asset Health
            </h2>
            {pieData.length > 0 ? (
              <ResponsiveContainer width="100%" height={200}>
                <PieChart>
                  <Pie
                    data={pieData}
                    cx="50%"
                    cy="50%"
                    innerRadius={55}
                    outerRadius={80}
                    paddingAngle={3}
                    dataKey="value"
                  >
                    {pieData.map((_, i) => (
                      <Cell key={i} fill={PIE_COLORS[i]} />
                    ))}
                  </Pie>
                  <Tooltip
                    contentStyle={{
                      backgroundColor: "#12121a",
                      border: "1px solid #252533",
                      borderRadius: 8,
                      fontSize: 12,
                    }}
                  />
                  <Legend
                    iconType="circle"
                    iconSize={8}
                    formatter={(v) => (
                      <span className="text-xs text-gray-400">{v}</span>
                    )}
                  />
                </PieChart>
              </ResponsiveContainer>
            ) : (
              <div className="h-[200px] flex items-center justify-center text-gray-600 text-sm">
                No data
              </div>
            )}
          </div>

          <div className="lg:col-span-2 glass rounded-xl p-5 border border-cyan-500/20">
            <h2 className="text-sm font-semibold text-gray-200 mb-4 flex items-center gap-2">
              <Server size={14} className="text-accent-blue" />
              Assets per Partition
            </h2>
            {barData.length > 0 ? (
              <ResponsiveContainer width="100%" height={200}>
                <BarChart
                  data={barData}
                  margin={{ top: 4, right: 8, left: -24, bottom: 0 }}
                  barSize={14}
                  barGap={2}
                >
                  <XAxis
                    dataKey="name"
                    tick={{ fill: "#94a3b8", fontSize: 11 }}
                    axisLine={false}
                    tickLine={false}
                  />
                  <YAxis
                    tick={{ fill: "#94a3b8", fontSize: 11 }}
                    axisLine={false}
                    tickLine={false}
                    allowDecimals={false}
                  />
                  <Tooltip
                    cursor={{ fill: "#ffffff08" }}
                    contentStyle={{
                      backgroundColor: "#12121a",
                      border: "1px solid #252533",
                      borderRadius: 8,
                      fontSize: 12,
                    }}
                    formatter={(v: number, name: string) => [v, name]}
                    labelFormatter={(label, payload) =>
                      payload?.[0]?.payload?.fullName ?? label
                    }
                  />
                  <Bar dataKey="Healthy" fill={HEALTH_GREEN} radius={[3, 3, 0, 0]} />
                  <Bar dataKey="Attention" fill={HEALTH_AMBER} radius={[3, 3, 0, 0]} />
                  <Bar dataKey="Failing" fill={HEALTH_RED} radius={[3, 3, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
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
            <h2 className="text-sm font-semibold text-gray-200">Infrastructure Feed</h2>
          </div>
          {loading && !overview ? (
            <div className="py-12 text-center text-gray-600 text-sm">
              Loading\u2026
            </div>
          ) : (overview?.partitions.length ?? 0) === 0 ? (
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
                  <th className="px-4 py-2.5 text-right">Intents</th>
                  <th className="px-4 py-2.5 hidden md:table-cell">
                    <span className="flex items-center gap-1">
                      <CheckCircle size={11} className="text-green-500" />
                      OK
                    </span>
                  </th>
                  <th className="px-4 py-2.5 hidden md:table-cell">
                    <span className="flex items-center gap-1">
                      <AlertTriangle size={11} className="text-amber-500" />
                      Attn
                    </span>
                  </th>
                  <th className="px-4 py-2.5 hidden md:table-cell">
                    <span className="flex items-center gap-1">
                      <XCircle size={11} className="text-red-500" />
                      Fail
                    </span>
                  </th>
                  <th className="px-4 py-2.5 hidden lg:table-cell text-right">
                    Last reconciled
                  </th>
                </tr>
              </thead>
              <tbody className="divide-y divide-dark-600">
                {overview?.partitions.map((p) => (
                  <tr
                    key={p.name}
                    className="hover:bg-dark-700 transition-colors"
                  >
                    <td className="px-5 py-3 font-medium text-gray-200">
                      {p.name}
                    </td>
                    <td className="px-4 py-3">
                      <StatusBadge health={p.health || p.displayStatus} />
                    </td>
                    <td className="px-4 py-3 text-right tabular-nums text-gray-300">
                      {p.assetCount}
                    </td>
                    <td className="px-4 py-3 text-right tabular-nums text-gray-300">
                      {p.intentCount}
                    </td>
                    <td className="px-4 py-3 hidden md:table-cell tabular-nums text-green-500">
                      {p.healthyAssets}
                    </td>
                    <td className="px-4 py-3 hidden md:table-cell tabular-nums text-amber-500">
                      {p.attentionAssets}
                    </td>
                    <td className="px-4 py-3 hidden md:table-cell tabular-nums text-red-500">
                      {p.failingAssets}
                    </td>
                    <td className="px-4 py-3 hidden lg:table-cell text-right text-gray-600 text-xs">
                      {p.lastReconciledAt
                        ? new Date(p.lastReconciledAt).toLocaleString()
                        : "\u2014"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </div>
  );
}
