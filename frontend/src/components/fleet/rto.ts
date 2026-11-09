import type { GuardianEventRecord } from "../../types/api";

export interface RtoIncident {
  key: string;
  asset?: string;
  intent?: string;
  startedAt: number;
  endedAt?: number;
  durationMs?: number;
  message?: string;
  open: boolean;
}

export interface RtoBucket {
  key: string;
  incidents: RtoIncident[];
  open: RtoIncident[];
  p50: number;
  p95: number;
  max: number;
  avg: number;
  lastFailureAt?: number;
}

export interface RtoSummary {
  incidents: RtoIncident[];
  completed: RtoIncident[];
  open: RtoIncident[];
  p50: number;
  p95: number;
  max: number;
  avg: number;
  currentOutageMs: number;
  byKey: RtoBucket[];
  byIntent: RtoBucket[];
  breaches: number;
}

const FAILURE_PATTERN = /fail|crit|error|unhealthy|unavailable|down|degrad|drift|timeout|outage|broken/i;
const RECOVERY_PATTERN = /recover|resolv|restor|healthy|success|returned|\bok\b/i;

export function eventTime(event: GuardianEventRecord): number {
  if (!event.timestamp) return 0;
  const value = new Date(event.timestamp).getTime();
  return Number.isFinite(value) ? value : 0;
}

export function classifyEvent(event: GuardianEventRecord): "failure" | "recovery" | "neutral" {
  const text = [event.kind, event.status, event.message].filter(Boolean).join(" ");
  if (!text.trim()) return "neutral";
  if (FAILURE_PATTERN.test(text)) return "failure";
  if (RECOVERY_PATTERN.test(text)) return "recovery";
  return "neutral";
}

function percentile(values: number[], p: number): number {
  if (values.length === 0) return 0;
  const sorted = [...values].sort((a, b) => a - b);
  const index = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1));
  return sorted[index];
}

function bucketize(incidents: RtoIncident[], keyOf: (incident: RtoIncident) => string): RtoBucket[] {
  const map = new Map<string, RtoIncident[]>();
  for (const incident of incidents) {
    const key = keyOf(incident) || "unknown";
    const list = map.get(key) ?? [];
    list.push(incident);
    map.set(key, list);
  }
  return [...map.entries()]
    .map(([key, list]) => {
      const completed = list.filter((incident) => !incident.open && incident.durationMs !== undefined);
      const durations = completed.map((incident) => incident.durationMs ?? 0);
      const open = list.filter((incident) => incident.open);
      return {
        key,
        incidents: list.sort((a, b) => a.startedAt - b.startedAt),
        open,
        p50: percentile(durations, 50),
        p95: percentile(durations, 95),
        max: durations.length > 0 ? Math.max(...durations) : 0,
        avg: durations.length > 0 ? durations.reduce((sum, value) => sum + value, 0) / durations.length : 0,
        lastFailureAt: list.length > 0 ? Math.max(...list.map((incident) => incident.startedAt)) : undefined,
      };
    })
    .sort((a, b) => b.open.length - a.open.length || b.p95 - a.p95 || a.key.localeCompare(b.key));
}

export function computeRto(events: GuardianEventRecord[], now = Date.now(), targetMs = 0): RtoSummary {
  const byKey = new Map<string, { key: string; asset?: string; intent?: string; lastFailure?: RtoIncident }>();
  const incidents: RtoIncident[] = [];

  const ordered = [...events]
    .map((event) => ({ event, at: eventTime(event) }))
    .filter((row) => row.at > 0)
    .sort((a, b) => a.at - b.at);

  for (const { event, at } of ordered) {
    const kind = classifyEvent(event);
    if (kind === "neutral") continue;
    const key = event.asset || event.intent || "partition";
    let state = byKey.get(key);
    if (!state) {
      state = { key, asset: event.asset, intent: event.intent };
      byKey.set(key, state);
    }
    if (kind === "failure") {
      if (!state.lastFailure) {
        const incident: RtoIncident = {
          key,
          asset: event.asset,
          intent: event.intent,
          startedAt: at,
          message: event.message,
          open: true,
        };
        state.lastFailure = incident;
        incidents.push(incident);
      }
      continue;
    }
    if (kind === "recovery" && state.lastFailure) {
      state.lastFailure.endedAt = at;
      state.lastFailure.durationMs = Math.max(0, at - state.lastFailure.startedAt);
      state.lastFailure.open = false;
      state.lastFailure = undefined;
    }
  }

  const completed = incidents.filter((incident) => !incident.open && incident.durationMs !== undefined);
  const open = incidents.filter((incident) => incident.open);
  const durations = completed.map((incident) => incident.durationMs ?? 0);
  const currentOutageMs = open.length > 0 ? Math.max(...open.map((incident) => Math.max(0, now - incident.startedAt))) : 0;
  const breaches = targetMs > 0
    ? completed.filter((incident) => (incident.durationMs ?? 0) > targetMs).length +
      open.filter((incident) => now - incident.startedAt > targetMs).length
    : 0;

  return {
    incidents,
    completed,
    open,
    p50: percentile(durations, 50),
    p95: percentile(durations, 95),
    max: durations.length > 0 ? Math.max(...durations) : 0,
    avg: durations.length > 0 ? durations.reduce((sum, value) => sum + value, 0) / durations.length : 0,
    currentOutageMs,
    byKey: bucketize(incidents, (incident) => incident.asset || incident.intent || "partition"),
    byIntent: bucketize(incidents, (incident) => incident.intent || incident.asset || "partition"),
    breaches,
  };
}

export function formatDurationMs(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0m";
  const minutes = Math.round(value / 60_000);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (hours < 24) return rest > 0 ? `${hours}h ${rest}m` : `${hours}h`;
  const days = Math.floor(hours / 24);
  const restHours = hours % 24;
  return restHours > 0 ? `${days}d ${restHours}h` : `${days}d`;
}

export function rtoStatus(summary: RtoSummary, targetMs: number): "within" | "at-risk" | "breached" {
  if (targetMs <= 0) return "within";
  if (summary.currentOutageMs > targetMs || summary.p95 > targetMs) return "breached";
  if (summary.currentOutageMs > targetMs * 0.75 || summary.p95 > targetMs * 0.75) return "at-risk";
  return "within";
}
