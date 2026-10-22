import { useState, useEffect, useCallback, useRef } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Check, Copy, FileText, Link2 } from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import { copyText } from "../lib/clipboard";
import TraceDetails, { fmtTime } from "../components/charts/TraceDetails";
import type { TraceSummary, TraceRecord } from "../types/api";

interface TraceRowProps {
  trace: TraceSummary;
  tenant: string;
  onOpenTrace?: (traceId: string) => void;
}

function TraceRow({ trace, tenant, onOpenTrace: _onOpenTrace }: TraceRowProps) {
  const [expanded, setExpanded] = useState(false);
  const [spans, setSpans] = useState<TraceRecord[] | null>(null);
  const [wfError, setWfError] = useState<string | null>(null);
  const loaded = useRef(false);

  const toggle = () => {
    setExpanded((v) => !v);
    if (!loaded.current) {
      loaded.current = true;
      api
        .getTrace(trace.trace_id, tenant)
        .then((data) => {
          const enriched = (data.spans ?? []).map((s) => {
            const d = new Date(s.end_time).getTime() - new Date(s.start_time).getTime();
            return {
              ...s,
              duration_ms: isNaN(d) || d < 0 ? "-" : d >= 1000 ? `${(d / 1000).toFixed(2)}s` : `${d}ms`,
            };
          });
          setSpans(enriched);
        })
        .catch((err: unknown) => {
          setWfError(err instanceof Error ? err.message : "Load failed");
        });
    }
  };

  return (
    <div className={"trace-row" + (expanded ? " xp-expanded" : "")}>
      <div className="trace-cells" onClick={toggle}>
        <div className="trace-partition-cell" title={trace.service}>
          <span className="trace-part">{trace.partition || "—"}</span>
          {trace.service && <span className="trace-svc">{trace.service}</span>}
        </div>
        <div className="trace-tid">
          {trace.trace_id}
          <button
            type="button"
            className="copy-mini"
            title="Copy trace ID"
            onClick={(event) => {
              event.stopPropagation();
              void copyText(trace.trace_id);
            }}
          >
            <Copy size={11} />
          </button>
        </div>
        <div className="trace-spans">{trace.span_count ?? "-"}</div>
        <div className="trace-time">{fmtTime(trace.max_time)}</div>
      </div>
      {expanded && (
        <div className="trace-wf-wrap">
          {wfError ? (
            <div style={{ color: "var(--err)", fontSize: "0.84rem" }}>{wfError}</div>
          ) : spans === null ? (
            <div style={{ color: "var(--muted)", fontSize: "0.84rem" }}>
              Loading waterfall…
            </div>
          ) : (
            <TraceDetails spans={spans} />
          )}
        </div>
      )}
    </div>
  );
}

