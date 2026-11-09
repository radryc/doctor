import type { AlertRoute, Route } from "../types/alerts";

export interface SourceAlertKey {
  source: string;
  alertname: string;
}

function routeKey(route: { source: string; alertname: string }): string {
  return `${route.source}\u0000${route.alertname}`;
}

/** isSourceRoute reports whether a generic Route is an exact source+alertname route. */
export function isSourceRoute(
  route: Route,
  source: string,
  alertname: string
): boolean {
  const exact = new Map<string, string>();
  for (const [name, value] of Object.entries(route.matchers ?? {})) {
    exact.set(name, value);
  }
  for (const matcher of route.match ?? []) {
    if (matcher.operator !== "=" && matcher.operator !== "==") return false;
    exact.set(matcher.name, matcher.value);
  }
  return (
    exact.size === 2 &&
    exact.get("source") === source &&
    exact.get("alertname") === alertname
  );
}

/** sourceRouteDestination returns the explicit receiver for a source alert, if any. */
export function sourceRouteDestination(
  alertRoutes: AlertRoute[] | undefined,
  source: string,
  alertname: string
): string {
  const route = (alertRoutes ?? []).find(
    (candidate) => candidate.source === source && candidate.alertname === alertname
  );
  return route?.receivers?.[0] ?? "";
}

/**
 * assignSourceRoute returns a new AlertRoute list with the destination for one
 * source alert set to receiver. A null receiver removes the explicit route and
 * falls back to the partition/default routing.
 */
export function assignSourceRoute(
  alertRoutes: AlertRoute[] | undefined,
  source: string,
  alertname: string,
  receiver: string | null
): AlertRoute[] {
  const key = routeKey({ source, alertname });
  const rest = (alertRoutes ?? []).filter(
    (route) => routeKey(route) !== key
  );
  if (!receiver) return rest;
  return [...rest, { source, alertname, receivers: [receiver] }];
}

/** mergeSourceAlerts deduplicates source alert identities by source + alertname. */
export function mergeSourceAlerts(
  alerts: Array<{ source: string; alertname: string; severity?: string }>
): SourceAlertKey[] {
  const seen = new Set<string>();
  const out: SourceAlertKey[] = [];
  for (const alert of alerts) {
    const key = routeKey(alert);
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ source: alert.source, alertname: alert.alertname });
  }
  return out;
}
