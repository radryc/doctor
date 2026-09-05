import { Fragment, useState, useEffect, useCallback, useRef } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import TraceWaterfall from "../components/charts/TraceWaterfall";
import type { TraceSummary, TraceRecord } from "../types/api";

function fmtTime(ts: string): string {
  const d = new Date(ts);
  if (isNaN(d.getTime())) return "-";
  const now = Date.now();
  if (now - d.getTime() < 86400000) {
    return [d.getHours(), d.getMinutes(), d.getSeconds()]
      .map((n) => String(n).padStart(2, "0"))
      .join(":");
  }
  return d.toLocaleDateString() + " " + d.toLocaleTimeString();
}

function payloadEntryCount(span: TraceRecord): number {
  return (
    Object.keys(span.attributes ?? {}).length +
    Object.keys(span.resource_attributes ?? {}).length +
    (span.events?.length ?? 0) +
    (span.status_message ? 1 : 0)
  );
}

/** Decode OTLP any-value JSON blobs into a plain string.
 *  Handles: stringValue, intValue, doubleValue, boolValue, arrayValue, kvlistValue.
 *  Falls back to the raw string if it cannot be parsed. */
function decodeOtlpValue(raw: string): string {
  if (!raw.startsWith("{")) return raw;
  try {
    const v = JSON.parse(raw) as Record<string, unknown>;
    if ("stringValue" in v) return String(v.stringValue);
    if ("intValue" in v) return String(v.intValue);
    if ("doubleValue" in v) return String(v.doubleValue);
    if ("boolValue" in v) return String(v.boolValue);
    if ("arrayValue" in v) {
      const arr = (v.arrayValue as { values?: unknown[] }).values ?? [];
      return arr.map((item) => decodeOtlpValue(JSON.stringify(item))).join(", ");
    }
    if ("kvlistValue" in v) {
      const pairs = (v.kvlistValue as { values?: Array<{ key: string; value: unknown }> }).values ?? [];
      return pairs.map((p) => `${p.key}=${decodeOtlpValue(JSON.stringify(p.value))}`).join(", ");
    }
  } catch {
    // not OTLP JSON — fall through
  }
  return raw;
}

function renderKV(entries: Array<{ key: string; value: string }>) {
  if (entries.length === 0) {
    return <p className="empty">No values.</p>;
  }

  return (
    <div className="trace-kv">
      {entries.map((entry) => {
        const decoded = decodeOtlpValue(entry.value);
        const isLong = decoded.length > 80;
        return (
          <Fragment key={entry.key}>
            <div className="trace-kv-key" title={entry.key}>{entry.key}</div>
            <div className={"trace-kv-val" + (isLong ? " trace-kv-val--wrap" : "")}>{decoded}</div>
          </Fragment>
        );
      })}
    </div>
  );
}

function spanSummary(span: TraceRecord): string {
  const parts = [span.service || "unknown", span.duration_ms || "-"];
  const payloadCount = payloadEntryCount(span);
  if (payloadCount > 0) {
    parts.push(`${payloadCount} payload item${payloadCount === 1 ? "" : "s"}`);
  }
  return parts.join(" • ");
}

function spanMetaEntries(span: TraceRecord): Array<{ key: string; value: string }> {
  return [
    { key: "trace_id", value: span.trace_id },
    { key: "span_id", value: span.span_id },
    { key: "parent_span_id", value: span.parent_span_id ?? "" },
    { key: "kind", value: span.kind ?? "" },
    { key: "status_code", value: span.status_code ?? "" },
    { key: "status_message", value: span.status_message ?? "" },
    { key: "start_time", value: span.start_time },
    { key: "end_time", value: span.end_time },
    { key: "duration", value: span.duration_ms ?? "" },
  ].filter((entry) => entry.value);
}

function attributeEntries(prefix: string, values?: Record<string, string>) {
  return Object.entries(values ?? {}).map(([key, value]) => ({
    key: `${prefix}.${key}`,
    value,
  }));
}

function eventEntries(span: TraceRecord) {
  return (span.events ?? []).flatMap((event, index) => {
    const entries: Array<{ key: string; value: string }> = [
      { key: `event.${index}.name`, value: event.name },
      { key: `event.${index}.timestamp`, value: event.timestamp },
    ];

    Object.entries(event.attributes ?? {}).forEach(([key, value]) => {
      entries.push({ key: `event.${index}.attr.${key}`, value });
    });

    return entries.filter((entry) => entry.value);
  });
}