// ── Single-trace direct view ──────────────────────────────────────────────────
function SingleTraceView({ traceId, tenant, onBack }: { traceId: string; tenant: string; onBack: () => void }) {
  const navigate = useNavigate();
  const [spans, setSpans] = useState<TraceRecord[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setSpans(null);
    setError(null);
    api
      .getTrace(traceId, tenant)
      .then((data) => {
        const enriched = (data.spans ?? []).map((s) => {
          const d = new Date(s.end_time).getTime() - new Date(s.start_time).getTime();
          return {
            ...s,
            duration_ms: isNaN(d) || d < 0 ? "-" : d >= 1000 ? `${(d / 1000).toFixed(2)}s` : `${d}ms`,
          };
        });
        setSpans(enriched);
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : "Load failed"));
  }, [traceId, tenant]);

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Trace</h2>
          <p className="trace-direct-id">{traceId}</p>
        </div>
        <div className="xp-toolbar">
          <button
            className="xp-live-btn"
            onClick={() => void copyText(traceId)}
            title="Copy trace ID"
          >
            <Copy size={12} /> Copy ID
          </button>
          <button
            className="xp-live-btn"
            onClick={() => navigate(`/logs?q=${encodeURIComponent(traceId)}&range=1440`)}
            title="Search logs carrying this trace ID"
          >
            <FileText size={12} /> Logs for trace
          </button>
          <button className="xp-back-btn" onClick={onBack}>← Back to list</button>
        </div>
      </div>
      <div className="panel-body">
        <div style={{ flex: 1, minHeight: 0, overflowY: "auto" }}>
          {error ? (
            <div style={{ color: "var(--err)", fontSize: "0.88rem", padding: "1rem" }}>{error}</div>
          ) : spans === null ? (
            <div style={{ color: "var(--muted)", fontSize: "0.88rem", padding: "1rem" }}>Loading trace…</div>
          ) : spans.length === 0 ? (
            <div style={{ color: "var(--muted)", fontSize: "0.88rem", padding: "1rem" }}>No spans found for this trace ID.</div>
          ) : (
            <div style={{ display: "grid", gap: "1rem", padding: "0.25rem 0" }}>
              <TraceDetails spans={spans} />
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

export default function TracesPage() {
  const { config } = useConfig();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();

  const focusedTraceId = searchParams.get("trace_id") ?? "";
  const initialRange = Number(searchParams.get("range"));

  const [minutes, setMinutes] = useState([15, 60, 360, 1440].includes(initialRange) ? initialRange : 60);
  const [service, setService] = useState(searchParams.get("service") ?? "");
  const [partitionFilter, setPartitionFilter] = useState(searchParams.get("partition") ?? "");
  const [textFilter, setTextFilter] = useState("");
  const [liveOn, setLiveOn] = useState(false);
  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [services, setServices] = useState<string[]>([]);
  const [partitions, setPartitions] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [status, setStatus] = useState("Searching…");
  const [copiedView, setCopiedView] = useState(false);
  const [inputVal, setInputVal] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const to = new Date();
      const from = new Date(to.getTime() - minutes * 60000);
      const params = new URLSearchParams({
        tenant: config.default_tenant,
        from: from.toISOString(),
        to: to.toISOString(),
        limit: "200",
      });
      if (service) params.set("service", service);
      if (partitionFilter) params.set("partition", partitionFilter);
      const data = await api.getRecentTraces(params);
      setTraces(data.traces ?? []);
    } catch (err) {
      setStatus("Error: " + (err instanceof Error ? err.message : "unknown"));
    } finally {
      setLoading(false);
    }
  }, [config.default_tenant, minutes, service, partitionFilter]);

  // Load the service list for the selected partition (and the full partition
  // list seen in telemetry).
  useEffect(() => {
    api
      .getLogServices(config.default_tenant, partitionFilter, true)
      .then((d) => {
        setServices(d.services ?? []);
        setPartitions((prev) => [...new Set([...prev, ...(d.partitions ?? [])])].sort());
      })
      .catch(() => {});
  }, [config.default_tenant, partitionFilter]);

  useEffect(() => {
    if (service && !services.includes(service)) {
      setService("");
    }
  }, [service, services]);

  // Keep the partition picker aligned with the traces currently loaded. This
  // covers partitions that have traces but no log records in the scan window.
  useEffect(() => {
    const seen = new Set(partitions);
    for (const trace of traces) {
      if (trace.partition) seen.add(trace.partition);
    }
    const next = [...seen].sort();
    if (next.length !== partitions.length || next.some((value, index) => value !== partitions[index])) {
      setPartitions(next);
    }
  }, [traces, partitions]);

  // Nested route changes (dashboard triage links, command palette) should
  // replace the current investigation window without a full remount.
  const paramKey = searchParams.toString();
  useEffect(() => {
    const range = Number(searchParams.get("range"));
    if ([15, 60, 360, 1440].includes(range)) setMinutes(range);
    setPartitionFilter(searchParams.get("partition") ?? "");
    setService(searchParams.get("service") ?? "");
  }, [paramKey]);

  useEffect(() => {
    void load();
  }, [load]);

  // Live refresh
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

  // Derived filtered list
  const tf = textFilter.toLowerCase();
  const selectedPartition = partitionFilter.trim();
  const filtered = traces.filter((t) => {
    if (service && t.service !== service) return false;
    if (selectedPartition && t.partition !== selectedPartition) return false;
    if (tf && !t.trace_id.toLowerCase().includes(tf) && !(t.service ?? "").toLowerCase().includes(tf))
      return false;
    return true;
  });

  const rangeLabel = minutes < 60 ? `${minutes}m` : `${minutes / 60}h`;

  useEffect(() => {
    if (loading) {
      setStatus("Searching…");
    } else if (filtered.length === 0) {
      setStatus(`0 traces — last ${rangeLabel}`);
    } else {
      setStatus(`${filtered.length} traces — last ${rangeLabel}`);
    }
  }, [loading, filtered.length, rangeLabel]);

  const openTrace = (traceId: string) => {
    navigate("/traces?trace_id=" + encodeURIComponent(traceId));
  };

  const copyViewLink = () => {
    const params = new URLSearchParams();
    params.set("range", String(minutes));
    if (partitionFilter) params.set("partition", partitionFilter);
    const base = window.location.href.split("#")[0];
    void copyText(`${base}#/traces?${params.toString()}`).then((ok) => {
      if (!ok) return;
      setCopiedView(true);
      window.setTimeout(() => setCopiedView(false), 1600);
    });
  };

  // ── Direct trace link: show single-trace view ──────────────────────────────
  if (focusedTraceId) {
    return (
      <SingleTraceView
        traceId={focusedTraceId}
        tenant={config.default_tenant}
        onBack={() => navigate("/traces")}
      />
    );
  }

  const presets = [15, 60, 360, 1440];

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Traces</h2>
          <p>{status}</p>
        </div>
        <div className="xp-toolbar">
          <div className="xp-presets">
            {presets.map((m) => (
              <button
                key={m}
                className={"xp-preset" + (minutes === m ? " xp-active" : "")}
                onClick={() => setMinutes(m)}
              >
                {m < 60 ? `${m}m` : `${m / 60}h`}
              </button>
            ))}
          </div>
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
            value={service}
            onChange={(e) => setService(e.target.value)}
          >
            <option value="">All services</option>
            {services.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
          <input
            type="text"
            className="xp-search"
            placeholder="Filter trace ID or partition…"
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
            onClick={copyViewLink}
            title="Copy a shareable link to this trace view"
          >
            {copiedView ? <><Check size={11} /> Copied</> : <><Link2 size={11} /> Copy view</>}
          </button>
        </div>
      </div>

      <div className="panel-body">
        <div className="xp-body">
          <div className="xp-table-header traces-header">
            <span>Partition / Service</span>
            <span>Trace ID</span>
            <span>Spans</span>
            <span>Time</span>
          </div>
          <div className="xp-rows">
            {filtered.length === 0 ? (
              <div className="xp-empty">
                {traces.length === 0
                  ? "No traces found for this time range."
                  : "No traces match the current filters."}
              </div>
            ) : (
              filtered.map((t) => (
                <TraceRow
                  key={t.trace_id}
                  trace={t}
                  tenant={config.default_tenant}
                  onOpenTrace={openTrace}
                />
              ))
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
