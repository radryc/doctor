import { describe, expect, it } from "vitest";
import { computeRto, formatDurationMs, rtoStatus } from "./rto";
import type { GuardianEventRecord } from "../../types/api";

function event(partial: GuardianEventRecord): GuardianEventRecord {
  return partial;
}

describe("computeRto", () => {
  it("pairs failure and recovery events", () => {
    const base = Date.parse("2026-11-08T10:00:00Z");
    const events = [
      event({ kind: "asset.failed", asset: "checkout", intent: "web", timestamp: new Date(base).toISOString() }),
      event({ kind: "asset.recovered", asset: "checkout", intent: "web", timestamp: new Date(base + 10 * 60_000).toISOString() }),
    ];
    const summary = computeRto(events, base + 60 * 60_000, 30 * 60_000);
    expect(summary.completed).toHaveLength(1);
    expect(summary.completed[0].durationMs).toBe(10 * 60_000);
    expect(summary.p50).toBe(10 * 60_000);
    expect(summary.currentOutageMs).toBe(0);
    expect(summary.byIntent[0].key).toBe("web");
    expect(rtoStatus(summary, 30 * 60_000)).toBe("within");
  });

  it("keeps an open incident when no recovery follows", () => {
    const base = Date.parse("2026-11-08T10:00:00Z");
    const events = [
      event({ kind: "intent.failing", intent: "payments", timestamp: new Date(base).toISOString() }),
    ];
    const summary = computeRto(events, base + 45 * 60_000, 30 * 60_000);
    expect(summary.open).toHaveLength(1);
    expect(summary.currentOutageMs).toBe(45 * 60_000);
    expect(rtoStatus(summary, 30 * 60_000)).toBe("breached");
  });

  it("formats durations compactly", () => {
    expect(formatDurationMs(0)).toBe("0m");
    expect(formatDurationMs(90 * 60_000)).toBe("1h 30m");
    expect(formatDurationMs(26 * 60 * 60_000)).toBe("1d 2h");
  });
});
