import { useMemo, useState } from "react";
import { BookOpen, ListChecks, Search, SlidersHorizontal } from "lucide-react";
import AlertRulesTab from "./AlertRulesTab";
import type {
  AlertDefinition,
  AlertInstanceCounts,
  AlertRuleGroupSummary,
  Capabilities,
  Receiver,
} from "../../types/alerts";

interface AlertInventoryTabProps {
  tenant: string;
  partition: string;
  definitions: AlertDefinition[];
  groups: AlertRuleGroupSummary[];
  endpoints: Receiver[];
  caps: Capabilities | null;
  canWrite: boolean;
  onReload: () => Promise<void>;
  onNotice: (message: string) => void;
  onError: (message: string) => void;
  onOpenInstances: (alertname: string, source?: string) => void;
}

const ENGINE_LABELS: Record<string, string> = {
  promql: "PromQL rule",
  guided: "Guided rule",
  guardian: "Guardian built-in",
  aws: "AWS source",
  gcp: "GCP source",
  source: "Unmanaged source",
};

function engineLabel(engine: string): string {
  return ENGINE_LABELS[engine] ?? engine;
}

function engineClass(engine: string): string {
  switch (engine) {
    case "promql":
      return "border-violet-500/40 bg-violet-500/10 text-violet-300";
    case "guided":
      return "border-sky-500/40 bg-sky-500/10 text-sky-300";
    case "guardian":
      return "border-emerald-500/40 bg-emerald-500/10 text-emerald-300";
    case "aws":
      return "border-amber-500/40 bg-amber-500/10 text-amber-300";
    case "gcp":
      return "border-cyan-500/40 bg-cyan-500/10 text-cyan-300";
    default:
      return "border-slate-500/40 bg-slate-500/10 text-slate-300";
  }
}

function severityClass(severity?: string): string {
  switch ((severity ?? "").toLowerCase()) {
    case "critical":
      return "border-rose-500/40 bg-rose-500/10 text-rose-300";
    case "warning":
      return "border-amber-500/40 bg-amber-500/10 text-amber-300";
    default:
      return "border-sky-500/40 bg-sky-500/10 text-sky-300";
  }
}

function instanceBadges(counts: AlertInstanceCounts) {
  const items: Array<{ key: string; value: number; className: string }> = [
    { key: "firing", value: counts.firing, className: "border-rose-500/40 bg-rose-500/10 text-rose-300" },
    { key: "pending", value: counts.pending, className: "border-amber-500/40 bg-amber-500/10 text-amber-300" },
    { key: "silenced", value: counts.silenced, className: "border-violet-500/40 bg-violet-500/10 text-violet-300" },
    { key: "resolved", value: counts.resolved, className: "border-emerald-500/40 bg-emerald-500/10 text-emerald-300" },
  ].filter((item) => item.value > 0);
  if (items.length === 0) {
    return <span className="text-[11px] text-slate-500">none</span>;
  }
  return (
    <span className="flex flex-wrap gap-1">
      {items.map((item) => (
        <span
          key={item.key}
          className={`rounded border px-1.5 py-0.5 text-[10px] font-semibold ${item.className}`}
          title={`${item.value} ${item.key}`}
        >
          {item.value} {item.key}
        </span>
      ))}
    </span>
  );
}

function definitionDetail(def: AlertDefinition) {
  if (def.expr) {
    return (
      <code
        className="block max-w-[520px] truncate rounded bg-slate-950/70 px-2 py-1 font-mono text-[10px] text-slate-300"
        title={def.expr}
      >
        {def.expr}
      </code>
    );
  }
  if (def.engine === "guardian") {
    return (
      <span className="text-[11px] text-slate-400">
        Built-in Guardian partition alert{def.for && def.for !== "0s" ? ` · for ${def.for}` : ""}
      </span>
    );
  }
  if (def.engine === "aws" || def.engine === "gcp") {
    return <span className="text-[11px] text-slate-400">{def.summary || "Managed pull source"}</span>;
  }
  return <span className="text-[11px] text-slate-400">{def.summary || "No expression recorded"}</span>;
}

