import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  CheckCircle,
  Grid3X3,
  GripHorizontal,
  RefreshCw,
  Search,
  Server,
  Upload,
} from "lucide-react";
import FleetView, {
  type AssetGroup,
  type HealthStatus,
  type PartitionModel,
  type SortKey,
  type SortOrder,
} from "./FleetView";
import HubDetailView from "./HubDetailView";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import type {
  GuardianEventsResponse,
  GuardianHealthResponse,
  GuardianOverview,
  GuardianTopologyResponse,
} from "../types/api";
import type { HelperFileManifest } from "../types/observability";

export { type HelperFileManifest };

type Tone = "healthy" | "attention" | "critical" | "unknown";

function mapLevelToHealth(level?: string): HealthStatus {
  const normalized = (level || "").toLowerCase();
  if (normalized.includes("fail") || normalized.includes("crit") || normalized.includes("error")) {
    return "critical";
  }
  if (normalized.includes("degrad") || normalized.includes("attention") || normalized.includes("drift")) {
    return "degraded";
  }
  return "healthy";
}

function toneFor(health: string | undefined): Tone {
  const value = (health || "").toLowerCase();
  if (value.includes("fail") || value.includes("crit") || value.includes("error") || value.includes("unhealthy")) {
    return "critical";
  }
  if (
    value.includes("attention") ||
    value.includes("degrad") ||
    value.includes("drift") ||
    value.includes("pending")
  ) {
    return "attention";
  }
  if (value.includes("healthy") || value.includes("locked") || value.includes("stable") || value.includes("ok")) {
    return "healthy";
  }
  return "unknown";
}

function toneRank(tone: Tone): number {
  if (tone === "critical") return 0;
  if (tone === "attention") return 1;
  if (tone === "healthy") return 2;
  return 3;
}

function buildFleetModel(
  overview: GuardianOverview | null,
  topology: GuardianTopologyResponse | null
): PartitionModel[] {
  const topologyNodes = topology?.nodes ?? [];
  const intentsByPartition = new Map<string, Map<string, AssetGroup[]>>();

  function ensureIntent(partition: string, intent: string): AssetGroup[] {
    let byIntent = intentsByPartition.get(partition);
    if (!byIntent) {
      byIntent = new Map();
      intentsByPartition.set(partition, byIntent);
    }
    let assets = byIntent.get(intent);
    if (!assets) {
      assets = [];
      byIntent.set(intent, assets);
    }
    return assets;
  }

  for (const node of topologyNodes) {
    if (node.type === "intent") {
      ensureIntent(node.partition || "unknown", node.name || node.intent || "Intent");
      continue;
    }
    if (node.type !== "asset") continue;
    const partition = node.partition || "unknown";
    const intent = node.intent || "Services";
    ensureIntent(partition, intent).push({
      id: node.id,
      name: node.name,
      status: mapLevelToHealth(node.health?.level),
      partition,
      intent,
    });
  }

  return (overview?.partitions ?? []).map((partition) => {
    const byIntent = intentsByPartition.get(partition.name);
    const intents = byIntent
      ? [...byIntent.entries()]
          .map(([name, assets]) => ({
            name,
            assets: [...assets].sort((a, b) => a.name.localeCompare(b.name)),
          }))
          .sort((a, b) => a.name.localeCompare(b.name))
      : [];

    return {
      id: partition.name,
      region: partition.name,
      intents,
      summary: {
        health: partition.health,
        status: partition.displayStatus || partition.status,
        assetCount: partition.assetCount,
        intentCount: partition.intentCount,
        healthyAssets: partition.healthyAssets,
        attentionAssets: partition.attentionAssets,
        failingAssets: partition.failingAssets,
      },
    };
  });
}

function minutesLabel(minutes: number): string {
  if (minutes < 60) return `${minutes}m`;
  if (minutes < 60 * 24) return `${minutes / 60}h`;
  return `${minutes / (60 * 24)}d`;
}

