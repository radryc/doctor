import { useEffect, useMemo, useState } from "react";
import type { KeyboardEvent } from "react";
import { AlertTriangle, Database, Loader2, Play, RefreshCw, ShieldCheck } from "lucide-react";
import { api } from "../api/client";
import Tabs from "../components/ui/Tabs";
import type { DuckDBSQLNodeResult, DuckDBSQLResponse } from "../types/api";

type Dataset = "logs" | "metrics" | "traces";

const SAMPLE_QUERIES: Record<Dataset, string> = {
  logs: `SELECT service, level, count(*) AS rows
FROM logs
GROUP BY 1, 2
ORDER BY rows DESC
LIMIT 50`,
  metrics: `SELECT service, metric_name,
       count(*) AS points,
       round(avg(value), 3) AS avg_value
FROM metrics
GROUP BY 1, 2
ORDER BY points DESC
LIMIT 50`,
  traces: `SELECT service, status_code, count(*) AS spans
FROM traces
GROUP BY 1, 2
ORDER BY spans DESC
LIMIT 50`,
};

const DATASET_HELP: Record<Dataset, string> = {
  logs: "Columns: time, timestamp, level, service, trace_id, span_id, raw_message, labels_json, partition.",
  metrics: "Columns: time, timestamp, service, metric_name, value, labels_json, partition.",
  traces: "Columns: time, timestamp, end_time, end_time_ts, trace_id, span_id, parent_span_id, service, name, status_code, attributes_json, partition.",
};

function SyntaxHelp() {
  const code = "rounded bg-slate-800/70 px-1 py-0.5 font-mono text-slate-200";
  return (
    <details className="mt-3 rounded-lg border border-slate-700/50 bg-slate-950/50 px-3 py-2 text-[11px] leading-5 text-slate-400">
      <summary className="cursor-pointer select-none font-medium text-slate-300">Syntax help</summary>
      <div className="mt-3 grid gap-4 md:grid-cols-2">
        <div>
          <p className="font-semibold text-slate-300">Strings and identifiers</p>
          <ul className="mt-1 list-disc space-y-1 pl-4">
            <li>Text uses single quotes: <code className={code}>service = 'gcp'</code></li>
            <li>
              Double quotes are identifiers: <code className={code}>"gcp"</code> means a column named{" "}
              <code className={code}>gcp</code>, causing <code className={code}>UNKNOWN_IDENTIFIER</code>.
            </li>
            <li>
              Substring match: <code className={code}>service LIKE '%gcp%'</code>;{" "}
              <code className={code}>ILIKE</code> ignores case. <code className={code}>LIKE 'gcp'</code> is exact.
            </li>
          </ul>
        </div>
        <div>
          <p className="font-semibold text-slate-300">Labels and time</p>
          <ul className="mt-1 list-disc space-y-1 pl-4">
            <li>
              DuckDB: <code className={code}>json_extract_string(labels_json, '$.env')</code>
            </li>
            <li>
              ClickHouse: <code className={code}>JSONExtractString(labels_json, 'env')</code>
            </li>
            <li>Traces use <code className={code}>attributes_json</code>.</li>
            <li>
              <code className={code}>timestamp</code> is epoch nanoseconds; the range selector filters it
              automatically. <code className={code}>time</code> differs by backend, so prefer{" "}
              <code className={code}>timestamp</code> in predicates.
            </li>
          </ul>
        </div>
        <div>
          <p className="font-semibold text-slate-300">Counting rows</p>
          <pre className="mt-1 overflow-x-auto rounded bg-slate-950/80 p-2 font-mono text-[10px] leading-4 text-slate-300">{`SELECT service, level, count(*) AS rows
FROM logs
WHERE service ILIKE '%gcp%'
GROUP BY service, level
ORDER BY rows DESC
LIMIT 50`}</pre>
        </div>
        <div>
          <p className="font-semibold text-slate-300">Limits and results</p>
          <ul className="mt-1 list-disc space-y-1 pl-4">
            <li>One read-only <code className={code}>SELECT</code> / <code className={code}>WITH</code> statement.</li>
            <li>Maximum 7 days, 5,000 rows, 32 KiB of SQL.</li>
            <li>
              DuckDB returns per-node partial aggregates because chunks are shard-local; ClickHouse
              returns one global result.
            </li>
            <li>
              Don't alias <code className={code}>labels_json</code> as <code className={code}>rows</code> and
              sort by it; sort by a <code className={code}>count(*)</code> alias instead.
            </li>
          </ul>
        </div>
      </div>
    </details>
  );
}

