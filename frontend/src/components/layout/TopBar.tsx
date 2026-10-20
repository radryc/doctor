import { useEffect, useState } from "react";
import { useLocation } from "react-router-dom";
import { api } from "../../api/client";
import ThemePicker from "./ThemePicker";

const PAGE_TITLES: Record<string, string> = {
  "/": "On-call",
  "/rules": "Recording Rules",
  "/traces": "Traces",
  "/logs": "Logs",
  "/metrics": "Metrics",
  "/fleet": "Fleet",
};

function pageTitle(pathname: string): string {
  if (pathname.startsWith("/partition/")) return "Partition";
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
