import { useState, useEffect, useCallback } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import LogHistogram from "../components/charts/LogHistogram";
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
      <div className="log-comp">{comp}</div>
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
            <button
              className="log-trace-btn"
              onClick={(e) => {
                e.stopPropagation();
                onOpenTrace(record.trace_id!);
              }}
            >
              &#8594; View trace {record.trace_id.slice(0, 16)}…
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

const PRESETS = [15, 60, 360, 1440];
const SEV_PILLS: { key: SevKey; cls: string; label: string }[] = [
  { key: "ERR", cls: "sev-pill-err", label: "ERR" },
  { key: "WARN", cls: "sev-pill-warn", label: "WARN" },
  { key: "INFO", cls: "sev-pill-info", label: "INFO" },
  { key: "DBG", cls: "sev-pill-dbg", label: "DBG" },
];

export default function LogsPage() {
  const { config } = useConfig();
  const navigate = useNavigate();

  const [minutes, setMinutes] = useState(60);
  const [service, setService] = useState("");
  const [sevActive, setSevActive] = useState<Record<SevKey, boolean>>({
    ERR: true, WARN: true, INFO: true, DBG: true,
  });
  const [inputVal, setInputVal] = useState("");
  const [textFilter, setTextFilter] = useState("");
  const [queryInput, setQueryInput] = useState("");
  const [queryApplied, setQueryApplied] = useState("");
  const [showHelp, setShowHelp] = useState(false);
  const [liveOn, setLiveOn] = useState(false);
  const [allRecords, setAllRecords] = useState<LogRecord[]>([]);
  const [services, setServices] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [status, setStatus] = useState("Searching…");

  // Load services once
  useEffect(() => {
    api
      .getLogServices(config.default_tenant)
      .then((d) => setServices(d.services ?? []))
      .catch(() => {});
  }, [config.default_tenant]);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const to = new Date();
      const from = new Date(to.getTime() - minutes * 60000);
      const params = new URLSearchParams({
        tenant: config.default_tenant,
        from: from.toISOString(),
        to: to.toISOString(),
        limit: "500",
      });
      if (service) params.set("service", service);
      if (queryApplied) params.set("q", queryApplied);
      const data = await api.searchLogs(params);
      setAllRecords(data.results ?? []);
    } catch (err) {
      setStatus("Error: " + (err instanceof Error ? err.message : "unknown"));
    } finally {
      setLoading(false);
    }
  }, [config.default_tenant, minutes, service, queryApplied]);

  useEffect(() => {
    void load();
  }, [load]);

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
  const filtered = allRecords.filter((r) => {
    const k = sevKey(r.severity_text ?? "");
    if (!sevActive[k]) return false;
    if (tf) {
      const comp =
        r.resource_attributes?.["guardian.component"] ?? r.service ?? "";
      if (
        !(r.body ?? "").toLowerCase().includes(tf) &&
        !(r.service ?? "").toLowerCase().includes(tf) &&
        !comp.toLowerCase().includes(tf)
      )
        return false;
    }
    return true;
  });

  const rangeLabel = minutes < 60 ? `${minutes}m` : `${minutes / 60}h`;

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

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Logs</h2>
          <p>{status}</p>
        </div>
        <div className="xp-toolbar">
          <div className="xp-presets">
            {PRESETS.map((m) => (
              <button
                key={m}
                className={"xp-preset" + (minutes === m ? " xp-active" : "")}
                onClick={() => setMinutes(m)}
              >
                {m < 60 ? `${m}m` : `${m / 60}h`}
              </button>
            ))}
          </div>
          <select
            className="xp-select"
            value={service}
            onChange={(e) => setService(e.target.value)}
          >
            <option value="">All services</option>
            {services.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
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
            placeholder='Search log bodies — e.g.  error   or   timeout'
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
                  <span>Use the 15m / 1h / 6h / 24h presets to narrow the window</span>
                </div>
                <div className="log-query-help-row">
                  <code>&#9679; Service filter</code>
                  <span>Pick a service in the dropdown — results are pre-filtered server-side</span>
                </div>
                <div className="log-query-help-row">
                  <code>&#9679; Quick filter</code>
                  <span>"Filter loaded results…" box filters already-fetched rows instantly (no round-trip)</span>
                </div>
                <div className="log-query-help-row">
                  <code>&#9679; Severity pills</code>
                  <span>ERR / WARN / INFO / DBG toggle visibility of matching severity levels</span>
                </div>
                <div className="log-query-help-row">
                  <code>&#9679; Live</code>
                  <span>Re-fetches every 30 s when enabled — disable while browsing</span>
                </div>
              </div>
            </div>
          </div>
        )}
      </div>

      <div className="panel-body">
        <div className="xp-body">
          <div className="logs-hist-wrap">
            <LogHistogram records={allRecords} />
          </div>
          <div className="xp-table-header log-header">
            <span>Time</span>
            <span>Sev</span>
            <span>Component</span>
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
  );
}
