import { describe, expect, it } from "vitest";
import { guardianPartitionUrl } from "../links";

describe("guardianPartitionUrl", () => {
  it("adds the partition query parameter to an absolute URL", () => {
    expect(guardianPartitionUrl("http://127.0.0.1:8090", "diva-shared")).toBe(
      "http://127.0.0.1:8090/?partition=diva-shared"
    );
  });

  it("preserves existing query parameters", () => {
    expect(
      guardianPartitionUrl("http://guardian.local/?panel=topologyPanel", "diva-shared")
    ).toBe("http://guardian.local/?panel=topologyPanel&partition=diva-shared");
  });

  it("escapes the partition name", () => {
    expect(guardianPartitionUrl("http://guardian.local", "team a/b")).toBe(
      "http://guardian.local/?partition=team+a%2Fb"
    );
  });

  it("falls back for relative URLs", () => {
    expect(guardianPartitionUrl("/guardian?x=1", "diva-shared")).toBe(
      "/guardian?x=1&partition=diva-shared"
    );
  });

  it("returns an empty string when the base is blank", () => {
    expect(guardianPartitionUrl("   ", "diva-shared")).toBe("");
  });
});
