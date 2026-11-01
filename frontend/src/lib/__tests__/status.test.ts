import { describe, expect, it } from "vitest";
import { statusLabel, statusToken, statusTone } from "../status";

describe("statusTone", () => {
  it("maps failure and degraded states", () => {
    expect(statusTone("ApplyFailed")).toBe("critical");
    expect(statusTone("Unhealthy")).toBe("critical");
    expect(statusTone("Drifted")).toBe("warning");
    expect(statusTone("Progressing")).toBe("pending");
    expect(statusTone("Healthy")).toBe("healthy");
    expect(statusTone("something-else")).toBe("unknown");
    expect(statusTone(null)).toBe("unknown");
  });
});

describe("statusLabel and statusToken", () => {
  it("returns human labels and CSS tokens", () => {
    expect(statusLabel("ApplyFailed")).toBe("Critical");
    expect(statusLabel("Drifted")).toBe("Warning");
    expect(statusToken("Healthy")).toBe("--strata-healthy");
    expect(statusToken("Progressing")).toBe("--strata-pending");
    expect(statusToken("mystery")).toBe("--strata-unknown");
  });
});
