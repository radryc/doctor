import { describe, expect, it } from "vitest";
import {
  assignSourceRoute,
  isSourceRoute,
  sourceRouteDestination,
} from "../routing";

describe("source alert routes", () => {
  it("recognises an exact source + alertname route", () => {
    expect(
      isSourceRoute(
        {
          name: "guardian-attention",
          match: [
            { name: "source", operator: "=", value: "guardian" },
            { name: "alertname", operator: "=", value: "GuardianPartitionAttention" },
          ],
          receivers: ["pd"],
        },
        "guardian",
        "GuardianPartitionAttention"
      )
    ).toBe(true);
  });

  it("does not treat a broader route as an explicit source route", () => {
    expect(
      isSourceRoute(
        {
          name: "critical",
          match: [{ name: "severity", operator: "=", value: "critical" }],
          receivers: ["pd"],
        },
        "guardian",
        "GuardianPartitionAttention"
      )
    ).toBe(false);
  });

  it("supports the legacy matchers map", () => {
    expect(
      isSourceRoute(
        {
          name: "legacy",
          matchers: { source: "guardian", alertname: "GuardianPartitionError" },
          receivers: ["pd"],
        },
        "guardian",
        "GuardianPartitionError"
      )
    ).toBe(true);
  });

  it("assigns and replaces a source alert destination", () => {
    const first = assignSourceRoute([], "guardian", "GuardianPartitionAttention", "slack");
    expect(sourceRouteDestination(first, "guardian", "GuardianPartitionAttention")).toBe("slack");
    const second = assignSourceRoute(
      first,
      "guardian",
      "GuardianPartitionAttention",
      "pagerduty"
    );
    expect(second).toHaveLength(1);
    expect(sourceRouteDestination(second, "guardian", "GuardianPartitionAttention")).toBe(
      "pagerduty"
    );
  });

  it("removes the explicit route when the destination is cleared", () => {
    const routes = assignSourceRoute([], "guardian", "GuardianPartitionAttention", "slack");
    const cleared = assignSourceRoute(
      routes,
      "guardian",
      "GuardianPartitionAttention",
      null
    );
    expect(cleared).toEqual([]);
    expect(sourceRouteDestination(cleared, "guardian", "GuardianPartitionAttention")).toBe("");
  });

  it("leaves other source alert routes untouched", () => {
    const routes = assignSourceRoute([], "guardian", "GuardianPartitionError", "pd");
    const next = assignSourceRoute(
      routes,
      "guardian",
      "GuardianPartitionAttention",
      "slack"
    );
    expect(next).toHaveLength(2);
    expect(sourceRouteDestination(next, "guardian", "GuardianPartitionError")).toBe("pd");
    expect(sourceRouteDestination(next, "guardian", "GuardianPartitionAttention")).toBe(
      "slack"
    );
  });
});
