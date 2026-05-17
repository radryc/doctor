import { NavLink } from "react-router-dom";
import { useConfig } from "../../context/ConfigContext";

const NAV = [
  { to: "/", label: "Dashboard", icon: "▪" },
  { to: "/traces", label: "Traces", icon: "◆" },
  { to: "/logs", label: "Logs", icon: "≡" },
  { to: "/metrics", label: "Metrics", icon: "∿" },
] as const;

export default function Sidebar() {
  const { config } = useConfig();

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
        {NAV.map(({ to, label, icon }) => (
          <NavLink
            key={to}
            to={to}
            end={to === "/"}
            className={({ isActive }) =>
              "tab-link" + (isActive ? " is-active" : "")
            }
          >
            <em className="tab-link__icon">{icon}</em>
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
