import { describe, expect, it } from "vitest";
import { serviceLabel, serviceProbeQueries } from "../serviceRed";

describe("serviceProbeQueries", () => {
  it("builds probe RED expressions for a canonical service name", () => {
    const queries = serviceProbeQueries("diva-shared.gcp.app");

    expect(queries.requestRate).toBe(
      'sum(rate(guardian_probe_runs{service="diva-shared.gcp.app"}[$__rate_interval]))',
    );
    expect(queries.errorRate).toBe(
      '((sum(rate(guardian_probe_failures{service="diva-shared.gcp.app"}[$__rate_interval])) or vector(0)) / sum(rate(guardian_probe_runs{service="diva-shared.gcp.app"}[$__rate_interval]))) * 100',
    );
    expect(queries.p95Latency).toBe(
      'histogram_quantile(0.95, sum(rate(guardian_probe_duration_histogram_seconds_bucket{service="diva-shared.gcp.app"}[$__rate_interval])) by (le)) * 1000',
    );
    expect(queries.p99Latency).toBe(
      'histogram_quantile(0.99, sum(rate(guardian_probe_duration_histogram_seconds_bucket{service="diva-shared.gcp.app"}[$__rate_interval])) by (le)) * 1000',
    );
  });

  it("escapes quotes and backslashes in the service matcher", () => {
    expect(serviceLabel('svc"with\\slashes')).toBe('svc\\"with\\\\slashes');

    const queries = serviceProbeQueries('svc"with\\slashes');
    expect(queries.requestRate).toContain('service="svc\\"with\\\\slashes"');
  });
});
