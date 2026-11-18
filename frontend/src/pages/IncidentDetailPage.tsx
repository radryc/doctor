import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import {
  ArrowLeft,
  CheckCircle2,
  Clock,
  FileText,
  RefreshCw,
  ShieldCheck,
  Sparkles,
  Upload,
} from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import type { Incident, Postmortem } from "../types/incidents";
import "./incidents.css";

function short(value?: string | null): string {
  if (!value) return "—";
  return new Date(value).toLocaleString();
}

function between(from?: string | null, to?: string | null): string {
  if (!from) return "—";
  const end = to ? new Date(to).getTime() : Date.now();
  const ms = end - new Date(from).getTime();
  if (!Number.isFinite(ms) || ms < 0) return "—";
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes % 60}m`;
}

export default function IncidentDetailPage() {
  const { id = "" } = useParams();
  const { config } = useConfig();
  const [params] = useSearchParams();
  const tenant = config.default_tenant || "default";
  const partition = params.get("partition") || config.default_partition || "default";
  const [incident, setIncident] = useState<Incident | null>(null);
  const [postmortem, setPostmortem] = useState<Postmortem | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [tab, setTab] = useState(params.get("tab") === "postmortem" ? "postmortem" : "timeline");
  const [note, setNote] = useState("");
  const [mitigateNote, setMitigateNote] = useState("");
  const [override, setOverride] = useState(false);
  const [resolution, setResolution] = useState("");
  const [draft, setDraft] = useState("");

  const load = useCallback(async () => {
    if (!id) return;
    setLoading(true);
    setError("");
    try {
      const current = await api.getIncident(tenant, partition, id);
      setIncident(current);
      try {
        const doc = await api.getIncidentPostmortem(tenant, partition, id);
        setPostmortem(doc);
        setDraft(doc.markdown ?? "");
      } catch {
        setPostmortem(null);
      }
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to load incident");
    } finally {
      setLoading(false);
    }
  }, [tenant, partition, id]);

  useEffect(() => { void load(); }, [load]);
  useEffect(() => {
    const timer = window.setInterval(() => { void load(); }, 10_000);
    return () => window.clearInterval(timer);
  }, [load]);

  const act = async (name: string, fn: () => Promise<unknown>) => {
    setBusy(name);
    setError("");
    try {
      await fn();
      await load();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Action failed");
    } finally {
      setBusy("");
    }
  };

  const metrics = useMemo(() => {
    if (!incident) return null;
    return [
      ["Detect → ack", between(incident.openedAt, incident.acknowledgedAt)],
      ["Detect → mitigate", between(incident.openedAt, incident.mitigatedAt)],
      ["Detect → resolve", between(incident.openedAt, incident.resolvedAt)],
    ];
  }, [incident]);

  if (loading && !incident) {
    return <div className="tab-panel incidents-page"><p className="incidents-muted">Loading incident…</p></div>;
  }
  if (!incident) {
    return (
      <div className="tab-panel incidents-page">
        <Link className="incident-link" to="/incidents"><ArrowLeft size={13} /> Back to incidents</Link>
        <div className="incidents-error">{error || "Incident not found"}</div>
      </div>
    );
  }

  return (
    <div className="tab-panel incidents-page">
      <header className="incidents-header">
        <div>
          <Link className="incident-link" to="/incidents"><ArrowLeft size={13} /> Incidents</Link>
          <h2>{incident.title || incident.set}</h2>
          <p className="incidents-subtitle">
            {incident.partition} · {incident.set} · {incident.groupKey} · opened {short(incident.openedAt)}
          </p>
        </div>
        <div className="incidents-header-actions">
          <span className={`incident-state incident-state--${incident.state}`}>{incident.state}</span>
          <span className={`incident-severity incident-severity--${incident.severity}`}>{incident.severity}</span>
          <button className="incident-secondary-button" type="button" onClick={() => void load()} disabled={loading}>
            <RefreshCw size={14} className={loading ? "animate-spin" : ""} /> Refresh
          </button>
        </div>
      </header>

      {error && <div className="incidents-error">{error}</div>}

      <div className="incidents-stats">
        {metrics?.map(([label, value]) => (
          <div className="incident-stat" key={label}>
            <Clock size={16} />
            <span>{label}</span>
            <strong>{value}</strong>
          </div>
        ))}
      </div>

      <section className="incidents-panel incident-actions">
        {incident.state === "open" && (
          <button className="incident-primary-button" type="button" disabled={busy !== ""} onClick={() => void act("ack", () => api.ackIncident(tenant, partition, incident.id))}>
            Acknowledge
          </button>
        )}
        {incident.state === "acknowledged" && (
          <button className="incident-secondary-button" type="button" disabled={busy !== ""} onClick={() => void act("unack", () => api.unackIncident(tenant, partition, incident.id))}>
            Unacknowledge
          </button>
        )}
        {(incident.state === "open" || incident.state === "acknowledged") && (
          <div className="incident-inline-form">
            <input value={mitigateNote} placeholder="Mitigation note" onChange={(event) => setMitigateNote(event.target.value)} />
            <label className="incident-check">
              <input type="checkbox" checked={override} onChange={(event) => setOverride(event.target.checked)} />
              Alerts still firing
            </label>
            <button
              className="incident-secondary-button"
              type="button"
              disabled={busy !== ""}
              onClick={() => void act("mitigate", () => api.mitigateIncident(tenant, partition, incident.id, { note: mitigateNote, override }))}
            >
              <ShieldCheck size={14} /> Mark mitigated
            </button>
          </div>
        )}
        {incident.state === "mitigated" && (
          <div className="incident-inline-form">
            <input value={resolution} placeholder="Resolution summary" onChange={(event) => setResolution(event.target.value)} />
            <button
              className="incident-primary-button"
              type="button"
              disabled={busy !== ""}
              onClick={() => void act("resolve", () => api.resolveIncident(tenant, partition, incident.id, resolution))}
            >
              <CheckCircle2 size={14} /> Resolve &amp; generate postmortem
            </button>
          </div>
        )}
        {incident.state === "resolved" && (
          <button className="incident-secondary-button" type="button" disabled={busy !== ""} onClick={() => void act("reopen", () => api.reopenIncident(tenant, partition, incident.id))}>
            Reopen
          </button>
        )}
        <div className="incident-inline-form">
          <input value={note} placeholder="Add a timeline note" onChange={(event) => setNote(event.target.value)} />
          <button
            className="incident-secondary-button"
            type="button"
            disabled={busy !== "" || !note.trim()}
            onClick={() => void act("note", async () => {
              await api.noteIncident(tenant, partition, incident.id, note);
              setNote("");
            })}
          >
            Add note
          </button>
        </div>
        {incident.mitigateDueAt && (
          <span className="incident-countdown">
            Auto-mitigate at {short(incident.mitigateDueAt)}
          </span>
        )}
      </section>

      <nav className="incidents-tabs">
        {(["timeline", "alerts", "postmortem"] as const).map((value) => (
          <button
            key={value}
            type="button"
            className={"incidents-tab" + (tab === value ? " is-active" : "")}
            onClick={() => setTab(value)}
          >
            {value === "postmortem" ? "Postmortem" : value === "alerts" ? `Alerts (${incident.memberAlerts?.length ?? 0})` : "Timeline"}
          </button>
        ))}
      </nav>

      {tab === "timeline" && (
        <section className="incidents-panel">
          <ol className="incident-timeline">
            {(incident.timeline ?? []).slice().reverse().map((entry) => (
              <li key={entry.seq}>
                <span className="incident-timeline__dot" />
                <div>
                  <strong>{entry.kind}</strong>
                  {entry.actor && <span className="incident-pill">{entry.actor}</span>}
                  <p>{entry.message}</p>
                  <time>{short(entry.at)}</time>
                </div>
              </li>
            ))}
          </ol>
        </section>
      )}

      {tab === "alerts" && (
        <section className="incidents-panel">
          {(incident.memberAlerts?.length ?? 0) === 0 ? (
            <p className="incidents-muted">No member alerts recorded.</p>
          ) : (
            <div className="incident-table">
              <div className="incident-table__head">
                <span>Alert</span><span>Severity</span><span>Status</span><span>Fingerprint</span>
              </div>
              {incident.memberAlerts?.map((alert) => (
                <div className="incident-table__row" key={alert.fingerprint}>
                  <span>{alert.alertname}</span>
                  <span>{alert.severity}</span>
                  <span>{alert.status}</span>
                  <code>{alert.fingerprint.slice(0, 12)}</code>
                </div>
              ))}
            </div>
          )}
        </section>
      )}

      {tab === "postmortem" && (
        <section className="incidents-panel">
          <div className="incidents-panel-head">
            <h3><FileText size={15} /> Postmortem</h3>
            {postmortem && <span className={`incident-state incident-state--${postmortem.status}`}>{postmortem.status}</span>}
          </div>
          {!postmortem ? (
            <p className="incidents-muted">No postmortem yet. Resolve the incident to generate one.</p>
          ) : (
            <>
              <div className="incident-card__meta">
                <span>engine {postmortem.engine}</span>
                <span>revision {postmortem.revision}</span>
                <span>provider {postmortem.analysisProvider || "none"}</span>
                {postmortem.analysis?.confidence != null && <span>confidence {(postmortem.analysis.confidence * 100).toFixed(0)}%</span>}
                {postmortem.error && <span className="incidents-error-inline">{postmortem.error}</span>}
              </div>
              <div className="incident-form-actions">
                <button className="incident-secondary-button" type="button" disabled={busy !== ""} onClick={() => void act("analyze", () => api.analyzeIncidentPostmortem(tenant, partition, incident.id))}>
                  <Sparkles size={14} /> Run agent analysis
                </button>
                <button className="incident-primary-button" type="button" disabled={busy !== ""} onClick={() => void act("publish", () => api.publishIncidentPostmortem(tenant, partition, incident.id))}>
                  <Upload size={14} /> Publish to {postmortem.engine}
                </button>
                {postmortem.url && (
                  <a className="incident-link" href={postmortem.url} target="_blank" rel="noreferrer">
                    Open published document
                  </a>
                )}
              </div>
              {postmortem.analysis && (
                <div className="incident-analysis">
                  <h4>Agent analysis</h4>
                  <p>{postmortem.analysis.summary}</p>
                  {postmortem.analysis.rootCause && <p><strong>Root cause:</strong> {postmortem.analysis.rootCause}</p>}
                  {(postmortem.analysis.contributingFactors ?? []).map((factor, index) => <p key={index}>• {factor}</p>)}
                  {(postmortem.analysis.actionItems ?? []).length > 0 && (
                    <>
                      <h4>Action items</h4>
                      <ul>{postmortem.analysis.actionItems?.map((item, index) => <li key={index}>{item.title}{item.target ? ` (${item.target})` : ""}</li>)}</ul>
                    </>
                  )}
                </div>
              )}
              <label className="incident-field">
                Draft markdown
                <textarea rows={18} value={draft} onChange={(event) => setDraft(event.target.value)} />
              </label>
              <div className="incident-form-actions">
                <button className="incident-secondary-button" type="button" disabled={busy !== ""} onClick={() => void act("save", () => api.updateIncidentPostmortem(tenant, partition, incident.id, draft))}>
                  Save draft
                </button>
              </div>
            </>
          )}
        </section>
      )}
    </div>
  );
}