const RANGE_OPTIONS = [
  { label: "Last 1h", ms: 60 * 60 * 1000 },
  { label: "Last 6h", ms: 6 * 60 * 60 * 1000 },
  { label: "Last 24h", ms: 24 * 60 * 60 * 1000 },
  { label: "Last 7d", ms: 7 * 24 * 60 * 60 * 1000 },
];

function formatCell(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "object") {
    try {
      return JSON.stringify(value);
    } catch {
      return String(value);
    }
  }
  return String(value);
}

export default function SqlPage() {
  const [dataset, setDataset] = useState<Dataset>("logs");
  const [query, setQuery] = useState(SAMPLE_QUERIES.logs);
  const [rangeMs, setRangeMs] = useState(RANGE_OPTIONS[0].ms);
  const [limit, setLimit] = useState(1000);
  const [response, setResponse] = useState<DuckDBSQLResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [running, setRunning] = useState(false);
  const [selectedNode, setSelectedNode] = useState(0);

  useEffect(() => {
    setQuery(SAMPLE_QUERIES[dataset]);
    setResponse(null);
    setError(null);
    setSelectedNode(0);
  }, [dataset]);

  const run = async () => {
    if (running || !query.trim()) return;
    setRunning(true);
    setError(null);
    try {
      const to = new Date();
      const from = new Date(to.getTime() - rangeMs);
      const data = await api.queryDuckDBSQL({
        dataset,
        query,
        from: from.toISOString(),
        to: to.toISOString(),
        limit,
      });
      setResponse(data);
      const firstWithResult = data.nodes.findIndex((node) => node.result);
      setSelectedNode(firstWithResult >= 0 ? firstWithResult : 0);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "SQL query failed");
      setResponse(null);
    } finally {
      setRunning(false);
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
      event.preventDefault();
      void run();
    }
  };

  const nodes = response?.nodes ?? [];
  const activeNode: DuckDBSQLNodeResult | undefined = nodes[selectedNode];
  const activeResult = activeNode?.result;
  const totalRows = useMemo(
    () => nodes.reduce((sum, node) => sum + (node.result?.row_count ?? 0), 0),
    [nodes]
  );
  const failedNodes = nodes.filter((node) => node.error).length;

  return (
    <div className="h-full overflow-y-auto bg-slate-950 p-5 text-slate-100">
      <div className="mx-auto max-w-[1500px] space-y-4">
        <header className="flex flex-wrap items-end justify-between gap-3">
          <div>
            <p className="text-[10px] font-bold uppercase tracking-[0.2em] text-[var(--strata-accent-text)]">SQL</p>
            <h1 className="mt-1 text-xl font-semibold">SQL console</h1>
            <p className="mt-1 max-w-3xl text-xs text-slate-400">
              Read-only SQL over the MonoFS telemetry backend. Queries run on the configured engine
              (DuckDB/Parquet or ClickHouse) and are filtered to your visible partitions.
            </p>
          </div>
          <div className="flex items-center gap-2 text-[11px] text-slate-400">
            <ShieldCheck size={14} className="text-emerald-400" />
            RBAC enforced by Doctor and MonoFS
          </div>
        </header>

        <section className="rounded-xl border border-slate-700/50 bg-slate-900/60 p-4">
          <div className="flex flex-wrap items-center gap-2">
            <Tabs
              items={[
                { key: "logs", label: "logs" },
                { key: "metrics", label: "metrics" },
                { key: "traces", label: "traces" },
              ] as const}
              value={dataset}
              onChange={setDataset}
              ariaLabel="SQL dataset"
            />
            <select
              value={rangeMs}
              onChange={(event) => setRangeMs(Number(event.target.value))}
              className="rounded-lg border border-slate-700/50 bg-slate-950/50 px-2.5 py-1.5 text-xs text-slate-200 outline-none focus:border-cyan-500/60"
              aria-label="Query time range"
            >
              {RANGE_OPTIONS.map((option) => (
                <option key={option.ms} value={option.ms}>{option.label}</option>
              ))}
            </select>
            <select
              value={limit}
              onChange={(event) => setLimit(Number(event.target.value))}
              className="rounded-lg border border-slate-700/50 bg-slate-950/50 px-2.5 py-1.5 text-xs text-slate-200 outline-none focus:border-cyan-500/60"
              aria-label="Row limit"
            >
              {[100, 500, 1000, 5000].map((value) => (
                <option key={value} value={value}>{value} rows</option>
              ))}
            </select>
            <span className="ml-auto text-[11px] text-slate-500">⌘/Ctrl + Enter to run</span>
            <button
              type="button"
              onClick={() => void run()}
              disabled={running || !query.trim()}
              className="inline-flex items-center gap-1.5 rounded-lg border border-cyan-500/40 bg-cyan-500/15 px-3 py-1.5 text-xs font-semibold text-[var(--strata-accent-text)] transition-colors hover:bg-cyan-500/25 disabled:cursor-not-allowed disabled:opacity-50"
            >
              {running ? <Loader2 size={14} className="animate-spin" /> : <Play size={14} />}
              {running ? "Running" : "Run query"}
            </button>
          </div>

          <textarea
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            onKeyDown={onKeyDown}
            spellCheck={false}
            rows={14}
            className="mt-3 w-full resize-y rounded-lg border border-slate-700/50 bg-slate-950/80 p-3 font-mono text-[12px] leading-5 text-slate-200 outline-none focus:border-cyan-500/60"
            aria-label="SQL query"
          />
          <p className="mt-2 text-[11px] text-slate-500">{DATASET_HELP[dataset]}</p>
          <SyntaxHelp />
        </section>

        {error && (
          <div className="flex items-start gap-2 rounded-xl border border-rose-500/30 bg-rose-500/10 px-4 py-3 text-xs text-rose-300">
            <AlertTriangle size={15} className="mt-0.5 shrink-0" />
            <span>{error}</span>
          </div>
        )}

        {response && (
          <section className="rounded-xl border border-slate-700/50 bg-slate-900/60">
            <div className="flex flex-wrap items-center gap-3 border-b border-slate-700/40 px-4 py-3">
              <div className="flex items-center gap-2 text-sm font-semibold text-slate-200">
                <Database size={15} className="text-cyan-400" />
                Results
              </div>
              <span className="rounded-full border border-slate-700/50 bg-slate-800/40 px-2 py-0.5 text-[10px] text-slate-400">
                {nodes.length} node{nodes.length === 1 ? "" : "s"}
              </span>
              <span className="rounded-full border border-slate-700/50 bg-slate-800/40 px-2 py-0.5 text-[10px] text-slate-400">
                {totalRows.toLocaleString()} rows
              </span>
              {failedNodes > 0 && (
                <span className="rounded-full border border-amber-500/30 bg-amber-500/10 px-2 py-0.5 text-[10px] text-amber-300">
                  {failedNodes} node{failedNodes === 1 ? "" : "s"} failed
                </span>
              )}
              {response.restricted && (
                <span
                  className="rounded-full border border-emerald-500/30 bg-emerald-500/10 px-2 py-0.5 text-[10px] text-emerald-300"
                  title={response.visible_partitions?.join(", ") || "No visible partitions"}
                >
                  {response.visible_partitions?.length
                    ? `${response.visible_partitions.length} visible partition${response.visible_partitions.length === 1 ? "" : "s"}`
                    : "no visible partitions"}
                </span>
              )}
              <button
                type="button"
                onClick={() => void run()}
                className="ml-auto inline-flex items-center gap-1 text-[11px] text-slate-400 hover:text-slate-200"
              >
                <RefreshCw size={12} /> Re-run
              </button>
            </div>

            {nodes.length === 0 ? (
              <div className="px-4 py-10 text-center text-xs text-slate-400">No MonoFS nodes returned a result.</div>
            ) : (
              <>
                <div className="flex flex-wrap gap-1 border-b border-slate-700/40 px-3 py-2">
                  {nodes.map((node, index) => (
                    <button
                      key={`${node.node_id}-${index}`}
                      type="button"
                      onClick={() => setSelectedNode(index)}
                      className={`inline-flex items-center gap-1.5 rounded-md px-2.5 py-1 text-[11px] ${
                        selectedNode === index
                          ? "bg-cyan-500/15 text-[var(--strata-accent-text)]"
                          : "text-slate-400 hover:bg-slate-800/60 hover:text-slate-200"
                      }`}
                    >
                      <span className={`h-1.5 w-1.5 rounded-full ${node.error ? "bg-rose-400" : "bg-emerald-400"}`} />
                      {node.node_id || node.address}
                      {node.result && <span className="text-slate-500">{node.result.row_count}</span>}
                    </button>
                  ))}
                </div>

                {activeNode?.error ? (
                  <div className="px-4 py-8 text-xs text-rose-300">
                    <div className="font-semibold">{activeNode.node_id || activeNode.address}</div>
                    <div className="mt-1 font-mono">{activeNode.error}</div>
                  </div>
                ) : activeResult ? (
                  <div>
                    <div className="flex flex-wrap items-center gap-3 px-4 py-2 text-[11px] text-slate-400">
                      <span>{activeResult.row_count.toLocaleString()} rows</span>
                      <span>{activeResult.duration_ms} ms</span>
                      {activeResult.truncated && (
                        <span className="text-amber-300">truncated at {activeResult.row_count.toLocaleString()} rows</span>
                      )}
                      <span className="ml-auto font-mono">{activeNode?.address}</span>
                    </div>
                    <div className="max-h-[60vh] overflow-auto border-t border-slate-700/40">
                      <table className="w-full text-xs">
                        <thead className="sticky top-0 bg-slate-900">
                          <tr className="border-b border-slate-700/40 text-left uppercase tracking-wider text-slate-400">
                            {activeResult.columns.map((column) => (
                              <th key={column} className="whitespace-nowrap px-3 py-2 font-medium">{column}</th>
                            ))}
                          </tr>
                        </thead>
                        <tbody className="divide-y divide-slate-700/20">
                          {activeResult.rows.map((row, rowIndex) => (
                            <tr key={rowIndex} className="hover:bg-slate-800/30">
                              {row.map((cell, cellIndex) => (
                                <td key={cellIndex} className="max-w-[420px] truncate px-3 py-1.5 font-mono text-slate-300" title={formatCell(cell)}>
                                  {formatCell(cell)}
                                </td>
                              ))}
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  </div>
                ) : (
                  <div className="px-4 py-10 text-center text-xs text-slate-400">This node returned no rows.</div>
                )}
              </>
            )}
          </section>
        )}

        <p className="text-[11px] text-slate-500">
          Results are per MonoFS node. With the local logengine, chunks are shard-local and aggregate
          functions return partial values per node; with ClickHouse, results come from one node because
          the tables are shared. Use the Logs, Metrics, or Traces pages for globally merged queries.
        </p>
      </div>
    </div>
  );
}
