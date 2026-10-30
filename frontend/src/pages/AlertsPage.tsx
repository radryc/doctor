import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  Bell,
  BellOff,
  CheckCircle2,
  Clock3,
  Mail,
  RefreshCw,
  Send,
  Trash2,
  XCircle,
} from "lucide-react";
import { api } from "../api/client";
import { useConfig } from "../context/ConfigContext";
import type {
  Alert,
  AlertListResponse,
  Capabilities,
  DeliveryRecord,
  ReceiverConfig,
  Silence,
  SourceStatus,
  AWSPolicy,
  SubspaceRequest,
  AlertRuleGroupSummary,
} from "../types/alerts";

const TABS = [
  { key: "inbox", label: "Inbox" },
  { key: "subspace", label: "Subspace" },
  { key: "rules", label: "Rules" },
  { key: "silences", label: "Silences" },
  { key: "sources", label: "Sources" },
  { key: "receivers", label: "Receivers" },
  { key: "delivery", label: "Delivery" },
] as const;

type TabKey = (typeof TABS)[number]["key"];

function initialTab(pathname: string): TabKey {
  const part = pathname.split("/")[2];
  return (TABS.find((tab) => tab.key === part)?.key ?? "inbox") as TabKey;
}

function severityClass(severity: string): string {
  switch (severity.toLowerCase()) {
    case "critical":
      return "border-rose-500/40 bg-rose-500/10 text-rose-300";
    case "warning":
      return "border-amber-500/40 bg-amber-500/10 text-amber-300";
    default:
      return "border-sky-500/40 bg-sky-500/10 text-sky-300";
  }
}

function statusClass(status: string): string {
  switch (status) {
    case "firing":
      return "border-rose-500/40 bg-rose-500/10 text-rose-300";
    case "pending":
      return "border-amber-500/40 bg-amber-500/10 text-amber-300";
    case "resolved":
      return "border-emerald-500/40 bg-emerald-500/10 text-emerald-300";
    case "no_data":
      return "border-slate-500/40 bg-slate-500/10 text-slate-300";
    default:
      return "border-slate-500/40 bg-slate-500/10 text-slate-300";
  }
}