function KpiCard({
  icon: Icon,
  label,
  value,
  sub,
  tone = "neutral",
}: {
  icon: React.ElementType;
  label: string;
  value: number | string;
  sub: string;
  tone?: "neutral" | "healthy" | "attention" | "critical";
}) {
  const color =
    tone === "healthy"
      ? "text-emerald-300"
      : tone === "attention"
        ? "text-amber-300"
        : tone === "critical"
          ? "text-rose-300"
          : "text-gray-100";
  return (
    <div className="rounded-xl border border-dark-600 bg-dark-800 p-4">
      <div className="flex items-center gap-2 text-[10px] font-bold uppercase tracking-[0.14em] text-gray-500">
        <Icon size={13} />
        {label}
      </div>
      <p className={`mt-2 text-2xl font-bold tabular-nums ${color}`}>{value}</p>
      <p className="mt-0.5 text-[11px] text-gray-500">{sub}</p>
    </div>
  );
}

export default function FleetDashboard() {
  const { config } = useConfig();
  const navigate = useNavigate();
  const [overview, setOverview] = useState<GuardianOverview | null>(null);
  const [topology, setTopology] = useState<GuardianTopologyResponse | null>(null);
  const [selectedHub, setSelectedHub] = useState<AssetGroup | null>(null);
  const [selectedHealth, setSelectedHealth] = useState<GuardianHealthResponse | null>(null);
  const [selectedEvents, setSelectedEvents] = useState<GuardianEventsResponse | null>(null);
  const [rangeMinutes, setRangeMinutes] = useState(60);
  const [partitionSearch, setPartitionSearch] = useState("");
  const [sortKey, setSortKey] = useState<SortKey>("health");
  const [sortOrder, setSortOrder] = useState<SortOrder>("asc");
  const [viewMode, setViewMode] = useState<"compact" | "detailed">("detailed");
  const [loading, setLoading] = useState(true);
  const [lastSync, setLastSync] = useState<Date | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [topologyError, setTopologyError] = useState<string | null>(null);
  const [helperManifest, setHelperManifest] = useState<HelperFileManifest | null>(null);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const tenant = config.default_tenant || "default";

  const loadFleet = useCallback(async () => {
    setLoading(true);
    setError(null);
    setTopologyError(null);
    const params = new URLSearchParams();
    params.set("tenant", tenant);

    const [overviewResult, topologyResult] = await Promise.allSettled([
      api.guardianOverview(),
      api.getGuardianTopology(params),
    ]);

    if (overviewResult.status === "fulfilled") {
      setOverview(overviewResult.value);
    } else {
      const message =
        overviewResult.reason instanceof Error ? overviewResult.reason.message : "Failed to load fleet data";
      setError(message);
    }

    if (topologyResult.status === "fulfilled") {
      setTopology(topologyResult.value);
    } else {
      setTopology(null);
      setTopologyError("Topology details are unavailable. Partition health still comes from Guardian.");
    }

    setLastSync(new Date());
    setLoading(false);
  }, [tenant]);

  useEffect(() => {
    void loadFleet();
    const interval = setInterval(() => void loadFleet(), 30_000);
    return () => clearInterval(interval);
  }, [loadFleet]);

  useEffect(() => {
    if (!selectedHub) return;
    const window = minutesLabel(rangeMinutes);

    const healthParams = new URLSearchParams();
    healthParams.set("tenant", tenant);
    healthParams.set("partition", selectedHub.partition);
    healthParams.set("window", window);

    const eventParams = new URLSearchParams();
    eventParams.set("tenant", tenant);
    eventParams.set("partition", selectedHub.partition);
    eventParams.set("window", window);
    eventParams.set("limit", "40");

    Promise.all([api.getGuardianHealth(healthParams), api.getGuardianEvents(eventParams)])
      .then(([healthData, eventData]) => {
        setSelectedHealth(healthData);
        setSelectedEvents(eventData);
      })
      .catch(() => {
        setSelectedHealth(null);
        setSelectedEvents(null);
      });
  }, [selectedHub, tenant, rangeMinutes]);

  const handleFileUpload = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setUploadError(null);
    const file = e.target.files?.[0];
    if (!file) return;

    if (!file.name.endsWith(".json")) {
      setUploadError("Only JSON files are supported");
      return;
    }

    const reader = new FileReader();
    reader.onload = (event) => {
      try {
        const json = JSON.parse(event.target?.result as string);
        if (!json.version || (!json.assets && !json.intents && !json.partitions)) {
          throw new Error("Invalid manifest format");
        }
        setHelperManifest(json as HelperFileManifest);
      } catch {
        setUploadError(
          "Failed to parse helper file. Expected JSON with version and asset, intent, or partition sections."
        );
      }
    };
    reader.onerror = () => setUploadError("Failed to read file");
    reader.readAsText(file);
    e.target.value = "";
  }, []);

  const handleSortChange = useCallback(
    (key: SortKey) => {
      if (sortKey === key) {
        setSortOrder((prev) => (prev === "asc" ? "desc" : "asc"));
      } else {
        setSortKey(key);
        setSortOrder("asc");
      }
    },
    [sortKey]
  );

  const handlePartitionSelect = useCallback(
    (partition: PartitionModel) => {
      navigate(`/partition/${encodeURIComponent(partition.id)}/assets`, {
        state: {
          partitionModel: partition,
          topology,
          helperManifest,
        },
      });
    },
    [navigate, topology, helperManifest]
  );

  const fleetPartitions = useMemo(
    () => buildFleetModel(overview, topology),
    [overview, topology]
  );

  const filteredPartitions = useMemo(() => {
    const needle = partitionSearch.trim().toLowerCase();
    if (!needle) return fleetPartitions;
    return fleetPartitions.filter((partition) => {
      const haystack = [
        partition.region,
        ...partition.intents.map((intent) => intent.name),
        ...partition.intents.flatMap((intent) => intent.assets.map((asset) => asset.name)),
      ]
        .join(" ")
        .toLowerCase();
      return haystack.includes(needle);
    });
  }, [fleetPartitions, partitionSearch]);

  const sortedPartitions = useMemo(() => {
    const sorted = [...filteredPartitions];
    sorted.sort((a, b) => {
      let cmp = 0;
      if (sortKey === "name") {
        cmp = a.region.localeCompare(b.region);
      } else if (sortKey === "health") {
        const byTone = toneRank(toneFor(a.summary.health)) - toneRank(toneFor(b.summary.health));
        if (byTone !== 0) {
          cmp = byTone;
        } else {
          cmp = b.summary.failingAssets - a.summary.failingAssets;
          if (cmp === 0) cmp = b.summary.attentionAssets - a.summary.attentionAssets;
        }
      } else {
        cmp = a.summary.assetCount - b.summary.assetCount;
      }
      return sortOrder === "desc" ? -cmp : cmp;
    });
    return sorted;
  }, [filteredPartitions, sortKey, sortOrder]);

  const summary = overview?.summary;
  const totalAssets = summary?.assets ?? fleetPartitions.reduce((total, p) => total + p.summary.assetCount, 0);
  const healthyAssets = summary?.healthyAssets ?? fleetPartitions.reduce((total, p) => total + p.summary.healthyAssets, 0);
  const attentionAssets =
    summary?.attentionAssets ?? fleetPartitions.reduce((total, p) => total + p.summary.attentionAssets, 0);
  const failingAssets =
    summary?.failingAssets ?? fleetPartitions.reduce((total, p) => total + p.summary.failingAssets, 0);
  const healthyPct = totalAssets > 0 ? Math.round((healthyAssets / totalAssets) * 100) : 0;
  const failingPartitions = fleetPartitions.filter(
    (p) => p.summary.failingAssets > 0 || toneFor(p.summary.health) === "critical"
  ).length;
  const attentionPartitions = fleetPartitions.filter(
    (p) => p.summary.attentionAssets > 0 || toneFor(p.summary.health) === "attention"
  ).length;
  const healthyPartitions = fleetPartitions.filter((p) => toneFor(p.summary.health) === "healthy").length;
  const overallTone: Tone =
    failingAssets > 0 || failingPartitions > 0
      ? "critical"
      : attentionAssets > 0 || attentionPartitions > 0
        ? "attention"
        : "healthy";
  const shownAssets = sortedPartitions.reduce((total, p) => total + p.summary.assetCount, 0);
  const syncedAt = lastSync ? lastSync.toLocaleTimeString() : "not yet synced";

  if (selectedHub) {
    return (
      <HubDetailView
        asset={selectedHub}
        health={selectedHealth}
        events={selectedEvents?.events || []}
        tenant={tenant}
        rangeMinutes={rangeMinutes}
        onRangeChange={setRangeMinutes}
        onBack={() => setSelectedHub(null)}
      />
    );
  }

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Fleet</h2>
          <p>
            {loading && !overview
              ? "Synchronizing partitions and topology…"
              : `${fleetPartitions.length} partitions · ${totalAssets} assets · ${healthyPct}% healthy · synced ${syncedAt}`}
          </p>
        </div>
        <div className="xp-toolbar">
          <div className="xp-presets" role="group" aria-label="Detail time window">
            {[15, 60, 360, 1440].map((minutes) => (
              <button
                key={minutes}
                type="button"
                className={"xp-preset" + (rangeMinutes === minutes ? " xp-active" : "")}
                onClick={() => setRangeMinutes(minutes)}
                title={`Use the last ${minutes < 60 ? `${minutes} minutes` : `${minutes / 60} hours`} when inspecting an asset`}
              >
                {minutesLabel(minutes)}
              </button>
            ))}
          </div>
          <label className="xp-live-btn relative cursor-pointer" title="Import a helper manifest">
            <Upload size={12} className="mr-1 inline" />
            Helper
            <input
              ref={fileInputRef}
              type="file"
              accept=".json"
              onChange={handleFileUpload}
              className="absolute inset-0 cursor-pointer opacity-0"
            />
          </label>
          <button
            type="button"
            className="xp-live-btn"
            onClick={() => void loadFleet()}
            disabled={loading}
            title="Reload fleet data"
          >
            <RefreshCw size={12} className={"mr-1 inline " + (loading ? "animate-spin" : "")} />
            {loading ? "Refreshing…" : "Refresh"}
          </button>
        </div>
      </div>

      <div className="flex flex-1 min-h-0 flex-col gap-5 overflow-y-auto p-5">
        {error && (
          <div className="flex flex-wrap items-center gap-3 rounded-xl border border-rose-500/30 bg-rose-500/10 px-4 py-3 text-sm text-rose-300">
            <AlertTriangle size={16} className="shrink-0" />
            <span className="min-w-0 flex-1">Could not load fleet data: {error}</span>
            <button
              type="button"
              className="rounded-md border border-rose-400/40 px-2.5 py-1 text-xs font-semibold hover:bg-rose-500/10"
              onClick={() => void loadFleet()}
            >
              Retry
            </button>
          </div>
        )}
        {topologyError && !error && (
          <div className="flex items-center gap-3 rounded-xl border border-amber-500/30 bg-amber-500/10 px-4 py-2.5 text-xs text-amber-300">
            <AlertTriangle size={14} className="shrink-0" />
            {topologyError}
          </div>
        )}

        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <KpiCard icon={Server} label="Partitions" value={fleetPartitions.length} sub={`${healthyPartitions} healthy`} />
          <KpiCard
            icon={Activity}
            label="Assets"
            value={totalAssets}
            sub={`${healthyPct}% healthy`}
            tone={healthyPct >= 99 ? "healthy" : healthyPct >= 90 ? "attention" : "critical"}
          />
          <KpiCard
            icon={AlertTriangle}
            label="Needs attention"
            value={attentionAssets}
            sub={`${attentionPartitions} partition${attentionPartitions === 1 ? "" : "s"} with drift`}
            tone={attentionAssets > 0 ? "attention" : "neutral"}
          />
          <KpiCard
            icon={AlertTriangle}
            label="Failing"
            value={failingAssets}
            sub={`${failingPartitions} partition${failingPartitions === 1 ? "" : "s"} failing`}
            tone={failingAssets > 0 ? "critical" : "healthy"}
          />
        </div>

        {overview && (
          <div
            className={
              "flex flex-wrap items-center gap-3 rounded-xl border px-4 py-3 text-sm " +
              (overallTone === "critical"
                ? "border-rose-500/30 bg-rose-500/10 text-rose-200"
                : overallTone === "attention"
                  ? "border-amber-500/30 bg-amber-500/10 text-amber-200"
                  : "border-emerald-500/30 bg-emerald-500/10 text-emerald-200")
            }
          >
            {overallTone === "healthy" ? <CheckCircle size={16} /> : <AlertTriangle size={16} />}
            <span className="font-medium">
              {overallTone === "healthy"
                ? "Fleet is healthy"
                : overallTone === "critical"
                  ? `${failingAssets} asset${failingAssets === 1 ? "" : "s"} failing across ${failingPartitions} partition${failingPartitions === 1 ? "" : "s"}`
                  : `${attentionAssets} asset${attentionAssets === 1 ? "" : "s"} need attention across ${attentionPartitions} partition${attentionPartitions === 1 ? "" : "s"}`}
            </span>
            <span className="text-xs opacity-75">
              {overallTone === "healthy"
                ? "No failing or drifted assets in the current snapshot."
                : "Open a partition below to inspect its intents and assets."}
            </span>
          </div>
        )}

        <div className="flex flex-wrap items-center gap-2 rounded-xl border border-dark-600 bg-dark-800 p-3">
          <div className="relative min-w-[220px] flex-1">
            <Search size={13} className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-500" />
            <input
              type="text"
              value={partitionSearch}
              onChange={(event) => setPartitionSearch(event.target.value)}
              placeholder="Search partitions, intents, or assets…"
              className="w-full rounded-lg border border-dark-600 bg-dark-900 py-1.5 pl-8 pr-3 text-xs text-gray-200 placeholder:text-gray-600 focus:border-accent-blue focus:outline-none focus:ring-1 focus:ring-accent-blue/40"
            />
          </div>

          <div className="flex items-center gap-1" role="group" aria-label="Sort partitions">
            {(["health", "name", "assets"] as SortKey[]).map((key) => (
              <button
                key={key}
                type="button"
                className={"xp-preset" + (sortKey === key ? " xp-active" : "")}
                onClick={() => handleSortChange(key)}
                title={`Sort by ${key}`}
              >
                {key === "name" ? "Name" : key === "health" ? "Health" : "Assets"}
                {sortKey === key && (sortOrder === "asc" ? " ↑" : " ↓")}
              </button>
            ))}
          </div>

          <div className="flex items-center gap-1 border-l border-dark-600 pl-2">
            <button
              type="button"
              onClick={() => setViewMode("compact")}
              className={"rounded-md p-1.5 " + (viewMode === "compact" ? "bg-accent-blue/15 text-sky-300" : "text-gray-600 hover:text-gray-300")}
              title="Compact view"
              aria-label="Compact view"
            >
              <Grid3X3 size={14} />
            </button>
            <button
              type="button"
              onClick={() => setViewMode("detailed")}
              className={"rounded-md p-1.5 " + (viewMode === "detailed" ? "bg-accent-blue/15 text-sky-300" : "text-gray-600 hover:text-gray-300")}
              title="Detailed view"
              aria-label="Detailed view"
            >
              <GripHorizontal size={14} />
            </button>
          </div>

          <span className="ml-auto text-[11px] text-gray-500">
            Showing {sortedPartitions.length}/{fleetPartitions.length} partitions · {shownAssets} assets
          </span>
        </div>

        {uploadError && (
          <div className="flex items-center gap-2 text-xs text-amber-300">
            <AlertTriangle size={13} /> {uploadError}
          </div>
        )}
        {helperManifest && (
          <div className="flex items-center gap-2 text-xs text-emerald-300">
            <CheckCircle size={13} /> Helper manifest loaded ({Object.keys(helperManifest.assets ?? {}).length} assets configured)
          </div>
        )}

        {loading && !overview ? (
          <div className="rounded-xl border border-dark-600 bg-dark-800 px-6 py-16 text-center text-sm text-gray-500">
            Loading fleet partitions…
          </div>
        ) : (
          <FleetView
            partitions={sortedPartitions}
            viewMode={viewMode}
            onSelectPartition={handlePartitionSelect}
            onSelectHub={setSelectedHub}
          />
        )}
      </div>
    </div>
  );
}
