import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { RefreshCw, Upload } from "lucide-react";
import FleetView, { type AssetGroup, type HealthStatus, type PartitionModel, type SortKey, type SortOrder } from "./FleetView";
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

type SourceFilter = "all" | "asset" | "fallback";

function mapLevelToHealth(level?: string): HealthStatus {
  const normalized = (level || "").toLowerCase();
  if (normalized === "healthy") return "healthy";
  if (normalized === "degraded" || normalized === "attention") return "degraded";
  return "critical";
}

function makeNodeStrip(status: HealthStatus): HealthStatus[] {
  if (status === "healthy") return Array(16).fill("healthy");
  if (status === "degraded") {
    return [...Array(11).fill("healthy"), ...Array(4).fill("degraded"), "critical"];
  }
  return [...Array(10).fill("healthy"), ...Array(2).fill("degraded"), ...Array(4).fill("critical")];
}

function minutesToWindow(minutes: number): string {
  if (minutes % (60 * 24) === 0) return `${minutes / (60 * 24)}d`;
  if (minutes % 60 === 0) return `${minutes / 60}h`;
  return `${minutes}m`;
}

function rangeLabel(minutes: number): string {
  if (minutes < 60) return `${minutes}m`;
  if (minutes < 60 * 24) return `${minutes / 60}h`;
  return `${minutes / (60 * 24)}d`;
}

