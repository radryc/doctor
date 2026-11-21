import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { readHiddenBuiltIns, writeHiddenBuiltIns } from "../hiddenDashboards";

function memoryStorage() {
  const values = new Map<string, string>();
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => {
      values.set(key, value);
    },
    removeItem: (key: string) => {
      values.delete(key);
    },
    clear: () => {
      values.clear();
    },
  };
}

describe("hidden built-in dashboards", () => {
  beforeEach(() => {
    vi.stubGlobal("localStorage", memoryStorage());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("round-trips hidden ids per tenant", () => {
    writeHiddenBuiltIns("default", new Set(["preset-b", "preset-a"]));
    expect([...readHiddenBuiltIns("default")]).toEqual(["preset-a", "preset-b"]);
    expect(readHiddenBuiltIns("other")).toEqual(new Set());
  });

  it("ignores malformed stored values", () => {
    localStorage.setItem("doctor.dashboards.hiddenBuiltIns.default", "{not json");
    expect(readHiddenBuiltIns("default")).toEqual(new Set());
    localStorage.setItem("doctor.dashboards.hiddenBuiltIns.default", JSON.stringify([1, "ok", null]));
    expect(readHiddenBuiltIns("default")).toEqual(new Set(["ok"]));
  });
});
