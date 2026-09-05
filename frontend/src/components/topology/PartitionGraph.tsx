import FleetGraph from "./FleetGraph";
import type {
  GuardianTopologyEdge,
  GuardianTopologyNode,
} from "../../types/api";

interface PartitionGraphProps {
  nodes: GuardianTopologyNode[];
  edges: GuardianTopologyEdge[];
  selectedNodeId?: string;
  onSelectNode?: (node: GuardianTopologyNode) => void;
}

export default function PartitionGraph(props: PartitionGraphProps) {
  return <FleetGraph {...props} />;
}
