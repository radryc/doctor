import { NavLink } from "react-router-dom";
import { Command, Network, FileText, BarChart3, GitBranch, LayoutDashboard, Search } from "lucide-react";
import { useConfig } from "../../context/ConfigContext";

const NAV = [
  { to: "/", label: "On-call", icon: LayoutDashboard },
  { to: "/services", label: "Services", icon: Network },
  { to: "/traces", label: "Traces", icon: GitBranch },
  { to: "/logs", label: "Logs", icon: FileText },
  { to: "/metrics", label: "Metrics", icon: BarChart3 },
  { to: "/fleet", label: "Fleet", icon: Search },
] as const;

interface SidebarProps {
  onOpenPalette: () => void;
}

export default function Sidebar({ onOpenPalette }: SidebarProps) {
  const { config } = useConfig();

  return (
    <aside className="sidebar">
      <div className="brand">
        <div className="brand__mark">
          <img src="/doctor.png" alt="Doctor" style={{ height: "2rem", width: "auto" }} />
        </div>
        <div className="brand__text">
          <strong>Doctor</strong>
          <span>Telemetry triage</span>
        </div>
      </div>

      <button className="command-trigger" type="button" onClick={onOpenPalette}>
        <Command size={14} aria-hidden="true" />
        <span>Quick jump</span>
        <kbd>⌘K</kbd>
      </button>

      <nav className="tab-nav">
        {NAV.map(({ to, label, icon: Icon }) => (
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

      <div className="sidebar-footer">
        <div className="sidebar-meta">
          <span>Tenant</span>
          <strong>{config.default_tenant || "default"}</strong>
        </div>
        {config.guardian_url && (
          <a href={config.guardian_url} target="_blank" rel="noreferrer">
            Guardian UI
          </a>
        )}
      </div>
    </aside>
  );
}
