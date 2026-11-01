import { describe, expect, it } from "vitest";
import type { LogRecord } from "../../types/api";
import { logIdentity, logService, splitServiceIdentity } from "../logs";

function record(overrides: Partial<LogRecord>): LogRecord {
  return { timestamp: "2026-01-01T00:00:00Z", body: "", severity_text: "", ...overrides } as LogRecord;
}

describe("logIdentity", () => {
  it("prefers typed identity over attributes", () => {
    const identity = logIdentity(
      record({
        service: "demo.web.api",
        partition: "demo",
        intent: "web",
        asset: "api",
        attributes: { partition: "other", intent: "other", asset: "other" },
      })
    );
    expect(identity).toEqual({ partition: "demo", intent: "web", asset: "api" });
  });

  it("falls back to resource attributes then point attributes", () => {
    expect(
      logIdentity(
        record({
          service: "demo.web.api",
          resource_attributes: { "guardian.partition": "edge", "guardian.intent": "gateway" },
        })
      )
    ).toEqual({ partition: "edge", intent: "gateway", asset: "" });

    expect(
      logIdentity(
        record({
          service: "demo.web.api",
          attributes: { "guardian_partition": "point", "guardian_asset": "pod-1" },
        })
      )
    ).toEqual({ partition: "point", intent: "", asset: "pod-1" });
  });
});

describe("logService", () => {
  it("uses the canonical service first", () => {
    expect(logService(record({ service: "demo.web.api", attributes: { "guardian.component": "legacy" } }))).toBe(
      "demo.web.api"
    );
    expect(logService(record({ service: "", attributes: { "guardian.component": "legacy" } }))).toBe("legacy");
  });
});

describe("splitServiceIdentity", () => {
  it("splits canonical three-part names", () => {
    expect(splitServiceIdentity("demo.web.api")).toEqual({
      partition: "demo",
      intent: "web",
      asset: "api",
    });
  });

  it("rejects aliases and malformed names", () => {
    expect(splitServiceIdentity("api")).toBeNull();
    expect(splitServiceIdentity("a..b")).toBeNull();
  });
});
