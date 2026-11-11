import { useCallback, useEffect, useMemo, useState } from "react";
import {
  AlertTriangle,
  ChevronDown,
  ChevronUp,
  Gauge,
  RefreshCw,
  ShieldCheck,
  Users,
} from "lucide-react";
import { api } from "../api/client";
import type {
  Quota,
  QuotaDashboardResponse,
  QuotaPartition,
  QuotaUsage,
} from "../types/api";
import "./quota.css";

function number(value: number | undefined): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function formatRate(value: number | undefined, suffix = "/s"): string {
  const n = number(value);
  if (n <= 0) return "∞";
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)}G${suffix}`;
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M${suffix}`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k${suffix}`;
  return `${n.toFixed(n < 10 ? 1 : 0)}${suffix}`;
}

function formatBytes(value: number | undefined): string {
  const n = number(value);
  if (n <= 0) return "∞";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let index = 0;
  let size = n;
  while (size >= 1024 && index < units.length - 1) {
    size /= 1024;
    index += 1;
  }
  return `${size.toFixed(size < 10 ? 1 : 0)}${units[index]}`;
}

function formatCount(value: number | undefined): string {
  const n = number(value);
  if (n <= 0) return "∞";
  return n.toLocaleString();
}

function usageRatio(usage: QuotaUsage | undefined): number {
  if (!usage || usage.limit <= 0) return 0;
  return Math.min(1, Math.max(0, usage.used / usage.limit));
}

function findUsage(usage: QuotaUsage[] | undefined, suffix: string): QuotaUsage | undefined {
  return usage?.find((entry) => entry.key.endsWith(suffix));
}

function maxIngestUsage(
  queryUsage: QuotaUsage[] | undefined,
  ingestUsage: QuotaUsage[] | undefined,
  partition: string,
  resource: string,
): QuotaUsage | undefined {
  const candidates = [
    ...(queryUsage ?? []),
    ...(ingestUsage ?? []).filter((entry) => entry.key.startsWith(`partition:${partition}:ingest:`)),
  ].filter((entry) => entry.resource.endsWith(resource) && entry.resource !== "storage.bytes");
  if (candidates.length === 0) return undefined;
  return candidates.reduce((best, entry) =>
    usageRatio(entry) > usageRatio(best) ? entry : best,
  );
}

function UsageBar({ usage }: { usage?: QuotaUsage }) {
  const ratio = usageRatio(usage);
  const cls =
    ratio >= 0.9
      ? "quota-bar__fill quota-bar__fill--danger"
      : ratio >= 0.7
        ? "quota-bar__fill quota-bar__fill--warn"
        : "quota-bar__fill";
  return (
    <div className="quota-usage__row">
      <span className="quota-bar">
        <span className={cls} style={{ width: `${Math.round(ratio * 100)}%` }} />
      </span>
      <span className="quota-usage__value">
        {number(usage?.used).toFixed(usage && usage.used < 10 ? 1 : 0)}/
        {usage && usage.limit > 0 ? usage.limit.toFixed(usage.limit < 10 ? 1 : 0) : "∞"}
      </span>
    </div>
  );
}

function LimitGrid({ quota }: { quota: Quota }) {
  const ingest = quota.ingest;
  const query = quota.query;
  const rules = quota.rules;
  const metrics = ingest?.metrics;
  return (
    <div className="quota-limits">
      <span>
        Traces
        <strong>{formatBytes(ingest?.traces?.bytes_per_second)}/s</strong>
      </span>
      <span>
        Logs
        <strong>{formatBytes(ingest?.logs?.bytes_per_second)}/s</strong>
      </span>
      <span>
        Metrics
        <strong>{formatBytes(metrics?.bytes_per_second)}/s</strong>
      </span>
      <span>
        Series
        <strong>{formatCount(metrics?.max_active_series)}</strong>
      </span>
      <span>
        Query
        <strong>{formatRate(query?.requests_per_second)}</strong>
      </span>
      <span>
        Concurrent
        <strong>{formatCount(query?.max_concurrent)}</strong>
      </span>
      <span>
        Rules
        <strong>{formatCount(rules?.max_rules)}</strong>
      </span>
      <span>
        Groups
        <strong>{formatCount(rules?.max_groups)}</strong>
      </span>
      <span>
        Budget
        <strong>{formatCount(query?.budget_per_second)}/s</strong>
      </span>
      <span>
        Range
        <strong>{query?.max_range ?? "∞"}</strong>
      </span>
      <span>
        Samples
        <strong>{formatCount(query?.max_samples)}</strong>
      </span>
      <span>
        Series cap
        <strong>{formatCount(query?.max_series)}</strong>
      </span>
      <span>
        Storage
        <strong>{formatBytes(quota.storage?.max_bytes)}</strong>
      </span>
    </div>
  );
}

function EnforcementBadge({ partition }: { partition: QuotaPartition }) {
  if (partition.blocked) {
    return <span className="quota-badge quota-badge--blocked">blocked</span>;
  }
  return (
    <span
      className={`quota-badge ${
        partition.enforcement === "enforce" ? "quota-badge--enforce" : "quota-badge--observe"
      }`}
    >
      {partition.enforcement}
    </span>
  );
}

function SubjectQuotas({ data }: { data: QuotaDashboardResponse }) {
  if (!data.subject || data.subject.length === 0) {
    return (
      <div className="quota-empty">
        No user, group, or client quota applies to this identity. Partition quotas still apply.
      </div>
    );
  }
  return (
    <div className="quota-subjects">
      {data.subject.map((scoped) => (
        <div className="quota-subject" key={scoped.key}>
          <div className="quota-subject__key" title={scoped.key}>
            {scoped.key}
            {scoped.quota.blocked && (
              <span className="quota-badge quota-badge--blocked" style={{ marginLeft: "0.35rem" }}>
                blocked
              </span>
            )}
          </div>
          <div className="quota-subject__grid">
            <div className="quota-metric">
              <div className="quota-metric__label">Requests</div>
              <div className="quota-metric__value">
                {formatRate(scoped.quota.query?.requests_per_second)}
              </div>
            </div>
            <div className="quota-metric">
              <div className="quota-metric__label">Concurrent</div>
              <div className="quota-metric__value">
                {formatCount(scoped.quota.query?.max_concurrent)}
              </div>
            </div>
            <div className="quota-metric">
              <div className="quota-metric__label">Budget</div>
              <div className="quota-metric__value">
                {formatCount(scoped.quota.query?.budget_per_second)}/s
              </div>
            </div>
            <div className="quota-metric">
              <div className="quota-metric__label">Max range</div>
              <div className="quota-metric__value">{scoped.quota.query?.max_range ?? "∞"}</div>
            </div>
          </div>
          <div className="quota-usage" style={{ marginTop: "0.55rem" }}>
            <div className="quota-usage__label">query rps</div>
            <UsageBar usage={findUsage(scoped.usage, ":query:requests")} />
            <div className="quota-usage__label">concurrent</div>
            <UsageBar usage={findUsage(scoped.usage, ":query:concurrent")} />
          </div>
        </div>
      ))}
    </div>
  );
}

export default function QuotaPage() {
  const [data, setData] = useState<QuotaDashboardResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [filter, setFilter] = useState("");
  const [openSections, setOpenSections] = useState<Record<"subject" | "partitions", boolean>>(() => {
    if (typeof window === "undefined") return { subject: true, partitions: true };
    try {
      const raw = window.localStorage.getItem("doctor.quota.sections");
      if (raw) {
        const parsed = JSON.parse(raw) as { subject?: boolean; partitions?: boolean };
        return { subject: parsed.subject !== false, partitions: parsed.partitions !== false };
      }
    } catch {
      // Ignore restricted storage and fall back to open sections.
    }
    return { subject: true, partitions: true };
  });

  useEffect(() => {
    try {
      window.localStorage.setItem("doctor.quota.sections", JSON.stringify(openSections));
    } catch {
      // Storage can be unavailable in private/restricted contexts.
    }
  }, [openSections]);

  const allSectionsOpen = openSections.subject && openSections.partitions;
  const toggleSection = (section: "subject" | "partitions") =>
    setOpenSections((current) => ({ ...current, [section]: !current[section] }));
  const setAllSections = (open: boolean) => setOpenSections({ subject: open, partitions: open });

  const load = useCallback(() => {
    setLoading(true);
    setError("");
    api
      .getQuota()
      .then(setData)
      .catch((err) => setError(err instanceof Error ? err.message : "Failed to load quotas"))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 30_000);
    return () => window.clearInterval(timer);
  }, [load]);

  const summary = useMemo(() => {
    const partitions = data?.partitions ?? [];
    return {
      total: partitions.length,
      enforced: partitions.filter((part) => part.enforcement === "enforce" && !part.blocked).length,
      observed: partitions.filter((part) => part.enforcement !== "enforce" && !part.blocked).length,
      blocked: partitions.filter((part) => part.blocked).length,
      subjects: data?.subject?.length ?? 0,
    };
  }, [data]);

  const filteredPartitions = useMemo(() => {
    const partitions = data?.partitions ?? [];
    const needle = filter.trim().toLowerCase();
    if (!needle) return partitions;
    return partitions.filter((partition) => partition.name.toLowerCase().includes(needle));
  }, [data, filter]);

  return (
    <div className="tab-panel quota-page">
      <div className="panel-header">
        <div className="panel-header__left">
          <h2>Partition Quotas</h2>
          <p>
            {loading && !data
              ? "Loading quota policy…"
              : data?.enabled
                ? `Policy ${data.policy_version?.slice(0, 19) || "pending"} · default mode ${data.mode} · ${summary.total} partition${summary.total === 1 ? "" : "s"}`
                : "Quota enforcement is not configured for this deployment."}
          </p>
        </div>
        <div className="xp-toolbar">
          <input
            type="text"
            className="xp-search"
            placeholder="Filter partitions…"
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
          />
          <button
            type="button"
            className="quota-collapse"
            onClick={() => setAllSections(!allSectionsOpen)}
            aria-label={allSectionsOpen ? "Collapse all quota sections" : "Expand all quota sections"}
          >
            {allSectionsOpen ? <ChevronUp size={12} /> : <ChevronDown size={12} />}
            {allSectionsOpen ? "Collapse all" : "Expand all"}
          </button>
          {data?.ingest?.configured && (
            <span className="quota-badge quota-badge--observe">
              ingest {data.ingest.error ? "unreachable" : "connected"}
            </span>
          )}
          <button className="xp-live-btn" onClick={load} disabled={loading}>
            <RefreshCw size={12} className={loading ? "animate-spin" : ""} /> Refresh
          </button>
        </div>
      </div>

      {error && <div className="quota-error">{error}</div>}

      {!data?.enabled && !loading && (
        <div className="quota-section">
          <div className="quota-empty">
            Set <span className="quota-mono">DOCTOR_QUOTA_POLICY_URL</span> (or{" "}
            <span className="quota-mono">DOCTOR_ACCESS_POLICY_URL</span>) to load partition quotas.
          </div>
        </div>
      )}

      {data?.enabled && (
        <>
          <div className="quota-summary">
            <div className="quota-card">
              <div className="quota-card__label">Partitions</div>
              <div className="quota-card__value">{summary.total}</div>
              <div className="quota-card__sub">visible to you</div>
            </div>
            <div className="quota-card">
              <div className="quota-card__label">Enforced</div>
              <div className="quota-card__value">{summary.enforced}</div>
              <div className="quota-card__sub">reject over quota</div>
            </div>
            <div className="quota-card">
              <div className="quota-card__label">Observe</div>
              <div className="quota-card__value">{summary.observed}</div>
              <div className="quota-card__sub">log only</div>
            </div>
            <div className="quota-card">
              <div className="quota-card__label">Blocked</div>
              <div className="quota-card__value">{summary.blocked}</div>
              <div className="quota-card__sub">emergency stop</div>
            </div>
            <div className="quota-card">
              <div className="quota-card__label">Your buckets</div>
              <div className="quota-card__value">{summary.subjects}</div>
              <div className="quota-card__sub">user / group / client</div>
            </div>
          </div>

          <div className="quota-section">
            <div className="quota-section__header">
              <div>
                <h3>
                  <Users size={13} className="inline mr-1" /> Your limits
                </h3>
                <p>Every request must satisfy these buckets and the target partition bucket.</p>
              </div>
              <div className="quota-section__actions">
                <span className="quota-badge quota-badge--observe">{data.mode}</span>
                <button
                  type="button"
                  className="quota-collapse"
                  aria-expanded={openSections.subject}
                  onClick={() => toggleSection("subject")}
                >
                  {openSections.subject ? <ChevronUp size={12} /> : <ChevronDown size={12} />}
                  {openSections.subject ? "Collapse" : "Expand"}
                </button>
              </div>
            </div>
            {openSections.subject && <SubjectQuotas data={data} />}
          </div>

          <div className="quota-section">
            <div className="quota-section__header">
              <div>
                <h3>
                  <Gauge size={13} className="inline mr-1" /> Partition envelopes
                </h3>
                <p>Effective limits after profile, partition override, and policy defaults.</p>
              </div>
              <div className="quota-section__actions">
                <span className="quota-badge quota-badge--observe">
                  <ShieldCheck size={11} /> policy {data.policy_ready ? "ready" : "pending"}
                </span>
                <button
                  type="button"
                  className="quota-collapse"
                  aria-expanded={openSections.partitions}
                  onClick={() => toggleSection("partitions")}
                >
                  {openSections.partitions ? <ChevronUp size={12} /> : <ChevronDown size={12} />}
                  {openSections.partitions ? "Collapse" : "Expand"}
                </button>
              </div>
            </div>
            {openSections.partitions && (
              <>
                {filteredPartitions.length === 0 ? (
              <div className="quota-empty">No partitions match this filter.</div>
            ) : (
              <div className="overflow-x-auto">
                <table className="quota-table">
                  <thead>
                    <tr>
                      <th>Partition</th>
                      <th>Mode</th>
                      <th>Ingest / Query / Rules</th>
                      <th>Usage (per replica)</th>
                    </tr>
                  </thead>
                  <tbody>
                    {filteredPartitions.map((partition) => {
                      const queryUsage = partition.usage;
                      const ingestUsage = data.ingest?.usage;
                      const queryRate = findUsage(queryUsage, ":query:requests");
                      const concurrency = findUsage(queryUsage, ":query:concurrent");
                      const ingestBytes = maxIngestUsage(
                        queryUsage,
                        ingestUsage,
                        partition.name,
                        ".bytes",
                      );
                      const ingestRecords = maxIngestUsage(
                        queryUsage,
                        ingestUsage,
                        partition.name,
                        ".records",
                      );
                      const storageUsage = [...(queryUsage ?? []), ...(ingestUsage ?? [])].find(
                        (entry) => entry.key === `partition:${partition.name}:storage:bytes`,
                      );
                      return (
                        <tr key={partition.name}>
                          <td>
                            <div className="quota-name">{partition.name}</div>
                            <div className="quota-mono" style={{ fontSize: "0.66rem" }}>
                              {partition.profile ? `profile: ${partition.profile}` : "defaults"}
                            </div>
                          </td>
                          <td>
                            <EnforcementBadge partition={partition} />
                          </td>
                          <td>
                            <LimitGrid quota={partition.quota} />
                          </td>
                          <td>
                            <div className="quota-usage">
                              <div className="quota-usage__label">query rps</div>
                              <UsageBar usage={queryRate} />
                              <div className="quota-usage__label">concurrent</div>
                              <UsageBar usage={concurrency} />
                              <div className="quota-usage__label">ingest bytes</div>
                              <UsageBar usage={ingestBytes} />
                              <div className="quota-usage__label">ingest records</div>
                              <UsageBar usage={ingestRecords} />
                              <div className="quota-usage__label">storage</div>
                              <UsageBar usage={storageUsage} />
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
                {data.truncated && (
                  <div className="quota-empty">
                    Showing the first partitions only. Filter by partition to narrow the view.
                  </div>
                )}
              </>
            )}
          </div>

          {data.ingest?.configured && data.ingest.error && (
            <div className="quota-error">
              <AlertTriangle size={13} className="inline mr-1" />
              Ingest usage unavailable: {data.ingest.error}
            </div>
          )}
        </>
      )}
    </div>
  );
}
