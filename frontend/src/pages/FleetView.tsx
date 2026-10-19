import { AlertTriangle, ArrowRight, CheckCircle, CircleDot } from "lucide-react";

export type HealthStatus = "healthy" | "degraded" | "critical";

export interface AssetGroup {
  id: string;
  name: string;
  status: HealthStatus;
  partition: string;
  intent: string;
}

export interface PartitionSummary {
  health: string;
  status?: string;
  assetCount: number;
  intentCount: number;
  healthyAssets: number;
  attentionAssets: number;
  failingAssets: number;
}

export interface PartitionModel {
  id: string;
  region: string;
  intents: {
    name: string;
    assets: AssetGroup[];
  }[];
  summary: PartitionSummary;
}

export type SortKey = "name" | "health" | "assets";
export type SortOrder = "asc" | "desc";

type Tone = "healthy" | "attention" | "critical" | "unknown";

interface FleetViewProps {
  partitions: PartitionModel[];
  viewMode: "compact" | "detailed";
  onSelectPartition: (partition: PartitionModel) => void;
  onSelectHub: (asset: AssetGroup) => void;
}

function toneFor(health: string | undefined): Tone {
  const value = (health || "").toLowerCase();
  if (value.includes("fail") || value.includes("crit") || value.includes("error") || value.includes("unhealthy")) {
    return "critical";
  }
  if (
    value.includes("attention") ||
    value.includes("degrad") ||
    value.includes("drift") ||
    value.includes("pending")
  ) {
    return "attention";
  }
  if (value.includes("healthy") || value.includes("locked") || value.includes("stable") || value.includes("ok")) {
    return "healthy";
  }
  return "unknown";
}

function toneColor(tone: Tone): string {
  if (tone === "healthy") return "var(--ui-green)";
  if (tone === "attention") return "var(--ui-amber)";
  if (tone === "critical") return "var(--ui-red)";
  return "var(--ui-faint)";
}

function toneLabel(tone: Tone): string {
  if (tone === "critical") return "Failing";
  if (tone === "attention") return "Attention";
  if (tone === "healthy") return "Healthy";
  return "Unknown";
}

function statusTone(status: HealthStatus): Tone {
  if (status === "critical") return "critical";
  if (status === "degraded") return "attention";
  return "healthy";
}

function assetTone(partition: PartitionModel): Tone {
  if (partition.summary.failingAssets > 0) return "critical";
  if (partition.summary.attentionAssets > 0) return "attention";
  const declared = toneFor(partition.summary.health);
  return declared === "unknown" ? "healthy" : declared;
}

function healthPercent(partition: PartitionModel): number {
  const total = partition.summary.assetCount;
  if (total <= 0) return 0;
  return Math.round((partition.summary.healthyAssets / total) * 100);
}

