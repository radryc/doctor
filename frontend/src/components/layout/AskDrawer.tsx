import { useEffect, useMemo, useRef, useState } from "react";
import { ExternalLink, Sparkles, X } from "lucide-react";
import { useLocation } from "react-router-dom";

function agentBase(): string {
  return (
    (import.meta.env.VITE_AGENT_URL as string | undefined) ||
    "http://localhost:3000"
  ).replace(/\/$/, "");
}

export default function AskDrawer() {
  const [open, setOpen] = useState(false);
  const drawerRef = useRef<HTMLElement>(null);
  const closeRef = useRef<HTMLButtonElement>(null);
  const previousFocus = useRef<HTMLElement | null>(null);
  const location = useLocation();
  const url = useMemo(() => {
    const context = `${location.pathname}${location.search}`;
    return `${agentBase()}/embed/ask?context=${encodeURIComponent(`Doctor · ${context}`)}`;
  }, [location.pathname, location.search]);

  useEffect(() => {
    if (!open) return;
    previousFocus.current = document.activeElement as HTMLElement | null;
    const timer = window.setTimeout(() => closeRef.current?.focus(), 30);
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setOpen(false);
        return;
      }
      if (event.key !== "Tab" || !drawerRef.current) return;
      const focusable = drawerRef.current.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), iframe, input, select, textarea, [tabindex]:not([tabindex="-1"])'
      );
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => {
      window.clearTimeout(timer);
      document.removeEventListener("keydown", onKey);
      previousFocus.current?.focus();
    };
  }, [open]);

  return (
    <>
      <button
        type="button"
        className="s-btn s-btn-secondary"
        onClick={() => setOpen(true)}
        title="Ask Strata about this view"
      >
        <Sparkles size={14} className="text-accent-purple" />
        Ask
      </button>
      {open && (
        <div className="s-drawer-backdrop" onClick={() => setOpen(false)}>
          <aside
            ref={drawerRef}
            className="s-drawer"
            role="dialog"
            aria-modal="true"
            aria-label="Ask Strata"
            onClick={(event) => event.stopPropagation()}
          >
            <header className="s-drawer__header">
              <div className="flex items-center gap-2">
                <Sparkles size={16} className="text-accent-purple" />
                <span className="s-drawer__title">Ask Strata</span>
              </div>
              <div className="flex items-center gap-1">
                <a
                  className="s-icon-btn"
                  href={url}
                  target="_blank"
                  rel="noreferrer"
                  title="Open in Agent"
                  aria-label="Open in Agent"
                >
                  <ExternalLink size={15} />
                </a>
                <button
                  ref={closeRef}
                  type="button"
                  className="s-icon-btn"
                  onClick={() => setOpen(false)}
                  aria-label="Close Ask Strata"
                >
                  <X size={16} />
                </button>
              </div>
            </header>
            <div className="s-drawer__body">
              <iframe
                title="Ask Strata"
                src={url}
                className="h-full w-full border-0"
                allow="clipboard-read; clipboard-write"
              />
            </div>
          </aside>
        </div>
      )}
    </>
  );
}
