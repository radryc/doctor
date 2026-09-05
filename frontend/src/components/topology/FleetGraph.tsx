import { useMemo, useState } from "react";
import type {
  GuardianTopologyEdge,
  GuardianTopologyNode,
} from "../../types/api";
import { resolveNodeKind } from "./nodeKind";

interface Point {
  x: number;
  y: number;
}

interface FleetGraphProps {
  nodes: GuardianTopologyNode[];
  edges: GuardianTopologyEdge[];
  selectedNodeId?: string;
  onSelectNode?: (node: GuardianTopologyNode) => void;
}

interface LayoutNode extends GuardianTopologyNode {
  pos: Point;
}

function nodeKind(node: GuardianTopologyNode): "partition" | "intent" | "asset" {
  return resolveNodeKind(node);
}

function healthColor(level?: string): string {
  const normalized = (level || "").toLowerCase();
  if (normalized === "healthy") return "#22c55e";
  if (normalized === "degraded" || normalized === "attention") return "#f59e0b";
  if (normalized === "unhealthy" || normalized === "failing" || normalized === "failed") return "#ef4444";
  return "#64748b";
}

function groupKey(node: GuardianTopologyNode): string {
  return node.partition || (nodeKind(node) === "partition" ? node.name : "unassigned");
}

function buildLayout(nodes: GuardianTopologyNode[]): { laidOut: LayoutNode[]; width: number; height: number } {
  const groups = new Map<string, GuardianTopologyNode[]>();
  for (const node of nodes) {
    const key = groupKey(node);
    const current = groups.get(key);
    if (current) {
      current.push(node);
    } else {
      groups.set(key, [node]);
    }
  }

  const sortedGroups = [...groups.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  const laidOut: LayoutNode[] = [];
  const colWidth = 360;
  const startX = 80;
  const startY = 60;

  sortedGroups.forEach(([_, members], index) => {
    const baseX = startX + index * colWidth;
    const partitions = members.filter((n) => nodeKind(n) === "partition");
    const intents = members.filter((n) => nodeKind(n) === "intent");
    const assets = members.filter((n) => nodeKind(n) === "asset");

    partitions.forEach((node, i) => {
      laidOut.push({ ...node, pos: { x: baseX + i * 220, y: startY } });
    });

    intents.forEach((node, i) => {
      laidOut.push({ ...node, pos: { x: baseX + (i % 2) * 170, y: startY + 150 + Math.floor(i / 2) * 90 } });
    });

    assets.forEach((node, i) => {
      laidOut.push({ ...node, pos: { x: baseX + (i % 2) * 170, y: startY + 340 + Math.floor(i / 2) * 80 } });
    });
  });

  const width = Math.max(1300, sortedGroups.length * colWidth + 220);
  const maxY = laidOut.reduce((acc, n) => Math.max(acc, n.pos.y), 0);
  const height = Math.max(720, maxY + 140);
  return { laidOut, width, height };
}

export default function FleetGraph({
  nodes,
  edges,
  selectedNodeId,
  onSelectNode,
}: FleetGraphProps) {
  const [zoom, setZoom] = useState(1);
  const [offset, setOffset] = useState<Point>({ x: 0, y: 0 });
  const [dragStart, setDragStart] = useState<Point | null>(null);
  const [isDragging, setIsDragging] = useState(false);

  const { laidOut, width, height } = useMemo(() => buildLayout(nodes), [nodes]);
  const byId = useMemo(() => new Map(laidOut.map((n) => [n.id, n])), [laidOut]);

  function onWheel(event: React.WheelEvent<SVGSVGElement>) {
    event.preventDefault();
    const direction = event.deltaY > 0 ? -0.08 : 0.08;
    setZoom((prev) => Math.min(2.6, Math.max(0.45, prev + direction)));
  }

  function onMouseDown(event: React.MouseEvent<SVGSVGElement>) {
    setDragStart({ x: event.clientX - offset.x, y: event.clientY - offset.y });
    setIsDragging(true);
  }

  function onMouseMove(event: React.MouseEvent<SVGSVGElement>) {
    if (!isDragging || !dragStart) return;
    setOffset({ x: event.clientX - dragStart.x, y: event.clientY - dragStart.y });
  }

  function stopDrag() {
    setIsDragging(false);
    setDragStart(null);
  }

  return (
    <div className="glass rounded-xl border border-dark-600 overflow-hidden">
      <div className="px-3 py-2 border-b border-dark-600 flex items-center justify-between text-xs text-gray-400">
        <span>Fleet Topology</span>
        <div className="flex items-center gap-2">
          <button className="px-2 py-1 rounded bg-dark-700 hover:bg-dark-600" onClick={() => setZoom((z) => Math.min(2.6, z + 0.1))}>+</button>
          <button className="px-2 py-1 rounded bg-dark-700 hover:bg-dark-600" onClick={() => setZoom((z) => Math.max(0.45, z - 0.1))}>-</button>
          <button className="px-2 py-1 rounded bg-dark-700 hover:bg-dark-600" onClick={() => { setZoom(1); setOffset({ x: 0, y: 0 }); }}>Reset</button>
        </div>
      </div>

      <svg
        width="100%"
        height={680}
        viewBox={`0 0 ${width} ${height}`}
        className="bg-dark-900"
        onWheel={onWheel}
        onMouseDown={onMouseDown}
        onMouseMove={onMouseMove}
        onMouseUp={stopDrag}
        onMouseLeave={stopDrag}
      >
        <defs>
          <pattern id="fleet-grid" width="40" height="40" patternUnits="userSpaceOnUse">
            <path d="M 40 0 L 0 0 0 40" fill="none" stroke="rgba(148,163,184,0.08)" strokeWidth="1" />
          </pattern>
        </defs>
        <rect width={width} height={height} fill="url(#fleet-grid)" />

        <g transform={`translate(${offset.x} ${offset.y}) scale(${zoom})`}>
          {edges.map((edge, idx) => {
            const from = byId.get(edge.from);
            const to = byId.get(edge.to);
            if (!from || !to) return null;
            return (
              <line
                key={`${edge.from}-${edge.to}-${idx}`}
                x1={from.pos.x + 65}
                y1={from.pos.y + 22}
                x2={to.pos.x + 65}
                y2={to.pos.y + 22}
                stroke="rgba(56,189,248,0.35)"
                strokeWidth={1.5}
              />
            );
          })}

          {laidOut.map((node) => {
            const kind = nodeKind(node);
            const fill = kind === "partition" ? "#0b2239" : kind === "intent" ? "#1a2e4b" : "#111827";
            const stroke = node.id === selectedNodeId ? "#38bdf8" : "rgba(148,163,184,0.35)";
            const accent = healthColor(node.health?.level);
            return (
              <g key={node.id} transform={`translate(${node.pos.x} ${node.pos.y})`} onClick={() => onSelectNode?.(node)} style={{ cursor: "pointer" }}>
                <rect x={0} y={0} rx={8} ry={8} width={130} height={44} fill={fill} stroke={stroke} strokeWidth={node.id === selectedNodeId ? 2 : 1} />
                <circle cx={10} cy={22} r={4} fill={accent} />
                <text x={20} y={18} fill="#cbd5e1" fontSize={10}>{kind.toUpperCase()}</text>
                <text x={20} y={33} fill="#f8fafc" fontSize={11}>
                  {node.name.length > 16 ? `${node.name.slice(0, 14)}..` : node.name}
                </text>
              </g>
            );
          })}
        </g>
      </svg>
    </div>
  );
}
