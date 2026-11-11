import { useEffect, useMemo, useState } from "react";
import { Code2, FolderPlus, ListChecks, Play, Save, Sparkles, Trash2 } from "lucide-react";
import { parse as parseYaml, stringify as stringifyYaml } from "yaml";
import { api } from "../../api/client";
import type {
  AlertPreviewResponse,
  AlertRule,
  AlertRuleFile,
  AlertRuleGroup,
  AlertRuleGroupSummary,
  Capabilities,
  Receiver,
} from "../../types/alerts";
import AlertBuilder from "./AlertBuilder";
import { defaultSpec, renderSummary } from "./signals";

interface AlertRulesTabProps {
  tenant: string;
  partition: string;
  groups: AlertRuleGroupSummary[];
  endpoints: Receiver[];
  caps: Capabilities | null;
  canWrite: boolean;
  onReload: () => Promise<void>;
  onNotice: (message: string) => void;
  onError: (message: string) => void;
}

function newRule(partition: string, index: number): AlertRule {
  const spec = defaultSpec("errors", partition);
  spec.summary = renderSummary(spec);
  return {
    alert: index === 0 ? "NewAlert" : `NewAlert${index + 1}`,
    mode: "guided",
    spec,
    expr: "",
    for: spec.for,
    labels: { severity: spec.severity || "warning" },
    annotations: { summary: spec.summary, "sentinel.signal": spec.signal },
    notify: [],
  };
}

function emptyFile(tenant: string, partition: string, name: string): AlertRuleFile {
  return {
    version: 1,
    groups: [
      {
        name,
        tenant,
        partition,
        interval: "1m",
        rules: [newRule(partition, 0)],
      },
    ],
  };
}

