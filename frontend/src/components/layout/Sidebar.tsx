import { NavLink } from "react-router-dom";
import { useConfig } from "../../context/ConfigContext";
import { LayoutDashboard, Network, Search, FileText, BarChart3, GitBranch } from "lucide-react";

const NAV = [
  { to: "/", label: "Dashboard", icon: LayoutDashboard },
  { to: "/services", label: "Services", icon: Network },
  { to: "/traces", label: "Traces", icon: GitBranch },
  { to: "/logs", label: "Logs", icon: FileText },
  { to: "/metrics", label: "Metrics", icon: BarChart3 },
  { to: "/fleet", label: "Fleet", icon: Search },
] as const;

export default function Sidebar() {
  const { config } = useConfig();

  const nav = [...NAV];

  return (
    <aside className="sidebar">
      <div className="brand">
        <div className="brand__mark">
          <img src="/doctor.png" alt="Doctor" style={{ height: "2rem", width: "auto" }} />
        </div>
        <div className="brand__text">
          <strong>Doctor</strong>
          <span>Telemetry</span>
        </div>
      </div>

      <nav className="tab-nav">
        {nav.map(({ to, label, icon: Icon }) => (
          <NavLink
            key={to}
            to={to}
            end={to === "/"}
            className={({ isActive }) =>
              "tab-link" + (isActive ? " is-active" : "")
            }
          >
            <Icon className="w-4 h-4" />
            {label}
          </NavLink>
        ))}
      </nav>

      {config.guardian_url && (
        <div className="sidebar-footer">
          <a
            href={config.guardian_url}
            target="_blank"
            rel="noreferrer"
          >
            Guardian UI
          </a>
        </div>
      )}
    </aside>
  );
}
