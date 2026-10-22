import { useState, useEffect, useCallback, useRef } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Check, Copy, Link2, Download } from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import { useTimeRange } from "../context/TimeRangeContext";
import { copyText } from "../lib/clipboard";
import { downloadJSON } from "../lib/download";
import LogHistogram from "../components/charts/LogHistogram";
import LogFacetSidebar from "../components/logs/LogFacetSidebar";
import type { LogRecord } from "../types/api";

type SevKey = "ERR" | "WARN" | "INFO" | "DBG";

function sevKey(text: string): SevKey {
  const s = (text ?? "").toUpperCase();
  if (s.includes("FATAL") || s.includes("ERROR")) return "ERR";
  if (s.includes("WARN")) return "WARN";
  if (s.includes("DEBUG") || s.includes("TRACE")) return "DBG";
  return "INFO";
}

function fmtTime(ts: string): string {
  const d = new Date(ts);
  if (isNaN(d.getTime())) return "-";
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}

function barClass(k: SevKey): string {
  return { ERR: "bar-err", WARN: "bar-warn", INFO: "bar-info", DBG: "bar-dbg" }[k];
}

function badgeClass(k: SevKey): string {
  return { ERR: "badge-err", WARN: "badge-warn", INFO: "badge-info", DBG: "badge-dbg" }[k];
}

function logPartition(record: LogRecord): string {
  return (
    record.partition ||
    record.resource_attributes?.["guardian_partition"] ||
    record.resource_attributes?.["guardian.partition"] ||
    record.resource_attributes?.["partition"] ||
    ""
  );
}

interface LogRowProps {
  record: LogRecord;
  onOpenTrace: (id: string) => void;
}

