import type { GuardianTopologyNode } from "../../types/api";

function healthColor(level?: string): string {
  const normalized = (level || "").toLowerCase();
  if (normalized === "healthy") return "#22c55e";
  if (normalized === "degraded" || normalized === "attention") return "#f59e0b";
  if (normalized === "unhealthy" || normalized === "failing" || normalized === "failed") return "#ef4444";
  return "#64748b";
}

interface NodeDetailsPanelProps {
  node: GuardianTopologyNode | null;
  title?: string;
}

export default function NodeDetailsPanel({
  node,
  title = "Node Details",
}: NodeDetailsPanelProps) {
  return (
    <aside className="glass rounded-xl border border-dark-600 p-4">
      <h3 className="text-sm font-semibold text-gray-200">{title}</h3>
      {!node ? (
        <p className="text-xs text-gray-500 mt-3">Select a node to inspect status, health, and metadata.</p>
      ) : (
        <div className="mt-3 space-y-3">
          <div>
            <p className="text-[11px] uppercase tracking-wide text-gray-500">Name</p>
            <p className="text-sm font-medium text-gray-100 mt-1">{node.name}</p>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <p className="text-[11px] uppercase tracking-wide text-gray-500">Type</p>
              <p className="text-sm text-gray-200 mt-1">{node.type}</p>
            </div>
            <div>
              <p className="text-[11px] uppercase tracking-wide text-gray-500">Status</p>
              <p className="text-sm text-gray-200 mt-1">{node.status || "unknown"}</p>
            </div>
          </div>
          <div>
            <p className="text-[11px] uppercase tracking-wide text-gray-500">Health</p>
            <div className="mt-1 inline-flex items-center gap-2 rounded-full border border-dark-500 px-2 py-1 text-xs"
              style={{ color: healthColor(node.health?.level) }}>
              <span
                className="inline-block h-2 w-2 rounded-full"
                style={{ backgroundColor: healthColor(node.health?.level) }}
              />
              {node.health?.level || "unknown"}
              {typeof node.health?.score === "number" && (
                <span className="text-gray-400">({Math.round(node.health.score * 100)}%)</span>
              )}
            </div>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <p className="text-[11px] uppercase tracking-wide text-gray-500">Partition</p>
              <p className="text-sm text-gray-200 mt-1">{node.partition || "-"}</p>
            </div>
            <div>
              <p className="text-[11px] uppercase tracking-wide text-gray-500">Intent</p>
              <p className="text-sm text-gray-200 mt-1">{node.intent || "-"}</p>
            </div>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <p className="text-[11px] uppercase tracking-wide text-gray-500">Flow State</p>
              <p className="text-sm text-gray-200 mt-1">{node.flow_state || "-"}</p>
            </div>
            <div>
              <p className="text-[11px] uppercase tracking-wide text-gray-500">Flow Source</p>
              <p className="text-sm text-gray-200 mt-1">{node.flow_source || "-"}</p>
            </div>
          </div>
          {node.meta && Object.keys(node.meta).length > 0 && (
            <div>
              <p className="text-[11px] uppercase tracking-wide text-gray-500">Metadata</p>
              <pre className="mt-1 text-xs">{JSON.stringify(node.meta, null, 2)}</pre>
            </div>
          )}
        </div>
      )}
    </aside>
  );
}
