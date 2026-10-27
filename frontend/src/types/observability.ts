export interface AssetHealthConfig {
  metric: string;
  mode?: "errors" | "latency" | "saturation" | "availability";
  /** Healthy at or below this value (at or above for availability). */
  healthy?: number;
  /** Warning at or below this value (at or above for availability). */
  warning?: number;
  unit?: string;
}

export interface AssetObservabilityConfig {
  metrics: string[];
  logs: {
    query: string;
    level?: string;
  }[];
  traces: {
    service: string;
    operation?: string;
    thresholdMs?: number;
  }[];
  /** Optional override for the automatic health metric/rule. */
  health?: AssetHealthConfig;
}

export type ObservabilityConfig = AssetObservabilityConfig;

export interface HelperFileManifest {
  version: 1;
  description?: string;
  partitions?: Record<string, ObservabilityConfig>;
  intents?: Record<string, ObservabilityConfig>;
  assets: Record<string, AssetObservabilityConfig>;
}