function age(value: string): string {
  const ms = Date.now() - new Date(value).getTime();
  if (!Number.isFinite(ms) || ms < 0) return "now";
  const minutes = Math.floor(ms / 60000);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

function can(caps: Capabilities | null, capability: string): boolean {
  return Boolean(caps?.capabilities?.includes(capability));
}

export default function AlertsPage() {
  const { config } = useConfig();
  const location = useLocation();
  const navigate = useNavigate();
  const tab = initialTab(location.pathname);

  const [tenant, setTenant] = useState(config.default_tenant || "default");
  const [partition, setPartition] = useState(config.default_partition || "");
  const [caps, setCaps] = useState<Capabilities | null>(null);
  const [alerts, setAlerts] = useState<Alert[]>([]);
  const [subspaceAlerts, setSubspaceAlerts] = useState<Alert[]>([]);
  const [ruleGroups, setRuleGroups] = useState<AlertRuleGroupSummary[]>([]);
  const [ruleSelected, setRuleSelected] = useState<AlertRuleGroupSummary | null>(null);
  const [ruleYaml, setRuleYaml] = useState("");
  const [ruleResult, setRuleResult] = useState("");
  const [silences, setSilences] = useState<Silence[]>([]);
  const [sources, setSources] = useState<SourceStatus[]>([]);
  const [policy, setPolicy] = useState<AWSPolicy | null>(null);
  const [policyText, setPolicyText] = useState("");
  const [receivers, setReceivers] = useState<ReceiverConfig | null>(null);
  const [receiverText, setReceiverText] = useState("");
  const [delivery, setDelivery] = useState<DeliveryRecord[]>([]);
  const [selected, setSelected] = useState<Alert | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [statusFilter, setStatusFilter] = useState("all");
  const [severityFilter, setSeverityFilter] = useState("all");
  const [sourceFilter, setSourceFilter] = useState("all");
  const [silenceFor, setSilenceFor] = useState<Alert | null>(null);
  const [silenceScope, setSilenceScope] = useState("instance");
  const [silenceMinutes, setSilenceMinutes] = useState("60");
  const [silenceComment, setSilenceComment] = useState("");
  const [saving, setSaving] = useState(false);
  const [broadcastOpen, setBroadcastOpen] = useState(false);
  const [broadcastName, setBroadcastName] = useState("PlatformNotice");
  const [broadcastSummary, setBroadcastSummary] = useState("");
  const [broadcastSeverity, setBroadcastSeverity] = useState("warning");
  const [broadcastAudience, setBroadcastAudience] = useState("partition-owners");
  const [broadcastTargets, setBroadcastTargets] = useState("");
  const [broadcastDedupe, setBroadcastDedupe] = useState("");
  const [broadcastMinutes, setBroadcastMinutes] = useState("60");

  const params = useMemo(() => {
    const p = new URLSearchParams();
    if (tenant) p.set("tenant", tenant);
    if (partition) p.set("partition", partition);
    return p;
  }, [tenant, partition]);

  const loadCaps = useCallback(async () => {
    if (!tenant || !partition) {
      setCaps(null);
      return;
    }
    try {
      setCaps(await api.getAlertCapabilities(tenant, partition));
    } catch {
      setCaps(null);
    }
  }, [tenant, partition]);

  const loadInbox = useCallback(async () => {
    if (!tenant || !partition) {
      setAlerts([]);
      return;
    }
    const p = new URLSearchParams(params);
    if (statusFilter !== "all") p.set("status", statusFilter);
    if (severityFilter !== "all") p.set("severity", severityFilter);
    if (sourceFilter !== "all") p.set("source", sourceFilter);
    try {
      const data: AlertListResponse = await api.getAlerts(p);
      setAlerts(data.alerts ?? []);
      setError("");
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not load alerts");
    }
  }, [params, partition, severityFilter, sourceFilter, statusFilter, tenant]);

  const loadTab = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      await loadCaps();
      if (tab === "inbox") await loadInbox();
      if (tab === "subspace") {
        const p = new URLSearchParams({ tenant, scope: "subspace" });
        if (partition) p.set("partition", partition);
        const data = await api.getAlerts(p);
        setSubspaceAlerts(data.alerts ?? []);
      }
      if (tab === "rules" && partition) {
        const data = await api.getAlertRuleGroups(tenant, partition);
        setRuleGroups(data.groups ?? []);
      }
      if (tab === "silences") {
        const p = new URLSearchParams(params);
        const data = await api.getSilences(p);
        setSilences(data.silences ?? []);
      }
      if (tab === "sources") {
        const [sourceData, policyData] = await Promise.all([
          api.getSources(params),
          api.getAWSPolicy(params),
        ]);
        setSources(sourceData.sources ?? []);
        setPolicy(policyData);
        setPolicyText(JSON.stringify(policyData, null, 2));
      }
      if (tab === "receivers") {
        const data = await api.getReceivers(params);
        setReceivers(data);
        setReceiverText(JSON.stringify(data, null, 2));
      }
      if (tab === "delivery") {
        const data = await api.getDelivery(params);
        setDelivery(data.delivery ?? []);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "request failed");
    } finally {
      setLoading(false);
    }
  }, [loadCaps, loadInbox, params, tab, tenant]);

  useEffect(() => {
    void loadTab();
    const timer = window.setInterval(() => void loadTab(), 20_000);
    return () => window.clearInterval(timer);
  }, [loadTab]);

  const switchTab = (key: TabKey) => {
    setSelected(null);
    setSilenceFor(null);
    navigate(key === "inbox" ? "/alerts" : `/alerts/${key}`);
  };

  const acknowledge = async (alert: Alert) => {
    try {
      const updated = await api.ackAlert(tenant, partition, alert.fingerprint, !alert.acked);
      setAlerts((prev) => prev.map((item) => (item.fingerprint === updated.fingerprint ? updated : item)));
      setSelected(updated);
      setNotice(updated.acked ? "Alert acknowledged" : "Acknowledgement removed");
    } catch (e) {
      setError(e instanceof Error ? e.message : "ack failed");
    }
  };

  const openSilence = (alert: Alert) => {
    setSilenceFor(alert);
    setSilenceScope("instance");
    setSilenceMinutes("60");
    setSilenceComment("");
  };

  const createSilence = async () => {
    if (!silenceFor) return;
    setSaving(true);
    setError("");
    const now = new Date();
    const ends = new Date(now.getTime() + Number(silenceMinutes) * 60_000);
    const matchers: Array<{ name: string; operator: string; value: string }> = [];
    if (silenceScope === "partition") {
      matchers.push({ name: "partition", operator: "=", value: partition });
    } else {
      matchers.push({ name: "alertname", operator: "=", value: silenceFor.labels.alertname || "alert" });
      matchers.push({ name: "partition", operator: "=", value: partition });
      if (silenceScope === "service" && silenceFor.labels.service) {
        matchers.push({ name: "service", operator: "=", value: silenceFor.labels.service });
      }
      if (silenceScope === "instance") {
        for (const [key, value] of Object.entries(silenceFor.labels)) {
          if (key === "alertname" || key === "partition" || key === "source") continue;
          matchers.push({ name: key, operator: "=", value });
        }
      }
    }
    try {
      await api.createSilence(tenant, partition, {
        tenant,
        partition,
        matchers,
        startsAt: now.toISOString(),
        endsAt: ends.toISOString(),
        scope: silenceScope,
        comment: silenceComment,
      });
      setSilenceFor(null);
      setNotice("Silence created");
      await loadTab();
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not create silence");
    } finally {
      setSaving(false);
    }
  };

  const removeSilence = async (silence: Silence) => {
    try {
      await api.deleteSilence(silence.tenant, silence.partition, silence.id);
      setSilences((prev) => prev.filter((item) => item.id !== silence.id));
      setNotice("Silence deleted");
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not delete silence");
    }
  };

  const savePolicy = async () => {
    try {
      const parsed = JSON.parse(policyText) as AWSPolicy;
      const result = await api.putAWSPolicy(params, parsed);
      setPolicy(result.policy);
      setNotice("Alarm selection policy saved");
    } catch (e) {
      setError(e instanceof Error ? e.message : "invalid policy JSON");
    }
  };

  const saveReceivers = async () => {
    try {
      const parsed = JSON.parse(receiverText) as ReceiverConfig;
      await api.putReceivers(params, parsed);
      setReceivers(parsed);
      setNotice("Receiver configuration saved");
    } catch (e) {
      setError(e instanceof Error ? e.message : "invalid receiver JSON");
    }
  };

  const broadcast = async () => {
    setSaving(true);
    setError("");
    const now = new Date();
    const targets = broadcastTargets.split(",").map((value) => value.trim()).filter(Boolean);
    const req: SubspaceRequest = {
      tenant,
      source: "sentinel",
      sourcePartition: partition,
      severity: broadcastSeverity,
      dedupeKey: broadcastDedupe || `platform.${broadcastName}.${now.toISOString()}`,
      labels: { alertname: broadcastName },
      annotations: { summary: broadcastSummary || broadcastName },
      audience: {
        type: broadcastAudience,
        ...(broadcastAudience === "partitions" || broadcastAudience === "partition-owners"
          ? { partitions: targets }
          : {}),
        ...(broadcastAudience === "groups" ? { groups: targets } : {}),
      },
      expiresAt: new Date(now.getTime() + Number(broadcastMinutes) * 60_000).toISOString(),
    };
    try {
      await api.createSubspaceAlert(tenant, req);
      setBroadcastOpen(false);
      setNotice("Subspace alert broadcast");
      await loadTab();
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not broadcast subspace alert");
    } finally {
      setSaving(false);
    }
  };

  const resolveSubspace = async (alert: Alert) => {
    if (!alert.dedupeKey) return;
    try {
      await api.resolveSubspaceAlert(tenant, alert.dedupeKey);
      setNotice("Subspace alert resolved");
      await loadTab();
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not resolve subspace alert");
    }
  };

  const openRule = async (group: AlertRuleGroupSummary) => {
    setRuleSelected(group);
    setRuleResult("");
    try {
      setRuleYaml(await api.getAlertRuleGroup(group.tenant, group.partition, group.group));
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not load rule group");
    }
  };

  const newRule = () => {
    setRuleSelected({ tenant, partition, group: "new-group", interval: "1m", rules: 0 });
    setRuleYaml(`version: 1
groups:
  - name: new-group
    tenant: ${tenant}
    partition: ${partition}
    interval: 1m
    rules:
      - alert: ExampleAlert
        expr: sum(rate(http_requests_total{status=~"5.."}[5m])) > 0.05
        for: 5m
        labels:
          severity: warning
`);
    setRuleResult("");
  };

  const saveRule = async () => {
    if (!ruleSelected) return;
    try {
      await api.putAlertRuleGroup(ruleSelected.tenant, ruleSelected.partition, ruleSelected.group, ruleYaml);
      setNotice("Rule group saved");
      await loadTab();
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not save rule group");
    }
  };

  const testRule = async () => {
    if (!ruleSelected) return;
    try {
      const result = await api.testAlertRuleGroup(ruleSelected.tenant, ruleSelected.partition, ruleSelected.group);
      setRuleResult(JSON.stringify(result, null, 2));
    } catch (e) {
      setRuleResult(e instanceof Error ? e.message : "evaluation failed");
    }
  };

  const deleteRule = async () => {
    if (!ruleSelected) return;
    try {
      await api.deleteAlertRuleGroup(ruleSelected.tenant, ruleSelected.partition, ruleSelected.group);
      setRuleSelected(null);
      setNotice("Rule group deleted");
      await loadTab();
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not delete rule group");
    }
  };

  const filteredAlerts = alerts;

  return (
    <div className="h-full w-full overflow-hidden bg-slate-950 text-slate-100">
      <div className="flex h-full flex-col">
        <header className="border-b border-slate-800 px-5 py-4">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <p className="text-[10px] font-bold uppercase tracking-[0.2em] text-cyan-400/80">Sentinel</p>
              <h1 className="mt-1 text-xl font-semibold">Alerts & delivery</h1>
              <p className="mt-1 text-xs text-slate-500">
                Partition-scoped alert state, silences, sources, receivers, and delivery log.
              </p>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <input
                value={tenant}
                onChange={(event) => setTenant(event.target.value)}
                className="w-28 rounded border border-slate-700 bg-slate-900 px-2 py-1 text-xs"
                placeholder="tenant"
              />
              <input
                value={partition}
                onChange={(event) => setPartition(event.target.value)}
                className="w-40 rounded border border-slate-700 bg-slate-900 px-2 py-1 text-xs"
                placeholder="partition"
              />
              <button
                type="button"
                onClick={() => void loadTab()}
                className="inline-flex items-center gap-1 rounded border border-slate-700 bg-slate-900 px-2.5 py-1.5 text-xs hover:border-cyan-500/40 hover:text-cyan-300"
              >
                <RefreshCw size={13} className={loading ? "animate-spin" : ""} /> Refresh
              </button>
            </div>
          </div>
          <nav className="mt-4 flex flex-wrap gap-1">
            {TABS.map((item) => (
              <button
                key={item.key}
                type="button"
                onClick={() => switchTab(item.key)}
                className={`rounded px-3 py-1.5 text-xs font-medium ${
                  tab === item.key
                    ? "bg-cyan-500/15 text-cyan-300"
                    : "text-slate-400 hover:bg-slate-900 hover:text-slate-200"
                }`}
              >
                {item.label}
              </button>
            ))}
          </nav>
        </header>

        {(error || notice) && (
          <div className={`border-b px-5 py-2 text-xs ${error ? "border-rose-500/30 bg-rose-500/10 text-rose-300" : "border-emerald-500/30 bg-emerald-500/10 text-emerald-300"}`}>
            {error || notice}
          </div>
        )}

        <div className="min-h-0 flex-1 overflow-auto p-5">
          {!partition && (
            <div className="rounded-lg border border-amber-500/30 bg-amber-500/10 p-4 text-sm text-amber-200">
              Enter a partition to load Sentinel alerts. Partition is the access boundary.
            </div>
          )}

          {tab === "inbox" && partition && (
            <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1fr)_390px]">
              <section className="rounded-lg border border-slate-800 bg-slate-900/50">
                <div className="flex flex-wrap items-center gap-2 border-b border-slate-800 px-4 py-3">
                  <select value={statusFilter} onChange={(e) => setStatusFilter(e.target.value)} className="rounded border border-slate-700 bg-slate-950 px-2 py-1 text-xs">
                    <option value="all">All states</option>
                    <option value="firing">Firing</option>
                    <option value="pending">Pending</option>
                    <option value="resolved">Resolved</option>
                    <option value="no_data">No data</option>
                  </select>
                  <select value={severityFilter} onChange={(e) => setSeverityFilter(e.target.value)} className="rounded border border-slate-700 bg-slate-950 px-2 py-1 text-xs">
                    <option value="all">All severities</option>
                    <option value="critical">Critical</option>
                    <option value="warning">Warning</option>
                    <option value="info">Info</option>
                  </select>
                  <select value={sourceFilter} onChange={(e) => setSourceFilter(e.target.value)} className="rounded border border-slate-700 bg-slate-950 px-2 py-1 text-xs">
                    <option value="all">All sources</option>
                    <option value="guardian">Guardian</option>
                    <option value="aws">AWS</option>
                    <option value="alertmanager">Alertmanager</option>
                    <option value="sentinel">Sentinel rule</option>
                  </select>
                  <span className="ml-auto text-[11px] text-slate-500">{filteredAlerts.length} alerts</span>
                </div>
                <div className="overflow-x-auto">
                  <table className="w-full text-left text-xs">
                    <thead className="bg-slate-950/70 text-slate-500">
                      <tr>
                        <th className="px-3 py-2">Severity</th>
                        <th className="px-3 py-2">State</th>
                        <th className="px-3 py-2">Alert</th>
                        <th className="px-3 py-2">Source</th>
                        <th className="px-3 py-2">Age</th>
                        <th className="px-3 py-2">Flags</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-slate-800">
                      {filteredAlerts.map((alert) => (
                        <tr
                          key={alert.fingerprint}
                          className={`cursor-pointer hover:bg-slate-800/60 ${selected?.fingerprint === alert.fingerprint ? "bg-slate-800/70" : ""}`}
                          onClick={() => setSelected(alert)}
                        >
                          <td className="px-3 py-2">
                            <span className={`rounded border px-1.5 py-0.5 text-[10px] font-semibold uppercase ${severityClass(alert.severity)}`}>
                              {alert.severity}
                            </span>
                          </td>
                          <td className="px-3 py-2">
                            <span className={`rounded border px-1.5 py-0.5 text-[10px] font-semibold uppercase ${statusClass(alert.status)}`}>
                              {alert.status}
                            </span>
                          </td>
                          <td className="px-3 py-2">
                            <div className="font-medium text-slate-200">{alert.labels.alertname || alert.fingerprint.slice(0, 12)}</div>
                            <div className="mt-0.5 truncate text-[10px] text-slate-500">{alert.partition} · {alert.tenant}</div>
                          </td>
                          <td className="px-3 py-2 text-slate-400">{alert.source}</td>
                          <td className="px-3 py-2 text-slate-400">{age(alert.startsAt)}</td>
                          <td className="px-3 py-2">
                            <div className="flex gap-1">
                              {alert.acked && <span className="rounded border border-emerald-500/30 bg-emerald-500/10 px-1 text-[10px] text-emerald-300">ack</span>}
                              {alert.silenceIds?.length ? <span className="rounded border border-amber-500/30 bg-amber-500/10 px-1 text-[10px] text-amber-300">silenced</span> : null}
                            </div>
                          </td>
                        </tr>
                      ))}
                      {filteredAlerts.length === 0 && (
                        <tr>
                          <td colSpan={6} className="px-4 py-10 text-center text-slate-500">No alerts in this partition.</td>
                        </tr>
                      )}
                    </tbody>
                  </table>
                </div>
              </section>

              <aside className="rounded-lg border border-slate-800 bg-slate-900/50">
                {selected ? (
                  <div className="flex h-full flex-col">
                    <div className="border-b border-slate-800 p-4">
                      <div className="flex items-start justify-between gap-2">
                        <div>
                          <h2 className="text-sm font-semibold text-slate-100">{selected.labels.alertname}</h2>
                          <p className="mt-1 text-[11px] text-slate-500">{selected.source} · {selected.partition}</p>
                        </div>
                        <span className={`rounded border px-1.5 py-0.5 text-[10px] uppercase ${severityClass(selected.severity)}`}>{selected.severity}</span>
                      </div>
                      <p className="mt-3 text-xs text-slate-300">{selected.annotations?.summary || "No summary"}</p>
                      <div className="mt-3 flex flex-wrap gap-2">
                        {can(caps, "alerts.ack") && (
                          <button type="button" onClick={() => void acknowledge(selected)} className="btn text-xs">
                            {selected.acked ? "Unack" : "Ack"}
                          </button>
                        )}
                        {can(caps, "alerts.silence") && (
                          <button type="button" onClick={() => openSilence(selected)} className="btn text-xs">
                            Silence
                          </button>
                        )}
                        {selected.labels.runbook_url && (
                          <a href={selected.labels.runbook_url} target="_blank" rel="noreferrer" className="btn text-xs">Runbook</a>
                        )}
                      </div>
                    </div>
                    <div className="flex-1 overflow-auto p-4 text-xs">
                      <h3 className="mb-2 text-[10px] font-bold uppercase tracking-wide text-slate-500">Timeline</h3>
                      <div className="space-y-2 border-l border-slate-700 pl-3">
                        <div><Clock3 size={12} className="inline" /> Started {new Date(selected.startsAt).toLocaleString()}</div>
                        {selected.lastNotifiedAt && <div><Send size={12} className="inline" /> Last notified {new Date(selected.lastNotifiedAt).toLocaleString()}</div>}
                        {selected.acked && <div><CheckCircle2 size={12} className="inline text-emerald-400" /> Acked {selected.ackedBy}</div>}
                        {selected.endsAt && <div><CheckCircle2 size={12} className="inline text-emerald-400" /> Resolved {new Date(selected.endsAt).toLocaleString()}</div>}
                      </div>
                      <h3 className="mb-2 mt-5 text-[10px] font-bold uppercase tracking-wide text-slate-500">Labels</h3>
                      <pre className="max-h-48 overflow-auto rounded border border-slate-800 bg-slate-950 p-2 text-[10px] text-slate-300">{JSON.stringify(selected.labels, null, 2)}</pre>
                      <h3 className="mb-2 mt-5 text-[10px] font-bold uppercase tracking-wide text-slate-500">Annotations</h3>
                      <pre className="max-h-48 overflow-auto rounded border border-slate-800 bg-slate-950 p-2 text-[10px] text-slate-300">{JSON.stringify(selected.annotations || {}, null, 2)}</pre>
                    </div>
                  </div>
                ) : (
                  <div className="flex h-full min-h-[240px] flex-col items-center justify-center gap-2 p-6 text-center text-slate-500">
                    <Bell size={24} />
                    <p className="text-xs">Select an alert to inspect its state, labels, annotations, and actions.</p>
                  </div>
                )}
              </aside>
            </div>
          )}

          {tab === "subspace" && (
            <section className="rounded-lg border border-slate-800 bg-slate-900/50">
              <div className="flex flex-wrap items-center justify-between gap-2 border-b border-slate-800 px-4 py-3">
                <div>
                  <h2 className="text-sm font-semibold">Subspace alerts</h2>
                  <p className="text-[11px] text-slate-500">Platform-wide events delivered to an explicit audience.</p>
                </div>
                {can(caps, "alerts.subspace.write") && (
                  <button type="button" onClick={() => setBroadcastOpen(true)} className="btn btn--primary text-xs">
                    <Send size={13} /> Broadcast
                  </button>
                )}
              </div>
              <div className="overflow-x-auto">
                <table className="w-full text-left text-xs">
                  <thead className="bg-slate-950/70 text-slate-500">
                    <tr><th className="px-3 py-2">Alert</th><th className="px-3 py-2">Severity</th><th className="px-3 py-2">Audience</th><th className="px-3 py-2">Source partition</th><th className="px-3 py-2">State</th><th className="px-3 py-2" /></tr>
                  </thead>
                  <tbody className="divide-y divide-slate-800">
                    {subspaceAlerts.map((alert) => (
                      <tr key={alert.fingerprint}>
                        <td className="px-3 py-2">
                          <div className="font-medium text-slate-200">{alert.labels.alertname}</div>
                          <div className="text-[10px] text-slate-500">{alert.annotations?.summary}</div>
                        </td>
                        <td className="px-3 py-2"><span className={`rounded border px-1.5 py-0.5 text-[10px] uppercase ${severityClass(alert.severity)}`}>{alert.severity}</span></td>
                        <td className="px-3 py-2 text-slate-400">
                          {alert.audience?.type === "partitions" || alert.audience?.type === "partition-owners"
                            ? (alert.audience.partitions || []).join(", ")
                            : alert.audience?.type || "—"}
                        </td>
                        <td className="px-3 py-2 text-slate-500">{alert.sourcePartition || "—"}</td>
                        <td className="px-3 py-2"><span className={`rounded border px-1.5 py-0.5 text-[10px] uppercase ${statusClass(alert.status)}`}>{alert.status}</span></td>
                        <td className="px-3 py-2 text-right">
                          {can(caps, "alerts.subspace.write") && alert.status !== "resolved" && (
                            <button type="button" onClick={() => void resolveSubspace(alert)} className="btn text-xs">Resolve</button>
                          )}
                        </td>
                      </tr>
                    ))}
                    {subspaceAlerts.length === 0 && (
                      <tr><td colSpan={6} className="px-4 py-10 text-center text-slate-500">No subspace alerts are visible to this identity.</td></tr>
                    )}
                  </tbody>
                </table>
              </div>
            </section>
          )}

          {tab === "rules" && partition && (
            <div className="grid grid-cols-1 gap-4 xl:grid-cols-[420px_minmax(0,1fr)]">
              <section className="rounded-lg border border-slate-800 bg-slate-900/50">
                <div className="flex items-center justify-between border-b border-slate-800 px-4 py-3">
                  <div>
                    <h2 className="text-sm font-semibold">Sentinel rules</h2>
                    <p className="text-[11px] text-slate-500">PromQL alert rules for {partition}.</p>
                  </div>
                  {can(caps, "alerts.rules.write") && <button type="button" onClick={newRule} className="btn text-xs">New</button>}
                </div>
                <div className="divide-y divide-slate-800">
                  {ruleGroups.map((group) => (
                    <button key={`${group.tenant}/${group.partition}/${group.group}`} type="button" onClick={() => void openRule(group)} className={`block w-full px-4 py-3 text-left hover:bg-slate-800/60 ${ruleSelected?.group === group.group ? "bg-slate-800/70" : ""}`}>
                      <div className="flex items-center justify-between gap-2">
                        <span className="text-xs font-medium text-slate-200">{group.group}</span>
                        <span className="text-[10px] text-slate-500">{group.rules} rules</span>
                      </div>
                      <div className="mt-1 text-[10px] text-slate-500">{group.tenant} · {group.partition} · every {group.interval}</div>
                    </button>
                  ))}
                  {ruleGroups.length === 0 && <div className="px-4 py-10 text-center text-xs text-slate-500">No Sentinel rules in this partition.</div>}
                </div>
              </section>
              <section className="rounded-lg border border-slate-800 bg-slate-900/50">
                {ruleSelected ? (
                  <div className="flex h-full flex-col">
                    <div className="flex flex-wrap items-center gap-2 border-b border-slate-800 px-4 py-3">
                      <input value={ruleSelected.group} onChange={(e) => setRuleSelected({ ...ruleSelected, group: e.target.value })} className="rounded border border-slate-700 bg-slate-950 px-2 py-1 text-xs" />
                      <span className="text-[10px] text-slate-500">{ruleSelected.tenant} · {ruleSelected.partition}</span>
                      <div className="ml-auto flex gap-2">
                        <button type="button" onClick={() => void testRule()} disabled={!can(caps, "alerts.rules.read")} className="btn text-xs">Test</button>
                        <button type="button" onClick={() => void saveRule()} disabled={!can(caps, "alerts.rules.write")} className="btn btn--primary text-xs">Save</button>
                        <button type="button" onClick={() => void deleteRule()} disabled={!can(caps, "alerts.rules.write")} className="btn text-xs text-rose-300">Delete</button>
                      </div>
                    </div>
                    <textarea value={ruleYaml} onChange={(e) => setRuleYaml(e.target.value)} className="min-h-[260px] flex-1 resize-none bg-slate-950 p-4 font-mono text-[11px] text-slate-300" spellCheck={false} />
                    {ruleResult && <pre className="max-h-48 overflow-auto border-t border-slate-800 bg-slate-950 p-3 text-[10px] text-slate-300">{ruleResult}</pre>}
                  </div>
                ) : (
                  <div className="flex h-full min-h-[240px] items-center justify-center p-6 text-xs text-slate-500">Select a rule group or create a new one.</div>
                )}
              </section>
            </div>
          )}

          {tab === "silences" && partition && (
            <section className="rounded-lg border border-slate-800 bg-slate-900/50">
              <div className="flex items-center justify-between border-b border-slate-800 px-4 py-3">
                <div>
                  <h2 className="text-sm font-semibold">Silences</h2>
                  <p className="text-[11px] text-slate-500">Active, scheduled, and expired silences for {partition}.</p>
                </div>
                <BellOff size={16} className="text-slate-500" />
              </div>
              <div className="overflow-x-auto">
                <table className="w-full text-left text-xs">
                  <thead className="bg-slate-950/70 text-slate-500">
                    <tr><th className="px-3 py-2">Scope</th><th className="px-3 py-2">Matchers</th><th className="px-3 py-2">Created by</th><th className="px-3 py-2">Until</th><th className="px-3 py-2" /></tr>
                  </thead>
                  <tbody className="divide-y divide-slate-800">
                    {silences.map((silence) => (
                      <tr key={silence.id}>
                        <td className="px-3 py-2">{silence.scope || "matcher"}</td>
                        <td className="px-3 py-2 text-[10px] text-slate-400">{silence.matchers.map((m) => `${m.name}${m.operator}${m.value}`).join(", ")}</td>
                        <td className="px-3 py-2 text-slate-400">{silence.createdBy || "—"}</td>
                        <td className="px-3 py-2 text-slate-400">{new Date(silence.endsAt).toLocaleString()}</td>
                        <td className="px-3 py-2 text-right">
                          {can(caps, "alerts.silence") && (
                            <button type="button" onClick={() => void removeSilence(silence)} className="text-rose-400 hover:text-rose-300"><Trash2 size={13} /></button>
                          )}
                        </td>
                      </tr>
                    ))}
                    {silences.length === 0 && <tr><td colSpan={5} className="px-4 py-10 text-center text-slate-500">No silences.</td></tr>}
                  </tbody>
                </table>
              </div>
            </section>
          )}

          {tab === "sources" && (
            <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
              <section className="rounded-lg border border-slate-800 bg-slate-900/50 p-4">
                <h2 className="text-sm font-semibold">Sources</h2>
                <div className="mt-3 space-y-2">
                  {sources.map((source) => (
                    <div key={source.name} className="flex items-center justify-between rounded border border-slate-800 bg-slate-950/60 px-3 py-2 text-xs">
                      <span className="font-medium text-slate-200">{source.name}</span>
                      <span className="text-slate-500">{source.mode} · {source.status}</span>
                    </div>
                  ))}
                </div>
              </section>
              <section className="rounded-lg border border-slate-800 bg-slate-900/50 p-4">
                <div className="flex items-center justify-between">
                  <div>
                    <h2 className="text-sm font-semibold">AWS alarm selection</h2>
                    <p className="text-[11px] text-slate-500">Only matching CloudWatch alarms are processed by Sentinel.</p>
                  </div>
                  <button type="button" onClick={() => void savePolicy()} disabled={!can(caps, "alerts.sources.write")} className="btn text-xs">Save</button>
                </div>
                <textarea
                  value={policyText}
                  onChange={(event) => setPolicyText(event.target.value)}
                  className="mt-3 h-72 w-full rounded border border-slate-800 bg-slate-950 p-3 font-mono text-[11px] text-slate-300"
                  spellCheck={false}
                />
                {policy && <p className="mt-2 text-[11px] text-slate-500">Default: {policy.default} · unscoped: {policy.unscoped || "ignore"}</p>}
              </section>
            </div>
          )}

          {tab === "receivers" && (
            <section className="rounded-lg border border-slate-800 bg-slate-900/50 p-4">
              <div className="flex items-center justify-between">
                <div>
                  <h2 className="text-sm font-semibold">Receivers and routes</h2>
                  <p className="text-[11px] text-slate-500">Secrets are referenced by environment variable, never stored inline.</p>
                </div>
                <button type="button" onClick={() => void saveReceivers()} disabled={!can(caps, "alerts.receivers.write")} className="btn text-xs"><Mail size={13} /> Save</button>
              </div>
              <textarea
                value={receiverText}
                onChange={(event) => setReceiverText(event.target.value)}
                className="mt-3 h-80 w-full rounded border border-slate-800 bg-slate-950 p-3 font-mono text-[11px] text-slate-300"
                spellCheck={false}
              />
              {receivers && <p className="mt-2 text-[11px] text-slate-500">{receivers.receivers.length} receivers · {receivers.routes.length} routes</p>}
            </section>
          )}

          {tab === "delivery" && partition && (
            <section className="rounded-lg border border-slate-800 bg-slate-900/50">
              <div className="flex items-center justify-between border-b border-slate-800 px-4 py-3">
                <div>
                  <h2 className="text-sm font-semibold">Delivery log</h2>
                  <p className="text-[11px] text-slate-500">Outbox attempts, retries, and failures for {partition}.</p>
                </div>
                <Send size={16} className="text-slate-500" />
              </div>
              <div className="overflow-x-auto">
                <table className="w-full text-left text-xs">
                  <thead className="bg-slate-950/70 text-slate-500">
                    <tr><th className="px-3 py-2">Alert</th><th className="px-3 py-2">Channel</th><th className="px-3 py-2">Status</th><th className="px-3 py-2">Attempts</th><th className="px-3 py-2">Error</th><th className="px-3 py-2">Updated</th></tr>
                  </thead>
                  <tbody className="divide-y divide-slate-800">
                    {delivery.map((record) => (
                      <tr key={record.id}>
                        <td className="px-3 py-2 font-mono text-[10px] text-slate-400">{record.fingerprint.slice(0, 12)}</td>
                        <td className="px-3 py-2">{record.channel}</td>
                        <td className="px-3 py-2"><span className={`rounded border px-1.5 py-0.5 text-[10px] ${record.status === "failed" ? "border-rose-500/30 text-rose-300" : "border-slate-600 text-slate-400"}`}>{record.status}</span></td>
                        <td className="px-3 py-2">{record.attempts}</td>
                        <td className="max-w-[280px] truncate px-3 py-2 text-rose-300/80">{record.lastError || "—"}</td>
                        <td className="px-3 py-2 text-slate-500">{record.updatedAt ? new Date(record.updatedAt).toLocaleTimeString() : "—"}</td>
                      </tr>
                    ))}
                    {delivery.length === 0 && <tr><td colSpan={6} className="px-4 py-10 text-center text-slate-500">No delivery attempts.</td></tr>}
                  </tbody>
                </table>
              </div>
            </section>
          )}
        </div>
      </div>

      {broadcastOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
          <div className="w-full max-w-lg rounded-lg border border-slate-700 bg-slate-900 p-5 shadow-2xl">
            <div className="flex items-start justify-between gap-3">
              <div>
                <h2 className="text-sm font-semibold">Broadcast subspace alert</h2>
                <p className="mt-1 text-[11px] text-slate-500">Deliver a platform event to an explicit audience.</p>
              </div>
              <button type="button" onClick={() => setBroadcastOpen(false)} className="text-slate-500 hover:text-slate-200"><XCircle size={16} /></button>
            </div>
            <label className="mt-4 block text-[11px] text-slate-400">Alert name</label>
            <input value={broadcastName} onChange={(e) => setBroadcastName(e.target.value)} className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs" />
            <label className="mt-3 block text-[11px] text-slate-400">Summary</label>
            <input value={broadcastSummary} onChange={(e) => setBroadcastSummary(e.target.value)} className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs" />
            <div className="mt-3 grid grid-cols-2 gap-3">
              <label className="block text-[11px] text-slate-400">Severity
                <select value={broadcastSeverity} onChange={(e) => setBroadcastSeverity(e.target.value)} className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs">
                  <option value="info">Info</option>
                  <option value="warning">Warning</option>
                  <option value="critical">Critical</option>
                </select>
              </label>
              <label className="block text-[11px] text-slate-400">Audience
                <select value={broadcastAudience} onChange={(e) => setBroadcastAudience(e.target.value)} className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs">
                  <option value="partition-owners">Partition owners</option>
                  <option value="partitions">Specific partitions</option>
                  <option value="tenant-admins">Tenant admins</option>
                  <option value="groups">Groups</option>
                  <option value="all">All partition users</option>
                </select>
              </label>
            </div>
            {(broadcastAudience === "partitions" || broadcastAudience === "partition-owners" || broadcastAudience === "groups") && (
              <>
                <label className="mt-3 block text-[11px] text-slate-400">{broadcastAudience === "groups" ? "Groups" : "Partitions"} (comma separated)</label>
                <input value={broadcastTargets} onChange={(e) => setBroadcastTargets(e.target.value)} className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs" />
              </>
            )}
            <div className="mt-3 grid grid-cols-2 gap-3">
              <label className="block text-[11px] text-slate-400">Dedupe key (optional)
                <input value={broadcastDedupe} onChange={(e) => setBroadcastDedupe(e.target.value)} className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs" />
              </label>
              <label className="block text-[11px] text-slate-400">Expires (minutes)
                <input value={broadcastMinutes} onChange={(e) => setBroadcastMinutes(e.target.value)} type="number" min="1" className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs" />
              </label>
            </div>
            <div className="mt-4 flex justify-end gap-2">
              <button type="button" onClick={() => setBroadcastOpen(false)} className="btn text-xs">Cancel</button>
              <button type="button" onClick={() => void broadcast()} disabled={saving} className="btn btn--primary text-xs">{saving ? "Sending…" : "Broadcast"}</button>
            </div>
          </div>
        </div>
      )}

      {silenceFor && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
          <div className="w-full max-w-lg rounded-lg border border-slate-700 bg-slate-900 p-5 shadow-2xl">
            <div className="flex items-start justify-between gap-3">
              <div>
                <h2 className="text-sm font-semibold">Silence alert</h2>
                <p className="mt-1 text-[11px] text-slate-500">Only matching alerts stop notifying. State keeps updating.</p>
              </div>
              <button type="button" onClick={() => setSilenceFor(null)} className="text-slate-500 hover:text-slate-200"><XCircle size={16} /></button>
            </div>
            <label className="mt-4 block text-[11px] text-slate-400">Scope</label>
            <select value={silenceScope} onChange={(e) => setSilenceScope(e.target.value)} className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs">
              <option value="instance">This alert instance</option>
              <option value="rule">This rule in {partition}</option>
              <option value="service">This service in {partition}</option>
              <option value="partition">Entire partition {partition}</option>
            </select>
            <label className="mt-3 block text-[11px] text-slate-400">Duration (minutes)</label>
            <input value={silenceMinutes} onChange={(e) => setSilenceMinutes(e.target.value)} type="number" min="1" max="10080" className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs" />
            <label className="mt-3 block text-[11px] text-slate-400">Comment</label>
            <textarea value={silenceComment} onChange={(e) => setSilenceComment(e.target.value)} className="mt-1 h-20 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs" placeholder="Why is this silenced?" />
            {silenceScope === "partition" && !can(caps, "alerts.silence.partition") && (
              <p className="mt-2 text-[11px] text-rose-300">Entire-partition silence requires elevated permission.</p>
            )}
            <div className="mt-4 flex justify-end gap-2">
              <button type="button" onClick={() => setSilenceFor(null)} className="btn text-xs">Cancel</button>
              <button type="button" onClick={() => void createSilence()} disabled={saving || (silenceScope === "partition" && !can(caps, "alerts.silence.partition"))} className="btn btn--primary text-xs">{saving ? "Saving…" : "Create silence"}</button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
