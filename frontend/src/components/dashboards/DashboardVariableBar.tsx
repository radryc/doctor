import { useEffect, useState } from "react";
import { api } from "../../api/client";
import { useTimeRange } from "../../context/TimeRangeContext";
import type { DashboardVariable, VariableValues } from "../../types/dashboard";

interface Props {
  variables: DashboardVariable[];
  tenant: string;
  values: VariableValues;
  onChange: (values: VariableValues) => void;
}

export default function DashboardVariableBar({ variables, tenant, values, onChange }: Props) {
  if (!variables || variables.length === 0) return null;

  const set = (name: string, value: string) =>
    onChange({ ...values, [name]: value });

  return (
    <div className="dash-var-bar">
      <span className="dash-var-bar__label">Variables:</span>
      {variables.map((v) => (
        <VariableControl
          key={v.name}
          variable={v}
          tenant={tenant}
          value={values[v.name] ?? v.defaultValue}
          onChange={(val) => set(v.name, val)}
        />
      ))}
    </div>
  );
}

function VariableControl({
  variable,
  tenant,
  value,
  onChange,
}: {
  variable: DashboardVariable;
  tenant: string;
  value: string;
  onChange: (v: string) => void;
}) {
  const tr = useTimeRange();
  const { from, to } = tr.resolve();
  const [options, setOptions] = useState<string[]>(variable.options ?? []);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (variable.type === "custom" || variable.type === "text") return;
    let active = true;
    setLoading(true);

    const finish = (values: string[]) => {
      if (!active) return;
      setOptions([...new Set(values.filter(Boolean))].sort());
      setLoading(false);
    };

    if (variable.type === "service") {
      // Service variables are driven by the APM catalog so services that only
      // emit traces or metrics (no logs) still appear in the picker.
      const windowMinutes = Math.max(1, Math.ceil((to.getTime() - from.getTime()) / 60000));
      const params = new URLSearchParams({
        tenant,
        window: `${windowMinutes}m`,
      });
      api
        .discoverServices(params)
        .then((r) => finish((r.services ?? []).map((s) => s.name || s.service || "")))
        .catch(() => {
          const fallback = new URLSearchParams({
            tenant,
            start: from.toISOString(),
            end: to.toISOString(),
          });
          api
            .lokiLabelValues("service", fallback)
            .then((r) => finish(r.data ?? []))
            .catch(() => finish([]));
        });
      return () => { active = false; };
    }

    if (variable.type === "partition") {
      api
        .getLogServices(tenant)
        .then((r) => finish(r.partitions ?? []))
        .catch(() => {
          const fallback = new URLSearchParams({
            tenant,
            start: from.toISOString(),
            end: to.toISOString(),
          });
          api
            .lokiLabelValues("guardian_partition", fallback)
            .then((r) => finish(r.data ?? []))
            .catch(() => finish([]));
        });
      return () => { active = false; };
    }

    if (variable.type === "label_values" && variable.labelKey) {
      const params = new URLSearchParams({
        tenant,
        start: from.toISOString(),
        end: to.toISOString(),
      });
      api
        .lokiLabelValues(variable.labelKey, params)
        .then((r) => finish(r.data ?? []))
        .catch(() => finish([]));
    } else {
      setLoading(false);
    }

    return () => { active = false; };
  }, [variable, tenant, from, to]);

  if (variable.type === "text") {
    return (
      <label className="dash-var__item">
        <span>{variable.label}</span>
        <input
          className="dash-var__input"
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      </label>
    );
  }

  const allOptions = variable.includeAll ? ["$__all", ...options] : options;
  const displayOptions =
    value && !allOptions.includes(value) ? [value, ...allOptions] : allOptions;

  return (
    <label className="dash-var__item">
      <span>{variable.label}</span>
      <select
        className="dash-var__select"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={loading}
      >
        {displayOptions.length === 0 && <option value="">—</option>}
        {displayOptions.map((o) => (
          <option key={o} value={o}>
            {o === "$__all" ? "All" : o}
          </option>
        ))}
      </select>
    </label>
  );
}
