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
}

export type ObservabilityConfig = AssetObservabilityConfig;

export interface HelperFileManifest {
  version: 1;
  description?: string;
  partitions?: Record<string, ObservabilityConfig>;
  intents?: Record<string, ObservabilityConfig>;
  assets: Record<string, AssetObservabilityConfig>;
}
