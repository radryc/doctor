import { Fragment, useEffect, useState } from "react";
import { X } from "lucide-react";
import { useInspector } from "../../context/InspectorContext";
import { useConfig } from "../../context/ConfigContext";
import { api } from "../../api/client";
import type { TraceRecord, LogRecord } from "../../types/api";
import TraceDetails from "../charts/TraceDetails";

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
            <LogPanel
              logId={target.id}
              tenant={target.tenant ?? config.default_tenant}
              record={target.log}
            />
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
      .then((r) => {
        if (!active) return;
        const spans = (r.spans ?? []).map((span) => {
          const duration = new Date(span.end_time).getTime() - new Date(span.start_time).getTime();
          return {
            ...span,
            duration_ms:
              isNaN(duration) || duration < 0
                ? "-"
                : duration >= 1000
                ? `${(duration / 1000).toFixed(2)}s`
                : `${duration}ms`,
          };
        });
        setSpans(spans);
        setLoading(false);
      })
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
      <TraceDetails spans={spans} />
    </div>
  );
}

function LogPanel({
  logId,
  tenant,
  record,
}: {
  logId: string;
  tenant: string;
  record?: LogRecord;
}) {
  if (record) return <LogDetail record={record} />;
  return <LogSearchPanel logId={logId} tenant={tenant} />;
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

function LogDetail({ record }: { record: LogRecord }) {
  const { open } = useInspector();
  const partition = logPartition(record);
  const values: Record<string, string> = {
    timestamp: new Date(record.timestamp).toLocaleString(),
    service: record.service ?? "",
    partition,
    intent: record.intent ?? "",
    asset: record.asset ?? "",
    severity: record.severity_text ?? "",
  };
  if (record.trace_id) {
    values.trace_id = record.trace_id;
    values.span_id = record.span_id ?? "";
  }
  for (const [key, value] of Object.entries(record.resource_attributes ?? {})) {
    values[`res.${key}`] = value;
  }
  for (const [key, value] of Object.entries(record.attributes ?? {})) {
    values[`attr.${key}`] = value;
  }

  return (
    <div className="inspector-log-detail">
      <div className="inspector-meta">
        <span>{new Date(record.timestamp).toLocaleString()}</span>
        <span>{record.service || "unknown service"}</span>
        {partition && <span>partition: {partition}</span>}
        {record.severity_text && <span>{record.severity_text}</span>}
      </div>
      <pre className="inspector-log-full">{record.body}</pre>
      <div className="log-kv">
        {Object.entries(values)
          .filter(([, value]) => value)
          .map(([key, value]) => (
            <Fragment key={key}>
              <div className="log-kv-key">{key}</div>
              <div className="log-kv-val">{value}</div>
            </Fragment>
          ))}
      </div>
      {record.trace_id && (
        <button
          className="log-trace-btn"
          onClick={() =>
            open({ kind: "trace", id: record.trace_id!, tenant: record.tenant })
          }
        >
          &#8594; View trace {record.trace_id.slice(0, 16)}…
        </button>
      )}
    </div>
  );
}

function LogSearchPanel({ logId, tenant }: { logId: string; tenant: string }) {
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
