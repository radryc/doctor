export type WidgetKind =
  | "timeseries"
  | "stat"
  | "toplist"
  | "gauge"
  | "table"
  | "logs"
  | "traces"
  | "topology"
  | "markdown";

export type Datasource =
  | "prometheus"
  | "loki"
  | "tempo"
  | "native_logs"
  | "native_traces";

export type VariableType =
  | "label_values"
  | "custom"
  | "text"
  | "partition"
  | "service";

export interface DashboardVariable {
  name: string;
  label: string;
  type: VariableType;
  labelKey?: string;
  metricFilter?: string;
  options?: string[];
  defaultValue: string;
  multi?: boolean;
  includeAll?: boolean;
}

export interface GridPos {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface ThresholdLevel {
  value: number;
  color: "green" | "amber" | "red";
}

export interface WidgetVisualization {
  displayMode?: "line" | "area" | "bars";
  stacked?: boolean;
  showExemplars?: boolean;
  unit?: "none" | "short" | "percent" | "seconds" | "ms" | "bytes" | "bps" | "reqps";
  decimals?: number;
  thresholds?: ThresholdLevel[];
  markdownContent?: string;
}

export interface WidgetQuery {
  refId: string;
  datasource: Datasource;
  expr: string;
  legendFormat?: string;
  disabled?: boolean;
}

export interface DashboardWidget {
  id: string;
  title: string;
  description?: string;
  kind: WidgetKind;
  gridPos: GridPos;
  queries: WidgetQuery[];
  visualization?: WidgetVisualization;
}

export interface DashboardMeta {
  versionId?: string;
  updatedAt?: string;
  updatedBy?: string;
}

export interface DashboardDocument {
  version: "doctor.dashboard/v1";
  id: string;
  title: string;
  description?: string;
  tenant: string;
  tags?: string[];
  builtIn?: boolean;
  defaultTimeRange?: string;
  refreshInterval?: string;
  variables?: DashboardVariable[];
  layout?: DashboardWidget[];
  meta?: DashboardMeta;
}

export interface DashboardBundle {
  version: "doctor.dashboard.bundle/v1";
  exportedAt: string;
  dashboards: DashboardDocument[];
}

export interface DashboardVersion {
  versionId: string;
  batchRevisionId?: string;
  contentSha256?: string;
  committedAt: string;
  principalId?: string;
  reason?: string;
}

// Resolved variable values (name -> current value)
export type VariableValues = Record<string, string>;
