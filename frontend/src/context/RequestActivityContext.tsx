import {
  createContext,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";

interface RequestActivityState {
  /** Number of API requests currently in flight (shown after a short delay). */
  pending: number;
}

const RequestActivityContext = createContext<RequestActivityState>({ pending: 0 });

export function useRequestActivity(): RequestActivityState {
  return useContext(RequestActivityContext);
}

/**
 * Tracks API requests globally so the shell can show a clear refresh/progress
 * indicator even on pages that do not render their own loading state.
 *
 * api/client.ts dispatches these events around every fetch. The indicator is
 * delayed briefly so fast background polls do not make it flicker.
 */
export function RequestActivityProvider({ children }: { children: ReactNode }) {
  const [pending, setPending] = useState(0);
  const active = useRef(0);
  const showTimer = useRef<number | null>(null);

  useEffect(() => {
    const start = () => {
      active.current += 1;
      if (showTimer.current !== null) return;
      showTimer.current = window.setTimeout(() => {
        showTimer.current = null;
        if (active.current > 0) setPending(active.current);
      }, 150);
    };
    const end = () => {
      active.current = Math.max(0, active.current - 1);
      if (active.current > 0) return;
      if (showTimer.current !== null) {
        window.clearTimeout(showTimer.current);
        showTimer.current = null;
      }
      setPending(0);
    };
    window.addEventListener("doctor:request-start", start);
    window.addEventListener("doctor:request-end", end);
    return () => {
      window.removeEventListener("doctor:request-start", start);
      window.removeEventListener("doctor:request-end", end);
      if (showTimer.current !== null) window.clearTimeout(showTimer.current);
    };
  }, []);

  return (
    <RequestActivityContext.Provider value={{ pending }}>
      {children}
    </RequestActivityContext.Provider>
  );
}
