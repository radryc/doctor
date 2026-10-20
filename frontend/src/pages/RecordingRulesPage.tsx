import { useCallback, useEffect, useState } from "react";
import { AlertCircle, ArrowLeft, Copy, FileCode2, ListChecks, Play, RefreshCw } from "lucide-react";
import { api } from "../api/client";
import { copyText } from "../lib/clipboard";
import type { RecordingRuleGroupSummary } from "../types/api";

export default function RecordingRulesPage() {
  const [groups, setGroups] = useState<RecordingRuleGroupSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [selected, setSelected] = useState<RecordingRuleGroupSummary | null>(null);
  const [yaml, setYaml] = useState("");
  const [yamlLoading, setYamlLoading] = useState(false);
  const [evaluating, setEvaluating] = useState(false);
  const [evalResult, setEvalResult] = useState("");
  const [copied, setCopied] = useState(false);

  const loadGroups = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const data = await api.getRecordingRuleGroups();
      setGroups(data.groups ?? []);
    } catch (e) {
      setError(e instanceof Error ? e.message : "could not load recording rules");
      setGroups([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadGroups();
    const interval = setInterval(loadGroups, 30_000);
    return () => clearInterval(interval);
  }, [loadGroups]);

  const openGroup = async (group: RecordingRuleGroupSummary) => {
    setSelected(group);
    setYaml("");
    setEvalResult("");
    setYamlLoading(true);
    try {
      setYaml(await api.getRecordingRuleGroup(group.tenant, group.group));
    } catch (e) {
      setYaml(e instanceof Error ? e.message : "could not load rule group");
    } finally {
      setYamlLoading(false);
    }
  };

  const evaluate = async () => {
    if (!selected) return;
    setEvaluating(true);
    setEvalResult("");
    try {
      const result = await api.evaluateRecordingRuleGroup(selected.tenant, selected.group);
      setEvalResult(JSON.stringify(result, null, 2));
    } catch (e) {
      setEvalResult(e instanceof Error ? e.message : "evaluation failed");
    } finally {
      setEvaluating(false);
    }
  };

  const copyExample = async () => {
    await copyText(EXAMPLE_RULE);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1500);
  };

  if (selected) {
    return (
      <div className="h-full w-full overflow-auto bg-slate-950">
        <div className="px-6 pt-4">
          <button
            type="button"
            onClick={() => setSelected(null)}
            className="text-xs text-slate-400 hover:text-slate-200 mb-3 inline-flex items-center gap-1"
          >
            <ArrowLeft className="w-3.5 h-3.5" />
            Back to recording rules
          </button>
          <div className="flex items-center gap-2 mb-4 flex-wrap">
            <h2 className="text-lg font-semibold text-slate-100">{selected.group}</h2>
            <span className="text-[10px] px-1.5 py-0.5 rounded border border-sky-500/30 text-sky-300 bg-sky-500/10">
              {selected.tenant}
            </span>
            {selected.interval && (
              <span className="text-[10px] text-slate-500">every {selected.interval}</span>
            )}
            <div className="ml-auto flex items-center gap-2">
              <button
                type="button"
                onClick={() => void evaluate()}
                disabled={evaluating}
                className="px-2.5 py-1 rounded border border-emerald-600/40 bg-emerald-500/10 text-emerald-300 text-xs hover:bg-emerald-500/20 disabled:opacity-50 inline-flex items-center gap-1"
              >
                <Play className="w-3.5 h-3.5" />
                {evaluating ? "Evaluating…" : "Evaluate now"}
              </button>
            </div>
          </div>

          <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
            <div className="bg-slate-900/50 border border-slate-800 rounded-lg overflow-hidden">
              <div className="px-4 py-2 border-b border-slate-800 flex items-center gap-2 text-xs text-slate-400">
                <FileCode2 className="w-3.5 h-3.5" />
                Rule group YAML (stored in MonoFS)
              </div>
              {yamlLoading ? (
                <div className="p-6 text-xs text-slate-500">Loading rule group…</div>
              ) : (
                <pre className="p-4 text-[11px] leading-relaxed text-slate-300 overflow-x-auto whitespace-pre">
                  {yaml || "Rule group is empty."}
                </pre>
              )}
            </div>

            <div className="bg-slate-900/50 border border-slate-800 rounded-lg overflow-hidden">
              <div className="px-4 py-2 border-b border-slate-800 text-xs text-slate-400 flex items-center gap-2">
                <ListChecks className="w-3.5 h-3.5" />
                Manual evaluation
              </div>
              <div className="p-4">
                {evalResult ? (
                  <pre className="text-[11px] leading-relaxed text-slate-300 overflow-x-auto whitespace-pre-wrap">
                    {evalResult}
                  </pre>
                ) : (
                  <p className="text-xs text-slate-500">
                    Evaluate this group once through the doctor-rules API. The normal scheduler
                    evaluates it every {selected.interval || "1m0s"}.
                  </p>
                )}
              </div>
            </div>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="h-full w-full overflow-auto bg-slate-950">
      <div className="px-6 pt-4">
        <div className="flex items-center justify-between mb-4">
          <div>
            <h2 className="text-lg font-semibold text-slate-100">Recording Rules</h2>
            <p className="text-xs text-slate-500">
              PromQL rules evaluated by <code className="text-slate-400">doctor-rules</code> and
              written back to the logengine as derived metrics.
            </p>
          </div>
          <button
            type="button"
            onClick={() => void loadGroups()}
            className="px-2.5 py-1 rounded border border-slate-700 bg-slate-900 text-slate-300 text-xs hover:border-sky-500/40 hover:text-sky-300 inline-flex items-center gap-1"
          >
            <RefreshCw className="w-3.5 h-3.5" />
            Refresh
          </button>
        </div>

        {error && (
          <div className="mb-4 rounded-lg border border-rose-500/30 bg-rose-500/10 px-4 py-3 text-xs text-rose-300 flex items-center gap-2">
            <AlertCircle className="w-4 h-4 shrink-0" />
            {error}
          </div>
        )}

        {loading ? (
          <div className="text-xs text-slate-500 py-10 text-center">Loading rule groups…</div>
        ) : groups.length === 0 ? (
          <div className="grid grid-cols-1 xl:grid-cols-2 gap-4">
            <div className="rounded-lg border border-slate-800 bg-slate-900/50 p-5">
              <h3 className="text-sm font-semibold text-slate-200 mb-2">No rule groups yet</h3>
              <p className="text-xs text-slate-500 mb-3">
                Create one through the doctor-rules API. The scheduler will pick it up within
                the poll interval.
              </p>
              <pre className="text-[11px] leading-relaxed text-slate-300 bg-slate-950 border border-slate-800 rounded p-3 overflow-x-auto whitespace-pre">
{`curl -fsS -X PUT \\
  http://<doctor-rules>:18081/v1/rules/default/shard-health \\
  -H 'Content-Type: application/yaml' \\
  --data-binary @doctor-rules.yaml`}
              </pre>
            </div>
            <div className="rounded-lg border border-slate-800 bg-slate-900/50 p-5">
              <div className="flex items-center justify-between mb-2">
                <h3 className="text-sm font-semibold text-slate-200">Example rule group</h3>
                <button
                  type="button"
                  onClick={() => void copyExample()}
                  className="text-[10px] px-2 py-1 rounded border border-slate-700 text-slate-400 hover:text-slate-200 inline-flex items-center gap-1"
                >
                  <Copy className="w-3 h-3" />
                  {copied ? "Copied" : "Copy"}
                </button>
              </div>
              <pre className="text-[11px] leading-relaxed text-slate-300 bg-slate-950 border border-slate-800 rounded p-3 overflow-x-auto whitespace-pre">
                {EXAMPLE_RULE}
              </pre>
            </div>
          </div>
        ) : (
          <div className="overflow-x-auto rounded-lg border border-slate-800">
            <table className="w-full text-xs">
              <thead>
                <tr className="text-slate-400 uppercase tracking-wider border-b border-slate-800 bg-slate-900/60">
                  <th className="text-left px-4 py-2.5 font-medium">Tenant</th>
                  <th className="text-left px-4 py-2.5 font-medium">Group</th>
                  <th className="text-left px-4 py-2.5 font-medium">Interval</th>
                  <th className="text-right px-4 py-2.5 font-medium">Rules</th>
                  <th className="text-right px-4 py-2.5 font-medium"></th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-800/60">
                {groups.map((group) => (
                  <tr
                    key={`${group.tenant}/${group.group}`}
                    className="hover:bg-slate-800/30 cursor-pointer"
                    onClick={() => void openGroup(group)}
                  >
                    <td className="px-4 py-2.5 text-slate-400">{group.tenant}</td>
                    <td className="px-4 py-2.5 font-mono text-slate-200">{group.group}</td>
                    <td className="px-4 py-2.5 text-slate-400">{group.interval || "1m0s"}</td>
                    <td className="px-4 py-2.5 text-right text-slate-300">{group.rules}</td>
                    <td className="px-4 py-2.5 text-right text-sky-400">View →</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

const EXAMPLE_RULE = `version: 1
groups:
  - name: shard-health
    tenant: default
    interval: 30s
    rules:
      - record: doctor:up:sum
        expr: sum(up) by (job)
        labels:
          tier: edge
        merge: sum`;
