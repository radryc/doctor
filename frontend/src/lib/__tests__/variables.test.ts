import { describe, expect, it } from "vitest";
import {
  interpolate,
  parseDurationMs,
  variablesFromParams,
  variablesToParams,
} from "../variables";

describe("interpolate", () => {
  it("expands built-in macros", () => {
    expect(interpolate("rate(x[$__rate_interval])", {}, { range: "1h" })).toBe(
      "rate(x[15m])"
    );
    expect(interpolate("$__interval/$__range/$tenant", {}, { interval: "30s", range: "2h", tenant: "acme" })).toBe(
      "30s/2h/acme"
    );
  });

  it("expands variables and all-selection", () => {
    expect(interpolate('service=~"$service"', { service: "api,worker" })).toBe(
      'service=~"(api|worker)"'
    );
    expect(interpolate('service=~"${service}"', { service: "$__all" })).toBe(
      'service=~".*"'
    );
  });

  it("prefers the longest variable name", () => {
    expect(interpolate("$service $service_name", { service: "api", service_name: "api-v2" })).toBe(
      "api api-v2"
    );
  });
});

describe("parseDurationMs", () => {
  it("parses supported units", () => {
    expect(parseDurationMs("1500ms")).toBe(1500);
    expect(parseDurationMs("2m")).toBe(120000);
    expect(parseDurationMs("1h")).toBe(3600000);
    expect(parseDurationMs("bad")).toBe(0);
  });
});

describe("variable URL params", () => {
  it("round-trips values and drops all-selection", () => {
    const params = variablesToParams({ service: "api", partition: "$__all" });
    expect(params.get("var-service")).toBe("api");
    expect(params.get("var-partition")).toBeNull();
    expect(variablesFromParams(params)).toEqual({ service: "api" });
  });
});
