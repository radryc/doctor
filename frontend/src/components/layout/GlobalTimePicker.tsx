import { useEffect, useRef, useState } from "react";
import { ChevronLeft, ChevronRight, ZoomOut, RefreshCw } from "lucide-react";
import {
  useTimeRange,
  RELATIVE_PRESETS,
  AUTO_REFRESH_OPTIONS,
} from "../../context/TimeRangeContext";

function formatDatetimeLocal(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export default function GlobalTimePicker() {
  const tr = useTimeRange();
  const [open, setOpen] = useState(false);
  const [absFrom, setAbsFrom] = useState("");
  const [absTo, setAbsTo] = useState("");
  const ref = useRef<HTMLDivElement>(null);

  // Sync absolute inputs when popover opens.
  useEffect(() => {
    if (!open) return;
    const { from, to } = tr.resolve();
    setAbsFrom(formatDatetimeLocal(from));
    setAbsTo(formatDatetimeLocal(to));
  }, [open, tr]);

  // Close on outside click.
  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, []);

  const applyAbsolute = () => {
    const from = new Date(absFrom);
    const to = new Date(absTo);
    if (isNaN(from.getTime()) || isNaN(to.getTime()) || from >= to) return;
    tr.setAbsolute(from, to);
    setOpen(false);
  };

  const label =
    tr.mode === "relative"
      ? `Last ${tr.relativePreset}`
      : (() => {
          const { from, to } = tr.resolve();
          return `${from.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })} – ${to.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
        })();

  return (
    <div className="time-picker" ref={ref}>
      <button
        className="time-picker__nav"
        onClick={tr.shiftBack}
        title="Shift back (50% of range)"
      >
        <ChevronLeft size={14} />
      </button>

      <button
        className="time-picker__label"
        onClick={() => setOpen((o) => !o)}
        title="Open time range picker"
      >
        {label}
      </button>

      <button
        className="time-picker__nav"
        onClick={tr.shiftForward}
        title="Shift forward (50% of range)"
      >
        <ChevronRight size={14} />
      </button>

      <button
        className="time-picker__nav"
        onClick={tr.zoomOut}
        title="Zoom out 2×"
      >
        <ZoomOut size={14} />
      </button>

      {/* Auto-refresh selector */}
      <select
        className="time-picker__refresh-select"
        value={tr.autoRefresh}
        onChange={(e) => tr.setAutoRefresh(e.target.value)}
        title="Auto-refresh interval"
      >
        {AUTO_REFRESH_OPTIONS.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label === "Off" ? "Auto: Off" : `Auto: ${o.label}`}
          </option>
        ))}
      </select>

      <button
        className="time-picker__nav"
        onClick={tr.refresh}
        title="Refresh now"
      >
        <RefreshCw size={14} />
      </button>

      {/* Popover */}
      {open && (
        <div className="time-picker__popover">
          <div className="time-picker__section-label">Relative presets</div>
          <div className="time-picker__presets">
            {RELATIVE_PRESETS.map((p) => (
              <button
                key={p.value}
                className={
                  "time-picker__preset" +
                  (tr.mode === "relative" && tr.relativePreset === p.value
                    ? " is-active"
                    : "")
                }
                onClick={() => {
                  tr.setRelative(p.value);
                  setOpen(false);
                }}
              >
                {p.label}
              </button>
            ))}
          </div>

          <div className="time-picker__section-label" style={{ marginTop: "0.75rem" }}>
            Absolute time range
          </div>
          <div className="time-picker__abs-row">
            <label>
              From
              <input
                type="datetime-local"
                className="time-picker__abs-input"
                value={absFrom}
                onChange={(e) => setAbsFrom(e.target.value)}
              />
            </label>
            <label>
              To
              <input
                type="datetime-local"
                className="time-picker__abs-input"
                value={absTo}
                onChange={(e) => setAbsTo(e.target.value)}
              />
            </label>
          </div>
          <button className="time-picker__apply-btn" onClick={applyAbsolute}>
            Apply time range
          </button>
        </div>
      )}
    </div>
  );
}
