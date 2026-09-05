import { motion, AnimatePresence } from "framer-motion";
import {
  Activity,
  CheckCircle,
  AlertTriangle,
  Zap,
  Grid3X3,
  GripHorizontal,
  SlidersHorizontal,
} from "lucide-react";

export type HealthStatus = "healthy" | "degraded" | "critical";

export interface AssetGroup {
  id: string;
  name: string;
  status: HealthStatus;
  nodes: HealthStatus[];
  partition: string;
  intent: string;
  sourceScope: "asset" | "fallback";
}

export interface PartitionModel {
  id: string;
  region: string;
  intents: {
    name: string;
    assets: AssetGroup[];
  }[];
}

export type SortKey = "name" | "health" | "assets";
export type SortOrder = "asc" | "desc";

interface FleetViewProps {
  partitions: PartitionModel[];
  activePartitions: number;
  criticalAssets: number;
  aggregatedRps: number;
  sortKey: SortKey;
  sortOrder: SortOrder;
  viewMode: "compact" | "detailed";
  onSelectPartition: (partition: PartitionModel) => void;
  onSelectHub: (asset: AssetGroup) => void;
  onSortChange: (key: SortKey) => void;
  onViewModeChange: (mode: "compact" | "detailed") => void;
}

function statusColor(status: HealthStatus): string {
  if (status === "healthy") return "#10b981";
  if (status === "degraded") return "#f59e0b";
  return "#ef4444";
}

function countSeverity(assets: AssetGroup[]): { healthy: number; degraded: number; critical: number } {
  const counts = { healthy: 0, degraded: 0, critical: 0 };
  for (const a of assets) counts[a.status] += 1;
  return counts;
}

function overallStatus(intents: PartitionModel["intents"]): HealthStatus {
  let degraded = false;
  for (const i of intents) {
    for (const a of i.assets) {
      if (a.status === "critical") return "critical";
      if (a.status === "degraded") degraded = true;
    }
  }
  return degraded ? "degraded" : "healthy";
}

function healthPct(assets: AssetGroup[]): number {
  if (assets.length === 0) return 0;
  const healthy = assets.filter((a) => a.status === "healthy").length;
  return Math.round((healthy / assets.length) * 100);
}