export default function AlertRulesTab({
  tenant,
  partition,
  groups,
  endpoints,
  canWrite,
  onReload,
  onNotice,
  onError,
}: AlertRulesTabProps) {
  const [selectedName, setSelectedName] = useState("");
  const [originalName, setOriginalName] = useState("");
  const [file, setFile] = useState<AlertRuleFile | null>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [advanced, setAdvanced] = useState(false);
  const [raw, setRaw] = useState("");
  const [preview, setPreview] = useState<AlertPreviewResponse | null>(null);
  const [previewError, setPreviewError] = useState("");
  const [testing, setTesting] = useState(false);

  const group: AlertRuleGroup | null = file?.groups?.[0] ?? null;
  const selectedSummary = useMemo(
    () => groups.find((item) => item.group === selectedName),
    [groups, selectedName]
  );

  const openGroup = async (name: string) => {
    setLoading(true);
    setPreview(null);
    setPreviewError("");
    try {
      const data = await api.getAlertRuleGroup(tenant, partition, name);
      const normalized: AlertRuleFile = {
        version: data.version || 1,
        groups: (data.groups ?? []).map((item) => ({
          ...item,
          rules: (item.rules ?? []).map((rule) => ({
            ...rule,
            mode: rule.mode || (rule.spec ? "guided" : "promql"),
          })),
        })),
      };
      setFile(normalized);
      setSelectedName(name);
      setOriginalName(name);
      setRaw(stringifyYaml(normalized));
    } catch (error) {
      onError(error instanceof Error ? error.message : "could not load rule group");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (!selectedName && groups.length > 0) {
      void openGroup(groups[0].group);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [groups.length]);

  const newGroup = () => {
    const name = `custom-alerts-${groups.length + 1}`;
    const created = emptyFile(tenant, partition, name);
    setFile(created);
    setSelectedName(name);
    setOriginalName("");
    setRaw(stringifyYaml(created));
    setPreview(null);
    setPreviewError("");
  };

  const updateFile = (next: AlertRuleFile) => {
    setFile(next);
    setRaw(stringifyYaml(next));
  };

  const updateGroup = (patch: Partial<AlertRuleGroup>) => {
    if (!file || !group) return;
    updateFile({ ...file, groups: [{ ...group, ...patch }] });
  };

  const updateRule = (index: number, next: AlertRule) => {
    if (!group) return;
    const rules = group.rules.map((rule, i) => (i === index ? next : rule));
    updateGroup({ rules });
  };

  const addRule = () => {
    if (!group) return;
    updateGroup({ rules: [...group.rules, newRule(partition, group.rules.length)] });
  };

  const removeRule = (index: number) => {
    if (!group) return;
    updateGroup({ rules: group.rules.filter((_, i) => i !== index) });
  };

  const toggleMode = async (index: number, nextMode: "guided" | "promql") => {
    if (!group) return;
    const rule = group.rules[index];
    if (nextMode === "promql" && !rule.expr && rule.spec) {
      try {
        const compiled = await api.compileAlertRule(tenant, partition, rule.spec);
        updateRule(index, { ...rule, mode: "promql", expr: compiled.expr });
        return;
      } catch {
        // Keep the spec; the expression can be written or generated on save.
      }
    }
    updateRule(index, { ...rule, mode: nextMode });
  };

  const save = async () => {
    if (!group || !file) return;
    if (!group.name.trim()) {
      onError("Rule group needs a name");
      return;
    }
    setSaving(true);
    try {
      let next = file;
      if (advanced) {
        const parsed = parseYaml(raw) as AlertRuleFile;
        if (!parsed || !Array.isArray(parsed.groups) || parsed.groups.length === 0) {
          throw new Error("YAML must contain at least one group");
        }
        next = parsed;
      }
      const first = next.groups[0];
      first.tenant = tenant;
      first.partition = partition;
      await api.putAlertRuleGroup(tenant, partition, first.name, next);
      if (originalName && originalName !== first.name) {
        await api.deleteAlertRuleGroup(tenant, partition, originalName);
      }
      setSelectedName(first.name);
      setOriginalName(first.name);
      setFile(next);
      setRaw(stringifyYaml(next));
      onNotice("Alert rules saved");
      await onReload();
    } catch (error) {
      onError(error instanceof Error ? error.message : "could not save alert rules");
    } finally {
      setSaving(false);
    }
  };

  const removeGroup = async () => {
    if (!originalName) {
      setFile(null);
      setSelectedName("");
      setOriginalName("");
      return;
    }
    try {
      await api.deleteAlertRuleGroup(tenant, partition, originalName);
      setFile(null);
      setSelectedName("");
      setOriginalName("");
      onNotice("Rule group deleted");
      await onReload();
    } catch (error) {
      onError(error instanceof Error ? error.message : "could not delete rule group");
    }
  };

  const testGroup = async () => {
    if (!originalName) {
      onError("Save the group before testing it");
      return;
    }
    setTesting(true);
    setPreviewError("");
    try {
      const result = await api.testAlertRuleGroup(tenant, partition, originalName);
      setPreview({ expr: "", data: result } as AlertPreviewResponse);
    } catch (error) {
      setPreview(null);
      setPreviewError(error instanceof Error ? error.message : "evaluation failed");
    } finally {
      setTesting(false);
    }
  };

  const previewRule = async (index: number) => {
    if (!group) return;
    const rule = group.rules[index];
    setPreviewError("");
    setPreview(null);
    try {
      const result = await api.previewAlertRule(
        tenant,
        partition,
        rule.mode === "guided" && rule.spec ? { spec: rule.spec } : { expr: rule.expr }
      );
      setPreview(result);
    } catch (error) {
      setPreviewError(error instanceof Error ? error.message : "preview failed");
    }
  };

  return (
    <div className="grid min-h-full grid-cols-1 gap-4 xl:grid-cols-[320px_minmax(0,1fr)]">
      <section className="flex min-h-0 flex-col rounded-lg border border-slate-800 bg-slate-900/50">
        <div className="flex items-center justify-between border-b border-slate-800 px-4 py-3">
          <div>
            <h2 className="text-sm font-semibold">Alert rules</h2>
            <p className="text-[11px] text-slate-500">Guided or PromQL, for {partition}.</p>
          </div>
          {canWrite && (
            <button type="button" onClick={newGroup} className="btn text-xs" title="Create a rule group">
              <FolderPlus size={13} /> New
            </button>
          )}
        </div>
        <div className="min-h-0 flex-1 divide-y divide-slate-800 overflow-auto">
          {groups.map((item) => (
            <button
              key={`${item.tenant}/${item.partition}/${item.group}`}
              type="button"
              onClick={() => void openGroup(item.group)}
              className={`block w-full px-4 py-3 text-left hover:bg-slate-800/60 ${selectedName === item.group ? "bg-slate-800/70" : ""}`}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="text-xs font-medium text-slate-200">{item.group}</span>
                <span className="text-[10px] text-slate-500">{item.rules} rules</span>
              </div>
              <div className="mt-1 flex flex-wrap gap-1">
                {(item.alerts ?? []).slice(0, 4).map((alert) => (
                  <span
                    key={alert.alert}
                    className={`rounded border px-1 text-[9px] ${
                      alert.mode === "promql"
                        ? "border-violet-500/30 bg-violet-500/10 text-violet-300"
                        : "border-sky-500/30 bg-sky-500/10 text-sky-300"
                    }`}
                  >
                    {alert.alert}
                  </span>
                ))}
                {(item.alerts?.length ?? 0) > 4 && (
                  <span className="text-[9px] text-slate-500">+{(item.alerts?.length ?? 0) - 4}</span>
                )}
              </div>
              <div className="mt-1 text-[10px] text-slate-500">{item.tenant} · every {item.interval}</div>
            </button>
          ))}
          {groups.length === 0 && (
            <div className="px-4 py-10 text-center text-xs text-slate-500">
              No alert rules yet. Create one to get started.
            </div>
          )}
        </div>
      </section>

      <section className="flex min-h-[560px] flex-col rounded-lg border border-slate-800 bg-slate-900/50">
        {loading ? (
          <div className="flex flex-1 items-center justify-center p-6 text-xs text-slate-500">Loading…</div>
        ) : file && group ? (
          <div className="flex min-h-0 flex-1 flex-col">
            <div className="flex flex-wrap items-center gap-2 border-b border-slate-800 px-4 py-3">
              <input
                value={group.name}
                onChange={(event) => updateGroup({ name: event.target.value })}
                className="w-56 rounded border border-slate-700 bg-slate-950 px-2 py-1 text-xs"
                aria-label="Rule group name"
              />
              <select
                value={group.interval || "1m"}
                onChange={(event) => updateGroup({ interval: event.target.value })}
                className="rounded border border-slate-700 bg-slate-950 px-2 py-1 text-xs"
                aria-label="Evaluation interval"
              >
                {["30s", "1m", "5m", "15m", "1h"].map((value) => (
                  <option key={value} value={value}>every {value}</option>
                ))}
              </select>
              <span className="text-[10px] text-slate-500">{tenant} · {partition}</span>
              <div className="ml-auto flex flex-wrap gap-2">
                {canWrite && <button type="button" onClick={addRule} className="btn text-xs"><ListChecks size={13} /> Add alert</button>}
                <button type="button" onClick={() => setAdvanced((value) => !value)} className="btn text-xs" title="Toggle raw YAML">
                  <Code2 size={13} /> {advanced ? "Builder" : "Raw YAML"}
                </button>
                <button type="button" onClick={() => void testGroup()} disabled={testing} className="btn text-xs">
                  <Play size={13} /> Test
                </button>
                <button type="button" onClick={() => void save()} disabled={!canWrite || saving} className="btn btn--primary text-xs">
                  <Save size={13} /> {saving ? "Saving…" : "Save"}
                </button>
                <button type="button" onClick={() => void removeGroup()} disabled={!canWrite} className="btn text-xs text-rose-300">
                  <Trash2 size={13} /> Delete
                </button>
              </div>
            </div>

            <div className="min-h-0 flex-1 overflow-auto p-4">
              {advanced ? (
                <textarea
                  value={raw}
                  onChange={(event) => setRaw(event.target.value)}
                  className="h-[480px] w-full resize-y rounded border border-slate-800 bg-slate-950 p-3 font-mono text-[11px] text-slate-300"
                  spellCheck={false}
                />
              ) : (
                <div className="space-y-4">
                  {group.rules.map((rule, index) => (
                    <div key={`${rule.alert}-${index}`} className="rounded-lg border border-slate-800 bg-slate-900/40">
                      <div className="flex flex-wrap items-center gap-2 border-b border-slate-800 px-3 py-2">
                        <input
                          value={rule.alert}
                          onChange={(event) => updateRule(index, { ...rule, alert: event.target.value })}
                          className="w-56 rounded border border-slate-700 bg-slate-950 px-2 py-1 font-mono text-xs"
                          aria-label="Alert name"
                        />
                        <div className="inline-flex overflow-hidden rounded border border-slate-700">
                          <button
                            type="button"
                            onClick={() => void toggleMode(index, "guided")}
                            className={`px-2 py-1 text-[11px] ${rule.mode !== "promql" ? "bg-slate-700 text-slate-100" : "bg-slate-950 text-slate-400"}`}
                          >
                            <Sparkles size={11} className="inline" /> Guided
                          </button>
                          <button
                            type="button"
                            onClick={() => void toggleMode(index, "promql")}
                            className={`px-2 py-1 text-[11px] ${rule.mode === "promql" ? "bg-slate-700 text-slate-100" : "bg-slate-950 text-slate-400"}`}
                          >
                            <Code2 size={11} className="inline" /> PromQL
                          </button>
                        </div>
                        <span className={`rounded border px-1.5 py-0.5 text-[10px] uppercase ${
                          (rule.labels?.severity || rule.spec?.severity) === "critical"
                            ? "border-rose-500/40 bg-rose-500/10 text-rose-300"
                            : "border-amber-500/40 bg-amber-500/10 text-amber-300"
                        }`}>
                          {rule.labels?.severity || rule.spec?.severity || "warning"}
                        </span>
                        <div className="ml-auto flex gap-2">
                          <button type="button" onClick={() => void previewRule(index)} className="btn text-xs"><Play size={12} /> Preview</button>
                          {canWrite && (
                            <button type="button" onClick={() => removeRule(index)} className="text-slate-500 hover:text-rose-300" aria-label="Remove alert">
                              <Trash2 size={13} />
                            </button>
                          )}
                        </div>
                      </div>
                      <div className="p-3">
                        {rule.mode === "promql" ? (
                          <div className="space-y-2">
                            <textarea
                              value={rule.expr}
                              onChange={(event) => updateRule(index, { ...rule, expr: event.target.value })}
                              className="h-28 w-full resize-y rounded border border-slate-800 bg-slate-950 p-2 font-mono text-[11px] text-slate-300"
                              spellCheck={false}
                              placeholder="sum(rate(http_requests_total{status=~&quot;5..&quot;}[5m])) > 0.05"
                            />
                            <p className="text-[10px] text-slate-500">
                              PromQL is evaluated as written. Enrich it with labels, annotations, and destinations below if needed.
                            </p>
                            <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
                              <label className="block text-[11px] text-slate-400">
                                For
                                <select
                                  value={rule.for || "5m"}
                                  onChange={(event) => updateRule(index, { ...rule, for: event.target.value })}
                                  className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
                                >
                                  {["1m", "5m", "10m", "15m", "30m", "1h"].map((value) => (
                                    <option key={value} value={value}>{value}</option>
                                  ))}
                                </select>
                              </label>
                              <label className="block text-[11px] text-slate-400">
                                Severity
                                <select
                                  value={rule.labels?.severity || "warning"}
                                  onChange={(event) => updateRule(index, {
                                    ...rule,
                                    labels: { ...(rule.labels ?? {}), severity: event.target.value },
                                  })}
                                  className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
                                >
                                  <option value="info">Info</option>
                                  <option value="warning">Warning</option>
                                  <option value="critical">Critical</option>
                                </select>
                              </label>
                              <label className="block text-[11px] text-slate-400">
                                Destination
                                <select
                                  value={rule.notify?.[0] || ""}
                                  onChange={(event) => updateRule(index, {
                                    ...rule,
                                    notify: event.target.value ? [event.target.value] : [],
                                  })}
                                  className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
                                >
                                  <option value="">Use partition/default routing</option>
                                  {endpoints.map((endpoint) => (
                                    <option key={endpoint.name} value={endpoint.name}>{endpoint.name}</option>
                                  ))}
                                </select>
                              </label>
                            </div>
                            <label className="block text-[11px] text-slate-400">
                              Summary
                              <input
                                value={rule.annotations?.summary ?? ""}
                                onChange={(event) => updateRule(index, {
                                  ...rule,
                                  annotations: { ...(rule.annotations ?? {}), summary: event.target.value },
                                })}
                                className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-1.5 text-xs"
                              />
                            </label>
                          </div>
                        ) : (
                          <AlertBuilder
                            tenant={tenant}
                            partition={partition}
                            rule={rule}
                            endpoints={endpoints}
                            onChange={(next) => updateRule(index, next)}
                          />
                        )}
                      </div>
                    </div>
                  ))}
                  {group.rules.length === 0 && (
                    <div className="rounded border border-dashed border-slate-700 p-8 text-center text-xs text-slate-500">
                      No alerts in this group. Use “Add alert”.
                    </div>
                  )}
                </div>
              )}

              {(preview || previewError) && (
                <div className="mt-4 rounded border border-slate-800 bg-slate-950 p-3 text-[11px]">
                  {previewError ? (
                    <span className="text-rose-300">{previewError}</span>
                  ) : preview ? (
                    <div className="space-y-2">
                      {preview.expr && (
                        <code className="block overflow-x-auto rounded bg-slate-900 p-2 font-mono text-[10px] text-slate-300">{preview.expr}</code>
                      )}
                      {preview.instances && (
                        <div className="text-slate-400">
                          {preview.wouldFire ? "Would fire now" : "Not firing right now"} · {preview.instances.length} series
                        </div>
                      )}
                      {!preview.instances && preview.data !== undefined && (
                        <pre className="max-h-48 overflow-auto text-[10px] text-slate-300">{JSON.stringify(preview.data, null, 2)}</pre>
                      )}
                    </div>
                  ) : null}
                </div>
              )}
            </div>
          </div>
        ) : (
          <div className="flex flex-1 flex-col items-center justify-center gap-2 p-8 text-center text-slate-500">
            <ListChecks size={26} />
            <p className="text-xs">
              {selectedSummary ? "Loading rule group…" : "Select a rule group or create a new one."}
            </p>
          </div>
        )}
      </section>
    </div>
  );
}
