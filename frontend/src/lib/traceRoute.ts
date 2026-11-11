import type { TraceRecord } from "../types/api";

export interface TraceRoute {
  direction: "gcp-aws" | "aws-gcp";
  label: string;
}

// The demo app exposes one route per direction and records it on the root
// span's url.path, e.g. "/trace/gcp-aws". Both routes start at the k8s client,
// so the label shows the full k8s -> provider -> provider path.
const ROUTE_LABELS: Record<TraceRoute["direction"], string> = {
  "gcp-aws": "k8s → GCP → AWS",
  "aws-gcp": "k8s → AWS → GCP",
};

const ROUTE_PATH = /\/trace\/(gcp-aws|aws-gcp)(?:$|[?#/])/i;

/** Returns the cross-provider route recorded by the demo app, if any. */
export function traceRoute(spans: TraceRecord[]): TraceRoute | null {
  if (spans.length === 0) return null;

  // Prefer the trace root: a damaged/federated trace can contain orphaned
  // spans whose paths are not the route that started the trace.
  const spanIds = new Set(spans.map((span) => span.span_id));
  const roots = spans.filter(
    (span) => !span.parent_span_id || !spanIds.has(span.parent_span_id)
  );
  const ordered = roots.length > 0 ? [...roots, ...spans] : spans;

  for (const span of ordered) {
    const path = span.attributes?.["url.path"] ?? span.attributes?.["http.target"] ?? "";
    const match = ROUTE_PATH.exec(path);
    if (!match) continue;
    const direction = match[1].toLowerCase() as TraceRoute["direction"];
    return { direction, label: ROUTE_LABELS[direction] };
  }
  return null;
}
