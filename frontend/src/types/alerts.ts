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
  url?: string;
  secretEnv?: string;
  headers?: Record<string, string>;
  timeout?: string;
}

export interface Route {
  name: string;
  matchers?: Record<string, string>;
  receivers: string[];
  repeatInterval?: string;
  continue?: boolean;
}

export interface ReceiverConfig {
  receivers: Receiver[];
  routes: Route[];
}

export interface AlertRuleGroupSummary {
  tenant: string;
  partition: string;
  group: string;
  interval: string;
  rules: number;
}

export interface AlertRuleGroupsResponse {
  groups: AlertRuleGroupSummary[];
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
