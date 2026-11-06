import { parse as parseYaml, stringify as stringifyYaml } from "yaml";

export type GraphFileFormat = "json" | "yaml";

export interface ImportedGraph {
  id?: string;
  title: string;
  query: string;
  rangeMinutes: number;
  refreshIntervalSec: number;
  createdAt?: string;
  partitions: string[];
}

export interface ImportedDashboard {
  id?: string;
  name: string;
  graphs: ImportedGraph[];
  createdAt?: string;
  updatedAt?: string;
  partitions: string[];
}

export interface GraphBundle {
  version: number;
  exportedAt: string;
  savedGraphs: ImportedGraph[];
  savedDashboards: ImportedDashboard[];
}

export interface ParsedGraphFile {
  graphs: ImportedGraph[];
  dashboards: ImportedDashboard[];
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function firstString(...values: unknown[]): string {
  for (const value of values) {
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

function firstNumber(...values: unknown[]): number | null {
  for (const value of values) {
    const number = Number(value);
    if (Number.isFinite(number) && value !== "" && value !== null && value !== undefined) {
      return number;
    }
  }
  return null;
}

function normalisePartitions(...values: unknown[]): string[] {
  const out: string[] = [];
  const add = (value: string) => {
    const trimmed = value.trim();
    if (trimmed && !out.includes(trimmed)) out.push(trimmed);
  };
  for (const value of values) {
    if (Array.isArray(value)) {
      for (const item of value) {
        if (typeof item === "string") add(item);
      }
    } else if (typeof value === "string") {
      for (const item of value.split(",")) add(item);
    }
  }
  return out;
}

/**
 * Normalise a saved graph. Besides Doctor's own `{title, query}` shape we
 * accept common aliases (`name`/`expr`) so a hand-written YAML file or an
 * export from another tool can be imported without editing it first.
 */
export function normalizeGraph(value: unknown): ImportedGraph | null {
  const record = asRecord(value);
  if (!record) return null;
  const title = firstString(record.title, record.name, record.label);
  const query = firstString(
    record.query,
    record.expr,
    record.expression,
    record.promql,
    record.promql_query
  );
  if (!title || !query) return null;

  return {
    id: firstString(record.id) || undefined,
    title,
    query,
    rangeMinutes: firstNumber(record.rangeMinutes, record.range_minutes, record.range) ?? 60,
    refreshIntervalSec:
      firstNumber(
        record.refreshIntervalSec,
        record.refresh_interval_sec,
        record.refreshInterval,
        record.refresh
      ) ?? 0,
    createdAt: firstString(record.createdAt, record.created_at) || undefined,
    partitions: normalisePartitions(
      record.partitions,
      record.affected_partitions,
      record.affects
    ),
  };
}

export function normalizeDashboard(value: unknown): ImportedDashboard | null {
  const record = asRecord(value);
  if (!record) return null;
  const name = firstString(record.name, record.title);
  const rawGraphs = Array.isArray(record.graphs)
    ? record.graphs
    : Array.isArray(record.widgets)
      ? record.widgets
      : null;
  if (!name || !rawGraphs) return null;

  const graphs = rawGraphs
    .map((graph) => normalizeGraph(graph))
    .filter((graph): graph is ImportedGraph => graph !== null);
  if (graphs.length === 0) return null;

  const dashboardPartitions = normalisePartitions(
    record.partitions,
    record.affected_partitions,
    record.affects
  );
  return {
    id: firstString(record.id) || undefined,
    name,
    graphs,
    createdAt: firstString(record.createdAt, record.created_at) || undefined,
    updatedAt: firstString(record.updatedAt, record.updated_at) || undefined,
    partitions:
      dashboardPartitions.length > 0
        ? dashboardPartitions
        : [...new Set(graphs.flatMap((graph) => graph.partitions))],
  };
}

function looksLikeDashboard(value: unknown): boolean {
  const record = asRecord(value);
  return (
    !!record &&
    Array.isArray(record.graphs) &&
    firstString(record.name, record.title) !== ""
  );
}

function fromRoot(root: unknown): ParsedGraphFile {
  if (Array.isArray(root)) {
    const graphs: ImportedGraph[] = [];
    const dashboards: ImportedDashboard[] = [];
    for (const item of root) {
      if (looksLikeDashboard(item)) {
        const dashboard = normalizeDashboard(item);
        if (dashboard) dashboards.push(dashboard);
        continue;
      }
      const graph = normalizeGraph(item);
      if (graph) graphs.push(graph);
    }
    return { graphs, dashboards };
  }

  const record = asRecord(root);
  if (!record) {
    throw new Error("Graph file must contain a JSON/YAML object or array.");
  }

  const savedGraphs = Array.isArray(record.savedGraphs) ? record.savedGraphs : null;
  const savedDashboards = Array.isArray(record.savedDashboards) ? record.savedDashboards : null;
  if (savedGraphs || savedDashboards) {
    return {
      graphs: (savedGraphs ?? [])
        .map((graph) => normalizeGraph(graph))
        .filter((graph): graph is ImportedGraph => graph !== null),
      dashboards: (savedDashboards ?? [])
        .map((dashboard) => normalizeDashboard(dashboard))
        .filter((dashboard): dashboard is ImportedDashboard => dashboard !== null),
    };
  }

  const dashboard = normalizeDashboard(record);
  if (dashboard) return { graphs: [], dashboards: [dashboard] };

  const graph = normalizeGraph(record);
  if (graph) return { graphs: [graph], dashboards: [] };

  const graphList = Array.isArray(record.graphs) ? record.graphs : null;
  if (graphList) {
    return {
      graphs: graphList
        .map((item) => normalizeGraph(item))
        .filter((item): item is ImportedGraph => item !== null),
      dashboards: [],
    };
  }

  throw new Error(
    "No graph definitions found. Expected a graph ({title, query}), a list, or a bundle with savedGraphs/savedDashboards."
  );
}

/**
 * Parse an uploaded graph bundle. JSON is attempted first for `.json` files;
 * YAML is used for `.yaml`/`.yml` and as a fallback when no extension is known,
 * so pasted or renamed files still import correctly.
 */
export function parseGraphFile(text: string, filename = ""): ParsedGraphFile {
  const content = text.trim();
  if (!content) throw new Error("The selected file is empty.");

  const extension = filename.toLowerCase().split(".").pop() ?? "";
  const isJsonExtension = extension === "json";
  const isYamlExtension = extension === "yaml" || extension === "yml";

  let root: unknown;
  if (isJsonExtension || (!isYamlExtension && (content.startsWith("{") || content.startsWith("[")))) {
    try {
      root = JSON.parse(content);
    } catch (error) {
      const detail = error instanceof Error ? error.message : "invalid JSON";
      throw new Error(`Could not parse JSON: ${detail}`);
    }
  } else if (isYamlExtension) {
    try {
      root = parseYaml(content);
    } catch (error) {
      const detail = error instanceof Error ? error.message : "invalid YAML";
      throw new Error(`Could not parse YAML: ${detail}`);
    }
  } else {
    try {
      root = JSON.parse(content);
    } catch {
      try {
        root = parseYaml(content);
      } catch (error) {
        const detail = error instanceof Error ? error.message : "invalid JSON or YAML";
        throw new Error(`Could not parse file as JSON or YAML: ${detail}`);
      }
    }
  }

  const parsed = fromRoot(root);
  if (parsed.graphs.length === 0 && parsed.dashboards.length === 0) {
    throw new Error(
      "No saved graphs or dashboards found. Each graph needs a title and a query."
    );
  }
  return parsed;
}

export function serialiseGraphBundle(
  bundle: GraphBundle,
  format: GraphFileFormat
): { content: string; mimeType: string; extension: string } {
  if (format === "yaml") {
    return {
      content: stringifyYaml(bundle),
      mimeType: "application/yaml",
      extension: "yaml",
    };
  }
  return {
    content: JSON.stringify(bundle, null, 2),
    mimeType: "application/json",
    extension: "json",
  };
}
