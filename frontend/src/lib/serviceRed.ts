// RED metrics for services covered only by Guardian synthetic probes.
//
// The pusher exports cumulative run/failure counters and a duration histogram
// for every probe run (guardian/internal/pusher/probes/telemetry.go). Doctor
// normalizes the dotted OTLP names to Prometheus names, so they are queryable
// as guardian_probe_runs, guardian_probe_failures and
// guardian_probe_duration_histogram_seconds_bucket.

export function serviceLabel(name: string): string {
  return name.replaceAll("\\", "\\\\").replaceAll('"', '\\"');
}

export interface ServiceProbeQueries {
  requestRate: string;
  errorRate: string;
  p95Latency: string;
  p99Latency: string;
}

// serviceProbeQueries builds the probe fallback expressions for a raw service
// name. `$__rate_interval` is left for REDCard to resolve to the selected step.
export function serviceProbeQueries(service: string): ServiceProbeQueries {
  const svc = serviceLabel(service);
  const runs = `sum(rate(guardian_probe_runs{service="${svc}"}[$__rate_interval]))`;
  const failures = `sum(rate(guardian_probe_failures{service="${svc}"}[$__rate_interval]))`;
  const durationBuckets = `sum(rate(guardian_probe_duration_histogram_seconds_bucket{service="${svc}"}[$__rate_interval])) by (le)`;

  return {
    requestRate: runs,
    // A missing failure series means zero failures for this window, not
    // missing probe data.
    errorRate: `((${failures} or vector(0)) / ${runs}) * 100`,
    p95Latency: `histogram_quantile(0.95, ${durationBuckets}) * 1000`,
    p99Latency: `histogram_quantile(0.99, ${durationBuckets}) * 1000`,
  };
}
