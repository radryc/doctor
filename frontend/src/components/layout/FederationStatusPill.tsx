import { useEffect, useRef, useState } from "react";
import { api } from "../../api/client";
import type {
  ProxyEndpointInfo,
  ProxyEndpointSummary,
} from "../../types/api";

type Health = "healthy" | "degraded" | "offline";

function endpointHealth(ep: ProxyEndpointInfo): Health {
  if (ep.health) {
    const value = ep.health.toLowerCase();
    if (value === "healthy" || value === "degraded" || value === "offline") return value;
  }
  if (ep.healthy === false) return "offline";
  const status = (ep.status ?? "").toLowerCase();
  if (/(fail|offline|down|error|unhealthy)/.test(status)) return "offline";
  if (/(degrad|attention|warn|drift)/.test(status)) return "degraded";
  if (ep.healthy === true || /(active|healthy|ready|ok)/.test(status)) return "healthy";
  return "offline";
}

function healthClass(health: Health): string {
  return `fed-pill__dot--${health}`;
}

function healthLabel(summary: ProxyEndpointSummary | null, endpoints: ProxyEndpointInfo[]): string {
  if (summary) {
    if (summary.total === 0) return "No backends";
    const parts = [`${summary.healthy} Healthy`];
    if (summary.unhealthy > 0) parts.push(`${summary.unhealthy} Unhealthy`);
    return parts.join(" · ");
  }
  if (endpoints.length === 0) return "No backends";
  const counts = { healthy: 0, degraded: 0, offline: 0 };
  for (const endpoint of endpoints) counts[endpointHealth(endpoint)] += 1;
  if (counts.healthy === endpoints.length) return `${endpoints.length} Healthy`;
  const parts: string[] = [];
  if (counts.healthy > 0) parts.push(`${counts.healthy} Healthy`);
  if (counts.degraded > 0) parts.push(`${counts.degraded} Degraded`);
  if (counts.offline > 0) parts.push(`${counts.offline} Offline`);
  return parts.join(" · ");
}

function overallHealth(
  summary: ProxyEndpointSummary | null,
  endpoints: ProxyEndpointInfo[]
): Health {
  if (summary) {
    if (summary.total === 0) return "offline";
    if (summary.unhealthy === 0) return "healthy";
    if (summary.healthy === 0) return "offline";
    return "degraded";
  }
  if (endpoints.length === 0) return "offline";
  const states = endpoints.map(endpointHealth);
  if (states.every((state) => state === "healthy")) return "healthy";
  if (states.some((state) => state === "offline")) return "offline";
  return "degraded";
}

export default function FederationStatusPill() {
  const [summary, setSummary] = useState<ProxyEndpointSummary | null>(null);
  const [endpoints, setEndpoints] = useState<ProxyEndpointInfo[]>([]);
  const [open, setOpen] = useState(false);
  const [loadingDetails, setLoadingDetails] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  const loadSummary = () => {
    api
      .getProxyEndpoints(new URLSearchParams({ summary_only: "1" }))
      .then((r) => setSummary(r.summary ?? null))
      .catch(() => {});
  };

  const loadDetails = () => {
    setLoadingDetails(true);
    api
      .getProxyEndpoints(new URLSearchParams({ limit: "50", outliers: "1" }))
      .then((r) => {
        setEndpoints(r.endpoints ?? []);
        if (r.summary) setSummary(r.summary);
      })
      .catch(() => setEndpoints([]))
      .finally(() => setLoadingDetails(false));
  };

  useEffect(() => {
    loadSummary();
    const id = setInterval(loadSummary, 30_000);
    return () => clearInterval(id);
  }, []);

  useEffect(() => {
    if (!open) return;
    loadDetails();
  }, [open]);

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, []);

  const health = overallHealth(summary, endpoints);

  return (
    <div className="fed-pill" ref={ref}>
      <button
        className={`fed-pill__btn fed-pill__btn--${health}`}
        onClick={() => setOpen((o) => !o)}
        title="Federated backends health"
      >
        <span className={`fed-pill__dot ${healthClass(health)}`} />
        <span className="fed-pill__label">{healthLabel(summary, endpoints)}</span>
      </button>

      {open && (
        <div className="fed-pill__popover">
          <div className="fed-pill__popover-title">
            Federated Backends
            {summary && summary.total > 0 && (
              <span className="fed-pill__popover-count">
                {summary.healthy}/{summary.total} healthy
              </span>
            )}
          </div>
          {loadingDetails ? (
            <div className="fed-pill__empty">Loading backends…</div>
          ) : endpoints.length === 0 ? (
            <div className="fed-pill__empty">No backends configured</div>
          ) : (
            <table className="fed-pill__table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Kind</th>
                  <th>Signals</th>
                  <th>Status</th>
                </tr>
              </thead>
              <tbody>
                {endpoints.map((ep) => {
                  const epHealth = endpointHealth(ep);
                  return (
                    <tr key={ep.name}>
                      <td title={ep.name}>{ep.name}</td>
                      <td>{ep.kind || "—"}</td>
                      <td>{(ep.signals ?? []).join(", ") || "—"}</td>
                      <td>
                        <span className={`fed-pill__dot ${healthClass(epHealth)}`} />
                        {ep.status || epHealth}
                        {ep.reason && (
                          <span className="fed-pill__reason" title={ep.reason}>
                            {ep.reason}
                          </span>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  );
}