export default function FleetView({
  partitions,
  activePartitions,
  criticalAssets,
  aggregatedRps,
  sortKey,
  sortOrder,
  viewMode,
  onSelectPartition,
  onSelectHub,
  onSortChange,
  onViewModeChange,
}: FleetViewProps) {
  const totalAssets = partitions.reduce((s, p) => s + p.intents.reduce((s2, i) => s2 + i.assets.length, 0), 0);
  const totalHealthy = partitions.reduce((s, p) => s + p.intents.reduce((s2, i) => s2 + i.assets.filter((a) => a.status === "healthy").length, 0), 0);
  const healthyPct = totalAssets > 0 ? Math.round((totalHealthy / totalAssets) * 100) : 0;

  const sortLabels: Record<SortKey, string> = { name: "Name", health: "Health", assets: "Assets" };

  return (
    <div className="w-full bg-slate-950 text-slate-100 font-sans">
      {/* Summary bar */}
      <div className="px-6 pt-4 pb-3 grid grid-cols-2 md:grid-cols-4 gap-3">
        <div className="bg-slate-900 border border-slate-800 px-4 py-3 flex items-center gap-3">
          <Activity className="w-4 h-4 text-sky-400 shrink-0" />
          <div>
            <span className="text-[10px] text-slate-500 uppercase tracking-wider block">Partitions</span>
            <span className="text-lg font-bold text-slate-100 font-mono">{activePartitions}</span>
          </div>
        </div>
        <div className="bg-slate-900 border border-slate-800 px-4 py-3 flex items-center gap-3">
          <CheckCircle className="w-4 h-4 text-emerald-400 shrink-0" />
          <div>
            <span className="text-[10px] text-slate-500 uppercase tracking-wider block">Healthy</span>
            <span className="text-lg font-bold text-emerald-400 font-mono">{healthyPct}%</span>
          </div>
        </div>
        <div className="bg-slate-900 border border-slate-800 px-4 py-3 flex items-center gap-3">
          <AlertTriangle className="w-4 h-4 text-amber-400 shrink-0" />
          <div>
            <span className="text-[10px] text-slate-500 uppercase tracking-wider block">Critical Assets</span>
            <span className="text-lg font-bold text-rose-400 font-mono">{criticalAssets}</span>
          </div>
        </div>
        <div className="bg-slate-900 border border-slate-800 px-4 py-3 flex items-center gap-3">
          <Zap className="w-4 h-4 text-violet-400 shrink-0" />
          <div>
            <span className="text-[10px] text-slate-500 uppercase tracking-wider block">Aggregated RPS</span>
            <span className="text-lg font-bold text-sky-400 font-mono">{aggregatedRps.toLocaleString()}</span>
          </div>
        </div>
      </div>

      {/* Sort & view bar */}
      <div className="px-6 pb-3 flex items-center gap-2">
        <div className="flex items-center gap-1.5">
          <SlidersHorizontal size={12} className="text-slate-500" />
          {(["name", "health", "assets"] as SortKey[]).map((key) => (
            <button
              key={key}
              onClick={() => onSortChange(key)}
              className={`px-2 py-0.5 text-[10px] border rounded ${
                sortKey === key
                  ? "bg-sky-500/15 border-sky-400/50 text-sky-300"
                  : "border-slate-800 text-slate-500 hover:border-slate-700 hover:text-slate-400"
              }`}
            >
              {sortLabels[key]}
              {sortKey === key && (sortOrder === "asc" ? " ↑" : " ↓")}
            </button>
          ))}
        </div>
        <div className="flex-1" />
        <div className="flex items-center gap-1 border-l border-slate-800 pl-2">
          <button
            onClick={() => onViewModeChange("compact")}
            className={`p-1 rounded ${
              viewMode === "compact" ? "bg-sky-500/15 text-sky-300" : "text-slate-600 hover:text-slate-400"
            }`}
            title="Compact view"
          >
            <Grid3X3 size={14} />
          </button>
          <button
            onClick={() => onViewModeChange("detailed")}
            className={`p-1 rounded ${
              viewMode === "detailed" ? "bg-sky-500/15 text-sky-300" : "text-slate-600 hover:text-slate-400"
            }`}
            title="Detailed view"
          >
            <GripHorizontal size={14} />
          </button>
        </div>
      </div>

      {/* Partition card grid */}
      <div className="px-6 pb-6">
        <AnimatePresence mode="popLayout">
          {partitions.length > 0 ? (
            <motion.div
              layout
              className="grid grid-cols-1 sm:grid-cols-2 md:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5 2xl:grid-cols-6 gap-3"
            >
              {partitions.map((partition, pIdx) => {
                const pStatus = overallStatus(partition.intents);
                const pColor = statusColor(pStatus);
                const allPartAssets = partition.intents.flatMap((i) => i.assets);
                const pCounts = countSeverity(allPartAssets);
                const pct = healthPct(allPartAssets);

                return (
                  <motion.div
                    key={partition.id}
                    layout
                    initial={{ opacity: 0, scale: 0.95 }}
                    animate={{ opacity: 1, scale: 1 }}
                    transition={{ delay: pIdx * 0.02, duration: 0.2 }}
                    exit={{ opacity: 0, scale: 0.95 }}
                    className="bg-slate-900 border border-slate-800 hover:border-slate-600 cursor-pointer group transition-colors"
                    onClick={() => onSelectPartition(partition)}
                  >
                    {/* Health indicator bar + header */}
                    <div className="flex items-stretch">
                      <div className="w-1 shrink-0" style={{ backgroundColor: pColor }} />
                      <div className="flex-1 min-w-0 p-3">
                        <div className="flex items-center gap-2">
                          <div
                            className="w-1.5 h-1.5 rounded-full shrink-0"
                            style={{ backgroundColor: pColor }}
                          />
                          <span className="text-xs font-bold text-slate-200 uppercase tracking-wider truncate font-['Space_Grotesk']">
                            {partition.region}
                          </span>
                          <span
                            className="ml-auto shrink-0 text-[9px] font-semibold uppercase px-1.5 py-0.5"
                            style={{ color: pColor, border: `1px solid ${pColor}30`, backgroundColor: `${pColor}10` }}
                          >
                            {pStatus}
                          </span>
                        </div>

                        {/* Health bar */}
                        <div className="mt-2 flex items-center gap-2">
                          <div className="flex-1 h-1 bg-slate-800 flex min-w-0 overflow-hidden">
                            {pCounts.healthy > 0 && (
                              <motion.div
                                initial={{ width: 0 }}
                                animate={{ width: `${(pCounts.healthy / Math.max(allPartAssets.length, 1)) * 100}%` }}
                                transition={{ duration: 0.3, delay: pIdx * 0.02 + 0.1 }}
                                className="h-full bg-emerald-500/60 shrink-0"
                              />
                            )}
                            {pCounts.degraded > 0 && (
                              <motion.div
                                initial={{ width: 0 }}
                                animate={{ width: `${(pCounts.degraded / Math.max(allPartAssets.length, 1)) * 100}%` }}
                                transition={{ duration: 0.3, delay: pIdx * 0.02 + 0.15 }}
                                className="h-full bg-amber-500/60 shrink-0"
                              />
                            )}
                            {pCounts.critical > 0 && (
                              <motion.div
                                initial={{ width: 0 }}
                                animate={{ width: `${(pCounts.critical / Math.max(allPartAssets.length, 1)) * 100}%` }}
                                transition={{ duration: 0.3, delay: pIdx * 0.02 + 0.2 }}
                                className="h-full bg-red-500/70 shrink-0 animate-pulse"
                              />
                            )}
                          </div>
                          <span className="text-[10px] text-slate-600 font-mono shrink-0 w-7 text-right">{pct}%</span>
                        </div>

                        {/* Metadata */}
                        <div className="mt-1.5 flex items-center gap-2 text-[9px] text-slate-500">
                          <span>{partition.intents.length} intent{partition.intents.length !== 1 ? "s" : ""}</span>
                          <span className="text-slate-700">·</span>
                          <span>{allPartAssets.length} asset{allPartAssets.length !== 1 ? "s" : ""}</span>
                          <div className="flex-1" />
                          <div className="opacity-0 group-hover:opacity-100 transition-opacity text-slate-600 text-[9px]">
                            →
                          </div>
                        </div>

                        {/* Intent pills — only in detailed mode */}
                        {viewMode === "detailed" && (
                          <div className="mt-2 pt-2 border-t border-slate-800/60 flex flex-wrap items-center gap-1">
                            {partition.intents.map((intent) => {
                              const iCounts = countSeverity(intent.assets);
                              const iColor = iCounts.critical > 0 ? "#ef4444" : iCounts.degraded > 0 ? "#f59e0b" : "#64748b";

                              return (
                                <div
                                  key={`${partition.id}-${intent.name}`}
                                  className={`flex items-center gap-1 px-1.5 py-0.5 text-[9px] border ${
                                    iCounts.critical > 0 || iCounts.degraded > 0
                                      ? "border-amber-500/20 bg-amber-500/5 text-slate-300"
                                      : "border-slate-800 bg-slate-900/60 text-slate-400"
                                  }`}
                                >
                                  <span
                                    className="w-1 h-1 rounded-full shrink-0"
                                    style={{ backgroundColor: iColor }}
                                  />
                                  <span className="font-medium truncate max-w-[80px]">{intent.name}</span>
                                  <span className="font-mono text-slate-600 ml-0.5">{intent.assets.length}</span>
                                  <div className="flex items-center gap-px ml-1 border-l border-slate-800 pl-1">
                                    {intent.assets.slice(0, 8).map((asset) => (
                                      <motion.button
                                        key={asset.id}
                                        whileHover={{ y: -1.5 }}
                                        whileTap={{ scale: 0.95 }}
                                        onClick={(e) => {
                                          e.stopPropagation();
                                          onSelectHub(asset);
                                        }}
                                        className="w-1.5 h-1.5 shrink-0 cursor-pointer"
                                        style={{ backgroundColor: statusColor(asset.status) }}
                                        title={`${asset.name} - ${asset.status}`}
                                      />
                                    ))}
                                    {intent.assets.length > 8 && (
                                      <span className="text-[8px] text-slate-600 ml-0.5">+{intent.assets.length - 8}</span>
                                    )}
                                  </div>
                                </div>
                              );
                            })}
                          </div>
                        )}
                      </div>
                    </div>
                  </motion.div>
                );
              })}
            </motion.div>
          ) : (
            <div className="text-center py-16 text-slate-600 text-xs">
              No partitions match current filters
            </div>
          )}
        </AnimatePresence>
      </div>
    </div>
  );
}
