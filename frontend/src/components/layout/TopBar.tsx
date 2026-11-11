import { useLocation } from "react-router-dom";
import GlobalTimePicker from "./GlobalTimePicker";
import FederationStatusPill from "./FederationStatusPill";
import ThemePicker from "./ThemePicker";
import UserMenu from "./UserMenu";
import { Loader2, Menu } from "lucide-react";
import AskDrawer from "./AskDrawer";
import { useRequestActivity } from "../../context/RequestActivityContext";

const PAGE_TITLES: Record<string, string> = {
  "/": "On-call",
  "/dashboards": "Dashboards",
  "/services": "APM Services",
  "/federation": "Federation",
  "/probes": "Synthetics",
  "/rules": "Recording Rules",
  "/alerts": "Alerts",
  "/traces": "Traces",
  "/logs": "Logs",
  "/metrics": "Metrics",
  "/sql": "SQL",
  "/fleet": "Fleet",
};

function pageTitle(pathname: string): string {
  if (pathname.startsWith("/partition/")) return "Partition";
  if (pathname.startsWith("/dashboards")) return "Dashboards";
  if (pathname.startsWith("/services")) return "APM Services";
  if (pathname.startsWith("/federation")) return "Federation";
  if (pathname.startsWith("/probes")) return "Synthetics";
  if (pathname.startsWith("/alerts")) return "Alerts";
  return PAGE_TITLES[pathname] ?? "Doctor";
}

export default function TopBar({ onToggleSidebar }: { onToggleSidebar?: () => void }) {
  const { pathname } = useLocation();
  const { pending } = useRequestActivity();

  return (
    <header className="app-topbar" aria-busy={pending > 0}>
      <button type="button" className="s-icon-btn app-topbar__toggle" onClick={onToggleSidebar} aria-label="Toggle navigation">
        <Menu size={16} />
      </button>
      <div className="app-topbar__crumb">
        <span>Doctor</span>
        <span className="app-topbar__sep">/</span>
        <strong className="app-topbar__title">{pageTitle(pathname)}</strong>
      </div>
      <div className="app-topbar__controls">
        <GlobalTimePicker />
        {pending > 0 && (
          <span className="app-topbar__activity" role="status" aria-live="polite">
            <Loader2 size={12} className="animate-spin" />
            Refreshing…
          </span>
        )}
        <FederationStatusPill />
      </div>
      <div className="flex items-center gap-2">
        <AskDrawer />
        <ThemePicker />
        <UserMenu />
      </div>
      <span className={`app-topbar__progress${pending > 0 ? " is-active" : ""}`} aria-hidden="true" />
    </header>
  );
}
