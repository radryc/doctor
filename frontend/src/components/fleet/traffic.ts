import type { Alert } from "../../types/alerts";
import type {
  GuardianTopologyEdge,
  GuardianTopologyNode,
  GuardianTopologyResponse,
  PrometheusVectorResult,
} from "../../types/api";

export interface TrafficNode {
  id: string;
  name: string;
  type: string;
  intent: string;
  status: "healthy" | "degraded" | "critical";
  x: number;
  y: number;
  rps: number;
  alertCount: number;
  criticalAlerts: number;
  degree: number;
  raw?: GuardianTopologyNode;
}

export interface TrafficEdge {
  from: string;
  to: string;
  relation?: string;
  rps: number;
  inferred: boolean;
}

export interface TrafficModel {
  nodes: TrafficNode[];
  edges: TrafficEdge[];
  focusId: string | null;
  maxRps: number;
  explicitEdges: boolean;
}

export function normalizeToken(value?: string): string {
  return (value || "").toLowerCase().replace(/[^a-z0-9]/g, "");
}

export function topologyNodeStatus(node: GuardianTopologyNode): "healthy" | "degraded" | "critical" {
  const text = [node.health?.level, node.status, node.flow_state].filter(Boolean).join(" ").toLowerCase();
  if (/critical|failed|failing|error|unhealthy|unavailable|down/.test(text)) return "critical";
  if (/degrad|attention|drift|pending|timeout/.test(text)) return "degraded";
  return "healthy";
}

function edgeKey(from: string, to: string): string {
  return `${from}->${to}`;
}

function layoutNodes(
  nodes: GuardianTopologyNode[],
  edges: GuardianTopologyEdge[],
  focusId: string
): Map<string, { x: number; y: number }> {
  const adjacency = new Map<string, Set<string>>();
  for (const node of nodes) adjacency.set(node.id, new Set());
  for (const edge of edges) {
    adjacency.get(edge.from)?.add(edge.to);
    adjacency.get(edge.to)?.add(edge.from);
  }
  const layers = new Map<string, number>();
  layers.set(focusId, 0);
  const queue = [focusId];
  while (queue.length > 0) {
    const current = queue.shift();
    if (!current) continue;
    const depth = layers.get(current) ?? 0;
    for (const next of adjacency.get(current) ?? []) {
      if (!layers.has(next)) {
        layers.set(next, depth + 1);
        queue.push(next);
      }
    }
  }
  let maxLayer = 0;
  for (const value of layers.values()) maxLayer = Math.max(maxLayer, value);
  for (const node of nodes) {
    if (!layers.has(node.id)) {
      maxLayer += 1;
      layers.set(node.id, maxLayer);
    }
  }

  const byLayer = new Map<number, string[]>();
  for (const [id, layer] of layers.entries()) {
    const list = byLayer.get(layer) ?? [];
    list.push(id);
    byLayer.set(layer, list);
  }

  const positions = new Map<string, { x: number; y: number }>();
  for (const [layer, ids] of byLayer.entries()) {
    if (layer === 0) {
      for (const id of ids) positions.set(id, { x: 50, y: 50 });
      continue;
    }
    const radius = Math.min(46, 13 + layer * 11);
    const offset = (layer % 2) * 0.7;
    ids.forEach((id, index) => {
      const angle = (index / Math.max(ids.length, 1)) * Math.PI * 2 + offset;
      positions.set(id, {
        x: 50 + radius * Math.cos(angle),
        y: 50 + radius * Math.sin(angle),
      });
    });
  }
  return positions;
}

