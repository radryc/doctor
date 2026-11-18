import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  ArrowUpRight,
  CheckCircle2,
  Clock,
  Plus,
  RefreshCw,
  ShieldCheck,
  Trash2,
  XCircle,
} from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import type { Matcher, Receiver, ReceiverConfig } from "../types/alerts";
import type { AlertSet, EngineMetadata, Incident, IncidentPolicy } from "../types/incidents";
import "./incidents.css";

type Tab = "incidents" | "sets" | "policy" | "postmortems";

const STATE_ORDER = ["open", "acknowledged", "mitigated", "resolved"] as const;

function durationSince(value?: string | null): string {
  if (!value) return "—";
  const ms = Date.now() - new Date(value).getTime();
  if (!Number.isFinite(ms) || ms < 0) return "now";
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ${minutes % 60}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

function stateLabel(state: string): string {
  switch (state) {
    case "acknowledged":
      return "Acknowledged";
    case "mitigated":
      return "Mitigated";
    case "resolved":
      return "Resolved";
    default:
      return "Open";
  }
}

function stateIcon(state: string) {
  switch (state) {
    case "resolved":
      return CheckCircle2;
    case "mitigated":
      return ShieldCheck;
    case "acknowledged":
      return Clock;
    default:
      return AlertTriangle;
  }
}

function defaultSet(partition: string): AlertSet {
  return {
    tenant: "",
    partition,
    name: "",
    description: "",
    enabled: true,
    severityFloor: "critical",
    match: [{ name: "severity", operator: "=", value: "critical" }],
    exclude: [],
    respectSilences: true,
    respectAcks: false,
    groupBy: ["partition"],
    notify: { receivers: [], updateInterval: "30m", reminders: true, updateWhileAcked: false },
    postmortem: { engine: "builtin", autoPublish: false, analysisProvider: "agent" },
  };
}

function MatcherRows({
  matchers,
  onChange,
}: {
  matchers: Matcher[];
  onChange: (value: Matcher[]) => void;
}) {
  const update = (index: number, patch: Partial<Matcher>) => {
    const next = matchers.map((matcher, i) => (i === index ? { ...matcher, ...patch } : matcher));
    onChange(next);
  };
  return (
    <div className="incident-matchers">
      {matchers.map((matcher, index) => (
        <div className="incident-matcher-row" key={index}>
          <input
            aria-label="Label name"
            placeholder="label"
            value={matcher.name}
            onChange={(event) => update(index, { name: event.target.value })}
          />
          <select
            aria-label="Operator"
            value={matcher.operator}
            onChange={(event) => update(index, { operator: event.target.value })}
          >
            <option value="=">=</option>
            <option value="!=">!=</option>
            <option value="=~">=~</option>
            <option value="!~">!~</option>
          </select>
          <input
            aria-label="Value"
            placeholder="value"
            value={matcher.value}
            onChange={(event) => update(index, { value: event.target.value })}
          />
          <button
            type="button"
            className="incident-icon-button"
            onClick={() => onChange(matchers.filter((_, i) => i !== index))}
            aria-label="Remove matcher"
          >
            <Trash2 size={14} />
          </button>
        </div>
      ))}
      <button
        type="button"
        className="incident-secondary-button"
        onClick={() => onChange([...matchers, { name: "", operator: "=", value: "" }])}
      >
        <Plus size={13} /> Add matcher
      </button>
    </div>
  );
}

export default function IncidentsPage() {
  const { config } = useConfig();
  const tenant = config.default_tenant || "default";
  const partitions = config.accessible_partitions?.length
    ? config.accessible_partitions
    : [config.default_partition || "default"];
  const [partition, setPartition] = useState(partitions[0] || "default");
  const [tab, setTab] = useState<Tab>("incidents");
  const [incidents, setIncidents] = useState<Incident[]>([]);
  const [sets, setSets] = useState<AlertSet[]>([]);
  const [policy, setPolicy] = useState<IncidentPolicy | null>(null);
  const [engines, setEngines] = useState<EngineMetadata[]>([]);
  const [receivers, setReceivers] = useState<Receiver[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [stateFilter, setStateFilter] = useState("");
  const [editing, setEditing] = useState<AlertSet | null>(null);
  const [preview, setPreview] = useState<{ matchedAlerts: number; groups: Array<Record<string, unknown>> } | null>(null);
  const [saving, setSaving] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const params = new URLSearchParams({ tenant, partition });
      if (stateFilter) params.set("state", stateFilter);
      const [incidentResponse, setResponse, policyResponse, engineResponse, receiverResponse] =
        await Promise.all([
          api.getIncidents(params),
          api.getAlertSets(tenant, partition),
          api.getIncidentPolicy(tenant, partition),
          api.getPostmortemEngines(),
          api
            .getReceivers(new URLSearchParams({ tenant, partition }))
            .catch(() => ({ receivers: [], routes: [] } as ReceiverConfig)),
        ]);
      setIncidents(incidentResponse.incidents ?? []);
      setSets(setResponse.sets ?? []);
      setPolicy(policyResponse);
      setEngines(engineResponse.engines ?? []);
      setReceivers((receiverResponse as ReceiverConfig).receivers ?? []);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to load incidents");
    } finally {
      setLoading(false);
    }
  }, [tenant, partition, stateFilter]);

  useEffect(() => { void load(); }, [load]);
  useEffect(() => {
    const id = window.setInterval(() => { void load(); }, 15_000);
    return () => window.clearInterval(id);
  }, [load]);

  const counts = useMemo(() => {
    const out: Record<string, number> = { open: 0, acknowledged: 0, mitigated: 0, resolved: 0 };
    for (const incident of incidents) out[incident.state] = (out[incident.state] ?? 0) + 1;
    return out;
  }, [incidents]);

  const visible = useMemo(
    () => incidents.filter((incident) => !stateFilter || incident.state === stateFilter),
    [incidents, stateFilter]
  );

  const runAction = async (action: () => Promise<unknown>) => {
    setError("");
    try {
      await action();
      await load();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Action failed");
    }
  };

  const saveSet = async () => {
    if (!editing) return;
    setSaving(true);
    setError("");
    try {
      const payload: AlertSet = {
        ...editing,
        tenant,
        partition,
        groupBy: (editing.groupBy ?? []).filter(Boolean),
      };
      if (!payload.name.trim()) throw new Error("Name is required");
      const exists = sets.some((set) => set.name === payload.name);
      if (exists) await api.updateAlertSet(tenant, partition, payload.name, payload);
      else await api.createAlertSet(tenant, partition, payload);
      setEditing(null);
      setPreview(null);
      await load();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to save alert set");
    } finally {
      setSaving(false);
    }
  };

  const savePolicy = async () => {
    if (!policy) return;
    setSaving(true);
    setError("");
    try {
      const saved = await api.putIncidentPolicy(tenant, partition, policy);
      setPolicy(saved);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to save policy");
    } finally {
      setSaving(false);
    }
  };

  const resolved = visible.filter((incident) => incident.state === "resolved");

  return (
    <div className="tab-panel incidents-page">
      <header className="incidents-header">
        <div>
          <p className="incidents-eyebrow">On-call</p>
          <h2>Incidents</h2>
          <p className="incidents-subtitle">
            Alert sets group signals into incidents. Mitigation is automatic 30 minutes after the last member alert clears.
          </p>
        </div>
        <div className="incidents-header-actions">
          <select value={partition} onChange={(event) => setPartition(event.target.value)} aria-label="Partition">
            {partitions.map((name) => <option key={name} value={name}>{name}</option>)}
          </select>
          <button className="incident-secondary-button" type="button" onClick={() => void load()} disabled={loading}>
            <RefreshCw size={14} className={loading ? "animate-spin" : ""} /> Refresh
          </button>
        </div>
      </header>

      {error && <div className="incidents-error">{error}</div>}

      <nav className="incidents-tabs">
        {([
          ["incidents", "Incidents"],
          ["sets", "Alert sets"],
          ["policy", "Partition policy"],
          ["postmortems", "Postmortems"],
        ] as Array<[Tab, string]>).map(([value, label]) => (
          <button
            key={value}
            type="button"
            className={"incidents-tab" + (tab === value ? " is-active" : "")}
            onClick={() => setTab(value)}
          >
            {label}
            {value === "incidents" && counts.open > 0 && <span className="incidents-tab-count">{counts.open}</span>}
          </button>
        ))}
      </nav>

      {tab === "incidents" && (
        <>
          <div className="incidents-stats">
            {STATE_ORDER.map((state) => {
              const Icon = stateIcon(state);
              return (
                <button
                  key={state}
                  type="button"
                  className={"incident-stat incident-stat--" + state + (stateFilter === state ? " is-active" : "")}
                  onClick={() => setStateFilter(stateFilter === state ? "" : state)}
                >
                  <Icon size={16} />
                  <span>{stateLabel(state)}</span>
                  <strong>{counts[state] ?? 0}</strong>
                </button>
              );
            })}
          </div>

          {visible.length === 0 ? (
            <div className="incidents-empty">
              <CheckCircle2 size={28} />
              <p>No incidents in this view.</p>
              <p className="incidents-muted">
                Create an alert set to turn matching critical alerts into incidents.
              </p>
              <button className="incident-primary-button" type="button" onClick={() => { setTab("sets"); setEditing(defaultSet(partition)); }}>
                <Plus size={14} /> New alert set
              </button>
            </div>
          ) : (
            <div className="incident-list">
              {visible.map((incident) => {
                const Icon = stateIcon(incident.state);
                return (
                  <article key={incident.id} className={`incident-card incident-card--${incident.state}`}>
                    <div className="incident-card__head">
                      <Icon size={16} />
                      <Link to={`/incidents/${encodeURIComponent(incident.id)}?partition=${encodeURIComponent(incident.partition)}`}>
                        {incident.title || incident.set}
                      </Link>
                      <span className={`incident-severity incident-severity--${incident.severity}`}>{incident.severity}</span>
                      <span className="incident-state">{stateLabel(incident.state)}</span>
                    </div>
                    <div className="incident-card__meta">
                      <span>{incident.set}</span>
                      <span>{incident.groupKey}</span>
                      <span>{incident.memberAlerts?.length ?? 0} alert(s)</span>
                      <span>age {durationSince(incident.openedAt)}</span>
                      {incident.owner && <span>owner {incident.owner}</span>}
                      {incident.state === "mitigated" && <span>bleeding stopped</span>}
                    </div>
                    <div className="incident-card__actions">
                      {incident.state === "open" && (
                        <button type="button" className="incident-secondary-button" onClick={() => void runAction(() => api.ackIncident(tenant, incident.partition, incident.id))}>
                          Ack
                        </button>
                      )}
                      {incident.state === "acknowledged" && (
                        <button type="button" className="incident-secondary-button" onClick={() => void runAction(() => api.unackIncident(tenant, incident.partition, incident.id))}>
                          Unack
                        </button>
                      )}
                      {(incident.state === "open" || incident.state === "acknowledged") && (
                        <button
                          type="button"
                          className="incident-secondary-button"
                          onClick={() => {
                            const note = window.prompt("Mitigation note (required when alerts are still firing)");
                            if (note === null) return;
                            void runAction(() => api.mitigateIncident(tenant, incident.partition, incident.id, { note, override: true }));
                          }}
                        >
                          Mark mitigated
                        </button>
                      )}
                      {incident.state === "mitigated" && (
                        <button
                          type="button"
                          className="incident-primary-button"
                          onClick={() => {
                            const resolution = window.prompt("Resolution summary (agent will generate the postmortem)");
                            if (resolution === null) return;
                            void runAction(() => api.resolveIncident(tenant, incident.partition, incident.id, resolution));
                          }}
                        >
                          Resolve
                        </button>
                      )}
                      {incident.state === "resolved" && (
                        <button
                          type="button"
                          className="incident-secondary-button"
                          onClick={() => void runAction(() => api.reopenIncident(tenant, incident.partition, incident.id))}
                        >
                          Reopen
                        </button>
                      )}
                      <Link className="incident-link" to={`/incidents/${encodeURIComponent(incident.id)}?partition=${encodeURIComponent(incident.partition)}`}>
                        Open <ArrowUpRight size={12} />
                      </Link>
                    </div>
                  </article>
                );
              })}
            </div>
          )}
        </>
      )}

      {tab === "sets" && (
        <div className="incidents-two-column">
          <section className="incidents-panel">
            <div className="incidents-panel-head">
              <h3>Alert sets</h3>
              <button className="incident-primary-button" type="button" onClick={() => { setEditing(defaultSet(partition)); setPreview(null); }}>
                <Plus size={14} /> New set
              </button>
            </div>
            {sets.length === 0 ? (
              <p className="incidents-muted">No alert sets yet.</p>
            ) : (
              <div className="incident-set-list">
                {sets.map((set) => (
                  <button
                    key={set.name}
                    type="button"
                    className={"incident-set-row" + (editing?.name === set.name ? " is-active" : "")}
                    onClick={() => { setEditing(structuredClone(set)); setPreview(null); }}
                  >
                    <div>
                      <strong>{set.name}</strong>
                      <span>{set.match.length} matcher(s) · group by {(set.groupBy ?? []).join(", ") || "partition"}</span>
                    </div>
                    <span className="incident-pill">{set.severityFloor || "critical"}</span>
                  </button>
                ))}
              </div>
            )}
          </section>

          <section className="incidents-panel">
            {editing ? (
              <>
                <div className="incidents-panel-head">
                  <h3>{sets.some((set) => set.name === editing.name) ? "Edit set" : "New set"}</h3>
                  <button
                    className="incident-icon-button"
                    type="button"
                    title="Delete set"
                    onClick={() => {
                      if (!editing.name) { setEditing(null); return; }
                      if (!window.confirm(`Delete ${editing.name}?`)) return;
                      void runAction(async () => { await api.deleteAlertSet(tenant, partition, editing.name); setEditing(null); });
                    }}
                  >
                    <Trash2 size={15} />
                  </button>
                </div>
                <label className="incident-field">Name
                  <input value={editing.name} onChange={(event) => setEditing({ ...editing, name: event.target.value })} placeholder="platform-critical" />
                </label>
                <label className="incident-field">Description
                  <input value={editing.description ?? ""} onChange={(event) => setEditing({ ...editing, description: event.target.value })} />
                </label>
                <div className="incident-field-row">
                  <label className="incident-field">Severity floor
                    <select value={editing.severityFloor ?? "critical"} onChange={(event) => setEditing({ ...editing, severityFloor: event.target.value })}>
                      <option value="critical">critical</option>
                      <option value="warning">warning</option>
                      <option value="info">info</option>
                    </select>
                  </label>
                  <label className="incident-field">Group by
                    <input
                      value={(editing.groupBy ?? []).join(",")}
                      onChange={(event) => setEditing({ ...editing, groupBy: event.target.value.split(",").map((v) => v.trim()).filter(Boolean) })}
                      placeholder="partition,service"
                    />
                  </label>
                </div>
                <fieldset className="incident-fieldset">
                  <legend>Match alerts</legend>
                  <MatcherRows matchers={editing.match} onChange={(match) => setEditing({ ...editing, match })} />
                </fieldset>
                <fieldset className="incident-fieldset">
                  <legend>Exclude</legend>
                  <MatcherRows matchers={editing.exclude ?? []} onChange={(exclude) => setEditing({ ...editing, exclude })} />
                </fieldset>
                <div className="incident-field-row">
                  <label className="incident-check">
                    <input type="checkbox" checked={editing.respectSilences ?? true} onChange={(event) => setEditing({ ...editing, respectSilences: event.target.checked })} />
                    Respect silences
                  </label>
                  <label className="incident-check">
                    <input type="checkbox" checked={editing.respectAcks ?? false} onChange={(event) => setEditing({ ...editing, respectAcks: event.target.checked })} />
                    Ignore acked alerts
                  </label>
                </div>
                <label className="incident-field">Notify receivers (incident-capable)
                  <select
                    multiple
                    value={editing.notify?.receivers ?? []}
                    onChange={(event) => {
                      const selected = Array.from(event.target.selectedOptions).map((option) => option.value);
                      setEditing({ ...editing, notify: { ...(editing.notify ?? {}), receivers: selected } });
                    }}
                  >
                    {receivers
                      .filter((receiver) => ["", "webhook", "slack", "teams", "zoom", "zoomchat", "android"].includes((receiver.type || "").toLowerCase()))
                      .map((receiver) => <option key={receiver.name} value={receiver.name}>{receiver.name} ({receiver.type || "webhook"})</option>)}
                  </select>
                </label>
                <div className="incident-field-row">
                  <label className="incident-field">Update interval
                    <input value={editing.notify?.updateInterval ?? "30m"} onChange={(event) => setEditing({ ...editing, notify: { ...(editing.notify ?? {}), updateInterval: event.target.value } })} />
                  </label>
                  <label className="incident-field">Analysis
                    <select value={editing.postmortem?.analysisProvider ?? "agent"} onChange={(event) => setEditing({ ...editing, postmortem: { ...(editing.postmortem ?? {}), analysisProvider: event.target.value } })}>
                      <option value="agent">agent</option>
                      <option value="none">none</option>
                    </select>
                  </label>
                </div>
                <div className="incident-field-row">
                  <label className="incident-field">Postmortem engine
                    <select value={editing.postmortem?.engine ?? "builtin"} onChange={(event) => setEditing({ ...editing, postmortem: { ...(editing.postmortem ?? {}), engine: event.target.value } })}>
                      {engines.map((engine) => <option key={engine.name} value={engine.name}>{engine.label}</option>)}
                    </select>
                  </label>
                  <label className="incident-check">
                    <input type="checkbox" checked={editing.postmortem?.autoPublish ?? false} onChange={(event) => setEditing({ ...editing, postmortem: { ...(editing.postmortem ?? {}), autoPublish: event.target.checked } })} />
                    Auto-publish after analysis
                  </label>
                </div>
                <div className="incident-form-actions">
                  <button className="incident-primary-button" type="button" onClick={() => void saveSet()} disabled={saving}>
                    {saving ? "Saving…" : "Save set"}
                  </button>
                  <button
                    className="incident-secondary-button"
                    type="button"
                    onClick={() => void api.previewAlertSet(tenant, partition, editing).then(setPreview).catch((e) => setError(e instanceof Error ? e.message : "Preview failed"))}
                  >
                    Preview matches
                  </button>
                  <button className="incident-secondary-button" type="button" onClick={() => { setEditing(null); setPreview(null); }}>Cancel</button>
                </div>
                {preview && (
                  <div className="incident-preview">
                    <strong>{preview.matchedAlerts} firing alert(s)</strong>
                    <span>{preview.groups.length} incident group(s)</span>
                    {preview.groups.slice(0, 6).map((group, index) => (
                      <code key={index}>{String(group.key)} · {String(group.count)} alert(s)</code>
                    ))}
                  </div>
                )}
              </>
            ) : (
              <p className="incidents-muted">Select a set to edit, or create a new one.</p>
            )}
          </section>
        </div>
      )}

      {tab === "policy" && policy && (
        <section className="incidents-panel incidents-policy">
          <div className="incidents-panel-head">
            <h3>Partition policy · {partition}</h3>
            <span className="incident-pill">default 30m</span>
          </div>
          <p className="incidents-muted">
            Mitigate delay counts from the moment the last member alert clears. It applies to every incident in this partition.
          </p>
          <div className="incident-field-row">
            <label className="incident-field">Mitigate delay
              <input value={policy.mitigateDelay ?? "30m"} onChange={(event) => setPolicy({ ...policy, mitigateDelay: event.target.value })} />
            </label>
            <label className="incident-field">Auto-resolve delay
              <input value={policy.autoResolveDelay ?? "30m"} onChange={(event) => setPolicy({ ...policy, autoResolveDelay: event.target.value })} />
            </label>
          </div>
          <div className="incident-field-row">
            <label className="incident-check">
              <input type="checkbox" checked={policy.autoMitigate ?? true} onChange={(event) => setPolicy({ ...policy, autoMitigate: event.target.checked })} />
              Auto-mitigate when alerts clear
            </label>
            <label className="incident-check">
              <input type="checkbox" checked={policy.autoResolve ?? false} onChange={(event) => setPolicy({ ...policy, autoResolve: event.target.checked })} />
              Auto-resolve after mitigation
            </label>
          </div>
          <div className="incident-field-row">
            <label className="incident-field">Resume window
              <input value={policy.resumeWindow ?? "2h"} onChange={(event) => setPolicy({ ...policy, resumeWindow: event.target.value })} />
            </label>
            <label className="incident-field">Reopen window
              <input value={policy.reopenWindow ?? "1h"} onChange={(event) => setPolicy({ ...policy, reopenWindow: event.target.value })} />
            </label>
            <label className="incident-field">Updates per hour
              <input type="number" value={policy.maxUpdatesPerHour ?? 12} onChange={(event) => setPolicy({ ...policy, maxUpdatesPerHour: Number(event.target.value) })} />
            </label>
          </div>
          <div className="incident-field-row">
            <label className="incident-field">Analysis provider
              <select value={policy.analysisProvider ?? "agent"} onChange={(event) => setPolicy({ ...policy, analysisProvider: event.target.value })}>
                <option value="agent">agent</option>
                <option value="none">none</option>
              </select>
            </label>
            <label className="incident-field">Default postmortem engine
              <select value={policy.postmortemEngine ?? "builtin"} onChange={(event) => setPolicy({ ...policy, postmortemEngine: event.target.value })}>
                {engines.map((engine) => <option key={engine.name} value={engine.name}>{engine.label}</option>)}
              </select>
            </label>
            <label className="incident-check">
              <input type="checkbox" checked={policy.autoPublish ?? false} onChange={(event) => setPolicy({ ...policy, autoPublish: event.target.checked })} />
              Auto-publish postmortems
            </label>
          </div>
          <div className="incident-form-actions">
            <button className="incident-primary-button" type="button" onClick={() => void savePolicy()} disabled={saving}>
              {saving ? "Saving…" : "Save policy"}
            </button>
          </div>
        </section>
      )}

      {tab === "postmortems" && (
        <section className="incidents-panel">
          <div className="incidents-panel-head"><h3>Resolved incidents</h3></div>
          {resolved.length === 0 ? (
            <p className="incidents-muted">No resolved incidents in this view.</p>
          ) : (
            <div className="incident-list">
              {resolved.map((incident) => (
                <Link
                  key={incident.id}
                  className="incident-card incident-card--resolved"
                  to={`/incidents/${encodeURIComponent(incident.id)}?partition=${encodeURIComponent(incident.partition)}&tab=postmortem`}
                >
                  <div className="incident-card__head">
                    <CheckCircle2 size={16} />
                    <strong>{incident.title || incident.set}</strong>
                    <span className="incident-state">Resolved</span>
                  </div>
                  <div className="incident-card__meta">
                    <span>MTTR {durationSince(incident.openedAt)}</span>
                    <span>{incident.owner || "sentinel"}</span>
                    <span>{incident.resolution || "no resolution note"}</span>
                  </div>
                </Link>
              ))}
            </div>
          )}
        </section>
      )}

      <p className="incidents-footnote">
        <Activity size={12} /> Incidents are reconciled every 15 seconds and refreshed on this page every 15 seconds.
        <Link to="/alerts"> <XCircle size={12} /> Alert routing</Link>
      </p>
    </div>
  );
}