export default function AlertInventoryTab({
  tenant,
  partition,
  definitions,
  groups,
  endpoints,
  caps,
  canWrite,
  onReload,
  onNotice,
  onError,
  onOpenInstances,
}: AlertInventoryTabProps) {
  const [search, setSearch] = useState("");
  const [engineFilter, setEngineFilter] = useState("all");
  const [showRules, setShowRules] = useState(false);

  const definedCount = useMemo(() => definitions.filter((def) => def.defined).length, [definitions]);
  const unmanagedCount = definitions.length - definedCount;
  const firingCount = useMemo(
    () => definitions.reduce((sum, def) => sum + def.instances.firing + def.instances.pending, 0),
    [definitions]
  );
  const withInstances = definitions.filter((def) => def.instances.total > 0).length;

  const filtered = useMemo(() => {
    const query = search.trim().toLowerCase();
    return definitions.filter((def) => {
      if (engineFilter !== "all" && def.engine !== engineFilter) return false;
      if (!query) return true;
      return (
        def.name.toLowerCase().includes(query) ||
        (def.summary ?? "").toLowerCase().includes(query) ||
        (def.expr ?? "").toLowerCase().includes(query) ||
        (def.source ?? "").toLowerCase().includes(query)
      );
    });
  }, [definitions, engineFilter, search]);

  return (
    <div className="space-y-4">
      <section className="rounded-lg border border-slate-800 bg-slate-900/50">
        <div className="flex flex-wrap items-start justify-between gap-3 border-b border-slate-800 px-4 py-3">
          <div>
            <h2 className="flex items-center gap-2 text-sm font-semibold text-slate-100">
              <BookOpen size={16} /> Alerts inventory
            </h2>
            <p className="mt-1 text-[11px] text-slate-500">
              Every alert available for <span className="font-mono text-slate-400">{partition}</span>: Sentinel
              PromQL/guided rules, Guardian built-ins, AWS/GCP source policies, and unmanaged source alerts.
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2 text-[11px]">
            <span className="rounded border border-slate-600/40 bg-slate-800/40 px-2 py-1 text-slate-300">
              {definedCount} defined
            </span>
            <span className="rounded border border-amber-500/30 bg-amber-500/10 px-2 py-1 text-amber-300">
              {unmanagedCount} unmanaged
            </span>
            <span className="rounded border border-rose-500/30 bg-rose-500/10 px-2 py-1 text-rose-300">
              {firingCount} firing/pending
            </span>
            <button
              type="button"
              onClick={() => setShowRules((value) => !value)}
              className="btn inline-flex items-center gap-1 text-xs"
            >
              <SlidersHorizontal size={13} /> {showRules ? "Hide rule editor" : "Manage rules"}
            </button>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-2 border-b border-slate-800 px-4 py-2">
          <div className="relative">
            <Search size={12} className="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-slate-500" />
            <input
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder="Filter alerts, expressions, sources…"
              className="h-7 w-64 rounded border border-slate-700 bg-slate-950 pl-7 pr-2 text-xs text-slate-200 placeholder:text-slate-600"
            />
          </div>
          <select
            value={engineFilter}
            onChange={(event) => setEngineFilter(event.target.value)}
            className="h-7 rounded border border-slate-700 bg-slate-950 px-2 text-xs text-slate-300"
          >
            <option value="all">All kinds</option>
            <option value="promql">PromQL rules</option>
            <option value="guided">Guided rules</option>
            <option value="guardian">Guardian built-ins</option>
            <option value="aws">AWS source</option>
            <option value="gcp">GCP source</option>
            <option value="source">Unmanaged source</option>
          </select>
          <span className="ml-auto text-[11px] text-slate-500">
            {withInstances} with live instances · {filtered.length} shown
          </span>
        </div>

        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs">
            <thead className="bg-slate-950/70 text-slate-500">
              <tr>
                <th className="px-3 py-2">Alert</th>
                <th className="px-3 py-2">Kind</th>
                <th className="px-3 py-2">Definition</th>
                <th className="px-3 py-2">For</th>
                <th className="px-3 py-2">Severity</th>
                <th className="px-3 py-2">Notify</th>
                <th className="px-3 py-2">Instances</th>
                <th className="px-3 py-2">State</th>
                <th className="px-3 py-2" />
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800">
              {filtered.map((def) => (
                <tr key={`${def.source}:${def.engine}:${def.name}`} className="hover:bg-slate-800/40">
                  <td className="px-3 py-2">
                    <div className="font-medium text-slate-200">{def.name}</div>
                    <div className="mt-0.5 text-[10px] text-slate-500">
                      {def.group ? `group ${def.group}` : def.source}
                    </div>
                  </td>
                  <td className="px-3 py-2">
                    <span className={`rounded border px-1.5 py-0.5 text-[10px] font-semibold ${engineClass(def.engine)}`}>
                      {engineLabel(def.engine)}
                    </span>
                  </td>
                  <td className="px-3 py-2">{definitionDetail(def)}</td>
                  <td className="px-3 py-2 text-slate-400">{def.for && def.for !== "0s" ? def.for : "—"}</td>
                  <td className="px-3 py-2">
                    <span className={`rounded border px-1.5 py-0.5 text-[10px] font-semibold uppercase ${severityClass(def.severity)}`}>
                      {def.severity || "—"}
                    </span>
                  </td>
                  <td className="px-3 py-2 text-[11px] text-slate-400">
                    {def.notify?.length ? def.notify.join(", ") : "default routing"}
                  </td>
                  <td className="px-3 py-2">{instanceBadges(def.instances)}</td>
                  <td className="px-3 py-2">
                    {def.defined ? (
                      <span className="rounded border border-emerald-500/30 bg-emerald-500/10 px-1.5 py-0.5 text-[10px] text-emerald-300">
                        defined
                      </span>
                    ) : (
                      <span className="rounded border border-amber-500/30 bg-amber-500/10 px-1.5 py-0.5 text-[10px] text-amber-300">
                        unmanaged
                      </span>
                    )}
                  </td>
                  <td className="px-3 py-2 text-right">
                    {def.instances.total > 0 && (
                      <button
                        type="button"
                        onClick={() => onOpenInstances(def.name, def.source)}
                        className="text-[11px] text-cyan-300 hover:text-cyan-200"
                      >
                        View instances
                      </button>
                    )}
                  </td>
                </tr>
              ))}
              {filtered.length === 0 && (
                <tr>
                  <td colSpan={9} className="px-4 py-10 text-center text-slate-500">
                    No alert definitions match the current filter.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>

        <div className="border-t border-slate-800 px-4 py-2 text-[10px] text-slate-500">
          Defined rows are evaluated rules or source policies. Unmanaged rows are live source alerts without a
          rule definition (for example Guardian pushes or <span className="font-mono">source=test</span> smoke
          tests); they are listed here so they are never invisible.
        </div>
      </section>

      {showRules && (
        <section className="rounded-lg border border-slate-800 bg-slate-900/50">
          <div className="border-b border-slate-800 px-4 py-3">
            <h2 className="flex items-center gap-2 text-sm font-semibold text-slate-100">
              <ListChecks size={16} /> Rule editor
            </h2>
            <p className="mt-1 text-[11px] text-slate-500">
              Create PromQL or guided rules. Saving reloads the inventory and the alert instances.
            </p>
          </div>
          <div className="p-4">
            <AlertRulesTab
              tenant={tenant}
              partition={partition}
              groups={groups}
              endpoints={endpoints}
              caps={caps}
              canWrite={canWrite}
              onReload={onReload}
              onNotice={onNotice}
              onError={onError}
            />
          </div>
        </section>
      )}
    </div>
  );
}