export function buildTrafficModel(
  topology: GuardianTopologyResponse | null,
  focusNodeId: string,
  nodeRps: Map<string, number>,
  edgeRps: Map<string, number>,
  alerts: Alert[],
  explicitEdges: boolean
): TrafficModel {
  const topologyNodes = (topology?.nodes ?? []).filter(
    (node) => node.type === "asset" || node.type === "intent"
  );
  if (topologyNodes.length === 0) {
    return { nodes: [], edges: [], focusId: null, maxRps: 0, explicitEdges };
  }

  const nodeIds = new Set(topologyNodes.map((node) => node.id));
  const topologyEdges = (topology?.edges ?? []).filter(
    (edge) => nodeIds.has(edge.from) && nodeIds.has(edge.to)
  );

  const degree = new Map<string, number>();
  const adjacency = new Map<string, Set<string>>();
  for (const node of topologyNodes) {
    degree.set(node.id, 0);
    adjacency.set(node.id, new Set());
  }
  for (const edge of topologyEdges) {
    degree.set(edge.from, (degree.get(edge.from) ?? 0) + 1);
    degree.set(edge.to, (degree.get(edge.to) ?? 0) + 1);
    adjacency.get(edge.from)?.add(edge.to);
    adjacency.get(edge.to)?.add(edge.from);
  }

  let focus = focusNodeId;
  if (!nodeIds.has(focus)) {
    focus = [...topologyNodes].sort((a, b) => (degree.get(b.id) ?? 0) - (degree.get(a.id) ?? 0))[0]?.id ?? "";
  }
  if (!focus) return { nodes: [], edges: [], focusId: null, maxRps: 0, explicitEdges };

  // Keep the graph readable: focus + direct neighbours + the busiest remaining
  // nodes, capped at 90.
  const keep = new Set<string>([focus, ...(adjacency.get(focus) ?? [])]);
  const remaining = [...topologyNodes]
    .filter((node) => !keep.has(node.id))
    .sort((a, b) => {
      const ar = nodeRps.get(a.id) ?? 0;
      const br = nodeRps.get(b.id) ?? 0;
      if (ar !== br) return br - ar;
      return (degree.get(b.id) ?? 0) - (degree.get(a.id) ?? 0);
    });
  for (const node of remaining) {
    if (keep.size >= 90) break;
    keep.add(node.id);
  }
  const visibleNodes = topologyNodes.filter((node) => keep.has(node.id));
  const visibleEdges = topologyEdges.filter((edge) => keep.has(edge.from) && keep.has(edge.to));
  const positions = layoutNodes(visibleNodes, visibleEdges, focus);

  const alertMatches = matchAlerts(alerts, visibleNodes);
  let maxRps = 0;
  const nodes: TrafficNode[] = visibleNodes.map((node) => {
    const rps = nodeRps.get(node.id) ?? 0;
    maxRps = Math.max(maxRps, rps);
    const matched = alertMatches.get(node.id) ?? [];
    const position = positions.get(node.id) ?? { x: 50, y: 50 };
    return {
      id: node.id,
      name: node.name,
      type: node.type,
      intent: node.intent || "unknown",
      status: topologyNodeStatus(node),
      x: position.x,
      y: position.y,
      rps,
      alertCount: matched.length,
      criticalAlerts: matched.filter((alert) => alert.severity === "critical").length,
      degree: degree.get(node.id) ?? 0,
      raw: node,
    };
  });

  const edges: TrafficEdge[] = visibleEdges.map((edge) => {
    const direct = edgeRps.get(edgeKey(edge.from, edge.to));
    const reverse = edgeRps.get(edgeKey(edge.to, edge.from));
    const explicit = direct ?? reverse;
    const fromNode = nodeRps.get(edge.from) ?? 0;
    const rps = edge.relation === "contains" ? 0 : explicit ?? fromNode;
    return {
      from: edge.from,
      to: edge.to,
      relation: edge.relation,
      rps,
      inferred: edge.relation !== "contains" && explicit === undefined,
    };
  });

  return { nodes, edges, focusId: focus, maxRps, explicitEdges };
}

function matchAlerts(alerts: Alert[], nodes: GuardianTopologyNode[]): Map<string, Alert[]> {
  const out = new Map<string, Alert[]>();
  if (alerts.length === 0) return out;
  for (const alert of alerts) {
    if (alert.status === "resolved") continue;
    const values = [
      alert.labels.alertname,
      alert.labels.service,
      alert.labels.asset,
      alert.labels.intent,
      alert.labels.instance,
      alert.labels.job,
      alert.partition,
    ]
      .filter(Boolean)
      .map(normalizeToken)
      .filter((value) => value.length >= 2);
    if (values.length === 0) continue;
    let best: GuardianTopologyNode | null = null;
    let bestScore = 0;
    for (const node of nodes) {
      const candidates = [node.name, node.id, node.intent, node.asset].filter(Boolean).map(normalizeToken);
      let score = 0;
      for (const value of values) {
        for (const candidate of candidates) {
          if (value === candidate) score = Math.max(score, 4);
          else if (value.length >= 4 && (value.includes(candidate) || candidate.includes(value))) score = Math.max(score, 2);
        }
      }
      if (score > bestScore) {
        best = node;
        bestScore = score;
      }
    }
    if (best) {
      const list = out.get(best.id) ?? [];
      list.push(alert);
      out.set(best.id, list);
    }
  }
  return out;
}

