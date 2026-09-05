import type { GuardianTopologyNode } from "../../types/api";

export type TopologyNodeKind = "partition" | "intent" | "asset";

export function resolveNodeKind(node: GuardianTopologyNode): TopologyNodeKind {
  const raw = (node.type || "").toLowerCase();
  if (raw === "partition" || raw === "intent" || raw === "asset") return raw;

  const metaKind = (node.meta?.kind || node.meta?.node_kind || "").toLowerCase();
  if (metaKind === "partition" || metaKind === "intent" || metaKind === "asset") return metaKind;

  if (node.partition && !node.intent) return "partition";
  if (node.asset || node.meta?.asset || node.meta?.assetType || node.meta?.asset_type) return "asset";

  const id = node.id.toLowerCase();
  if (id.includes("asset")) return "asset";
  if (id.includes("intent")) return "intent";

  if (node.intent) return "intent";
  return "asset";
}