export default function FleetView({
  partitions,
  viewMode,
  onSelectPartition,
  onSelectHub,
}: FleetViewProps) {
  if (partitions.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-dark-600 bg-dark-800/40 px-6 py-16 text-center">
        <p className="text-sm font-medium text-gray-300">No partitions match the current filters</p>
        <p className="mt-1 text-xs text-gray-500">Clear the search or reload the fleet view.</p>
      </div>
    );
  }

  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-2 2xl:grid-cols-3">
      {partitions.map((partition) => {
        const tone = assetTone(partition);
        const color = toneColor(tone);
        const total = Math.max(partition.summary.assetCount, 1);
        const pct = healthPercent(partition);
        const hasTopology = partition.intents.some((intent) => intent.assets.length > 0);

        return (
          <section
            key={partition.id}
            className="group flex flex-col rounded-xl border border-dark-600 bg-dark-800 p-4 transition-colors hover:border-accent-blue/40"
          >
            <button
              type="button"
              className="flex w-full items-start gap-3 text-left"
              onClick={() => onSelectPartition(partition)}
              title={`Open ${partition.region} partition`}
            >
              <span
                className="mt-0.5 inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-lg border"
                style={{
                  color,
                  borderColor: `color-mix(in oklab, ${color} 35%, transparent)`,
                  backgroundColor: `color-mix(in oklab, ${color} 12%, transparent)`,
                }}
              >
                {tone === "healthy" ? <CheckCircle size={17} /> : tone === "unknown" ? <CircleDot size={17} /> : <AlertTriangle size={17} />}
              </span>
              <span className="min-w-0 flex-1">
                <span className="flex flex-wrap items-center gap-2">
                  <span className="truncate text-base font-semibold text-gray-100">{partition.region}</span>
                  <span
                    className="rounded-full border px-2 py-0.5 text-[10px] font-bold uppercase tracking-wide"
                    style={{
                      color,
                      borderColor: `color-mix(in oklab, ${color} 35%, transparent)`,
                      backgroundColor: `color-mix(in oklab, ${color} 10%, transparent)`,
                    }}
                  >
                    {toneLabel(tone)}
                  </span>
                </span>
                <span className="mt-1 block text-xs text-gray-500">
                  {partition.summary.intentCount} intent{partition.summary.intentCount === 1 ? "" : "s"} ·{" "}
                  {partition.summary.assetCount} asset{partition.summary.assetCount === 1 ? "" : "s"} · {pct}% healthy
                </span>
              </span>
              <ArrowRight size={15} className="mt-1 shrink-0 text-gray-600 transition-transform group-hover:translate-x-0.5 group-hover:text-accent-blue" />
            </button>

            <div className="mt-4 flex h-2 overflow-hidden rounded-full bg-dark-900" title={`${pct}% healthy`}>
              {partition.summary.healthyAssets > 0 && (
                <span
                  className="h-full"
                  style={{
                    width: `${(partition.summary.healthyAssets / total) * 100}%`,
                    backgroundColor: "color-mix(in oklab, var(--ui-green) 75%, transparent)",
                  }}
                />
              )}
              {partition.summary.attentionAssets > 0 && (
                <span
                  className="h-full"
                  style={{
                    width: `${(partition.summary.attentionAssets / total) * 100}%`,
                    backgroundColor: "color-mix(in oklab, var(--ui-amber) 80%, transparent)",
                  }}
                />
              )}
              {partition.summary.failingAssets > 0 && (
                <span
                  className="h-full"
                  style={{
                    width: `${(partition.summary.failingAssets / total) * 100}%`,
                    backgroundColor: "var(--ui-red)",
                  }}
                />
              )}
            </div>

            <div className="mt-3 grid grid-cols-3 gap-2 text-center">
              <div className="rounded-lg border border-dark-600 bg-dark-900/60 px-2 py-1.5">
                <div className="text-sm font-bold tabular-nums text-emerald-300">{partition.summary.healthyAssets}</div>
                <div className="text-[10px] uppercase tracking-wide text-gray-600">Healthy</div>
              </div>
              <div className="rounded-lg border border-dark-600 bg-dark-900/60 px-2 py-1.5">
                <div className="text-sm font-bold tabular-nums text-amber-300">{partition.summary.attentionAssets}</div>
                <div className="text-[10px] uppercase tracking-wide text-gray-600">Attention</div>
              </div>
              <div className="rounded-lg border border-dark-600 bg-dark-900/60 px-2 py-1.5">
                <div className="text-sm font-bold tabular-nums text-rose-300">{partition.summary.failingAssets}</div>
                <div className="text-[10px] uppercase tracking-wide text-gray-600">Failing</div>
              </div>
            </div>

            {viewMode === "detailed" && (
              <div className="mt-3 border-t border-dark-600 pt-3">
                {hasTopology ? (
                  <div className="space-y-2">
                    {partition.intents.map((intent) => {
                      const intentCritical = intent.assets.filter((asset) => asset.status === "critical").length;
                      const intentDegraded = intent.assets.filter((asset) => asset.status === "degraded").length;
                      const intentTone: Tone =
                        intentCritical > 0 ? "critical" : intentDegraded > 0 ? "attention" : "healthy";
                      return (
                        <div key={`${partition.id}-${intent.name}`} className="flex items-center gap-2">
                          <span
                            className="h-1.5 w-1.5 shrink-0 rounded-full"
                            style={{ backgroundColor: toneColor(intentTone) }}
                          />
                          <span className="min-w-0 flex-1 truncate text-xs text-gray-400" title={intent.name}>
                            {intent.name}
                          </span>
                          <span className="shrink-0 text-[10px] tabular-nums text-gray-600">
                            {intent.assets.length}
                          </span>
                          <span className="flex shrink-0 items-center gap-1 border-l border-dark-600 pl-2">
                            {intent.assets.slice(0, 12).map((asset) => (
                              <button
                                key={asset.id}
                                type="button"
                                className="h-2 w-2 rounded-full transition-transform hover:scale-150"
                                style={{ backgroundColor: toneColor(statusTone(asset.status)) }}
                                title={`${asset.name} · ${asset.status}`}
                                aria-label={`Inspect ${asset.name}`}
                                onClick={(event) => {
                                  event.stopPropagation();
                                  onSelectHub(asset);
                                }}
                              />
                            ))}
                            {intent.assets.length > 12 && (
                              <span className="text-[9px] text-gray-600">+{intent.assets.length - 12}</span>
                            )}
                          </span>
                        </div>
                      );
                    })}
                  </div>
                ) : (
                  <p className="text-[11px] text-gray-600">
                    Topology details unavailable — health counts come from Guardian.
                  </p>
                )}
              </div>
            )}
          </section>
        );
      })}
    </div>
  );
}
