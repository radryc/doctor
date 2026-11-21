import type { Matcher } from "./alerts";

export type IncidentState = "open" | "acknowledged" | "mitigated" | "resolved" | string;

export interface TimelineEntry {
  seq: number;
  at: string;
  kind: string;
  actor?: string;
  message?: string;
}

export interface MemberAlert {
  fingerprint: string;
  alertname: string;
  source?: string;
  severity?: string;
  status: string;
  startsAt?: string;
  labels?: Record<string, string>;
}

export interface Incident {
  id: string;
  tenant: string;
  partition: string;
  set: string;
  groupKey: string;
  groupLabels?: Record<string, string>;
  title: string;
  severity: string;
  state: IncidentState;
  owner?: string;
  openedAt: string;
  acknowledgedAt?: string | null;
  mitigatedAt?: string | null;
  resolvedAt?: string | null;
  mitigatedBy?: string;
  mitigation?: string;
  resolvedBy?: string;
  resolution?: string;
  autoMitigated?: boolean;
  autoResolved?: boolean;
  reopenCount?: number;
  memberFingerprints?: string[];
  memberAlerts?: MemberAlert[];
  membersClearAt?: string | null;
  mitigateDueAt?: string | null;
  resolveDueAt?: string | null;
  nextUpdateAt?: string | null;
  timeline?: TimelineEntry[];
  seq: number;
  createdAt: string;
  updatedAt: string;
}

export interface IncidentListResponse {
  incidents: Incident[];
  returned: number;
}

export interface AlertSet {
  tenant: string;
  partition: string;
  name: string;
  description?: string;
  enabled?: boolean;
  severityFloor?: string;
  match: Matcher[];
  exclude?: Matcher[];
  respectSilences?: boolean;
  respectAcks?: boolean;
  groupBy?: string[];
  notify?: {
    receivers?: string[];
    updateInterval?: string;
    reminders?: boolean;
    updateWhileAcked?: boolean;
  };
  postmortem?: {
    engine?: string;
    options?: Record<string, string>;
    autoPublish?: boolean;
    analysisProvider?: string;
    analysisTimeout?: string;
  };
  createdAt?: string;
  updatedAt?: string;
}

export interface AlertSetListResponse {
  sets: AlertSet[];
  returned: number;
}

export interface IncidentPolicy {
  version?: number;
  mitigateDelay?: string;
  autoMitigate?: boolean;
  autoResolve?: boolean;
  autoResolveDelay?: string;
  resumeWindow?: string;
  reopenWindow?: string;
  maxUpdatesPerHour?: number;
  analysisProvider?: string;
  analysisTimeout?: string;
  postmortemEngine?: string;
  autoPublish?: boolean;
}

export interface EvidenceRef {
  kind: string;
  query?: string;
  summary?: string;
  source?: string;
}

export interface ActionItem {
  title: string;
  category?: string;
  rationale?: string;
  target?: string;
  confidence?: number;
}

export interface Analysis {
  summary: string;
  impact?: string;
  rootCause?: string;
  contributingFactors?: string[];
  actionItems?: ActionItem[];
  evidence?: EvidenceRef[];
  confidence?: number;
  provider?: string;
  model?: string;
  generatedAt?: string;
  error?: string;
}

export interface Postmortem {
  incidentId: string;
  tenant: string;
  partition: string;
  revision: number;
  status: string;
  engine: string;
  engineOptions?: Record<string, string>;
  analysisProvider?: string;
  markdown?: string;
  analysis?: Analysis;
  url?: string;
  externalId?: string;
  error?: string;
  attempts?: number;
  createdAt: string;
  updatedAt: string;
}

export interface EngineMetadata {
  name: string;
  label: string;
  description?: string;
  fields?: Array<{
    key: string;
    label: string;
    type: string;
    required?: boolean;
    secret?: boolean;
    placeholder?: string;
  }>;
}

export interface EngineListResponse {
  engines: EngineMetadata[];
}