function LogRow({ record, onOpenTrace }: LogRowProps) {
  const [expanded, setExpanded] = useState(false);
  const k = sevKey(record.severity_text ?? "");
  const comp =
    record.resource_attributes?.["guardian.component"] ?? record.service ?? "";

  const kv: Record<string, string> = {
    timestamp: new Date(record.timestamp).toLocaleString(),
    service: record.service ?? "",
    partition: logPartition(record),
    intent: record.intent ?? "",
    asset: record.asset ?? "",
    severity: record.severity_text ?? "",
  };
  if (record.trace_id) {
    kv.trace_id = record.trace_id;
    kv.span_id = record.span_id ?? "";
  }
  if (record.resource_attributes) {
    Object.entries(record.resource_attributes).forEach(
      ([rk, rv]) => (kv[`res.${rk}`] = rv)
    );
  }
  if (record.attributes) {
    Object.entries(record.attributes).forEach(
      ([ak, av]) => (kv[`attr.${ak}`] = av)
    );
  }

  return (
    <div
      className={"log-row" + (expanded ? " xp-expanded" : "")}
      onClick={() => setExpanded((v) => !v)}
    >
      <div className={"log-sev-bar " + barClass(k)} />
      <div className="log-time">{fmtTime(record.timestamp)}</div>
      <div className={"log-badge " + badgeClass(k)}>{k}</div>
      <div className="log-comp" title={comp}>
        <span className="log-partition">{logPartition(record) || "—"}</span>
        {comp && <span className="log-service">{comp}</span>}
      </div>
      <div className="log-body-col">
        <div className="log-body-text">{record.body}</div>
        <div className="log-expand-detail">
          <div className="log-kv">
            {Object.entries(kv)
              .filter(([, v]) => v)
              .map(([key, val]) => (
                <>
                  <div key={`k-${key}`} className="log-kv-key">{key}</div>
                  <div key={`v-${key}`} className="log-kv-val">{val}</div>
                </>
              ))}
          </div>
          {record.trace_id && (
            <div style={{ display: "flex", gap: "0.5rem", flexWrap: "wrap" }}>
              <button
                className="log-trace-btn"
                onClick={(e) => {
                  e.stopPropagation();
                  onOpenTrace(record.trace_id!);
                }}
              >
                &#8594; View trace {record.trace_id.slice(0, 16)}…
              </button>
              <button
                className="copy-btn"
                title="Copy trace ID"
                onClick={(e) => {
                  e.stopPropagation();
                  void copyText(record.trace_id!);
                }}
              >
                <Copy size={10} /> Copy trace ID
              </button>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

const SEV_PILLS: { key: SevKey; cls: string; label: string }[] = [
  { key: "ERR", cls: "sev-pill-err", label: "ERR" },
  { key: "WARN", cls: "sev-pill-warn", label: "WARN" },
  { key: "INFO", cls: "sev-pill-info", label: "INFO" },
  { key: "DBG", cls: "sev-pill-dbg", label: "DBG" },
];

function parseSevParam(value: string | null): Record<SevKey, boolean> {
  const all = { ERR: true, WARN: true, INFO: true, DBG: true };
  if (!value) return all;
  const requested = value
    .split(",")
    .map((part) => part.trim().toUpperCase())
    .filter((part): part is SevKey => part === "ERR" || part === "WARN" || part === "INFO" || part === "DBG");
  if (requested.length === 0) return all;
  return {
    ERR: requested.includes("ERR"),
    WARN: requested.includes("WARN"),
    INFO: requested.includes("INFO"),
    DBG: requested.includes("DBG"),
  };
}

export default function LogsPage() {
  const { config } = useConfig();
  const navigate = useNavigate();
  const tr = useTimeRange();
  const [searchParams] = useSearchParams();

  const [componentFilter, setComponentFilter] = useState(searchParams.get("service") ?? "");
  const [partitionFilter, setPartitionFilter] = useState(searchParams.get("partition") ?? "");
  const [logGroup, setLogGroup] = useState(searchParams.get("logGroup") ?? "");
  const [sevActive, setSevActive] = useState<Record<SevKey, boolean>>(() =>
    parseSevParam(searchParams.get("sev"))
  );
  const [inputVal, setInputVal] = useState("");
  const [textFilter, setTextFilter] = useState("");
  const [queryInput, setQueryInput] = useState(searchParams.get("q") ?? "");
  const [queryApplied, setQueryApplied] = useState(searchParams.get("q") ?? "");
  const [showHelp, setShowHelp] = useState(false);
  const [liveOn, setLiveOn] = useState(false);
  const [allRecords, setAllRecords] = useState<LogRecord[]>([]);
  const [services, setServices] = useState<string[]>([]);
  const [partitions, setPartitions] = useState<string[]>([]);
  const [logGroups, setLogGroups] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [status, setStatus] = useState("Searching…");
  const [copiedView, setCopiedView] = useState(false);
  const [facetFilters, setFacetFilters] = useState<Record<string, string>>({});
  const autoAwsTried = useRef(false);

  useEffect(() => {
    api
      .getLogServices(config.default_tenant, partitionFilter, false, true)
      .then((d) => {
        setServices(d.services ?? []);
        setLogGroups(d.log_groups ?? []);
        setPartitions((prev) => [...new Set([...prev, ...(d.partitions ?? [])])].sort());
      })
      .catch(() => {});
  }, [config.default_tenant, partitionFilter]);

  useEffect(() => {
    const service = searchParams.get("service");
    if (service) setComponentFilter(service);
  }, [searchParams]);

  // Keep an explicitly requested service/log group even when the telemetry
  // scan does not list it (the catalog only covers the recent window). Clearing
  // it here silently turns a canonical link such as
  // /logs?service=div-a-orders.app.DivAOrdersStack into the global firehose,
  // which shows unrelated partitions instead of "no matches".
  const serviceOptions = componentFilter && !services.includes(componentFilter)
    ? [componentFilter, ...services]
    : services;
  const logGroupOptions = logGroup && !logGroups.includes(logGroup)
    ? [logGroup, ...logGroups]
    : logGroups;

  useEffect(() => {
    autoAwsTried.current = false;
  }, [partitionFilter]);

  useEffect(() => {
    if (!autoAwsTried.current && !loading && !logGroup && allRecords.length === 0 && logGroups.length > 0) {
      autoAwsTried.current = true;
      setLogGroup(logGroups[0]);
    }
  }, [allRecords.length, loading, logGroup, logGroups]);

  useEffect(() => {
    const seen = new Set(partitions);
    for (const record of allRecords) {
      const partition = logPartition(record);
      if (partition) seen.add(partition);
    }
    const next = [...seen].sort();
    if (next.length !== partitions.length || next.some((value, index) => value !== partitions[index])) {
      setPartitions(next);
    }
  }, [allRecords, partitions]);

  // Keep params in sync when navigated here from elsewhere.
  const paramKey = searchParams.toString();
  useEffect(() => {
    const nextQuery = searchParams.get("q") ?? "";
    setQueryInput(nextQuery);
    setQueryApplied(nextQuery);
    setPartitionFilter(searchParams.get("partition") ?? "");
    setLogGroup(searchParams.get("logGroup") ?? "");
    setSevActive(parseSevParam(searchParams.get("sev")));
  }, [paramKey]);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const { from, to } = tr.resolve();
      const params = new URLSearchParams({
        tenant: config.default_tenant,
        from: from.toISOString(),
        to: to.toISOString(),
        limit: "500",
      });
      if (queryApplied) params.set("q", queryApplied);
      // Filter server-side as well: a canonical service may not appear in the
      // first page of the tenant-wide firehose.
      if (componentFilter) params.set("service", componentFilter);
      if (partitionFilter) params.set("partition", partitionFilter);
      if (logGroup) params.set("logGroup", logGroup);
      const data = await api.searchLogs(params);
      setAllRecords(data.results ?? []);
    } catch (err) {
      setStatus("Error: " + (err instanceof Error ? err.message : "unknown"));
    } finally {
      setLoading(false);
    }
  }, [config.default_tenant, tr, queryApplied, componentFilter, partitionFilter, logGroup]);

  useEffect(() => {
    void load();
  }, [load, tr.refreshTick]);

  useEffect(() => {
    if (!liveOn) return;
    const t = setInterval(() => void load(), 30000);
    return () => clearInterval(t);
  }, [liveOn, load]);

  // Debounce text filter
  useEffect(() => {
    const t = setTimeout(() => setTextFilter(inputVal.trim()), 280);
    return () => clearTimeout(t);
  }, [inputVal]);

  // Filtered records
  const tf = textFilter.toLowerCase();
  const selectedPartition = partitionFilter.trim();
  const filtered = allRecords.filter((r) => {
    const k = sevKey(r.severity_text ?? "");
    if (!sevActive[k]) return false;
    const comp =
      r.resource_attributes?.["guardian.component"] ?? r.service ?? "";
    if (componentFilter && comp !== componentFilter) return false;
    if (selectedPartition && logPartition(r) !== selectedPartition) return false;
    if (tf) {
      if (
        !(r.body ?? "").toLowerCase().includes(tf) &&
        !(r.service ?? "").toLowerCase().includes(tf) &&
        !comp.toLowerCase().includes(tf)
      )
        return false;
    }
    // Facet filters from sidebar
    for (const [field, value] of Object.entries(facetFilters)) {
      const rv = getFieldValue(r, field);
      if (rv !== value) return false;
    }
    return true;
  });

  const rangeLabel = tr.rangeString();

  useEffect(() => {
    if (loading) {
      setStatus("Searching…");
    } else if (allRecords.length === 0) {
      setStatus(`0 logs — last ${rangeLabel}`);
    } else {
      setStatus(`${filtered.length} of ${allRecords.length} logs — last ${rangeLabel}`);
    }
  }, [loading, filtered.length, allRecords.length, rangeLabel]);

  const openTrace = (traceId: string) => {
    navigate("/traces?trace_id=" + encodeURIComponent(traceId));
  };

  const copyViewLink = () => {
    const params = new URLSearchParams();
    if (componentFilter) params.set("service", componentFilter);
    if (partitionFilter) params.set("partition", partitionFilter);
    if (logGroup) params.set("logGroup", logGroup);
    if (queryApplied) params.set("q", queryApplied);
    const active = (Object.keys(sevActive) as SevKey[]).filter((key) => sevActive[key]);
    if (active.length > 0 && active.length < 4) params.set("sev", active.join(","));
    const base = window.location.href.split("#")[0];
    void copyText(`${base}#/logs?${params.toString()}`).then((ok) => {
      if (!ok) return;
      setCopiedView(true);
      window.setTimeout(() => setCopiedView(false), 1600);
    });
  };

  const handleFacetInclude = (field: string, value: string) => {
    setFacetFilters((prev) => ({ ...prev, [field]: value }));
  };

  const handleFacetExclude = (field: string, value: string) => {
    // Add a negated query fragment to textFilter
    setQueryInput((q) => q ? `${q} AND NOT ${field}="${value}"` : `NOT ${field}="${value}"`);
    setQueryApplied((q) => q ? `${q} AND NOT ${field}="${value}"` : `NOT ${field}="${value}"`);
  };

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Logs</h2>
          <p>{status}</p>
        </div>
        <div className="xp-toolbar">
          <label className="filter-label">
            Partition
            <select
              className="xp-select"
              value={partitionFilter}
              onChange={(e) => setPartitionFilter(e.target.value)}
            >
              <option value="">All</option>
              {partitions.map((partition) => (
                <option key={partition} value={partition}>{partition}</option>
              ))}
            </select>
          </label>
          <select
            className="xp-select"
            value={componentFilter}
            onChange={(e) => setComponentFilter(e.target.value)}
          >
            <option value="">All services</option>
            {serviceOptions.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
          {(logGroupOptions.length > 0 || logGroup) && (
            <label className="filter-label">
              CloudWatch log group
              <select
                className="xp-select"
                value={logGroup}
                onChange={(e) => {
                  setLogGroup(e.target.value);
                  if (e.target.value) setComponentFilter("");
                }}
              >
                <option value="">All local logs</option>
                {logGroupOptions.map((group) => (
                  <option key={group} value={group}>{group}</option>
                ))}
              </select>
            </label>
          )}
          <div className="sev-filter">
            {SEV_PILLS.map(({ key, cls, label }) => (
              <button
                key={key}
                className={`sev-pill ${cls}` + (sevActive[key] ? " xp-active" : "")}
                onClick={() =>
                  setSevActive((prev) => ({ ...prev, [key]: !prev[key] }))
                }
              >
                {label}
              </button>
            ))}
          </div>
          <input
            type="text"
            className="xp-search"
            placeholder="Filter loaded results…"
            value={inputVal}
            onChange={(e) => setInputVal(e.target.value)}
          />
          <button
            className={"xp-live-btn" + (liveOn ? " xp-live-on" : "")}
            onClick={() => setLiveOn((v) => !v)}
            title="Auto-refresh every 30 s"
          >
            &#9679; Live
          </button>
          <button
            className="xp-live-btn"
            onClick={() => downloadJSON(filtered, "logs.json")}
            title="Download filtered logs as JSON"
          >
            <Download size={11} /> Download
          </button>
          <button
            className="xp-live-btn"
            onClick={copyViewLink}
            title="Copy a shareable link to this log view"
          >
            {copiedView ? <><Check size={11} /> Copied</> : <><Link2 size={11} /> Copy view</>}
          </button>
        </div>
      </div>

      {/* ── Query bar ──────────────────────────────────────────────────── */}
      <div className="log-query-bar">
        <form
          className="log-query-form"
          onSubmit={(e) => {
            e.preventDefault();
            setQueryApplied(queryInput.trim());
          }}
        >
          <span className="log-query-label">Query</span>
          <input
            type="text"
            className="log-query-input"
            placeholder='Search log bodies — e.g. error  or  timeout  or  {service="guardiand"} |= "error" (LogQL)'
            value={queryInput}
            onChange={(e) => setQueryInput(e.target.value)}
            spellCheck={false}
          />
          <button type="submit" className="log-query-run">Run</button>
          {queryApplied && (
            <button
              type="button"
              className="log-query-clear"
              onClick={() => { setQueryInput(""); setQueryApplied(""); }}
            >
              ✕ Clear
            </button>
          )}
          <button
            type="button"
            className={"log-query-help-btn" + (showHelp ? " active" : "")}
            onClick={() => setShowHelp((v) => !v)}
            title="Query syntax help"
          >
            ?
          </button>
        </form>
        {showHelp && (
          <div className="log-query-help">
            <div className="log-query-help-title">Log Query Reference</div>
            <div className="log-query-help-grid">
              <div className="log-query-help-section">
                <div className="log-query-help-head">Text search</div>
                <div className="log-query-help-row">
                  <code>error</code>
                  <span>Lines containing "error" (case-sensitive)</span>
                </div>
                <div className="log-query-help-row">
                  <code>timeout</code>
                  <span>Lines containing "timeout"</span>
                </div>
                <div className="log-query-help-row">
                  <code>connection refused</code>
                  <span>Lines containing the full phrase</span>
                </div>
              </div>
              <div className="log-query-help-section">
                <div className="log-query-help-head">Tips</div>
                <div className="log-query-help-row">
                  <code>&#9679; Time range</code>
                  <span>Use the global time picker in the top bar to control the window</span>
                </div>
                <div className="log-query-help-row">
                  <code>&#9679; Facet sidebar</code>
                  <span>Click + / − on any field value to include or exclude it</span>
                </div>
                <div className="log-query-help-row">
                  <code>&#9679; Download</code>
                  <span>Export filtered results as JSON</span>
                </div>
              </div>
            </div>
          </div>
        )}
        {/* Active facet filter chips */}
        {Object.entries(facetFilters).length > 0 && (
          <div className="log-facet-chips">
            {Object.entries(facetFilters).map(([field, value]) => (
              <span key={field} className="facet-chip">
                {field}="{value}"
                <button
                  className="facet-chip__remove"
                  onClick={() => setFacetFilters((prev) => {
                    const next = { ...prev };
                    delete next[field];
                    return next;
                  })}
                >
                  ×
                </button>
              </span>
            ))}
          </div>
        )}
      </div>

      {/* 3-pane layout: sidebar + histogram + log table */}
      <div className="panel-body">
        <div className="logs-3pane">
          {/* Left: Facet sidebar */}
          <div className="logs-3pane__sidebar">
            <LogFacetSidebar
              records={allRecords}
              activeFilters={facetFilters}
              onInclude={handleFacetInclude}
              onExclude={handleFacetExclude}
            />
          </div>

          {/* Right: histogram + table */}
          <div className="logs-3pane__main">
            <div className="logs-hist-wrap">
              <LogHistogram records={allRecords} />
            </div>
            <div className="xp-table-header log-header">
              <span>Time</span>
              <span>Sev</span>
              <span>Partition / Service</span>
              <span>Message</span>
            </div>
            <div className="xp-rows">
              {filtered.length === 0 ? (
                <div className="xp-empty">
                  {allRecords.length === 0
                    ? "No logs found for this time range."
                    : "No logs match the current filters."}
                </div>
              ) : (
                filtered.map((r, i) => (
                  <LogRow key={i} record={r} onOpenTrace={openTrace} />
                ))
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

function getFieldValue(r: LogRecord, field: string): string {
  if (field === "severity_text") {
    const s = (r.severity_text ?? "INFO").toUpperCase();
    if (s.includes("ERROR") || s.includes("FATAL")) return "ERROR";
    if (s.includes("WARN")) return "WARN";
    if (s.includes("DEBUG") || s.includes("TRACE")) return "DEBUG";
    return "INFO";
  }
  if (field === "service") return r.service ?? "";
  if (field === "partition") return logPartition(r);
  if (field === "intent") return r.intent ?? "";
  if (field === "asset") return r.asset ?? "";
  if (field.startsWith("attr.")) return r.attributes?.[field.slice(5)] ?? "";
  if (field.startsWith("res.")) return r.resource_attributes?.[field.slice(4)] ?? "";
  return "";
}
