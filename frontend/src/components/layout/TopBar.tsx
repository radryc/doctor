import { useLocation } from "react-router-dom";
import GlobalTimePicker from "./GlobalTimePicker";
import FederationStatusPill from "./FederationStatusPill";
import UserMenu from "./UserMenu";
import { Menu } from "lucide-react";
import AskDrawer from "./AskDrawer";

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

  return (
    <header className="app-topbar">
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
        <FederationStatusPill />
      </div>
      <div className="flex items-center gap-2">
        <AskDrawer />
        <UserMenu />
      </div>
    </header>
  );
}
