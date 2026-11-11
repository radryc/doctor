import { describe, expect, it } from "vitest";
import { isPartitionDenied, pickDefaultPartition } from "../partitionAccess";

describe("isPartitionDenied", () => {
  it("allows every partition when the identity is unrestricted", () => {
    expect(isPartitionDenied("doctor", undefined)).toBe(false);
  });

  it("allows a partition in the accessible list", () => {
    expect(isPartitionDenied("diva-shared", ["diva-shared"])).toBe(false);
  });

  it("denies a partition outside the accessible list", () => {
    expect(isPartitionDenied("doctor", ["diva-shared"])).toBe(true);
  });

  it("denies every partition when the accessible list is empty", () => {
    expect(isPartitionDenied("diva-shared", [])).toBe(true);
  });

  it("does not deny an empty selection", () => {
    expect(isPartitionDenied("", ["diva-shared"])).toBe(false);
  });
});

describe("pickDefaultPartition", () => {
  it("keeps the configured default when unrestricted", () => {
    expect(pickDefaultPartition("doctor", undefined)).toBe("doctor");
  });

  it("keeps the configured default when it is accessible", () => {
    expect(pickDefaultPartition("diva-shared", ["diva-shared", "divb-shared"])).toBe(
      "diva-shared"
    );
  });

  it("falls back to the first accessible partition", () => {
    expect(pickDefaultPartition("doctor", ["divb-shared", "diva-shared"])).toBe(
      "divb-shared"
    );
  });

  it("returns empty when no partitions are accessible", () => {
    expect(pickDefaultPartition("doctor", [])).toBe("");
  });
});
