import { describe, expect, it } from "vitest";
import { buildTrafficModel, edgeRpsFromVector, nodeRpsFromVector } from "./traffic";
import type { Alert } from "../../types/alerts";
import type { GuardianTopologyResponse, PrometheusVectorResult } from "../../types/api";

const topology: GuardianTopologyResponse = {
  nodes: [
    { id: "intent-web", type: "intent", name: "web", intent: "web", partition: "payments", status: "healthy" },
    { id: "asset-checkout", type: "asset", name: "checkout", intent: "web", partition: "payments", status: "healthy", health: { level: "healthy" } },
    { id: "asset-payments", type: "asset", name: "payments", intent: "web", partition: "payments", status: "degraded", health: { level: "degraded" } },
  ],
  edges: [
    { from: "intent-web", to: "asset-checkout", relation: "contains" },
    { from: "asset-checkout", to: "asset-payments", relation: "depends_on" },
  ],
};

function vector(labels: Record<string, string>, value: number): PrometheusVectorResult {
  return { metric: labels, value: [0, String(value)] };
}

describe("traffic model", () => {
  it("maps service RPS onto topology nodes", () => {
    const nodes = topology.nodes;
    const result = nodeRpsFromVector(
      [
        vector({ service: "checkout" }, 12.5),
        vector({ service: "payments-api" }, 4),
      ],
      nodes
    );
    expect(result.get("asset-checkout")).toBe(12.5);
    expect(result.get("asset-payments")).toBe(4);
  });

  it("maps explicit source/destination labels onto edges", () => {
    const nodes = topology.nodes;
    const result = edgeRpsFromVector(
      [vector({ source_service: "checkout", destination_service: "payments" }, 7.5)],
      nodes
    );
    expect(result.get("asset-checkout->asset-payments")).toBe(7.5);
  });

  it("builds a focused graph with alert badges and inferred edges", () => {
    const alert: Alert = {
      id: "a1",
      fingerprint: "a1",
      source: "sentinel",
      tenant: "default",
      partition: "payments",
      status: "firing",
      severity: "critical",
      labels: { alertname: "CheckoutErrors", service: "checkout" },
      startsAt: new Date().toISOString(),
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      delivery: "sentinel",
    };
    const model = buildTrafficModel(
      topology,
      "asset-checkout",
      new Map([["asset-checkout", 12.5], ["asset-payments", 4]]),
      new Map(),
      [alert],
      false
    );
    expect(model.focusId).toBe("asset-checkout");
    expect(model.nodes.find((node) => node.id === "asset-checkout")?.alertCount).toBe(1);
    const edge = model.edges.find((item) => item.relation === "depends_on");
    expect(edge?.rps).toBe(12.5);
    expect(edge?.inferred).toBe(true);
  });
});
