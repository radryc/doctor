import type { AlertSpec, SignalType } from "../../types/alerts";

export interface SignalDefinition {
  id: SignalType;
  label: string;
  question: string;
  description: string;
  accent: string;
  defaultFor: string;
  suggestedReceiverType: string;
}

export const SIGNALS: SignalDefinition[] = [
  {
    id: "errors",
    label: "Errors",
    question: "Are requests failing?",
    description: "Measure the share of requests that fail, or the absolute failure rate.",
    accent: "rose",
    defaultFor: "10m",
    suggestedReceiverType: "pagerduty",
  },
  {
    id: "latency",
    label: "Latency",
    question: "Are requests slow?",
    description: "Watch a latency percentile and alert when it stays above your limit.",
    accent: "amber",
    defaultFor: "10m",
    suggestedReceiverType: "slack",
  },
  {
    id: "traffic",
    label: "Traffic",
    question: "Is demand normal?",
    description: "Alert when request volume drops to zero or grows beyond capacity.",
    accent: "sky",
    defaultFor: "5m",
    suggestedReceiverType: "slack",
  },
  {
    id: "saturation",
    label: "Saturation",
    question: "Are we running out of capacity?",
    description: "Watch CPU, memory, queues, or any usage-versus-limit ratio.",
    accent: "violet",
    defaultFor: "15m",
    suggestedReceiverType: "slack",
  },
  {
    id: "availability",
    label: "Availability",
    question: "Is it reachable and reporting?",
    description: "Watch probes, up metrics, and missing data.",
    accent: "emerald",
    defaultFor: "5m",
    suggestedReceiverType: "pagerduty",
  },
];

export function signalDefinition(signal: string): SignalDefinition {
  return SIGNALS.find((item) => item.id === signal) ?? SIGNALS[0];
}

const DEFAULT_METRIC: Record<SignalType, string> = {
  errors: "http_server_request_duration_count",
  latency: "http_server_request_duration_bucket",
  traffic: "http_server_request_duration_count",
  saturation: "container_cpu_usage_millicores",
  availability: "guardian_probe_up",
};

export function defaultSpec(signal: SignalType, partition: string): AlertSpec {
  const definition = signalDefinition(signal);
  const filters = partition
    ? [{ name: "guardian_partition", operator: "=", value: partition }]
    : [];
  const base: AlertSpec = {
    signal,
    primary: {
      metric: DEFAULT_METRIC[signal],
      filters,
      function: signal === "errors" || signal === "traffic" ? "rate" : "none",
      reduce: signal === "saturation" ? "max" : signal === "availability" ? "min" : "sum",
      by: signal === "saturation" ? ["container_name"] : ["service"],
    },
    condition: { op: "gt", value: 5, unit: "percent" },
    window: "5m",
    for: definition.defaultFor,
    severity: signal === "errors" || signal === "availability" ? "critical" : "warning",
    summary: "",
  };
  switch (signal) {
    case "latency":
      base.primary.quantile = 0.95;
      base.condition = { op: "gt", value: 1000, unit: "ms" };
      break;
    case "traffic":
      base.condition = { op: "lt", value: 0.01, unit: "rps" };
      break;
    case "saturation":
      base.condition = { op: "gt", value: 85, unit: "percent" };
      break;
    case "availability":
      base.condition = { op: "eq", value: 0, unit: "" };
      base.primary.by = ["service"];
      break;
  }
  return base;
}

export function renderSummary(spec: AlertSpec): string {
  const metric = spec.primary.metric || "the selected signal";
  const value = spec.condition.value;
  switch (spec.signal) {
    case "errors":
      return `${metric} error rate is above ${value}%`;
    case "latency":
      return `${metric} p${Math.round((spec.primary.quantile || 0.95) * 100)} latency is above ${value}ms`;
    case "traffic":
      return spec.condition.op === "lt"
        ? `${metric} traffic has dropped below ${value} requests/second`
        : `${metric} traffic is above ${value} requests/second`;
    case "saturation":
      return `${metric} utilisation is above ${value}${spec.condition.unit === "percent" ? "%" : ""}`;
    case "availability":
      return spec.condition.op === "absent"
        ? `${metric} has stopped reporting`
        : `${metric} is reporting an unhealthy state`;
    default:
      return `${metric} crossed its threshold`;
  }
}

export function describeSpec(spec: AlertSpec): string {
  const definition = signalDefinition(spec.signal);
  const forText = spec.for ? ` for ${spec.for}` : "";
  return `${definition.question} Alert when ${renderSummary(spec)}${forText}.`;
}

export function inferSignal(metric: string, metadataType?: string): SignalType {
  const name = metric.toLowerCase();
  if (metadataType === "histogram" || name.includes("_bucket") || name.includes("duration")) {
    return "latency";
  }
  if (name.includes("probe_up") || name === "up" || name.includes("availability") || name.includes("health")) {
    return "availability";
  }
  if (name.includes("cpu") || name.includes("memory") || name.includes("disk") || name.includes("queue") || name.includes("usage")) {
    return "saturation";
  }
  if (name.includes("error") || name.includes("fail") || name.includes("status")) {
    return "errors";
  }
  if (name.endsWith("_total") || name.endsWith("_count") || name.includes("request")) {
    return "traffic";
  }
  return "traffic";
}

export function latencyQuantileLabel(value?: number): string {
  return `p${Math.round((value || 0.95) * 100)}`;
}
