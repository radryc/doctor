import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Activity, AlertTriangle, Network, RefreshCw, Server } from "lucide-react";
import { api } from "../api/client";
import type { GuardianOverview, GuardianTopologyNode, GuardianTopologyResponse } from "../types/api";
import FleetGraph from "../components/topology/FleetGraph";
import NodeDetailsPanel from "../components/topology/NodeDetailsPanel";

function StatCard({
  icon: Icon,
  label,
  value,
}: {
  icon: React.ElementType;
  label: string;
  value: number;
}) {
  return (
    <div className="bg-dark-800 border border-dark-600 rounded-xl p-4">
      <div className="flex items-center gap-2 text-xs text-gray-500 uppercase tracking-wide">
        <Icon size={13} />
        {label}
      </div>
      <p className="mt-2 text-2xl font-bold text-gray-100">{value}</p>
    </div>
  );
}

export default function FleetViewPage() {
  const navigate = useNavigate();
  const [overview, setOverview] = useState<GuardianOverview | null>(null);
  const [topology, setTopology] = useState<GuardianTopologyResponse | null>(null);
  const [selectedNode, setSelectedNode] = useState<GuardianTopologyNode | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [refreshedAt, setRefreshedAt] = useState<Date>(new Date());

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams();
      params.set("tenant", "default");
      const [overviewData, topologyData] = await Promise.all([
        api.guardianOverview(),
        api.getGuardianTopology(params),
      ]);
      setOverview(overviewData);
      setTopology(topologyData);
      setRefreshedAt(new Date());
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to load fleet view");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, [load]);

  const summary = overview?.summary;
  const nodeCount = topology?.nodes.length ?? 0;
  const edgeCount = topology?.edges.length ?? 0;
  const critical = useMemo(
    () => topology?.nodes.filter((n) => (n.health?.level || "").toLowerCase() === "unhealthy").length ?? 0,
    [topology]
  );

  function openPartition() {
    if (!selectedNode?.partition) return;
    navigate(`/partition/${encodeURIComponent(selectedNode.partition)}`);
  }

  return (
    <div className="h-full flex flex-col overflow-hidden bg-dark-900">
      <header className="shrink-0 border-b border-dark-600 bg-[radial-gradient(circle_at_15%_20%,rgba(6,182,212,0.12),transparent_40%),radial-gradient(circle_at_85%_15%,rgba(59,130,246,0.16),transparent_35%)]">
        <div className="px-6 py-4 flex items-center justify-between gap-4">
          <div>
            <p className="text-[11px] uppercase tracking-[0.22em] text-accent-cyan/80 font-semibold">Topology</p>
            <h1 className="text-2xl font-bold text-gray-100 mt-1">Fleet View</h1>
            <p className="text-xs text-gray-500 mt-1">
              {loading ? "Synchronizing topology..." : `Synced at ${refreshedAt.toLocaleTimeString()}`}
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

      <div className="flex-1 overflow-y-auto p-6 space-y-5">
        {error && (
          <div className="rounded-lg bg-red-500/10 border border-red-500/30 px-4 py-3 text-sm text-red-400">
            {error}
          </div>
        )}

        <div className="grid grid-cols-1 md:grid-cols-5 gap-3">
          <StatCard icon={Server} label="Partitions" value={summary?.partitions ?? 0} />
          <StatCard icon={Activity} label="Assets" value={summary?.assets ?? 0} />
          <StatCard icon={AlertTriangle} label="Critical" value={critical} />
          <StatCard icon={Network} label="Nodes" value={nodeCount} />
          <StatCard icon={Network} label="Edges" value={edgeCount} />
        </div>

        <div className="grid grid-cols-1 xl:grid-cols-[1fr_320px] gap-4">
          <FleetGraph
            nodes={topology?.nodes ?? []}
            edges={topology?.edges ?? []}
            selectedNodeId={selectedNode?.id}
            onSelectNode={setSelectedNode}
          />

          <div className="space-y-3">
            <NodeDetailsPanel node={selectedNode} title="Selected Fleet Node" />
            <button
              className="w-full px-3 py-2 rounded-lg bg-dark-700 border border-dark-500 text-sm text-gray-200 disabled:opacity-40"
              disabled={!selectedNode?.partition}
              onClick={openPartition}
            >
              Open Partition View
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