function buildFleetModel(
  overview: GuardianOverview | null,
  topology: GuardianTopologyResponse | null
): PartitionModel[] {
  if (!overview?.partitions?.length) return [];

  const partitionOrder = overview.partitions.map((p) => p.name);
  const partitionMap = new Map<string, PartitionModel>();

  for (const part of overview.partitions) {
    partitionMap.set(part.name, {
      id: part.name,
      region: part.name,
      intents: [],
    });
  }

  const topologyNodes = topology?.nodes || [];
  const intentsByPartition = new Map<string, Map<string, PartitionModel["intents"][number]>>();

  const intentNodes = topologyNodes.filter((n) => n.type === "intent");
  const assetNodes = topologyNodes.filter((n) => n.type === "asset");

  for (const node of intentNodes) {
    const partitionName = node.partition || "unknown";
    const intentName = node.name || node.intent || "Intent";
    if (!intentsByPartition.has(partitionName)) {
      intentsByPartition.set(partitionName, new Map());
    }
    const intents = intentsByPartition.get(partitionName);
    if (!intents) continue;
    if (!intents.has(intentName)) {
      intents.set(intentName, { name: intentName, assets: [] });
    }
  }

  for (const node of assetNodes) {
    const partitionName = node.partition || "unknown";
    const intentName = node.intent || "Services";
    if (!intentsByPartition.has(partitionName)) {
      intentsByPartition.set(partitionName, new Map());
    }
    const intents = intentsByPartition.get(partitionName);
    if (!intents) continue;
    if (!intents.has(intentName)) {
      intents.set(intentName, { name: intentName, assets: [] });
    }

    const status = mapLevelToHealth(node.health?.level);
    intents.get(intentName)?.assets.push({
      id: node.id,
      name: node.name,
      status,
      nodes: makeNodeStrip(status),
      partition: partitionName,
      intent: intentName,
      sourceScope: "asset",
    });
  }

  for (const partName of partitionOrder) {
    const model = partitionMap.get(partName);
    if (!model) continue;
    const intents = intentsByPartition.get(partName);
    if (intents && intents.size > 0) {
      model.intents = [...intents.values()].sort((a, b) => a.name.localeCompare(b.name));
    } else {
      const summary = overview.partitions.find((p) => p.name === partName);
      const status = mapLevelToHealth(summary?.health);
      model.intents = [
        {
          name: "Services",
          assets: [
            {
              id: `${partName}-summary`,
              name: partName,
              status,
              nodes: makeNodeStrip(status),
              partition: partName,
              intent: "Services",
              sourceScope: "fallback",
            },
          ],
        },
      ];
    }
  }

  return partitionOrder
    .map((name) => partitionMap.get(name))
    .filter((p): p is PartitionModel => Boolean(p));
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
  const [sourceFilter, setSourceFilter] = useState<SourceFilter>("all");
  const [sortKey, setSortKey] = useState<SortKey>("health");
  const [sortOrder, setSortOrder] = useState<SortOrder>("asc");
  const [viewMode, setViewMode] = useState<"compact" | "detailed">("detailed");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [helperManifest, setHelperManifest] = useState<HelperFileManifest | null>(null);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const tenant = config.default_tenant || "default";

  const loadFleet = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams();
      params.set("tenant", tenant);
      const [overviewData, topologyData] = await Promise.all([
        api.guardianOverview(),
        api.getGuardianTopology(params),
      ]);
      setOverview(overviewData);
      setTopology(topologyData);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to load fleet data");
    } finally {
      setLoading(false);
    }
  }, [tenant]);

  useEffect(() => {
    loadFleet();
    const interval = setInterval(loadFleet, 30_000);
    return () => clearInterval(interval);
  }, [loadFleet]);

  useEffect(() => {
    if (!selectedHub) return;
    const window = minutesToWindow(rangeMinutes);

    const healthParams = new URLSearchParams();
    healthParams.set("tenant", tenant);
    healthParams.set("partition", selectedHub.partition);
    healthParams.set("window", window);

    const eventParams = new URLSearchParams();
    eventParams.set("tenant", tenant);
    eventParams.set("partition", selectedHub.partition);
    eventParams.set("window", window);
    eventParams.set("limit", "40");

    Promise.all([
      api.getGuardianHealth(healthParams),
      api.getGuardianEvents(eventParams),
    ])
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
        setUploadError("Failed to parse helper file. Expected JSON with version and asset, intent, or partition sections.");
      }
    };
    reader.onerror = () => {
      setUploadError("Failed to read file");
    };
    reader.readAsText(file);
    e.target.value = "";
  }, []);

  const handleSortChange = useCallback(
    (key: SortKey) => {
      if (sortKey === key) {
        setSortOrder((prev) => (prev === "asc" ? "desc" : "asc"));
      } else {
        setSortKey(key);
        setSortOrder(key === "health" ? "asc" : "asc");
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
    return fleetPartitions
      .map((partition) => {
        const intents = partition.intents
          .map((intent) => {
            const assets = intent.assets.filter((asset) => {
              if (sourceFilter !== "all" && asset.sourceScope !== sourceFilter) {
                return false;
              }
              if (!needle) return true;
              const haystack = [
                partition.region,
                partition.id,
                intent.name,
                asset.name,
                asset.partition,
                asset.intent,
              ]
                .join(" ")
                .toLowerCase();
              return haystack.includes(needle);
            });
            return { ...intent, assets };
          })
          .filter((intent) => intent.assets.length > 0);
        return { ...partition, intents };
      })
      .filter((partition) => partition.intents.length > 0);
  }, [fleetPartitions, partitionSearch, sourceFilter]);

  const sortedPartitions = useMemo(() => {
    const sorted = [...filteredPartitions];
    sorted.sort((a, b) => {
      let cmp = 0;
      if (sortKey === "name") {
        cmp = a.region.localeCompare(b.region);
      } else if (sortKey === "health") {
        const allA = a.intents.flatMap((i) => i.assets);
        const allB = b.intents.flatMap((i) => i.assets);
        const pctA = allA.length > 0 ? allA.filter((x) => x.status === "healthy").length / allA.length : 0;
        const pctB = allB.length > 0 ? allB.filter((x) => x.status === "healthy").length / allB.length : 0;
        cmp = pctA - pctB;
      } else if (sortKey === "assets") {
        const countA = a.intents.reduce((s, i) => s + i.assets.length, 0);
        const countB = b.intents.reduce((s, i) => s + i.assets.length, 0);
        cmp = countA - countB;
      }
      return sortOrder === "desc" ? -cmp : cmp;
    });
    return sorted;
  }, [filteredPartitions, sortKey, sortOrder]);

  const criticalAssets = useMemo(
    () =>
      sortedPartitions
        .flatMap((p) => p.intents)
        .flatMap((i) => i.assets)
        .filter((a) => a.status === "critical").length,
    [sortedPartitions]
  );

  const aggregatedRps = useMemo(() => {
    const assets = sortedPartitions
      .flatMap((p) => p.intents)
      .flatMap((i) => i.assets).length;
    return Math.max(1200, assets * 55);
  }, [sortedPartitions]);

  const totalAssets = useMemo(
    () =>
      fleetPartitions
        .flatMap((p) => p.intents)
        .flatMap((i) => i.assets).length,
    [fleetPartitions]
  );

  const shownAssets = useMemo(
    () =>
      sortedPartitions
        .flatMap((p) => p.intents)
        .flatMap((i) => i.assets).length,
    [sortedPartitions]
  );

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
    <div className="h-full w-full overflow-auto bg-slate-950">
      <div className="px-6 pt-4 grid grid-cols-1 xl:grid-cols-[1fr_auto_auto_auto_auto_auto] gap-3 items-center">
        <div className="text-xs text-slate-400 min-w-0">
          {loading
            ? "Refreshing fleet topology..."
            : `Fleet topology ready - window ${rangeLabel(rangeMinutes)}`}
        </div>
        <div className="flex items-center gap-2">
          {[15, 60, 360, 1440].map((minutes) => (
            <button
              key={minutes}
              onClick={() => setRangeMinutes(minutes)}
              className={`px-2.5 py-1 rounded-md border text-xs ${
                rangeMinutes === minutes
                  ? "bg-sky-500/20 border-sky-400/60 text-sky-300"
                  : "bg-slate-900 border-slate-700 text-slate-300 hover:bg-slate-800"
              }`}
            >
              {rangeLabel(minutes)}
            </button>
          ))}
        </div>
        <div className="flex items-center gap-2 text-xs">
          <span className="text-slate-400">source</span>
          {(["all", "asset", "fallback"] as const).map((value) => (
            <button
              key={value}
              onClick={() => setSourceFilter(value)}
              className={`px-2 py-1 rounded border ${
                sourceFilter === value
                  ? "bg-sky-500/20 border-sky-400/60 text-sky-300"
                  : "bg-slate-900 border-slate-700 text-slate-300 hover:bg-slate-800"
              }`}
            >
              {value}
            </button>
          ))}
        </div>
        <input
          type="text"
          value={partitionSearch}
          onChange={(e) => setPartitionSearch(e.target.value)}
          placeholder="Search partition, intent, asset"
          className="w-full xl:w-64 rounded-lg border border-slate-700 bg-slate-900 px-3 py-1.5 text-xs text-slate-100 placeholder:text-slate-500 focus:outline-none focus:ring-1 focus:ring-sky-400"
        />
        <label className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-slate-900 border border-slate-700 text-xs text-slate-200 hover:bg-slate-800 cursor-pointer relative">
          <Upload size={13} />
          Helper
          <input
            ref={fileInputRef}
            type="file"
            accept=".json"
            onChange={handleFileUpload}
            className="absolute inset-0 opacity-0 cursor-pointer"
          />
        </label>
        <button
          onClick={loadFleet}
          className="flex items-center gap-2 px-3 py-1.5 rounded-lg bg-slate-900 border border-slate-700 text-xs text-slate-200 hover:bg-slate-800"
        >
          <RefreshCw size={13} />
          Refresh
        </button>
      </div>
      {uploadError && (
        <div className="px-6 pt-3 text-xs text-amber-400 flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full bg-amber-400" />
          {uploadError}
        </div>
      )}
      {helperManifest && (
        <div className="px-6 pt-1.5 text-xs text-emerald-400">
          Helper manifest loaded ({Object.keys(helperManifest.assets).length} assets configured)
        </div>
      )}
      {error && <div className="px-6 pt-3 text-xs text-rose-400">{error}</div>}
      <div className="px-6 pt-2 text-xs text-slate-500">
        Showing {shownAssets}/{totalAssets} assets across {sortedPartitions.length}/
        {fleetPartitions.length} partitions
      </div>
      <FleetView
        partitions={sortedPartitions}
        activePartitions={sortedPartitions.length}
        criticalAssets={criticalAssets}
        aggregatedRps={aggregatedRps}
        sortKey={sortKey}
        sortOrder={sortOrder}
        viewMode={viewMode}
        onSelectPartition={handlePartitionSelect}
        onSelectHub={setSelectedHub}
        onSortChange={handleSortChange}
        onViewModeChange={setViewMode}
      />
    </div>
  );
}
