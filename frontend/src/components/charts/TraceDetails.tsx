import { Fragment } from "react";
import TraceWaterfall from "./TraceWaterfall";
import type { TraceRecord } from "../../types/api";

export function fmtTime(ts: string): string {
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

export function payloadEntryCount(span: TraceRecord): number {
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
export function decodeOtlpValue(raw: string): string {
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

export function renderKV(entries: Array<{ key: string; value: string }>) {
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

export function spanSummary(span: TraceRecord): string {
  const parts = [span.service || "unknown", span.duration_ms || "-"];
  const payloadCount = payloadEntryCount(span);
  if (payloadCount > 0) {
    parts.push(`${payloadCount} payload item${payloadCount === 1 ? "" : "s"}`);
  }
  return parts.join(" • ");
}

export function spanMetaEntries(span: TraceRecord): Array<{ key: string; value: string }> {
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

export function attributeEntries(prefix: string, values?: Record<string, string>) {
  return Object.entries(values ?? {}).map(([key, value]) => ({
    key: `${prefix}.${key}`,
    value,
  }));
}

export function eventEntries(span: TraceRecord) {
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

export function SpanTable({ spans }: { spans: TraceRecord[] }) {
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

export default function TraceDetails({ spans }: { spans: TraceRecord[] }) {
  return (
    <>
      <TraceWaterfall spans={spans} />
      <SpanTable spans={spans} />
    </>
  );
}
