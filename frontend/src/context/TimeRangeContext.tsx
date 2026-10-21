import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ReactNode } from "react";
import { parseDurationMs } from "../lib/variables";

export interface AbsoluteTimeRange {
  from: Date;
  to: Date;
}

export interface RelativePreset {
  label: string;
  value: string; // e.g. "15m", "1h", "6h"
}

export const RELATIVE_PRESETS: RelativePreset[] = [
  { label: "5m", value: "5m" },
  { label: "15m", value: "15m" },
  { label: "30m", value: "30m" },
  { label: "1h", value: "1h" },
  { label: "3h", value: "3h" },
  { label: "6h", value: "6h" },
  { label: "12h", value: "12h" },
  { label: "24h", value: "24h" },
  { label: "2d", value: "2d" },
  { label: "7d", value: "7d" },
];

export const AUTO_REFRESH_OPTIONS = [
  { label: "Off", value: "" },
  { label: "10s", value: "10s" },
  { label: "30s", value: "30s" },
  { label: "1m", value: "1m" },
  { label: "5m", value: "5m" },
];

export type TimeMode = "relative" | "absolute";

interface TimeRangeState {
  mode: TimeMode;
  relativePreset: string; // e.g. "1h"
  absolute: AbsoluteTimeRange | null;
  autoRefresh: string; // "" = off, "30s", "1m", etc.
  /** Returns the resolved [from, to] Date pair for the current state. */
  resolve(): AbsoluteTimeRange;
  /** Returns the step interval string suitable for PromQL. */
  stepInterval(): string;
  /** Returns the range duration string (e.g. "1h"). */
  rangeString(): string;
  /** Set a relative preset (clears absolute). */
  setRelative(preset: string): void;
  /** Set an absolute time range (e.g. from chart brush). */
  setAbsolute(from: Date, to: Date): void;
  /** Shift the window backward by 50% of the current duration. */
  shiftBack(): void;
  /** Shift the window forward by 50% of the current duration. */
  shiftForward(): void;
  /** Zoom out 2×. */
  zoomOut(): void;
  setAutoRefresh(interval: string): void;
  /** Force an immediate refresh (increments the refresh counter). */
  refresh(): void;
  refreshTick: number;
}

const TimeRangeContext = createContext<TimeRangeState | null>(null);

export function useTimeRange(): TimeRangeState {
  const ctx = useContext(TimeRangeContext);
  if (!ctx) throw new Error("useTimeRange must be used within TimeRangeProvider");
  return ctx;
}

interface Props {
  children: ReactNode;
  defaultPreset?: string;
}

export function TimeRangeProvider({ children, defaultPreset = "1h" }: Props) {
  const [mode, setMode] = useState<TimeMode>("relative");
  const [relativePreset, setRelativePreset] = useState(defaultPreset);
  const [absolute, setAbsoluteState] = useState<AbsoluteTimeRange | null>(null);
  const [autoRefresh, setAutoRefreshState] = useState("");
  const [refreshTick, setRefreshTick] = useState(0);
  const refreshRef = useRef<ReturnType<typeof setInterval> | null>(null);

  // Auto-refresh loop
  useEffect(() => {
    if (refreshRef.current) clearInterval(refreshRef.current);
    if (!autoRefresh) return;
    const ms = parseDurationMs(autoRefresh);
    if (ms <= 0) return;
    refreshRef.current = setInterval(() => setRefreshTick((t) => t + 1), ms);
    return () => {
      if (refreshRef.current) clearInterval(refreshRef.current);
    };
  }, [autoRefresh]);

  // Resolve relative windows once per refresh tick (or time-mode change) so
  // consumers receive stable Date objects. Without this, every render produced
  // a new `to` timestamp and effects keyed on [from, to] re-fetched forever.
  const resolved = useMemo<AbsoluteTimeRange>(() => {
    if (mode === "absolute" && absolute) return absolute;
    const to = new Date();
    const ms = parseDurationMs(relativePreset);
    const from = new Date(to.getTime() - (ms || 3600_000));
    return { from, to };
  }, [mode, absolute, relativePreset, refreshTick]);

  const resolve = useCallback((): AbsoluteTimeRange => resolved, [resolved]);

  const rangeString = useCallback((): string => {
    if (mode === "absolute" && absolute) {
      const ms = absolute.to.getTime() - absolute.from.getTime();
      return formatDurationMs(ms);
    }
    return relativePreset;
  }, [mode, absolute, relativePreset]);

  const stepInterval = useCallback((): string => {
    const ms = resolved.to.getTime() - resolved.from.getTime();
    const step = Math.max(Math.ceil(ms / 4 / 1000), 15);
    return formatSeconds(step);
  }, [resolved]);

  const setRelative = useCallback((preset: string) => {
    setRelativePreset(preset);
    setMode("relative");
    setAbsoluteState(null);
    // Re-anchor the relative window to "now" even when the preset is unchanged.
    setRefreshTick((tick) => tick + 1);
  }, []);

  const setAbsolute = useCallback((from: Date, to: Date) => {
    setAbsoluteState({ from, to });
    setMode("absolute");
  }, []);

  const shiftBack = useCallback(() => {
    const { from, to } = resolve();
    const half = (to.getTime() - from.getTime()) / 2;
    setAbsolute(new Date(from.getTime() - half), new Date(to.getTime() - half));
  }, [resolve, setAbsolute]);

  const shiftForward = useCallback(() => {
    const { from, to } = resolve();
    const half = (to.getTime() - from.getTime()) / 2;
    setAbsolute(new Date(from.getTime() + half), new Date(to.getTime() + half));
  }, [resolve, setAbsolute]);

  const zoomOut = useCallback(() => {
    const { from, to } = resolve();
    const ms = to.getTime() - from.getTime();
    const center = (from.getTime() + to.getTime()) / 2;
    setAbsolute(new Date(center - ms), new Date(center + ms));
  }, [resolve, setAbsolute]);

  const setAutoRefresh = useCallback((interval: string) => {
    setAutoRefreshState(interval);
  }, []);

  const refresh = useCallback(() => {
    setRefreshTick((t) => t + 1);
  }, []);

  const value = useMemo<TimeRangeState>(
    () => ({
      mode,
      relativePreset,
      absolute,
      autoRefresh,
      resolve,
      rangeString,
      stepInterval,
      setRelative,
      setAbsolute,
      shiftBack,
      shiftForward,
      zoomOut,
      setAutoRefresh,
      refresh,
      refreshTick,
    }),
    [
      mode, relativePreset, absolute, autoRefresh, resolve, rangeString,
      stepInterval, setRelative, setAbsolute, shiftBack, shiftForward,
      zoomOut, setAutoRefresh, refresh, refreshTick,
    ]
  );

  return (
    <TimeRangeContext.Provider value={value}>
      {children}
    </TimeRangeContext.Provider>
  );
}

function formatDurationMs(ms: number): string {
  if (ms < 60_000) return `${Math.round(ms / 1000)}s`;
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)}m`;
  if (ms < 86_400_000) return `${Math.round(ms / 3_600_000)}h`;
  return `${Math.round(ms / 86_400_000)}d`;
}

function formatSeconds(s: number): string {
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${Math.round(s / 3600)}h`;
}
