import { describe, expect, it } from "vitest";
import { parseGraphFile, serialiseGraphBundle } from "../graphBundle";

const bundle = {
  version: 1,
  exportedAt: "2026-11-05T00:00:00.000Z",
  savedGraphs: [
    {
      id: "graph-1",
      title: "Latency",
      query: "histogram_quantile(0.95, rate(http_request_duration_bucket[5m]))",
      rangeMinutes: 60,
      refreshIntervalSec: 30,
      createdAt: "2026-11-05T00:00:00.000Z",
      partitions: ["agent"],
    },
  ],
  savedDashboards: [
    {
      id: "dash-1",
      name: "Ops",
      graphs: [
        {
          title: "Errors",
          query: "sum(rate(http_requests_total{status=~\"5..\"}[5m]))",
          rangeMinutes: 15,
          refreshIntervalSec: 0,
          partitions: ["agent"],
        },
      ],
      createdAt: "2026-11-05T00:00:00.000Z",
      updatedAt: "2026-11-05T00:00:00.000Z",
      partitions: ["agent"],
    },
  ],
};

describe("parseGraphFile", () => {
  it("parses a JSON bundle", () => {
    const parsed = parseGraphFile(JSON.stringify(bundle), "bundle.json");
    expect(parsed.graphs).toHaveLength(1);
    expect(parsed.graphs[0].title).toBe("Latency");
    expect(parsed.dashboards).toHaveLength(1);
    expect(parsed.dashboards[0].graphs[0].query).toContain("http_requests_total");
  });

  it("parses a YAML bundle, including range aliases", () => {
    const parsed = parseGraphFile(
      `version: 1
exportedAt: 2026-11-05T00:00:00Z
savedGraphs:
  - title: CPU
    expr: sum(rate(cpu_seconds_total[5m]))
    range_minutes: 15
    partitions: [agent]
savedDashboards: []
`,
      "bundle.yaml"
    );

    expect(parsed.graphs).toEqual([
      expect.objectContaining({
        title: "CPU",
        query: "sum(rate(cpu_seconds_total[5m]))",
        rangeMinutes: 15,
        refreshIntervalSec: 0,
        partitions: ["agent"],
      }),
    ]);
    expect(parsed.dashboards).toHaveLength(0);
  });

  it("parses a single graph object", () => {
    const parsed = parseGraphFile(
      `title: Throughput
query: sum(rate(requests_total[5m]))
rangeMinutes: 30
`,
      "graph.yaml"
    );
    expect(parsed.graphs).toHaveLength(1);
    expect(parsed.graphs[0].rangeMinutes).toBe(30);
  });

  it("parses an array of graphs", () => {
    const parsed = parseGraphFile(
      JSON.stringify([
        { name: "A", expression: "up" },
        { name: "B", promql: "up == 0" },
        { name: "ignored", query: "" },
      ]),
      "graphs.json"
    );
    expect(parsed.graphs.map((graph) => graph.title)).toEqual(["A", "B"]);
  });

  it("rejects files without graph definitions", () => {
    expect(() => parseGraphFile("{}", "empty.json")).toThrow(/No graph definitions/);
    expect(() => parseGraphFile("", "empty.yaml")).toThrow(/empty/);
  });
});

describe("serialiseGraphBundle", () => {
  it("round-trips JSON and YAML exports", () => {
    const json = serialiseGraphBundle(bundle, "json");
    expect(json.extension).toBe("json");
    expect(JSON.parse(json.content).savedGraphs).toHaveLength(1);

    const yaml = serialiseGraphBundle(bundle, "yaml");
    expect(yaml.extension).toBe("yaml");
    expect(parseGraphFile(yaml.content, "bundle.yaml").graphs[0].title).toBe("Latency");
  });
});
