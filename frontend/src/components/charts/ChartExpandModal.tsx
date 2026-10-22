import { useEffect } from "react";
import { X } from "lucide-react";
import TimeSeriesChart, { CHART_PALETTE, type ChartDisplayMode } from "./TimeSeriesChart";
import type { MetricSeries } from "../../types/api";

interface ChartExpandModalProps {
  open: boolean;
  title: string;
  description?: string;
  series: MetricSeries[];
  displayMode?: ChartDisplayMode;
  stacked?: boolean;
  queries?: string[];
  from: Date;
  to: Date;
  onClose: () => void;
}

export default function ChartExpandModal({
  open,
  title,
  description,
  series,
  displayMode = "line",
  stacked = false,
  queries = [],
  from,
  to,
  onClose,
}: ChartExpandModalProps) {
  useEffect(() => {
    if (!open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div
        className="modal chart-modal"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(event) => event.stopPropagation()}
      >
        <div className="modal__header">
          <span>{title}</span>
          <button type="button" onClick={onClose} title="Close (Esc)">
            <X size={16} />
          </button>
        </div>
        <div className="modal__body chart-modal__body">
          {description && <p className="chart-modal__description">{description}</p>}
          <TimeSeriesChart
            series={series}
            height={440}
            displayMode={displayMode}
            stacked={stacked}
            showLegend
          />
          {series.length > 0 && (
            <div className="chart-modal__legend">
              {series.map((item, index) => (
                <span key={`${item.label}-${index}`} className="chart-modal__legend-item" title={item.label}>
                  <span
                    className="chart-modal__swatch"
                    style={{ backgroundColor: CHART_PALETTE[index % CHART_PALETTE.length] }}
                  />
                  <code>{item.label}</code>
                </span>
              ))}
            </div>
          )}
          {queries.length > 0 && (
            <div className="chart-modal__queries">
              <div className="chart-modal__section-title">Queries</div>
              {queries.map((query, index) => (
                <code key={`${query}-${index}`} className="chart-modal__query">{query}</code>
              ))}
            </div>
          )}
        </div>
        <div className="modal__footer chart-modal__range">
          <span>{from.toLocaleString()}</span>
          <span>→</span>
          <span>{to.toLocaleString()}</span>
        </div>
      </div>
    </div>
  );
}
