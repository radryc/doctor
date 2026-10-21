import { useMemo } from "react";
import type { LogRecord } from "../../types/api";

interface FacetValue {
  value: string;
  count: number;
}

interface Facet {
  field: string;
  values: FacetValue[];
}

interface Props {
  records: LogRecord[];
  activeFilters: Record<string, string>;
  onInclude: (field: string, value: string) => void;
  onExclude: (field: string, value: string) => void;
}

const CORE_FIELDS = ["severity_text", "service", "partition", "intent", "asset"] as const;

export default function LogFacetSidebar({ records, activeFilters, onInclude, onExclude }: Props) {
  const facets = useMemo<Facet[]>(() => {
    if (records.length === 0) return [];

    const counts: Record<string, Record<string, number>> = {};

    const bump = (field: string, value: string) => {
      if (!value) return;
      if (!counts[field]) counts[field] = {};
      counts[field][value] = (counts[field][value] ?? 0) + 1;
    };

    for (const r of records) {
      bump("severity_text", normalizeSeverity(r.severity_text));
      bump("service", r.service ?? "");
      bump("partition", r.partition ?? r.resource_attributes?.["guardian_partition"] ?? r.resource_attributes?.["guardian.partition"] ?? "");
      bump("intent", r.intent ?? "");
      bump("asset", r.asset ?? "");

      // Extracted JSON fields from attributes
      if (r.attributes) {
        for (const [k, v] of Object.entries(r.attributes)) {
          if (v && typeof v === "string") bump(`attr.${k}`, v);
        }
      }
      if (r.resource_attributes) {
        for (const [k, v] of Object.entries(r.resource_attributes)) {
          if (v && typeof v === "string" && !CORE_FIELDS.includes(k as never)) {
            bump(`res.${k}`, v);
          }
        }
      }
    }

    return Object.entries(counts)
      .map(([field, valueCounts]) => ({
        field,
        values: Object.entries(valueCounts)
          .map(([value, count]) => ({ value, count }))
          .sort((a, b) => b.count - a.count)
          .slice(0, 10),
      }))
      .sort((a, b) => {
        const ai = CORE_FIELDS.indexOf(a.field as never);
        const bi = CORE_FIELDS.indexOf(b.field as never);
        if (ai >= 0 && bi >= 0) return ai - bi;
        if (ai >= 0) return -1;
        if (bi >= 0) return 1;
        return a.field.localeCompare(b.field);
      });
  }, [records]);

  if (facets.length === 0) {
    return (
      <div className="log-facet-sidebar">
        <div className="log-facet-empty">No data</div>
      </div>
    );
  }

  return (
    <div className="log-facet-sidebar">
      <div className="log-facet-title">Search Fields</div>
      {facets.map((facet) => (
        <FacetGroup
          key={facet.field}
          facet={facet}
          activeFilter={activeFilters[facet.field]}
          onInclude={onInclude}
          onExclude={onExclude}
          total={records.length}
        />
      ))}
    </div>
  );
}

interface GroupProps {
  facet: Facet;
  activeFilter?: string;
  onInclude: (field: string, value: string) => void;
  onExclude: (field: string, value: string) => void;
  total: number;
}

function FacetGroup({ facet, activeFilter, onInclude, onExclude }: GroupProps) {
  const max = facet.values[0]?.count ?? 1;

  return (
    <details className="log-facet-group" open>
      <summary className="log-facet-group__label">
        {facet.field}
        <span className="log-facet-group__count">({facet.values.length})</span>
      </summary>
      <div className="log-facet-group__values">
        {facet.values.map(({ value, count }) => (
          <div
            key={value}
            className={
              "log-facet-value" +
              (activeFilter === value ? " log-facet-value--active" : "")
            }
          >
            <div
              className="log-facet-value__bar"
              style={{ width: `${(count / max) * 100}%` }}
            />
            <span className="log-facet-value__text" title={value}>
              {value || "—"}
            </span>
            <span className="log-facet-value__count">{count}</span>
            <div className="log-facet-value__actions">
              <button
                className="log-facet-btn log-facet-btn--include"
                title={`Include ${facet.field}="${value}"`}
                onClick={() => onInclude(facet.field, value)}
              >
                +
              </button>
              <button
                className="log-facet-btn log-facet-btn--exclude"
                title={`Exclude ${facet.field}="${value}"`}
                onClick={() => onExclude(facet.field, value)}
              >
                −
              </button>
            </div>
          </div>
        ))}
      </div>
    </details>
  );
}

function normalizeSeverity(text?: string): string {
  const s = (text ?? "INFO").toUpperCase();
  if (s.includes("ERROR") || s.includes("FATAL")) return "ERROR";
  if (s.includes("WARN")) return "WARN";
  if (s.includes("DEBUG") || s.includes("TRACE")) return "DEBUG";
  return "INFO";
}
