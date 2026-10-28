import type { LogRecord } from "../types/api";

export interface SignalIdentity {
  partition: string;
  intent: string;
  asset: string;
}

/** Resolve canonical identity fields from a resource/point attribute map. */
function identityFromAttributes(attrs: Record<string, string> | undefined): SignalIdentity {
  return {
    partition:
      attrs?.["guardian.partition"] ||
      attrs?.["guardian_partition"] ||
      attrs?.["partition"] ||
      attrs?.["k8s.namespace.name"] ||
      attrs?.["namespace"] ||
      "",
    intent:
      attrs?.["guardian.intent"] ||
      attrs?.["guardian_intent"] ||
      attrs?.["intent"] ||
      attrs?.["app.kubernetes.io/component"] ||
      "",
    asset:
      attrs?.["guardian.asset"] ||
      attrs?.["guardian_asset"] ||
      attrs?.["asset"] ||
      attrs?.["k8s.pod.name"] ||
      attrs?.["k8s.container.name"] ||
      attrs?.["app.kubernetes.io/instance"] ||
      "",
  };
}

/**
 * Canonical identity for a log record. This mirrors the query backend's
 * logIdentity precedence: typed/resource identity wins, with point attributes
 * used as a fallback when the resource carries no identity at all.
 */
export function logIdentity(record: LogRecord): SignalIdentity {
  const resource = identityFromAttributes(record.resource_attributes);
  const identity = {
    partition: record.partition || resource.partition,
    intent: record.intent || resource.intent,
    asset: record.asset || resource.asset,
  };
  if (identity.partition || identity.intent || identity.asset) {
    return identity;
  }
  return identityFromAttributes(record.attributes);
}

/**
 * Canonical service identity of a log record. The backend catalog and the
 * service filter both key on LogRecord.Service first, so the UI must do the
 * same; guardian.component is only a fallback for records without a service.
 */
export function logService(record: LogRecord): string {
  return (
    record.service ||
    record.attributes?.["guardian.component"] ||
    record.resource_attributes?.["guardian.component"] ||
    record.attributes?.["guardian_component"] ||
    record.resource_attributes?.["guardian_component"] ||
    ""
  );
}

export function logPartition(record: LogRecord): string {
  return logIdentity(record).partition;
}

export function logIntent(record: LogRecord): string {
  return logIdentity(record).intent;
}

export function logAsset(record: LogRecord): string {
  return logIdentity(record).asset;
}

/**
 * Split a canonical <partition>.<intent>.<asset> service name. Returns null
 * for short aliases and producer-defined names.
 */
export function splitServiceIdentity(service: string): SignalIdentity | null {
  const parts = service.split(".");
  if (parts.length !== 3 || parts.some((part) => !part.trim())) {
    return null;
  }
  return { partition: parts[0], intent: parts[1], asset: parts[2] };
}
