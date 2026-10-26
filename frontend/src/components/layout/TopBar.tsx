import { useEffect, useState } from "react";
import { useLocation } from "react-router-dom";
import { api } from "../../api/client";
import ThemePicker from "./ThemePicker";
import GlobalTimePicker from "./GlobalTimePicker";
import FederationStatusPill from "./FederationStatusPill";

const PAGE_TITLES: Record<string, string> = {
  "/": "On-call",
  "/dashboards": "Dashboards",
  "/services": "APM Services",
  "/federation": "Federation",
  "/probes": "Synthetics",
  "/rules": "Recording Rules",
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
  return PAGE_TITLES[pathname] ?? "Doctor";
}

export default function TopBar() {
  const { pathname } = useLocation();
  const [user, setUser] = useState("");

  useEffect(() => {
    let active = true;
    api
      .getUser()
      .then((identity) => {
        if (!active) return;
        setUser(
          identity.display ||
            identity.email ||
            identity.subject ||
            identity.client_id ||
            ""
        );
      })
      .catch(() => {});
    return () => {
      active = false;
    };
  }, []);

  return (
    <header className="app-topbar">
      <div className="app-topbar__crumb">
        <span>Doctor</span>
        <span className="app-topbar__sep">/</span>
        <strong className="app-topbar__title">{pageTitle(pathname)}</strong>
      </div>
      <div className="app-topbar__controls">
        <GlobalTimePicker />
        <FederationStatusPill />
      </div>
      <div className="flex items-center gap-3">
        {user && (
          <span className="text-xs opacity-70" title="Signed in">
            {user}
          </span>
        )}
        <ThemePicker />
        <a href="/auth/logout" className="text-xs font-medium opacity-70 hover:opacity-100" title="Sign out">
          Sign out
        </a>
      </div>
    </header>
  );
}
