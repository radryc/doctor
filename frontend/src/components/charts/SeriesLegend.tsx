import type { MouseEvent } from "react";

export interface SeriesLegendItem {
  key: string;
  label: string;
  color: string;
}

interface SeriesLegendProps {
  items: SeriesLegendItem[];
  hidden: Set<string>;
  isolated: string | null;
  swatchShape?: "circle" | "square";
  onToggle: (key: string) => void;
  onIsolate: (key: string | null) => void;
}

/**
 * Grafana/Thanos-style legend:
 *  - click: show only this series (click again to show all)
 *  - Ctrl/Cmd-click: toggle this series' visibility
 */
export default function SeriesLegend({
  items,
  hidden,
  isolated,
  swatchShape = "circle",
  onToggle,
  onIsolate,
}: SeriesLegendProps) {
  const activeIsolation =
    isolated && items.some((item) => item.key === isolated) ? isolated : null;

  return (
    <div className="timeseries-chart__legend">
      {items.map((item) => {
        const isHidden = hidden.has(item.key);
        const isOnly = activeIsolation === item.key;
        const isMuted = activeIsolation !== null && !isOnly;
        const className =
          "timeseries-chart__legend-item" +
          (isHidden ? " is-hidden" : "") +
          (isOnly ? " is-isolated" : "") +
          (isMuted ? " is-muted" : "");
        const title = isOnly
          ? "Show all series"
          : "Show only this series · Ctrl/Cmd-click to toggle visibility";

        const handleClick = (event: MouseEvent<HTMLButtonElement>) => {
          event.stopPropagation();
          if (event.metaKey || event.ctrlKey) {
            if (activeIsolation === item.key) onIsolate(null);
            onToggle(item.key);
            return;
          }
          onIsolate(activeIsolation === item.key ? null : item.key);
        };

        return (
          <button
            type="button"
            key={item.key}
            className={className}
            title={title}
            aria-pressed={isOnly}
            onClick={handleClick}
          >
            <span
              className="timeseries-chart__swatch"
              style={{
                backgroundColor: item.color,
                borderRadius: swatchShape === "square" ? 2 : 999,
              }}
            />
            <span className="timeseries-chart__legend-label">{item.label}</span>
          </button>
        );
      })}
    </div>
  );
}