// alertsMatchingNode returns the unresolved alerts that belong to one node.
export function alertsMatchingNode(alerts: Alert[], node: GuardianTopologyNode | undefined): Alert[] {
  if (!node) return [];
  const candidates = [node.name, node.id, node.intent, node.asset].filter(Boolean).map(normalizeToken);
  return alerts.filter((alert) => {
    if (alert.status === "resolved") return false;
    const values = [alert.labels.service, alert.labels.asset, alert.labels.intent, alert.labels.instance, alert.labels.job, alert.labels.alertname]
      .filter(Boolean)
      .map(normalizeToken);
    return values.some((value) =>
      candidates.some(
        (candidate) =>
          value === candidate ||
          (value.length >= 5 && candidate.length >= 5 && (value.includes(candidate) || candidate.includes(value)))
      )
    );
  });
}

function vectorValue(item: PrometheusVectorResult): number {
  const raw = item.value?.[1];
  const value = Number(raw);
  return Number.isFinite(value) ? value : 0;
}

function matchNode(nodes: GuardianTopologyNode[], labels: Record<string, string>): GuardianTopologyNode | null {
  const candidates = [
    labels.service,
    labels.service_name,
    labels.asset,
    labels.asset_name,
    labels.instance,
    labels.job,
    labels.app,
    labels.name,
  ]
    .filter(Boolean)
    .map(normalizeToken)
    .filter((value) => value.length >= 2);
  if (candidates.length === 0) return null;
  for (const node of nodes) {
    const nodeTokens = [node.name, node.id, node.intent, node.asset].filter(Boolean).map(normalizeToken);
    for (const candidate of candidates) {
      if (nodeTokens.some((token) => token === candidate)) return node;
      if (nodeTokens.some((token) => candidate.length >= 5 && token.length >= 5 && (candidate.includes(token) || token.includes(candidate)))) {
        return node;
      }
    }
  }
  return null;
}

export function nodeRpsFromVector(
  items: PrometheusVectorResult[],
  nodes: GuardianTopologyNode[]
): Map<string, number> {
  const out = new Map<string, number>();
  for (const item of items) {
    const node = matchNode(nodes, item.metric);
    if (!node) continue;
    out.set(node.id, (out.get(node.id) ?? 0) + vectorValue(item));
  }
  return out;
}

const EDGE_LABEL_PAIRS: Array<[string, string]> = [
  ["source_service", "destination_service"],
  ["source", "destination"],
  ["from", "to"],
  ["client", "server"],
  ["service", "peer_service"],
  ["service", "upstream_service"],
  ["service", "target_service"],
  ["caller", "callee"],
];

export function edgeRpsFromVector(
  items: PrometheusVectorResult[],
  nodes: GuardianTopologyNode[]
): Map<string, number> {
  const out = new Map<string, number>();
  for (const item of items) {
    let from: GuardianTopologyNode | null = null;
    let to: GuardianTopologyNode | null = null;
    for (const [fromLabel, toLabel] of EDGE_LABEL_PAIRS) {
      const fromValue = item.metric[fromLabel];
      const toValue = item.metric[toLabel];
      if (!fromValue || !toValue) continue;
      from = matchNode(nodes, { service: fromValue });
      to = matchNode(nodes, { service: toValue });
      if (from && to) break;
    }
    if (!from || !to || from.id === to.id) continue;
    const key = edgeKey(from.id, to.id);
    out.set(key, (out.get(key) ?? 0) + vectorValue(item));
  }
  return out;
}
