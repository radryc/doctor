import { Fragment, useCallback, useEffect, useMemo, useState } from "react";
import {
  ChevronDown,
  ChevronRight,
  Cloud,
  Globe2,
  RefreshCw,
  Server,
  ShieldCheck,
  TriangleAlert,
  XCircle,
} from "lucide-react";
import { api } from "../api/client";
import type { FederationStatus, FederationTarget, ProxyEndpointInfo } from "../types/api";
import "./federation.css";

const PAGE_SIZE = 100;

type TargetHealth = "healthy" | "degraded" | "open" | "unhealthy";

function targetHealth(target: FederationTarget): TargetHealth {
  if (target.circuit?.open) return "open";
  if (!target.healthy) return "unhealthy";
  if ((target.circuit?.failures ?? 0) > 0) return "degraded";
  return "healthy";
}

function healthLabel(health: TargetHealth): string {
  switch (health) {
    case "open":
      return "Circuit open";
    case "unhealthy":
      return "Unhealthy";
    case "degraded":
      return "Degraded";
    default:
      return "Healthy";
  }
}

function formatAge(value?: string): string {
  if (!value) return "—";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime()) || date.getFullYear() < 2000) return "—";
  const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m ago`;
  if (seconds < 86_400) return `${Math.round(seconds / 3600)}h ago`;
  return date.toLocaleDateString();
}

function signalBadge(signal: string): string {
  switch (signal.toLowerCase()) {
    case "metrics":
      return "M";
    case "logs":
      return "L";
    case "traces":
      return "T";
    default:
      return signal.slice(0, 1).toUpperCase();
  }
}

function MetadataList({ title, items }: { title: string; items: string[] }) {
  return (
    <div className="fed-detail__section">
      <h4>
        {title}
        <span>{items.length}</span>
      </h4>
      {items.length === 0 ? (
        <p className="fed-detail__empty">No discovered entries.</p>
      ) : (
        <ul className="fed-detail__list">
          {items.map((item) => (
            <li key={item} title={item}>
              {item}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function StatCard({
  icon,
  label,
  value,
  tone,
}: {
  icon: React.ReactNode;
  label: string;
  value: number | string;
  tone?: "ok" | "warn" | "err";
}) {
  return (
    <div className={`fed-stat${tone ? ` fed-stat--${tone}` : ""}`}>
      <span className="fed-stat__icon">{icon}</span>
      <span className="fed-stat__value">{value}</span>
      <span className="fed-stat__label">{label}</span>
    </div>
  );
}

export default function FederationCatalogPage() {
  const [status, setStatus] = useState<FederationStatus | null>(null);
  const [targets, setTargets] = useState<FederationTarget[]>([]);
  const [endpoints, setEndpoints] = useState<ProxyEndpointInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [accountFilter, setAccountFilter] = useState("");
  const [regionFilter, setRegionFilter] = useState("");
  const [providerFilter, setProviderFilter] = useState("");
  const [partitionFilter, setPartitionFilter] = useState("");
  const [healthFilter, setHealthFilter] = useState("");
  const [cursor, setCursor] = useState("");
  const [cursorStack, setCursorStack] = useState<string[]>([]);
  const [nextCursor, setNextCursor] = useState("");
  const [hasMore, setHasMore] = useState(false);
  const [matched, setMatched] = useState(0);
  const [expandedId, setExpandedId] = useState("");
  const [detail, setDetail] = useState<FederationTarget | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);

  const load = useCallback(() => {
    setLoading(true);
    setError("");
    const params = new URLSearchParams({ limit: String(PAGE_SIZE) });
    if (accountFilter) params.set("account", accountFilter);
    if (regionFilter) params.set("region", regionFilter);
    if (partitionFilter) params.set("partition", partitionFilter);
    if (search.trim()) params.set("q", search.trim());
    if (cursor) params.set("cursor", cursor);

    Promise.all([
      api.getFederationStatus(),
      api.getFederationTargets(params),
      api.getProxyEndpoints(new URLSearchParams({ limit: "200" })),
    ])
      .then(([nextStatus, page, proxy]) => {
        setStatus(nextStatus);
        setTargets(page.targets ?? []);
        setEndpoints(proxy.endpoints ?? []);
        setMatched(page.matched ?? 0);
        setNextCursor(page.nextCursor ?? "");
        setHasMore(Boolean(page.hasMore));
      })
      .catch((err) => setError(err instanceof Error ? err.message : "Failed to load federation catalog"))
      .finally(() => setLoading(false));
  }, [accountFilter, cursor, partitionFilter, regionFilter, search]);

  useEffect(() => {
    const id = window.setTimeout(load, 250);
    return () => window.clearTimeout(id);
  }, [load]);

  useEffect(() => {
    const id = window.setInterval(load, 30_000);
    return () => window.clearInterval(id);
  }, [load]);

  const accountOptions = useMemo(() => {
    const values = new Set(Object.keys(status?.accounts ?? {}));
    for (const target of targets) values.add(target.account);
    return [...values].filter(Boolean).sort();
  }, [status, targets]);

  const regionOptions = useMemo(() => {
    const values = new Set(Object.keys(status?.regions ?? {}));
    for (const target of targets) values.add(target.region);
    return [...values].filter(Boolean).sort();
  }, [status, targets]);

  const partitionOptions = useMemo(() => {
    const values = new Set(Object.keys(status?.partitions ?? {}));
    for (const target of targets) {
      for (const partition of target.partitions) values.add(partition);
    }
    return [...values].filter(Boolean).sort();
  }, [status, targets]);

  const providerOf = (value: { provider?: string; kind?: string }) =>
    (value.provider || value.kind || "").toLowerCase();

  const providerOptions = useMemo(() => {
    const values = new Set<string>();
    for (const target of targets) values.add(providerOf(target));
    for (const endpoint of endpoints) values.add(providerOf(endpoint));
    return [...values].filter(Boolean).sort();
  }, [endpoints, targets]);

  const visibleTargets = useMemo(
    () =>
      targets.filter(
        (target) =>
          (!healthFilter || targetHealth(target) === healthFilter) &&
          (!providerFilter || providerOf(target) === providerFilter),
      ),
    [healthFilter, providerFilter, targets],
  );

  const visibleEndpoints = useMemo(
    () =>
      endpoints.filter(
        (endpoint) => !providerFilter || providerOf(endpoint) === providerFilter,
      ),
    [endpoints, providerFilter],
  );

  const resetPage = () => {
    setCursor("");
    setCursorStack([]);
    setNextCursor("");
  };

  const openDetail = async (target: FederationTarget) => {
    if (expandedId === target.id) {
      setExpandedId("");
      setDetail(null);
      return;
    }
    setExpandedId(target.id);
    setDetail(null);
    setDetailLoading(true);
    try {
      const params = new URLSearchParams({
        q: target.id,
        limit: "5",
        include_metadata: "1",
      });
      const page = await api.getFederationTargets(params);
      const exact = (page.targets ?? []).find((item) => item.id === target.id) ?? null;
      setDetail(exact);
    } catch {
      setDetail(target);
    } finally {
      setDetailLoading(false);
    }
  };

  const accounts = status?.account_count ?? accountOptions.length;
  const partitions = status?.partition_count ?? partitionOptions.length;
  const regions = Object.keys(status?.regions ?? {}).length;
  const targetCount = status?.targets ?? targets.length;
  const endpointCount = visibleEndpoints.length;
  const providers = providerOptions.length;
  const openCircuits = status?.open_circuits ?? 0;
  const enabled = status?.enabled !== false;

  return (
    <div className="tab-panel">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Federation Catalog</h2>
          <p>
            {loading
              ? "Loading…"
              : `${targetCount} catalog target${targetCount === 1 ? "" : "s"} · ${endpointCount} provider endpoint${
                  endpointCount === 1 ? "" : "s"
                } · ${partitions} partition${partitions === 1 ? "" : "s"} · ${providers} provider${
                  providers === 1 ? "" : "s"
                } visible to you`}
          </p>
        </div>
        <div className="xp-toolbar">
          <button className={`refresh-btn${loading ? " spinning" : ""}`} onClick={load} disabled={loading}>
            <RefreshCw size={14} className="spin-icon" />
            Refresh
          </button>
        </div>
      </div>

      <div className="panel-body fed-body">
        {!enabled && !loading ? (
          <div className="fed-banner fed-banner--warn">
            <TriangleAlert size={15} />
            <span>
              Federation catalog is not configured. Discovered provider endpoints (AWS, GCP, Kubernetes) are still
              shown below.
            </span>
          </div>
        ) : null}

        {status?.stale ? (
          <div className="fed-banner fed-banner--warn">
            <TriangleAlert size={15} />
            <span>
              The federation catalog is stale. Last loaded {formatAge(status.loaded_at)} from{" "}
              <code>{status.source || "unknown source"}</code>.
            </span>
          </div>
        ) : null}

        {error ? (
          <div className="fed-banner fed-banner--err">
            <XCircle size={15} />
            <span>{error}</span>
          </div>
        ) : null}

        <div className="fed-stats">
          <StatCard icon={<Server size={14} />} label="Targets" value={targetCount} />
          <StatCard icon={<Cloud size={14} />} label="Accounts / Projects" value={accounts} />
          <StatCard icon={<Server size={14} />} label="Provider endpoints" value={endpointCount} />
          <StatCard icon={<ShieldCheck size={14} />} label="Partitions" value={partitions} />
          <StatCard icon={<Globe2 size={14} />} label="Regions" value={regions} />
          <StatCard
            icon={<XCircle size={14} />}
            label="Open circuits"
            value={openCircuits}
            tone={openCircuits > 0 ? "err" : "ok"}
          />
        </div>

        {status?.enabled ? (
          <div className="fed-meta">
            <span>
              Catalog <code>{status.version || "—"}</code>
            </span>
            <span>
              Source <code>{status.source || "—"}</code>
            </span>
            <span>Loaded {formatAge(status.loaded_at)}</span>
            <span>Max fan-out {status.max_fanout ?? "—"}</span>
            {status.accounts_truncated ? <span>Account list truncated</span> : null}
            {status.partitions_truncated ? <span>Partition list truncated</span> : null}
          </div>
        ) : null}

        <div className="xp-toolbar fed-filters">
          <input
            type="text"
            className="xp-search"
            placeholder="Search target, account/project, region, endpoint, partition…"
            value={search}
            onChange={(event) => {
              setSearch(event.target.value);
              resetPage();
            }}
          />
          <label className="filter-label">
            Provider
            <select
              className="xp-select"
              value={providerFilter}
              onChange={(event) => {
                setProviderFilter(event.target.value);
                resetPage();
              }}
            >
              <option value="">All</option>
              {providerOptions.map((provider) => (
                <option key={provider} value={provider}>
                  {provider}
                </option>
              ))}
            </select>
          </label>
          <label className="filter-label">
            Account / Project
            <select
              className="xp-select"
              value={accountFilter}
              onChange={(event) => {
                setAccountFilter(event.target.value);
                resetPage();
              }}
            >
              <option value="">All</option>
              {accountOptions.map((account) => (
                <option key={account} value={account}>
                  {account}
                </option>
              ))}
            </select>
          </label>
          <label className="filter-label">
            Region
            <select
              className="xp-select"
              value={regionFilter}
              onChange={(event) => {
                setRegionFilter(event.target.value);
                resetPage();
              }}
            >
              <option value="">All</option>
              {regionOptions.map((region) => (
                <option key={region} value={region}>
                  {region}
                </option>
              ))}
            </select>
          </label>
          <label className="filter-label">
            Partition
            <select
              className="xp-select"
              value={partitionFilter}
              onChange={(event) => {
                setPartitionFilter(event.target.value);
                resetPage();
              }}
            >
              <option value="">All</option>
              {partitionOptions.map((partition) => (
                <option key={partition} value={partition}>
                  {partition}
                </option>
              ))}
            </select>
          </label>
          <label className="filter-label">
            Health
            <select
              className="xp-select"
              value={healthFilter}
              onChange={(event) => setHealthFilter(event.target.value)}
            >
              <option value="">All</option>
              <option value="healthy">Healthy</option>
              <option value="degraded">Degraded</option>
              <option value="open">Circuit open</option>
              <option value="unhealthy">Unhealthy</option>
            </select>
          </label>
        </div>

        <div className="fed-table">
          <div className="fed-table__head">
            <span>Target</span>
            <span>Account / Project</span>
            <span>Partitions</span>
            <span>Endpoint</span>
            <span>Signals</span>
            <span>Health</span>
            <span>Discovered</span>
            <span />
          </div>
          {loading ? (
            <div className="fed-empty">Loading federation targets…</div>
          ) : visibleTargets.length === 0 ? (
            <div className="fed-empty">
              {targets.length === 0
                ? "No targets are visible to your identity."
                : "No targets match the current filters."}
            </div>
          ) : (
            visibleTargets.map((target) => {
              const health = targetHealth(target);
              const expanded = expandedId === target.id;
              return (
                <Fragment key={target.id}>
                  <div
                    className={`fed-row${expanded ? " fed-row--open" : ""}`}
                    onClick={() => openDetail(target)}
                  >
                    <span className="fed-target">
                      <span className="fed-target__id" title={target.id}>
                        {target.id}
                      </span>
                      <small>{target.provider || "aws"}</small>
                    </span>
                    <span className="fed-account">
                      {target.account}
                      <small>{target.region}</small>
                    </span>
                    <span className="fed-partitions">
                      {target.partitions.map((partition) => (
                        <span key={partition} className="fed-partition-badge">
                          {partition}
                        </span>
                      ))}
                    </span>
                    <span className="fed-endpoint" title={target.endpoint}>
                      {target.endpoint}
                    </span>
                    <span className="fed-signals">
                      {(target.capabilities ?? []).map((signal) => (
                        <span
                          key={signal}
                          className={`fed-signal fed-signal--${signal.toLowerCase()}`}
                          title={signal}
                        >
                          {signalBadge(signal)}
                        </span>
                      ))}
                    </span>
                    <span className={`fed-health fed-health--${health}`}>
                      <span className="fed-health__dot" />
                      {healthLabel(health)}
                    </span>
                    <span className="fed-discovered">
                      <span title="Discovered metrics">M {target.metric_count}</span>
                      <span title="Discovered logs">L {target.log_group_count}</span>
                      <span title="Discovered traces">T {target.xray_service_count}</span>
                    </span>
                    <span className="fed-chevron">
                      {expanded ? <ChevronDown size={15} /> : <ChevronRight size={15} />}
                    </span>
                  </div>
                  {expanded ? (
                    <div className="fed-detail">
                      {detailLoading ? (
                        <div className="fed-detail__empty">Loading catalog metadata…</div>
                      ) : detail ? (
                        <>
                          <div className="fed-detail__meta">
                            <span>
                              Endpoint <code>{detail.endpoint}</code>
                            </span>
                            <span>
                              Metadata fetched <strong>{formatAge(detail.metadata_fetched_at)}</strong>
                            </span>
                            {detail.circuit?.last_error ? (
                              <span className="fed-detail__error">
                                Last circuit error: {detail.circuit.last_error}
                              </span>
                            ) : null}
                          </div>
                          <div className="fed-detail__grid">
                            <MetadataList title="Discovered metrics" items={detail.metric_names ?? []} />
                            <MetadataList title="Log groups" items={detail.log_groups ?? []} />
                            <MetadataList title="Discovered trace services" items={detail.xray_services ?? []} />
                          </div>
                          {detail.service_partitions &&
                          Object.keys(detail.service_partitions).length > 0 ? (
                            <div className="fed-detail__section">
                              <h4>
                                Service partitions
                                <span>{Object.keys(detail.service_partitions).length}</span>
                              </h4>
                              <ul className="fed-detail__list">
                                {Object.entries(detail.service_partitions).map(([service, partition]) => (
                                  <li key={service} title={`${service} -> ${partition}`}>
                                    {service} <span className="fed-detail__arrow">→</span> {partition}
                                  </li>
                                ))}
                              </ul>
                            </div>
                          ) : null}
                        </>
                      ) : (
                        <div className="fed-detail__empty">No catalog metadata available for this target.</div>
                      )}
                    </div>
                  ) : null}
                </Fragment>
              );
            })
          )}
        </div>

        <div className="fed-pager">
          <span>
            {loading ? "Loading…" : `${matched} matched · showing ${visibleTargets.length}`}
            {healthFilter ? " (health filter applies to the loaded page)" : ""}
          </span>
          <div className="fed-pager__actions">
            <button
              className="xp-live-btn"
              disabled={cursorStack.length === 0 || loading}
              onClick={() => {
                const stack = [...cursorStack];
                const previous = stack.pop() ?? "";
                setCursorStack(stack);
                setCursor(previous);
              }}
            >
              Previous
            </button>
            <button
              className="xp-live-btn"
              disabled={!hasMore || !nextCursor || loading}
              onClick={() => {
                setCursorStack([...cursorStack, cursor]);
                setCursor(nextCursor);
              }}
            >
              Next
            </button>
          </div>
        </div>

        <div className="fed-endpoints">
          <div className="fed-endpoints__header">
            <h3>Provider endpoints</h3>
            <p>
              Cloud read APIs discovered through Guardian. GCP endpoints appear here as soon as the pusher registers
              its <code>observedApiUrl</code>.
            </p>
          </div>
          {visibleEndpoints.length === 0 ? (
            <div className="fed-empty">No provider endpoints match the current filters.</div>
          ) : (
            <div className="fed-endpoints__table">
              <div className="fed-endpoints__head">
                <span>Name</span>
                <span>Provider</span>
                <span>Account / Project</span>
                <span>Region</span>
                <span>Signals</span>
                <span>Status</span>
                <span>Endpoint</span>
              </div>
              {visibleEndpoints.map((endpoint) => (
                <div className="fed-endpoints__row" key={endpoint.name}>
                  <span title={endpoint.name}>{endpoint.name}</span>
                  <span>
                    <span className={`fed-provider fed-provider--${providerOf(endpoint) || "unknown"}`}>
                      {providerOf(endpoint) || "unknown"}
                    </span>
                  </span>
                  <span>{endpoint.account || "—"}</span>
                  <span>{endpoint.region || "—"}</span>
                  <span className="fed-signals">
                    {(endpoint.signals ?? []).map((signal) => (
                      <span key={signal} className={`fed-signal fed-signal--${signal.toLowerCase()}`} title={signal}>
                        {signalBadge(signal)}
                      </span>
                    ))}
                  </span>
                  <span className={`fed-health fed-health--${endpoint.healthy === false ? "unhealthy" : "healthy"}`}>
                    <span className="fed-health__dot" />
                    {endpoint.status || (endpoint.healthy === false ? "Unhealthy" : "Healthy")}
                  </span>
                  <span className="fed-endpoint" title={endpoint.resolver || endpoint.ownersPath}>
                    {endpoint.resolver || endpoint.ownersPath || "—"}
                  </span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