function SpanTable({ spans }: { spans: TraceRecord[] }) {
  return (
    <section className="result-block">
      <h3>{`Spans (${spans.length})`}</h3>
      <div className="trace-span-table">
        <div className="trace-span-header">
          <span>Name</span>
          <span>Partition</span>
          <span>Duration</span>
          <span>Status</span>
          <span>Start</span>
        </div>
        <div className="trace-span-rows">
          {spans.map((span) => {
            const payloadCount = payloadEntryCount(span);
            const rowKey = `${span.span_id}-${span.start_time}`;
            return (
              <details key={rowKey} className="trace-span-row">
                <summary className="trace-span-summary">
                  <span className="trace-span-name">{span.name || span.span_id}</span>
                  <span className="trace-span-service">{span.service || "-"}</span>
                  <span className="trace-span-duration">{span.duration_ms || "-"}</span>
                  <span className="trace-span-status">{span.status_code || "-"}</span>
                  <span className="trace-span-start">{span.start_time}</span>
                </summary>
                <div className="trace-span-detail">
                  <div className="summary">
                    <span className="pill">{spanSummary(span)}</span>
                    {payloadCount > 0 && (
                      <span className="pill">payload: {payloadCount}</span>
                    )}
                    {(span.events?.length ?? 0) > 0 && (
                      <span className="pill">events: {span.events?.length ?? 0}</span>
                    )}
                  </div>
                  {renderKV(spanMetaEntries(span))}
                  <h4 className="trace-detail-heading">Attributes</h4>
                  {renderKV(attributeEntries("attr", span.attributes))}
                  <h4 className="trace-detail-heading">Resource attributes</h4>
                  {renderKV(attributeEntries("res", span.resource_attributes))}
                  <h4 className="trace-detail-heading">Events</h4>
                  {renderKV(eventEntries(span))}
                </div>
              </details>
            );
          })}
        </div>
      </div>
    </section>
  );
}

function TraceDetailsContent({ spans }: { spans: TraceRecord[] }) {
  return (
    <>
      <TraceWaterfall spans={spans} />
      <SpanTable spans={spans} />
    </>
  );
}

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
        <div className="trace-svc">{trace.service || "-"}</div>
        <div className="trace-tid">{trace.trace_id}</div>
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
            <TraceDetailsContent spans={spans} />
          )}
        </div>
      )}
    </div>
  );
}

// ── Single-trace direct view ──────────────────────────────────────────────────
function SingleTraceView({ traceId, tenant, onBack }: { traceId: string; tenant: string; onBack: () => void }) {
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
        <button className="xp-back-btn" onClick={onBack}>← Back to list</button>
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
              <TraceDetailsContent spans={spans} />
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

  const [minutes, setMinutes] = useState(60);
  const [service, setService] = useState("");
  const [textFilter, setTextFilter] = useState("");
  const [liveOn, setLiveOn] = useState(false);
  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [services, setServices] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [status, setStatus] = useState("Searching…");

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
      const data = await api.getRecentTraces(params);
      setTraces(data.traces ?? []);
    } catch (err) {
      setStatus("Error: " + (err instanceof Error ? err.message : "unknown"));
    } finally {
      setLoading(false);
    }
  }, [config.default_tenant, minutes, service]);

  // Load services once
  useEffect(() => {
    api
      .getLogServices(config.default_tenant)
      .then((d) => setServices(d.services ?? []))
      .catch(() => {});
  }, [config.default_tenant]);

  useEffect(() => {
    void load();
  }, [load]);

  // Live refresh
  useEffect(() => {
    if (!liveOn) return;
    const t = setInterval(() => void load(), 30000);
    return () => clearInterval(t);
  }, [liveOn, load]);

  // Derived filtered list
  const tf = textFilter.toLowerCase();
  const filtered = traces.filter((t) => {
    if (service && t.service !== service) return false;
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

  // Debounce text filter
  const [inputVal, setInputVal] = useState("");
  useEffect(() => {
    const t = setTimeout(() => setTextFilter(inputVal.trim()), 280);
    return () => clearTimeout(t);
  }, [inputVal]);

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
          <select
            className="xp-select"
            value={service}
            onChange={(e) => setService(e.target.value)}
          >
            <option value="">All partitions</option>
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
            title="Auto-refresh every 5 s"
          >
            &#9679; Live
          </button>
        </div>
      </div>

      <div className="panel-body">
        <div className="xp-body">
          <div className="xp-table-header traces-header">
            <span>Partition</span>
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
