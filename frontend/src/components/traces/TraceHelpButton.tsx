import { useEffect, useState } from "react";
import { CircleHelp, X } from "lucide-react";

interface TraceHelpButtonProps {
  compact?: boolean;
}

/**
 * Help affordance for the Traces page. Explains the cross-provider trace
 * lifecycle: how context moves between k8s, GCP, and AWS, where the spans are
 * stored, and how Doctor merges them at query time.
 */
export default function TraceHelpButton({ compact = false }: TraceHelpButtonProps) {
  const [open, setOpen] = useState(false);

  useEffect(() => {
    if (!open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  return (
    <>
      <button
        type="button"
        className={"xp-live-btn" + (open ? " xp-live-on" : "")}
        onClick={() => setOpen(true)}
        title="How distributed tracing works"
        aria-label="How distributed tracing works"
      >
        <CircleHelp size={12} /> {compact ? "Help" : "Tracing help"}
      </button>
      {open && (
        <div className="modal-backdrop" onClick={() => setOpen(false)}>
          <div
            className="modal trace-help"
            role="dialog"
            aria-modal="true"
            aria-label="How distributed tracing works"
            onClick={(event) => event.stopPropagation()}
          >
            <div className="modal__header">
              <span>How distributed tracing works</span>
              <button type="button" onClick={() => setOpen(false)} title="Close (Esc)">
                <X size={16} />
              </button>
            </div>
            <div className="modal__body trace-help__body">
              <TraceDiagram />
              <div className="trace-help__cards">
                <section className="trace-help__card trace-help__card--aws">
                  <h3>AWS quirks</h3>
                  <ul>
                    <li>
                      X-Ray trace IDs embed an epoch timestamp. IDs older than its
                      retention window (~30 days) are rejected, so roots need the
                      X-Ray-compatible ID generator.
                    </li>
                    <li>
                      Lambda reads <code>_X_AMZN_TRACE_ID</code>, not a header. Active
                      tracing alone records only the Lambda segment, not its work.
                    </li>
                    <li>
                      Queues need <code>AWSTraceHeader</code>. An ALB understands only{" "}
                      <code>X-Amzn-Trace-Id</code>, not W3C <code>traceparent</code>.
                    </li>
                  </ul>
                </section>
                <section className="trace-help__card trace-help__card--gcp">
                  <h3>GCP quirks</h3>
                  <ul>
                    <li>
                      Cloud Trace returns span IDs as decimal strings. They are
                      converted to hex, otherwise parents do not match OTLP spans and
                      the trace splits into islands.
                    </li>
                    <li>
                      The GCP app exports to Cloud Trace, not OTLP. Doctor reads it
                      back later through the federated <code>source="gcp"</code> path.
                    </li>
                    <li>
                      GCP sends W3C only, so an AWS load-balancer segment can end up in
                      a different trace even though the app spans join.
                    </li>
                  </ul>
                </section>
                <section className="trace-help__card trace-help__card--shared">
                  <h3>Shared rules</h3>
                  <ul>
                    <li>
                      When W3C and X-Ray headers disagree, W3C wins and the conflict is
                      flagged. Context is never silently split.
                    </li>
                    <li>
                      Spans merge by <code>(trace_id, span_id)</code>. A missing parent
                      is marked, never invented.
                    </li>
                    <li>
                      A backend that is down, aged out, or hidden by RBAC produces a
                      partial trace with a warning — not a fake “not found”.
                    </li>
                  </ul>
                </section>
              </div>
              <p className="trace-help__note">
                Applications use standard OpenTelemetry. Strata supplies the
                propagators, the X-Ray-compatible ID generator, the OTLP gateway, and
                the query-time merge.
              </p>
            </div>
          </div>
        </div>
      )}
    </>
  );
}

function TraceDiagram() {
  return (
    <svg
      className="trace-help__diagram"
      viewBox="0 0 968 476"
      role="img"
      aria-label="Request path from Kubernetes through GCP and AWS, the three span stores, and the Doctor merge"
    >
      <defs>
        <marker id="th-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
          <path d="M 0 0 L 10 5 L 0 10 z" fill="var(--ui-faint)" />
        </marker>
        <marker id="th-arrow-k8s" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
          <path d="M 0 0 L 10 5 L 0 10 z" fill="var(--ui-accent)" />
        </marker>
        <marker id="th-arrow-gcp" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
          <path d="M 0 0 L 10 5 L 0 10 z" fill="#4c9aff" />
        </marker>
        <marker id="th-arrow-aws" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
          <path d="M 0 0 L 10 5 L 0 10 z" fill="#f5a524" />
        </marker>
      </defs>

      <text x="16" y="28" className="th-title">
        One trace across Kubernetes, AWS and GCP
      </text>
      <text x="16" y="48" className="th-subtitle">
        The trace ID is the join key. Each hop forwards context; Doctor merges the pieces at query time.
      </text>

      {/* Lanes */}
      <rect x="16" y="68" width="296" height="236" rx="10" className="th-lane" />
      <rect x="336" y="68" width="296" height="236" rx="10" className="th-lane" />
      <rect x="656" y="68" width="296" height="236" rx="10" className="th-lane" />

      <text x="42" y="90" className="th-lane-label th-k8s">KUBERNETES</text>
      <text x="362" y="90" className="th-lane-label th-gcp">GCP</text>
      <text x="682" y="90" className="th-lane-label th-aws">AWS</text>

      {/* Request path nodes */}
      <rect x="42" y="106" width="244" height="54" rx="8" className="th-node th-node--k8s" />
      <text x="58" y="129" className="th-node-title">k8s client</text>
      <text x="58" y="146" className="th-node-sub">starts the root span</text>

      <rect x="362" y="106" width="244" height="54" rx="8" className="th-node th-node--gcp" />
      <text x="378" y="129" className="th-node-title">Cloud Run app</text>
      <text x="378" y="146" className="th-node-sub">continues the trace</text>

      <rect x="682" y="106" width="244" height="54" rx="8" className="th-node th-node--aws" />
      <text x="698" y="129" className="th-node-title">ALB → ECS → SQS → Lambda</text>
      <text x="698" y="146" className="th-node-sub">continues, then async hop</text>

      {/* Path arrows */}
      <path d="M 286 133 H 354" className="th-arrow th-arrow--k8s" markerEnd="url(#th-arrow-k8s)" />
      <text x="320" y="126" className="th-arrow-label" textAnchor="middle">traceparent</text>

      <path d="M 606 133 H 674" className="th-arrow th-arrow--gcp" markerEnd="url(#th-arrow-gcp)" />
      <text x="640" y="126" className="th-arrow-label" textAnchor="middle">W3C</text>

      {/* Exports to stores */}
      <path d="M 164 160 V 182" className="th-arrow th-arrow--k8s" markerEnd="url(#th-arrow-k8s)" />
      <path d="M 484 160 V 182" className="th-arrow th-arrow--gcp" markerEnd="url(#th-arrow-gcp)" />
      <path d="M 804 160 V 182" className="th-arrow th-arrow--aws" markerEnd="url(#th-arrow-aws)" />

      {/* Storage */}
      <rect x="42" y="186" width="244" height="84" rx="8" className="th-store th-store--k8s" />
      <text x="58" y="208" className="th-store-title">Doctor store</text>
      <text x="58" y="226" className="th-store-sub">OTLP from k8s + AWS apps</text>
      <text x="58" y="241" className="th-store-sub">merged at query time</text>

      <rect x="362" y="186" width="244" height="84" rx="8" className="th-store th-store--gcp" />
      <text x="378" y="208" className="th-store-title">Cloud Trace + Monitoring</text>
      <text x="378" y="226" className="th-store-sub">read-only · decimal span IDs</text>
      <text x="378" y="241" className="th-store-sub">joined via trace ID</text>

      <rect x="682" y="186" width="244" height="84" rx="8" className="th-store th-store--aws" />
      <text x="698" y="208" className="th-store-title">X-Ray + CloudWatch</text>
      <text x="698" y="226" className="th-store-sub">read-only · ~30-day retention</text>
      <text x="698" y="241" className="th-store-sub">partition resolved heuristically</text>

      {/* Merge */}
      <path d="M 164 270 V 318" className="th-arrow th-arrow--k8s" markerEnd="url(#th-arrow-k8s)" />
      <path d="M 484 270 V 318" className="th-arrow th-arrow--gcp" markerEnd="url(#th-arrow-gcp)" />
      <path d="M 804 270 V 318" className="th-arrow th-arrow--aws" markerEnd="url(#th-arrow-aws)" />

      <rect x="16" y="322" width="936" height="110" rx="10" className="th-merge" />
      <text x="40" y="348" className="th-merge-title">Doctor trace query</text>
      <text x="40" y="371" className="th-merge-line">
        Fetch from the Doctor store plus the read-only X-Ray, CloudWatch and Cloud Trace sources.
      </text>
      <text x="40" y="391" className="th-merge-line">
        Merge by (trace_id, span_id) · dedupe dual exports · apply RBAC per span.
      </text>
      <text x="40" y="411" className="th-merge-line">
        A missing or hidden side produces a partial trace with a warning — never a fabricated parent.
      </text>

      <text x="16" y="456" className="th-footnote">
        Storage is federated: there is no single trace database. Retention, availability and permissions differ per source.
      </text>
    </svg>
  );
}
