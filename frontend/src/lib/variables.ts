import type { VariableValues } from "../types/dashboard";

/**
 * Interpolates $variable and $__macro references in a query expression.
 *
 * Supported macros:
 *   $__interval     → step interval string (e.g. "30s")
 *   $__range        → time range duration string (e.g. "1h")
 *   $__rate_interval → (range * 4) capped — a safe interval for rate()
 *   $tenant         → current tenant
 *   $partition, $service, $source, etc. → from variableValues
 */
export function interpolate(
  expr: string,
  variableValues: VariableValues,
  macros: { interval?: string; range?: string; tenant?: string } = {}
): string {
  let result = expr;

  // Built-in macros
  const interval = macros.interval ?? "30s";
  const range = macros.range ?? "1h";
  const rateInterval = deriveRateInterval(range);

  result = result.replaceAll("$__interval", interval);
  result = result.replaceAll("$__rate_interval", rateInterval);
  result = result.replaceAll("$__range", range);
  if (macros.tenant) {
    result = result.replaceAll("$tenant", macros.tenant);
  }

  // Template variables — longest names first to avoid partial substitution.
  // `$__all` and an empty selection mean "match everything"; emit `.*` so the
  // variable can be used in a regex matcher (service=~"$service").
  const sortedNames = Object.keys(variableValues).sort((a, b) => b.length - a.length);
  for (const name of sortedNames) {
    const raw = variableValues[name];
    const value =
      raw === "$__all" || raw === "" ? ".*" : raw.includes(",") ? `(${raw.split(",").join("|")})` : raw;
    // Replace both $name and ${name} forms
    result = result.replaceAll(`\${${name}}`, value);
    result = result.replaceAll(`$${name}`, value);
  }

  // A dashboard may reference $__all without a matching variable value.
  result = result.replaceAll("$__all", ".*");

  return result;
}

/**
 * Derives a safe rate interval from the range string.
 * Falls back to "1m" when the range cannot be parsed.
 */
function deriveRateInterval(range: string): string {
  const ms = parseDurationMs(range);
  if (ms <= 0) return "1m";
  // Prometheus convention: at least 4 scrape intervals for stable rates.
  const step = Math.max(Math.ceil(ms / 4 / 1000), 60);
  return formatSeconds(step);
}

export function parseDurationMs(d: string): number {
  const match = d.match(/^(\d+(?:\.\d+)?)(ms|s|m|h|d|w)$/i);
  if (!match) return 0;
  const n = parseFloat(match[1]);
  switch (match[2].toLowerCase()) {
    case "ms": return n;
    case "s":  return n * 1000;
    case "m":  return n * 60 * 1000;
    case "h":  return n * 3600 * 1000;
    case "d":  return n * 86400 * 1000;
    case "w":  return n * 7 * 86400 * 1000;
    default:   return 0;
  }
}

function formatSeconds(s: number): string {
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${Math.round(s / 3600)}h`;
}

/**
 * Builds URL search params for syncing variable values.
 * Params are prefixed with "var-" (e.g. ?var-partition=prod-eu).
 */
export function variablesToParams(values: VariableValues): URLSearchParams {
  const params = new URLSearchParams();
  for (const [name, value] of Object.entries(values)) {
    if (value && value !== "$__all") {
      params.set(`var-${name}`, value);
    }
  }
  return params;
}

/**
 * Parses variable values from URL search params (var-* prefix).
 */
export function variablesFromParams(params: URLSearchParams): VariableValues {
  const values: VariableValues = {};
  for (const [key, value] of params.entries()) {
    if (key.startsWith("var-")) {
      values[key.slice(4)] = value;
    }
  }
  return values;
}
