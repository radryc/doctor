import { useCallback, useEffect, useState } from "react";
import { api } from "../../api/client";
import type { Alert } from "../../types/alerts";
import type {
  GuardianEventRecord,
  GuardianTopologyNode,
  GuardianTopologyResponse,
  PrometheusVectorResult,
} from "../../types/api";
import { edgeRpsFromVector, nodeRpsFromVector } from "./traffic";

export interface PartitionFleetData {
  topology: GuardianTopologyResponse | null;
  alerts: Alert[];
  events: GuardianEventRecord[];
  nodeRps: Map<string, number>;
  edgeRps: Map<string, number>;
  explicitEdges: boolean;
  loading: boolean;
  error: string;
  lastSync: Date | null;
  refresh: () => Promise<void>;
}

function escapePromQL(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

function windowLabel(minutes: number): string {
  if (minutes < 60) return `${minutes}m`;
  if (minutes < 60 * 24) return `${minutes / 60}h`;
  return `${minutes / (60 * 24)}d`;
}

async function instantVector(query: string, tenant: string): Promise<PrometheusVectorResult[]> {
  try {
    const response = await api.queryPrometheus(new URLSearchParams({ query, tenant }));
    if (response.status !== "success" || response.data.resultType !== "vector") return [];
    return response.data.result;
  } catch {
    return [];
  }
}

async function firstNonEmptyVector(queries: string[], tenant: string): Promise<PrometheusVectorResult[]> {
  for (const query of queries) {
    const result = await instantVector(query, tenant);
    if (result.length > 0) return result;
  }
  return [];
}

export function usePartitionFleet(
  tenant: string,
  partition: string,
  rangeMinutes: number
): PartitionFleetData {
  const [topology, setTopology] = useState<GuardianTopologyResponse | null>(null);
  const [alerts, setAlerts] = useState<Alert[]>([]);
  const [events, setEvents] = useState<GuardianEventRecord[]>([]);
  const [nodeRps, setNodeRps] = useState<Map<string, number>>(new Map());
  const [edgeRps, setEdgeRps] = useState<Map<string, number>>(new Map());
  const [explicitEdges, setExplicitEdges] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [lastSync, setLastSync] = useState<Date | null>(null);

  const load = useCallback(async () => {
    if (!partition) return;
    setLoading(true);
    setError("");
    const [topologyResult, alertResult, eventResult] = await Promise.allSettled([
      api.getGuardianTopology(new URLSearchParams({ tenant, partition })),
      api.getAlerts(new URLSearchParams({ tenant, partition })),
      api.getGuardianEvents(
        new URLSearchParams({ tenant, partition, window: windowLabel(rangeMinutes), limit: "80" })
      ),
    ]);
    if (topologyResult.status === "fulfilled") {
      setTopology(topologyResult.value);
    } else {
      setTopology(null);
      setError(topologyResult.reason instanceof Error ? topologyResult.reason.message : "topology unavailable");
    }
    if (alertResult.status === "fulfilled") setAlerts(alertResult.value.alerts ?? []);
    else setAlerts([]);
    if (eventResult.status === "fulfilled") setEvents(eventResult.value.events ?? []);
    else setEvents([]);

    const nodes: GuardianTopologyNode[] = (topologyResult.status === "fulfilled" ? topologyResult.value.nodes : [])
      .filter((node) => node.type === "asset" || node.type === "intent");
    const escaped = escapePromQL(partition);
    const nodeQueries = [
      `sum by (service) (rate(http_server_request_duration_count{guardian_partition="${escaped}"}[5m]))`,
      `sum by (service) (rate(http_requests_total{guardian_partition="${escaped}"}[5m]))`,
      `sum by (service) (rate(http_server_requests_seconds_count{guardian_partition="${escaped}"}[5m]))`,
      `sum by (service) (rate(http_server_request_duration_count[5m]))`,
      `sum by (service) (rate(http_requests_total[5m]))`,
    ];
    const edgeQueries = [
      `sum by (source_service, destination_service) (rate(http_requests_total{guardian_partition="${escaped}"}[5m]))`,
      `sum by (source_service, destination_service) (rate(http_server_request_duration_count{guardian_partition="${escaped}"}[5m]))`,
      `sum by (source, destination) (rate(http_requests_total{guardian_partition="${escaped}"}[5m]))`,
      `sum by (client, server) (rate(http_requests_total{guardian_partition="${escaped}"}[5m]))`,
      `sum by (service, peer_service) (rate(http_requests_total{guardian_partition="${escaped}"}[5m]))`,
      `sum by (service, upstream_service) (rate(http_requests_total{guardian_partition="${escaped}"}[5m]))`,
      `sum by (caller, callee) (rate(http_requests_total{guardian_partition="${escaped}"}[5m]))`,
    ];
    const [nodeItems, edgeItems] = await Promise.all([
      firstNonEmptyVector(nodeQueries, tenant),
      firstNonEmptyVector(edgeQueries, tenant),
    ]);
    setNodeRps(nodeRpsFromVector(nodeItems, nodes));
    const edgeValues = edgeRpsFromVector(edgeItems, nodes);
    setEdgeRps(edgeValues);
    setExplicitEdges(edgeItems.length > 0 && edgeValues.size > 0);
    setLastSync(new Date());
    setLoading(false);
  }, [tenant, partition, rangeMinutes]);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 30_000);
    return () => window.clearInterval(timer);
  }, [load]);

  return {
    topology,
    alerts,
    events,
    nodeRps,
    edgeRps,
    explicitEdges,
    loading,
    error,
    lastSync,
    refresh: load,
  };
}
