import { describe, expect, it } from "vitest";
import { traceRoute } from "../traceRoute";
import type { TraceRecord } from "../../types/api";

function span(overrides: Partial<TraceRecord>): TraceRecord {
  return {
    tenant: "default",
    trace_id: "abc",
    span_id: "1",
    name: "GET",
    kind: "SPAN_KIND_SERVER",
    service: "svc",
    start_time: "2026-11-10T15:52:02.921Z",
    end_time: "2026-11-10T15:52:03.056Z",
    ...overrides,
  };
}

describe("traceRoute", () => {
  it("reads the gcp-aws route from the root span", () => {
    const route = traceRoute([
      span({
        span_id: "root",
        attributes: { "url.path": "/trace/gcp-aws" },
      }),
      span({ span_id: "child", parent_span_id: "root" }),
    ]);
    expect(route).toEqual({ direction: "gcp-aws", label: "k8s → GCP → AWS" });
  });

  it("reads the aws-gcp route", () => {
    const route = traceRoute([
      span({
        span_id: "root",
        attributes: { "url.path": "/trace/aws-gcp" },
      }),
    ]);
    expect(route).toEqual({ direction: "aws-gcp", label: "k8s → AWS → GCP" });
  });

  it("prefers the root span over a nested span with a route-like path", () => {
    const route = traceRoute([
      span({
        span_id: "root",
        attributes: { "url.path": "/trace/aws-gcp" },
      }),
      span({
        span_id: "child",
        parent_span_id: "root",
        attributes: { "url.path": "/trace/gcp-aws" },
      }),
    ]);
    expect(route?.direction).toBe("aws-gcp");
  });

  it("ignores /trace and /trace/through", () => {
    const route = traceRoute([
      span({ attributes: { "url.path": "/trace" } }),
      span({ span_id: "2", parent_span_id: "1", attributes: { "url.path": "/trace/through" } }),
    ]);
    expect(route).toBeNull();
  });

  it("returns null for an empty trace", () => {
    expect(traceRoute([])).toBeNull();
  });
});
