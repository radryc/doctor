import { createContext, useCallback, useContext, useMemo, useState } from "react";
import type { ReactNode } from "react";
import type { LogRecord } from "../types/api";

export type InspectorKind = "trace" | "log" | "service" | "rollout";

export interface InspectorTarget {
  kind: InspectorKind;
  /** Trace ID for trace/log (when correlating via trace), service name for service. */
  id: string;
  /** Optional span ID for log correlation. */
  spanId?: string;
  /** Optional tenant context. */
  tenant?: string;
  /** Full log record when the target was opened from a log row. */
  log?: LogRecord;
}

interface InspectorState {
  target: InspectorTarget | null;
  isOpen: boolean;
  open(target: InspectorTarget): void;
  close(): void;
}

const InspectorContext = createContext<InspectorState | null>(null);

export function useInspector(): InspectorState {
  const ctx = useContext(InspectorContext);
  if (!ctx) throw new Error("useInspector must be used within InspectorProvider");
  return ctx;
}

export function InspectorProvider({ children }: { children: ReactNode }) {
  const [target, setTarget] = useState<InspectorTarget | null>(null);

  const open = useCallback((t: InspectorTarget) => setTarget(t), []);
  const close = useCallback(() => setTarget(null), []);

  const value = useMemo<InspectorState>(
    () => ({ target, isOpen: target !== null, open, close }),
    [target, open, close]
  );

  return (
    <InspectorContext.Provider value={value}>
      {children}
    </InspectorContext.Provider>
  );
}
