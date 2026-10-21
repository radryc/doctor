import { useEffect, useState } from "react";
import { X } from "lucide-react";
import { useInspector } from "../../context/InspectorContext";
import { useConfig } from "../../context/ConfigContext";
import { api } from "../../api/client";
import type { TraceRecord, LogRecord } from "../../types/api";
import TraceWaterfall from "../charts/TraceWaterfall";

export default function InspectorDrawer() {
  const { target, isOpen, close } = useInspector();
  const { config } = useConfig();

  // Close on Escape
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") close();
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [close]);

  if (!isOpen || !target) return null;

  return (
    <>
      <div className="inspector-backdrop" onClick={close} />
      <div className="inspector-drawer" role="dialog" aria-modal="true">
        <div className="inspector-drawer__header">
          <span className="inspector-drawer__kind">{target.kind.toUpperCase()}</span>
          <span className="inspector-drawer__id">{target.id}</span>
          <button className="inspector-drawer__close" onClick={close} title="Close (Esc)">
            <X size={16} />
          </button>
        </div>
        <div className="inspector-drawer__body">
          {target.kind === "trace" && (
            <TracePanel traceId={target.id} tenant={target.tenant ?? config.default_tenant} />
          )}
          {target.kind === "log" && (
            <LogPanel logId={target.id} tenant={target.tenant ?? config.default_tenant} />
          )}
          {target.kind === "service" && (
            <ServicePanel serviceName={target.id} tenant={target.tenant ?? config.default_tenant} />
          )}
        </div>
      </div>
    </>
  );
}

function TracePanel({ traceId, tenant }: { traceId: string; tenant: string }) {
  const [spans, setSpans] = useState<TraceRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setLoading(true);
    api
      .getTrace(traceId, tenant)
      .then((r) => { if (active) { setSpans(r.spans ?? []); setLoading(false); } })
      .catch((e) => { if (active) { setError(e.message); setLoading(false); } });
    return () => { active = false; };
  }, [traceId, tenant]);

  if (loading) return <div className="inspector-loading">Loading trace…</div>;
  if (error) return <div className="inspector-error">{error}</div>;
  if (spans.length === 0) return <div className="inspector-empty">No spans found.</div>;

  return (
    <div>
      <div className="inspector-meta">
        <span>Trace ID: <code>{traceId}</code></span>
        <span>{spans.length} spans</span>
        <span>Root: {spans[0]?.service ?? "—"}</span>
      </div>
      <TraceWaterfall spans={spans} />
    </div>
  );
}

function LogPanel({ logId, tenant }: { logId: string; tenant: string }) {
  const [logs, setLogs] = useState<LogRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setLoading(true);
    const params = new URLSearchParams({ tenant, q: logId, limit: "100" });
    api
      .searchLogs(params)
      .then((r) => { if (active) { setLogs(r.results ?? []); setLoading(false); } })
      .catch((e) => { if (active) { setError(e.message); setLoading(false); } });
    return () => { active = false; };
  }, [logId, tenant]);

  if (loading) return <div className="inspector-loading">Loading logs…</div>;
  if (error) return <div className="inspector-error">{error}</div>;

  return (
    <div className="inspector-logs">
      {logs.length === 0 ? (
        <div className="inspector-empty">No logs found for this ID.</div>
      ) : (
        logs.map((log, i) => (
          <div key={i} className="inspector-log-row">
            <span className="inspector-log-ts">{new Date(log.timestamp).toLocaleTimeString()}</span>
            <span className="inspector-log-svc">{log.service}</span>
            <span className="inspector-log-body">{log.body}</span>
          </div>
        ))
      )}
    </div>
  );
}

function ServicePanel({ serviceName, tenant }: { serviceName: string; tenant: string }) {
  const { close } = useInspector();

  return (
    <div className="inspector-service">
      <div className="inspector-meta">
        <strong>{serviceName}</strong>
        <span>Tenant: {tenant}</span>
      </div>
      <div className="inspector-service__links">
        <a
          href={`#/logs?q=${encodeURIComponent(serviceName)}`}
          onClick={close}
        >
          View Logs
        </a>
        <a
          href={`#/traces?service=${encodeURIComponent(serviceName)}`}
          onClick={close}
        >
          View Traces
        </a>
        <a
          href={`#/services/${encodeURIComponent(serviceName)}`}
          onClick={close}
        >
          Full Service Detail
        </a>
      </div>
    </div>
  );
}
