import { useState } from "react";
import { X } from "lucide-react";
import type {
  DashboardWidget,
  Datasource,
  WidgetKind,
  WidgetVisualization,
} from "../../types/dashboard";

const WIDGET_KINDS: WidgetKind[] = [
  "timeseries",
  "stat",
  "toplist",
  "gauge",
  "table",
  "logs",
  "traces",
  "topology",
  "markdown",
];

const DATASOURCES: Array<{ value: Datasource; label: string }> = [
  { value: "prometheus", label: "Doctor metrics (PromQL)" },
  { value: "tempo", label: "Doctor traces (TraceQL)" },
  { value: "loki", label: "Doctor logs (LogQL)" },
  { value: "native_logs", label: "Doctor logs (stored search)" },
  { value: "native_traces", label: "Doctor traces (recent)" },
];

interface Props {
  widget?: DashboardWidget;
  onSave: (widget: DashboardWidget) => void;
  onClose: () => void;
}

export default function WidgetEditorModal({ widget, onSave, onClose }: Props) {
  const [title, setTitle] = useState(widget?.title ?? "");
  const [description, setDescription] = useState(widget?.description ?? "");
  const [kind, setKind] = useState<WidgetKind>(widget?.kind ?? "timeseries");
  const [datasource, setDatasource] = useState<Datasource>(widget?.queries[0]?.datasource ?? "prometheus");
  const [expr, setExpr] = useState(widget?.queries[0]?.expr ?? "");
  const [legendFormat, setLegendFormat] = useState(widget?.queries[0]?.legendFormat ?? "");
  const [unit, setUnit] = useState<NonNullable<WidgetVisualization["unit"]>>(
    widget?.visualization?.unit ?? "none"
  );
  const [displayMode, setDisplayMode] = useState<"line" | "area" | "bars">(
    widget?.visualization?.displayMode ?? "line"
  );
  const [markdown, setMarkdown] = useState(widget?.visualization?.markdownContent ?? "");
  const [grid, setGrid] = useState(widget?.gridPos ?? { x: 0, y: 0, w: 6, h: 6 });
  const [error, setError] = useState("");

  const save = () => {
    if (!title.trim()) {
      setError("Give the widget a title.");
      return;
    }
    if (kind !== "markdown" && kind !== "topology" && !expr.trim()) {
      setError("Enter a query expression.");
      return;
    }
    const visualization: WidgetVisualization = {
      ...(widget?.visualization ?? {}),
      unit: unit as WidgetVisualization["unit"],
    };
    if (kind === "timeseries") visualization.displayMode = displayMode as "line" | "area" | "bars";
    if (kind === "markdown") visualization.markdownContent = markdown;

    onSave({
      id: widget?.id ?? `w-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
      title: title.trim(),
      description: description.trim() || undefined,
      kind,
      gridPos: {
        x: clamp(grid.x, 0, 11),
        y: Math.max(0, grid.y),
        w: clamp(grid.w, 1, 12),
        h: clamp(grid.h, 1, 24),
      },
      queries:
        kind === "markdown" || kind === "topology"
          ? []
          : [{ refId: "A", datasource, expr: expr.trim(), legendFormat: legendFormat.trim() || undefined }],
      visualization,
    });
  };

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal widget-editor" onClick={(e) => e.stopPropagation()}>
        <div className="modal__header">
          <span>{widget ? "Edit widget" : "Add widget"}</span>
          <button className="icon-btn" onClick={onClose} aria-label="Close">
            <X size={14} />
          </button>
        </div>
        <div className="modal__body widget-editor__body">
          <div className="widget-editor__grid">
            <label>
              Title
              <input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Request rate" />
            </label>
            <label>
              Type
              <select value={kind} onChange={(e) => setKind(e.target.value as WidgetKind)}>
                {WIDGET_KINDS.map((value) => <option key={value} value={value}>{value}</option>)}
              </select>
            </label>
          </div>

          <label>
            Description
            <input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Optional context or runbook link" />
          </label>

          {kind === "markdown" ? (
            <label>
              Markdown content
              <textarea rows={6} value={markdown} onChange={(e) => setMarkdown(e.target.value)} placeholder="Runbook notes…" />
            </label>
          ) : kind === "topology" ? (
            <p className="widget-editor__hint">
              Topology widgets render the live Guardian partition graph and use the dashboard
              <code> $partition </code> variable when present.
            </p>
          ) : (
            <>
              <div className="widget-editor__grid">
                <label>
                  Datasource
                  <select value={datasource} onChange={(e) => setDatasource(e.target.value as Datasource)}>
                    {DATASOURCES.map(({ value, label }) => <option key={value} value={value}>{label}</option>)}
                  </select>
                </label>
                <label>
                  Legend format
                  <input value={legendFormat} onChange={(e) => setLegendFormat(e.target.value)} placeholder="{{service}}" />
                </label>
              </div>
              <label>
                Query
                <textarea
                  rows={4}
                  value={expr}
                  onChange={(e) => setExpr(e.target.value)}
                  placeholder='PromQL, LogQL, or TraceQL — e.g. sum(rate(http_requests_total[$__rate_interval]))'
                  spellCheck={false}
                />
              </label>
            </>
          )}

          <div className="widget-editor__grid widget-editor__grid--four">
            <label>X<input type="number" min={0} max={11} value={grid.x} onChange={(e) => setGrid({ ...grid, x: Number(e.target.value) })} /></label>
            <label>Y<input type="number" min={0} value={grid.y} onChange={(e) => setGrid({ ...grid, y: Number(e.target.value) })} /></label>
            <label>Width<input type="number" min={1} max={12} value={grid.w} onChange={(e) => setGrid({ ...grid, w: Number(e.target.value) })} /></label>
            <label>Height<input type="number" min={1} max={24} value={grid.h} onChange={(e) => setGrid({ ...grid, h: Number(e.target.value) })} /></label>
          </div>

          {kind === "timeseries" && (
            <div className="widget-editor__grid">
              <label>
                Display
                <select value={displayMode} onChange={(e) => setDisplayMode(e.target.value as "line" | "area" | "bars")}>
                  <option value="line">line</option>
                  <option value="area">area</option>
                  <option value="bars">bars</option>
                </select>
              </label>
              <label>
                Unit
                <select value={unit} onChange={(e) => setUnit(e.target.value as NonNullable<WidgetVisualization["unit"]>)}>
                  <option value="none">none</option>
                  <option value="short">short</option>
                  <option value="percent">percent</option>
                  <option value="seconds">seconds</option>
                  <option value="ms">ms</option>
                  <option value="bytes">bytes</option>
                  <option value="bps">bps</option>
                  <option value="reqps">reqps</option>
                </select>
              </label>
            </div>
          )}

          {error && <div className="import-error">{error}</div>}
        </div>
        <div className="modal__footer">
          <button className="btn" onClick={onClose}>Cancel</button>
          <button className="btn btn--primary" onClick={save}>{widget ? "Apply" : "Add widget"}</button>
        </div>
      </div>
    </div>
  );
}

function clamp(value: number, min: number, max: number): number {
  if (!Number.isFinite(value)) return min;
  return Math.max(min, Math.min(max, Math.round(value)));
}
