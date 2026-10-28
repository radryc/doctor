import { useState, useEffect, useCallback, useRef } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Check, Copy, FileText, Link2, Loader2 } from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import { useTimeRange } from "../context/TimeRangeContext";
import { copyText } from "../lib/clipboard";
import { splitServiceIdentity } from "../lib/logs";
import TraceDetails, { fmtTime } from "../components/charts/TraceDetails";
import type { LogIdentityInfo, TraceSummary, TraceRecord } from "../types/api";

// Accepts the two formats Doctor emits and returns the canonical 32-hex form:
//   W3C:  4bf92f3577b34da6a3ce929d0e0e4736
//   X-Ray: 1-4bf92f35-77b34da6a3ce929d0e0e4736
function canonicalTraceId(raw: string): string {
  const value = raw.trim().toLowerCase();
  if (/^[0-9a-f]{32}$/.test(value)) {
    return /[1-9a-f]/.test(value) ? value : "";
  }
  const match = /^1-([0-9a-f]{8})-([0-9a-f]{24})$/.exec(value);
  return match ? match[1] + match[2] : "";
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
  const tr = useTimeRange();
  const { from, to } = tr.resolve();
  const rangeLabel = tr.rangeString();

  const focusedTraceId = searchParams.get("trace_id") ?? "";

  const initialService = searchParams.get("service") ?? "";
  const initialIdentity = splitServiceIdentity(initialService);
  const [service, setService] = useState(initialIdentity ? "" : initialService);
  const [partitionFilter, setPartitionFilter] = useState(
    searchParams.get("partition") ?? initialIdentity?.partition ?? ""
  );
  const [intentFilter, setIntentFilter] = useState(
    searchParams.get("intent") ?? initialIdentity?.intent ?? ""
  );
  const [assetFilter, setAssetFilter] = useState(
    searchParams.get("asset") ?? initialIdentity?.asset ?? ""
  );
  const [textFilter, setTextFilter] = useState("");
  const [liveOn, setLiveOn] = useState(false);
  const [traces, setTraces] = useState<TraceSummary[]>([]);
  const [identities, setIdentities] = useState<LogIdentityInfo[]>([]);
  const [partitions, setPartitions] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [status, setStatus] = useState("Fetching traces…");
  const [copiedView, setCopiedView] = useState(false);
  const [inputVal, setInputVal] = useState("");
  const [page, setPage] = useState(1);
  // pageCursors[page - 1] is the opaque cursor that produced that page.
  // Page 1 has no cursor; the array is reset whenever a server-side filter
  // or the time range changes.
  const [pageCursors, setPageCursors] = useState<Array<string | null>>([null]);
  const [nextCursor, setNextCursor] = useState("");
  const [hasMore, setHasMore] = useState(false);
  const [loadError, setLoadError] = useState("");

  const pageCursor = page > 1 ? pageCursors[page - 1] ?? "" : "";

  const resetPagination = () => {
    setPage(1);
    setPageCursors([null]);
    setNextCursor("");
    setHasMore(false);
    setLoadError("");
  };

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const params = new URLSearchParams({
        tenant: config.default_tenant,
        from: from.toISOString(),
        to: to.toISOString(),
        limit: "200",
      });
      if (service) params.set("service", service);
      if (partitionFilter) params.set("partition", partitionFilter);
      if (intentFilter) params.set("intent", intentFilter);
      if (assetFilter) params.set("asset", assetFilter);
      if (pageCursor) params.set("cursor", pageCursor);
      const data = await api.getRecentTraces(params);
      setTraces(data.traces ?? []);
      setHasMore(Boolean(data.has_more));
      setNextCursor(data.next_cursor ?? "");
      setLoadError("");
    } catch (err) {
      setTraces([]);
      setHasMore(false);
      setNextCursor("");
      setLoadError(err instanceof Error ? err.message : "unknown");
    } finally {
      setLoading(false);
    }
  }, [config.default_tenant, from, to, service, partitionFilter, intentFilter, assetFilter, pageCursor]);

  // Load the service list for the selected partition (and the full partition
  // list seen in telemetry).
  useEffect(() => {
    api
      .getLogServices(config.default_tenant, partitionFilter, true)
      .then((d) => {
        const nextIdentities = d.identities ?? (d.services ?? [])
          .map(splitServiceIdentity)
          .filter((identity): identity is NonNullable<typeof identity> => identity !== null)
          .map((identity) => ({ ...identity, service: "" }));
        setIdentities(nextIdentities);
        setPartitions((prev) => [...new Set([...prev, ...(d.partitions ?? [])])].sort());
      })
      .catch(() => {});
  }, [config.default_tenant, partitionFilter]);

  // Keep an explicitly requested service even when it is absent from the
  // recent-window catalog. Clearing it silently replaces a canonical trace
  // link with every trace in the tenant, including unrelated partitions.
  const partitionOptions = [...new Set([...partitions, partitionFilter].filter(Boolean))].sort();
  const intentOptions = (() => {
    const values = new Set<string>();
    if (intentFilter) values.add(intentFilter);
    for (const identity of identities) {
      if (partitionFilter && identity.partition !== partitionFilter) continue;
      if (identity.intent) values.add(identity.intent);
    }
    for (const trace of traces) {
      if (partitionFilter && trace.partition !== partitionFilter) continue;
      if (trace.intent) values.add(trace.intent);
    }
    return [...values].sort();
  })();
  const assetOptions = (() => {
    const values = new Set<string>();
    if (assetFilter) values.add(assetFilter);
    for (const identity of identities) {
      if (partitionFilter && identity.partition !== partitionFilter) continue;
      if (intentFilter && identity.intent !== intentFilter) continue;
      if (identity.asset) values.add(identity.asset);
    }
    for (const trace of traces) {
      if (partitionFilter && trace.partition !== partitionFilter) continue;
      if (intentFilter && trace.intent !== intentFilter) continue;
      if (trace.asset) values.add(trace.asset);
    }
    return [...values].sort();
  })();

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
    const rawRange = (searchParams.get("range") ?? "").trim();
    if (rawRange) {
      let preset = rawRange;
      if (/^\d+$/.test(rawRange)) {
        const mins = Number(rawRange);
        preset = mins <= 5 ? "5m" : mins <= 15 ? "15m" : mins <= 60 ? "1h" : mins <= 360 ? "6h" : "24h";
      }
      tr.setRelative(preset);
    }
    const nextService = searchParams.get("service") ?? "";
    const identity = splitServiceIdentity(nextService);
    setPartitionFilter(searchParams.get("partition") ?? identity?.partition ?? "");
    setIntentFilter(searchParams.get("intent") ?? identity?.intent ?? "");
    setAssetFilter(searchParams.get("asset") ?? identity?.asset ?? "");
    setService(identity ? "" : nextService);
    setPage(1);
    setPageCursors([null]);
    setNextCursor("");
    setHasMore(false);
    setLoadError("");
  }, [paramKey]);

  useEffect(() => {
    if (focusedTraceId) return;
    void load();
  }, [load, focusedTraceId]);

  // Live refresh
  useEffect(() => {
    if (!liveOn || focusedTraceId) return;
    const t = setInterval(() => void load(), 30000);
    return () => clearInterval(t);
  }, [liveOn, load, focusedTraceId]);

  // Debounce text filter
  useEffect(() => {
    const t = setTimeout(() => setTextFilter(inputVal.trim()), 280);
    return () => clearTimeout(t);
  }, [inputVal]);

  // A complete trace ID is an exact lookup, not a filter over the newest page
  // of summaries. The recent-trace scan is bounded (and very busy tenants can
  // fill it within seconds), so filtering locally would hide older traces even
  // though /v1/traces/{id} can still resolve them.
  const directTraceId = canonicalTraceId(inputVal);

  // Derived filtered list
  const tf = textFilter.toLowerCase();
  const selectedPartition = partitionFilter.trim();
  const filtered = traces.filter((t) => {
    if (service && t.service !== service) return false;
    if (selectedPartition && t.partition !== selectedPartition) return false;
    if (intentFilter && t.intent !== intentFilter) return false;
    if (assetFilter && t.asset !== assetFilter) return false;
    if (tf && !t.trace_id.toLowerCase().includes(tf) && !(t.service ?? "").toLowerCase().includes(tf))
      return false;
    return true;
  });

  useEffect(() => {
    if (loading) {
      setStatus("Fetching traces…");
    } else if (loadError) {
      setStatus("Error: " + loadError);
    } else {
      const more = hasMore ? " · more available" : "";
      setStatus(`${filtered.length} traces on page ${page} — last ${rangeLabel}${more}`);
    }
  }, [loading, loadError, filtered.length, rangeLabel, page, hasMore]);

  const goToNextPage = () => {
    if (loading || !hasMore || !nextCursor) return;
    setPageCursors((prev) => {
      const next = [...prev];
      next[page] = nextCursor;
      return next;
    });
    setPage(page + 1);
  };

  const goToPreviousPage = () => {
    if (loading || page <= 1) return;
    setPage(page - 1);
  };

  const openTrace = (traceId: string) => {
    navigate("/traces?trace_id=" + encodeURIComponent(traceId));
  };

  const openTraceFromInput = () => {
    if (!directTraceId) return;
    setInputVal("");
    openTrace(directTraceId);
  };

  const copyViewLink = () => {
    const params = new URLSearchParams();
    params.set("range", rangeLabel);
    if (service) params.set("service", service);
    if (partitionFilter) params.set("partition", partitionFilter);
    if (intentFilter) params.set("intent", intentFilter);
    if (assetFilter) params.set("asset", assetFilter);
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

  const changePartitionFilter = (next: string) => {
    setPartitionFilter(next);
    if (intentFilter && !identities.some((identity) =>
      (!next || identity.partition === next) && identity.intent === intentFilter)) {
      setIntentFilter("");
      setAssetFilter("");
    } else if (assetFilter && !identities.some((identity) =>
      (!next || identity.partition === next) &&
      (!intentFilter || identity.intent === intentFilter) &&
      identity.asset === assetFilter)) {
      setAssetFilter("");
    }
    resetPagination();
  };

  const changeIntentFilter = (next: string) => {
    setIntentFilter(next);
    if (assetFilter && !identities.some((identity) =>
      (!partitionFilter || identity.partition === partitionFilter) &&
      (!next || identity.intent === next) &&
      identity.asset === assetFilter)) {
      setAssetFilter("");
    }
    resetPagination();
  };

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Traces</h2>
          <p>{status}</p>
        </div>
        <div className="xp-toolbar">
          <label className="filter-label">
            Partition
            <select
              className="xp-select"
              value={partitionFilter}
              onChange={(e) => changePartitionFilter(e.target.value)}
            >
              <option value="">All</option>
              {partitionOptions.map((partition) => (
                <option key={partition} value={partition}>{partition}</option>
              ))}
            </select>
          </label>
          <label className="filter-label">
            Intent
            <select
              className="xp-select"
              value={intentFilter}
              onChange={(e) => changeIntentFilter(e.target.value)}
            >
              <option value="">All</option>
              {intentOptions.map((intent) => (
                <option key={intent} value={intent}>{intent}</option>
              ))}
            </select>
          </label>
          <label className="filter-label">
            Asset
            <select
              className="xp-select"
              value={assetFilter}
              onChange={(e) => {
                setAssetFilter(e.target.value);
                resetPagination();
              }}
            >
              <option value="">All</option>
              {assetOptions.map((asset) => (
                <option key={asset} value={asset}>{asset}</option>
              ))}
            </select>
          </label>
          {service && (
            <span className="facet-chip">
              service: {service}
              <button type="button" onClick={() => { setService(""); resetPagination(); }} title="Clear service filter">×</button>
            </span>
          )}
          <input
            type="text"
            className="xp-search"
            placeholder="Filter this page or open trace ID…"
            value={inputVal}
            onChange={(e) => setInputVal(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && directTraceId) {
                e.preventDefault();
                openTraceFromInput();
              }
            }}
            onPaste={(e) => {
              const pasted = canonicalTraceId(e.clipboardData.getData("text"));
              if (!pasted) return;
              e.preventDefault();
              setInputVal("");
              openTrace(pasted);
            }}
          />
          {directTraceId && (
            <button
              type="button"
              className="xp-live-btn"
              onClick={openTraceFromInput}
              title="Open this trace directly"
            >
              Open trace
            </button>
          )}
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
            {loading && traces.length === 0 ? (
              <div className="xp-empty flex items-center justify-center gap-2">
                <Loader2 size={14} className="animate-spin" />
                Fetching traces…
              </div>
            ) : filtered.length === 0 ? (
              <div className="xp-empty">
                {traces.length === 0
                  ? "No traces found for this time range."
                  : "No traces match the current filters. Paste a full trace ID and press Enter to open it directly."}
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
          <div className="trace-pager">
            <span className="trace-pager__label">Page {page}</span>
            <div className="trace-pager__actions">
              <button
                type="button"
                className="xp-live-btn"
                onClick={goToPreviousPage}
                disabled={loading || page <= 1}
                title="Newer traces"
              >
                ← Previous
              </button>
              <button
                type="button"
                className="xp-live-btn"
                onClick={goToNextPage}
                disabled={loading || !hasMore || !nextCursor}
                title="Older traces"
              >
                Next →
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
