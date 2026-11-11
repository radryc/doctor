export interface Alert {
  id: string;
  fingerprint: string;
  scope?: string;
  source: string;
  tenant: string;
  partition: string;
  sourcePartition?: string;
  audience?: {
    type: string;
    partitions?: string[];
    groups?: string[];
  };
  dedupeKey?: string;
  expiresAt?: string | null;
  status: "pending" | "firing" | "resolved" | "no_data" | "error" | string;
  severity: string;
  labels: Record<string, string>;
  annotations?: Record<string, string>;
  startsAt: string;
  endsAt?: string | null;
  value?: string;
  delivery: "sentinel" | "native" | string;
  notify?: string[];
  acked?: boolean;
  ackedBy?: string;
  ackedAt?: string | null;
  silenceIds?: string[];
  lastNotifiedAt?: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface Matcher {
  name: string;
  operator: "=" | "!=" | "=~" | "!~" | string;
  value: string;
}

export interface Silence {
  id: string;
  tenant: string;
  partition: string;
  matchers: Matcher[];
  startsAt: string;
  endsAt: string;
  scope?: string;
  createdBy?: string;
  comment?: string;
  createdAt: string;
  updatedAt: string;
}

export interface Capabilities {
  tenant: string;
  partition: string;
  user: string;
  capabilities: string[];
}

export interface AlertListResponse {
  alerts: Alert[];
  returned: number;
}

export interface SilenceListResponse {
  silences: Silence[];
}

export interface AWSPolicyRule {
  account?: string;
  region?: string;
  nameRegex?: string;
  tag?: string;
  equals?: string;
  alarmType?: string;
  namespace?: string;
  metric?: string;
}

export interface AWSPolicy {
  source?: string;
  default: "managed-only" | "import-all" | "ignore-all" | string;
  include?: AWSPolicyRule[];
  exclude?: AWSPolicyRule[];
  unscoped?: string;
}

export interface GCPPolicyRule {
  nameRegex?: string;
  severity?: string;
  label?: string;
  equals?: string;
}

export interface GCPPolicy {
  default: "import-all" | "ignore-all" | string;
  include?: GCPPolicyRule[];
  exclude?: GCPPolicyRule[];
}

export interface GuardianAlertPolicy {
  for?: string;
}

export interface GuardianPolicy {
  version?: number;
  tenant?: string;
  partition?: string;
  alertOnLocked?: boolean;
  alerts?: Record<string, GuardianAlertPolicy>;
}

export interface SourceStatus {
  name: string;
  mode: string;
  status: string;
}

export interface SourceListResponse {
  sources: SourceStatus[];
}

export interface DeliveryRecord {
  id: string;
  alertId: string;
  fingerprint: string;
  tenant: string;
  partition: string;
  channel: string;
  status: string;
  attempts: number;
  nextAttempt?: string;
  lastError?: string;
  createdAt: string;
  updatedAt: string;
}

export interface DeliveryResponse {
  delivery: DeliveryRecord[];
  returned: number;
}

export interface Receiver {
  name: string;
  type: string;
  description?: string;
  url?: string;
  secretEnv?: string;
  headers?: Record<string, string>;
  timeout?: string;
}

export interface Route {
  name: string;
  tenant?: string;
  partition?: string;
  matchers?: Record<string, string>;
  match?: Matcher[];
  receivers: string[];
  repeatInterval?: string;
  continue?: boolean;
  default?: boolean;
}

/**
 * AlertRoute assigns an explicit destination to a source-generated alert. It
 * matches the alert's source and alertname labels, and sits between a rule's
 * explicit destination and the partition destination.
 */
export interface AlertRoute {
  source: string;
  alertname: string;
  receivers: string[];
}

export interface ReceiverConfig {
  receivers: Receiver[];
  routes: Route[];
  alertRoutes?: AlertRoute[];
  partitionReceiver?: string;
  defaultReceiver?: string;
}

export type SignalType =
  | "errors"
  | "latency"
  | "traffic"
  | "saturation"
  | "availability";

export type RuleMode = "guided" | "promql";

export interface SignalQuery {
  metric: string;
  filters?: Matcher[];
  function?: string;
  reduce?: string;
  quantile?: number;
  by?: string[];
}

export interface AlertCondition {
  op: "gt" | "gte" | "lt" | "lte" | "eq" | "neq" | "absent" | string;
  value: number;
  unit?: string;
}

export interface AlertSpec {
  signal: SignalType | string;
  primary: SignalQuery;
  denominator?: SignalQuery | null;
  condition: AlertCondition;
  window?: string;
  for?: string;
  keepFiringFor?: string;
  severity?: string;
  summary?: string;
  runbookUrl?: string;
  destinations?: string[];
}

export interface AlertRuleSummary {
  alert: string;
  mode?: RuleMode | string;
  signal?: string;
  severity?: string;
  notify?: string[];
  summary?: string;
}

export interface AlertRuleGroupSummary {
  tenant: string;
  partition: string;
  group: string;
  interval: string;
  rules: number;
  alerts?: AlertRuleSummary[];
}

export interface AlertRuleGroupsResponse {
  groups: AlertRuleGroupSummary[];
}

export interface AlertRule {
  mode?: RuleMode | string;
  alert: string;
  spec?: AlertSpec | null;
  expr: string;
  for?: string;
  keep_firing_for?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  notify?: string[];
}

export interface AlertRuleGroup {
  name: string;
  tenant?: string;
  partition?: string;
  interval?: string;
  rules: AlertRule[];
}

export interface AlertRuleFile {
  version?: number;
  groups: AlertRuleGroup[];
}

export interface AlertPreviewInstance {
  labels: Record<string, string>;
  value?: string;
}

export interface AlertPreviewResponse {
  expr: string;
  data?: unknown;
  partial?: boolean;
  instances?: AlertPreviewInstance[];
  wouldFire?: boolean;
}

export interface RoutingOverviewPartition {
  tenant: string;
  partition: string;
  receivers: number;
  routes: number;
  partitionReceiver?: string;
  defaultReceiver?: string;
  receiverNames?: string[];
}

export interface RoutingOverviewResponse {
  partitions: RoutingOverviewPartition[];
}

export interface PartitionAlertSummary {
  tenant: string;
  partition: string;
  firing: number;
  pending: number;
  critical: number;
  total: number;
}

export interface AlertSummaryResponse {
  partitions: PartitionAlertSummary[];
}

export interface SubspaceRequest {
  tenant?: string;
  source?: string;
  sourcePartition?: string;
  severity?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  audience: {
    type: "tenant-admins" | "partition-owners" | "partitions" | "groups" | "all" | string;
    partitions?: string[];
    groups?: string[];
  };
  dedupeKey: string;
  startsAt?: string;
  expiresAt?: string;
}
