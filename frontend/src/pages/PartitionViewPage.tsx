import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Activity, AlertTriangle, Clock3, RefreshCw } from "lucide-react";
import { api } from "../api/client";
import type {
  GuardianEventRecord,
  GuardianEventsResponse,
  GuardianHealthResponse,
  GuardianTopologyNode,
  GuardianTopologyResponse,
} from "../types/api";
import PartitionGraph from "../components/topology/PartitionGraph";
import NodeDetailsPanel from "../components/topology/NodeDetailsPanel";

function HealthCard({
  title,
  value,
  sub,
}: {
  title: string;
  value: string | number;
  sub?: string;
}) {
  return (
    <div className="bg-dark-800 border border-dark-600 rounded-xl p-4">
      <p className="text-xs uppercase tracking-wide text-gray-500">{title}</p>
      <p className="mt-2 text-xl font-bold text-gray-100">{value}</p>
      {sub && <p className="mt-1 text-xs text-gray-500">{sub}</p>}
    </div>
  );
}

function EventsTable({ events }: { events: GuardianEventRecord[] }) {
  if (events.length === 0) {
    return <div className="text-sm text-gray-500">No recent events.</div>;
  }

  return (
    <div className="overflow-x-auto">
      <table>
        <thead>
          <tr>
            <th>Time</th>
            <th>Kind</th>
            <th>Status</th>
            <th>Intent</th>
            <th>Asset</th>
            <th>Message</th>
          </tr>
        </thead>
        <tbody>
          {events.map((ev, idx) => (
            <tr key={`${ev.timestamp || "na"}-${ev.kind || "evt"}-${idx}`}>
              <td>{ev.timestamp ? new Date(ev.timestamp).toLocaleTimeString() : "-"}</td>
              <td>{ev.kind || "-"}</td>
              <td>{ev.status || "-"}</td>
              <td>{ev.intent || "-"}</td>
              <td>{ev.asset || "-"}</td>
              <td>{ev.message || "-"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export default function PartitionViewPage() {
  const params = useParams<{ name: string }>();
  const partitionName = params.name || "monofs";

  const [topology, setTopology] = useState<GuardianTopologyResponse | null>(null);
  const [health, setHealth] = useState<GuardianHealthResponse | null>(null);
  const [events, setEvents] = useState<GuardianEventsResponse | null>(null);
  const [selectedNode, setSelectedNode] = useState<GuardianTopologyNode | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [refreshedAt, setRefreshedAt] = useState<Date>(new Date());

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const topologyParams = new URLSearchParams();
      topologyParams.set("tenant", "default");
      topologyParams.set("partition", partitionName);

      const healthParams = new URLSearchParams();
      healthParams.set("tenant", "default");
      healthParams.set("partition", partitionName);
      healthParams.set("window", "1h");

      const eventsParams = new URLSearchParams();
      eventsParams.set("tenant", "default");
      eventsParams.set("partition", partitionName);
      eventsParams.set("window", "1h");
      eventsParams.set("limit", "30");

      const [topologyData, healthData, eventsData] = await Promise.all([
        api.getGuardianTopology(topologyParams),
        api.getGuardianHealth(healthParams),
        api.getGuardianEvents(eventsParams),
      ]);

      setTopology(topologyData);
      setHealth(healthData);
      setEvents(eventsData);
      setRefreshedAt(new Date());
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to load partition view");
    } finally {
      setLoading(false);
    }
  }, [partitionName]);

  useEffect(() => {
    load();
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, [load]);

  const sortedEvents = useMemo(() => {
    const rows = [...(events?.events || [])];
    rows.sort((a, b) => (b.timestamp || "").localeCompare(a.timestamp || ""));
    return rows;
  }, [events]);

  return (
    <div className="h-full flex flex-col overflow-hidden bg-dark-900">
      <header className="shrink-0 border-b border-dark-600 bg-[radial-gradient(circle_at_15%_20%,rgba(6,182,212,0.12),transparent_40%),radial-gradient(circle_at_85%_15%,rgba(59,130,246,0.16),transparent_35%)]">
        <div className="px-6 py-4 flex items-center justify-between gap-4">
          <div>
            <p className="text-[11px] uppercase tracking-[0.22em] text-accent-cyan/80 font-semibold">Topology</p>
            <h1 className="text-2xl font-bold text-gray-100 mt-1">Partition: {partitionName}</h1>
            <p className="text-xs text-gray-500 mt-1">
              {loading ? "Synchronizing partition state..." : `Synced at ${refreshedAt.toLocaleTimeString()}`}
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

        <div className="grid grid-cols-1 md:grid-cols-4 gap-3">
          <HealthCard title="Health Level" value={health?.overall_health?.level || "unknown"} />
          <HealthCard title="Health Score" value={typeof health?.overall_health?.score === "number" ? `${Math.round(health.overall_health.score * 100)}%` : "-"} />
          <HealthCard title="Recent Failures" value={health?.recent_failures ?? 0} />
          <HealthCard title="Recent Drifts" value={health?.recent_drifts ?? 0} />
        </div>

        <div className="grid grid-cols-1 xl:grid-cols-[1fr_320px] gap-4">
          <PartitionGraph
            nodes={topology?.nodes ?? []}
            edges={topology?.edges ?? []}
            selectedNodeId={selectedNode?.id}
            onSelectNode={setSelectedNode}
          />

          <div className="space-y-3">
            <NodeDetailsPanel node={selectedNode} title="Selected Partition Node" />
            <div className="glass rounded-xl border border-dark-600 p-3">
              <p className="text-xs text-gray-400 flex items-center gap-2"><Clock3 size={12} /> Quick links</p>
              <div className="mt-2 grid grid-cols-1 gap-2 text-sm">
                <Link className="px-3 py-2 rounded bg-dark-700 border border-dark-500 text-gray-200" to="/logs">
                  Open Logs
                </Link>
                <Link className="px-3 py-2 rounded bg-dark-700 border border-dark-500 text-gray-200" to="/traces">
                  Open Traces
                </Link>
                <Link className="px-3 py-2 rounded bg-dark-700 border border-dark-500 text-gray-200" to="/metrics">
                  Open Metrics
                </Link>
              </div>
            </div>
          </div>
        </div>

        <section className="glass rounded-xl border border-dark-600 p-4">
          <h2 className="text-sm font-semibold text-gray-200 flex items-center gap-2">
            <Activity size={14} className="text-accent-cyan" />
            Recent Events (1h)
          </h2>
          <div className="mt-3">
            <EventsTable events={sortedEvents} />
          </div>
          {health?.overall_health?.reason && (
            <p className="mt-3 text-xs text-amber-300 flex items-center gap-2">
              <AlertTriangle size={12} />
              {health.overall_health.reason}
            </p>
          )}
        </section>
      </div>
    </div>
  );
}
