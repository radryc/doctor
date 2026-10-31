import { useEffect, useState } from "react";

export interface ChartTheme {
  palette: string[];
  text: string;
  muted: string;
  faint: string;
  grid: string;
  border: string;
  borderStrong: string;
  elevated: string;
  panel: string;
  input: string;
  accent: string;
  green: string;
  amber: string;
  red: string;
}

const FALLBACK_PALETTE = [
  "#00ade4",
  "#8b7cf6",
  "#00c369",
  "#fcb519",
  "#ee5f54",
  "#22d3ee",
  "#f97316",
  "#a855f7",
];

const FALLBACK_THEME: ChartTheme = {
  palette: FALLBACK_PALETTE,
  text: "#95a6c6",
  muted: "#9bb0cf",
  faint: "#7c8fa3",
  grid: "rgba(148, 163, 184, 0.10)",
  border: "rgba(255, 255, 255, 0.09)",
  borderStrong: "rgba(255, 255, 255, 0.14)",
  elevated: "#151b2b",
  panel: "#0d1220",
  input: "#0a0f1c",
  accent: "#00ade4",
  green: "#00c369",
  amber: "#fcb519",
  red: "#ee5f54",
};

/**
 * ECharts renders into a canvas, so CSS variables cannot be applied directly
 * to axis lines, labels, or series colors. Resolve the active theme tokens
 * once per theme change and feed concrete color values to ECharts.
 */
export function resolveChartTheme(): ChartTheme {
  if (typeof document === "undefined") return FALLBACK_THEME;
  const styles = getComputedStyle(document.documentElement);
  const value = (name: string, fallback: string) =>
    styles.getPropertyValue(name).trim() || fallback;

  return {
    palette: FALLBACK_PALETTE.map((fallback, index) =>
      value(`--chart-${index + 1}`, fallback)
    ),
    text: value("--chart-text", FALLBACK_THEME.text),
    muted: value("--ui-muted", FALLBACK_THEME.muted),
    faint: value("--ui-faint", FALLBACK_THEME.faint),
    grid: value("--chart-grid", FALLBACK_THEME.grid),
    border: value("--ui-border", FALLBACK_THEME.border),
    borderStrong: value("--ui-border-strong", FALLBACK_THEME.borderStrong),
    elevated: value("--ui-elevated", FALLBACK_THEME.elevated),
    panel: value("--ui-panel", FALLBACK_THEME.panel),
    input: value("--ui-input", FALLBACK_THEME.input),
    accent: value("--ui-accent", FALLBACK_THEME.accent),
    green: value("--ui-green", FALLBACK_THEME.green),
    amber: value("--ui-amber", FALLBACK_THEME.amber),
    red: value("--ui-red", FALLBACK_THEME.red),
  };
}

/** Re-resolve chart colors whenever the app theme changes. */
export function useChartTheme(): ChartTheme {
  const [theme, setTheme] = useState<ChartTheme>(resolveChartTheme);

  useEffect(() => {
    let frame = 0;
    const update = () => {
      frame = 0;
      setTheme(resolveChartTheme());
    };
    const observer = new MutationObserver(() => {
      if (frame) return;
      frame = window.requestAnimationFrame(update);
    });
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-theme"],
    });
    return () => {
      if (frame) window.cancelAnimationFrame(frame);
      observer.disconnect();
    };
  }, []);

  return theme;
}

export function escapeHtml(value: string): string {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}
