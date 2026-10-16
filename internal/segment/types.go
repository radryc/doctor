package segment

import (
	"encoding/json"
	"fmt"
	"time"
)

type Signal string

const (
	SignalTraces   Signal = "traces"
	SignalLogs     Signal = "logs"
	SignalMetric   Signal = "metrics"
	SignalGuardian Signal = "guardian"
)

type TraceRecord struct {
	Tenant             string            `json:"tenant"`
	TraceID            string            `json:"trace_id"`
	SpanID             string            `json:"span_id"`
	ParentSpanID       string            `json:"parent_span_id,omitempty"`
	Name               string            `json:"name"`
	Kind               string            `json:"kind"`
	Service            string            `json:"service"`
	Partition          string            `json:"partition,omitempty"`
	Intent             string            `json:"intent,omitempty"`
	Asset              string            `json:"asset,omitempty"`
	StartTime          time.Time         `json:"start_time"`
	EndTime            time.Time         `json:"end_time"`
	StatusCode         string            `json:"status_code,omitempty"`
	StatusMessage      string            `json:"status_message,omitempty"`
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
	Attributes         map[string]string `json:"attributes,omitempty"`
	Events             []TraceEvent      `json:"events,omitempty"`
}

type TraceEvent struct {
	Name       string            `json:"name"`
	Timestamp  time.Time         `json:"timestamp"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type LogRecord struct {
	Tenant             string            `json:"tenant"`
	Service            string            `json:"service"`
	Partition          string            `json:"partition,omitempty"`
	Intent             string            `json:"intent,omitempty"`
	Asset              string            `json:"asset,omitempty"`
	Timestamp          time.Time         `json:"timestamp"`
	SeverityNumber     int32             `json:"severity_number,omitempty"`
	SeverityText       string            `json:"severity_text,omitempty"`
	Body               string            `json:"body"`
	TraceID            string            `json:"trace_id,omitempty"`
	SpanID             string            `json:"span_id,omitempty"`
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
	Attributes         map[string]string `json:"attributes,omitempty"`
}

type MetricPointRecord struct {
	Tenant             string            `json:"tenant"`
	Service            string            `json:"service"`
	Partition          string            `json:"partition,omitempty"`
	Intent             string            `json:"intent,omitempty"`
	Asset              string            `json:"asset,omitempty"`
	TraceID            string            `json:"trace_id,omitempty"`
	SpanID             string            `json:"span_id,omitempty"`
	MetricName         string            `json:"metric_name"`
	Description        string            `json:"description,omitempty"`
	Unit               string            `json:"unit,omitempty"`
	Type               string            `json:"type"`
	Timestamp          time.Time         `json:"timestamp"`
	StartTime          time.Time         `json:"start_time,omitempty"`
	Temporality        string            `json:"temporality,omitempty"`
	Attributes         map[string]string `json:"attributes,omitempty"`
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
	HasValue           bool              `json:"has_value"`
	Value              float64           `json:"value,omitempty"`
	Raw                json.RawMessage   `json:"raw,omitempty"`
}

// MetricSeriesMeta describes one distinct metric series: its identity and the
// first/last sample times. It is served from the storage chunk series index so
// presence aggregations (count by __name__) and series discovery never scan
// sample data.
type MetricSeriesMeta struct {
	Tenant     string            `json:"tenant"`
	Service    string            `json:"service"`
	Partition  string            `json:"partition,omitempty"`
	Intent     string            `json:"intent,omitempty"`
	Asset      string            `json:"asset,omitempty"`
	MetricName string            `json:"metric_name"`
	Labels     map[string]string `json:"labels,omitempty"`
	MinTime    time.Time         `json:"min_time"`
	MaxTime    time.Time         `json:"max_time"`
}

// unboundedMetricLabels are per-record correlation identifiers. They must never
// become metric series labels: every sample would form its own series, which
// explodes cardinality and makes label/series discovery impossible to serve.
// Trace correlation belongs on the record's typed TraceID/SpanID fields (and
// exemplars), not in the series identity.
var unboundedMetricLabels = map[string]struct{}{
	"trace_id":   {},
	"span_id":    {},
	"task_id":    {},
	"log_id":     {},
	"request_id": {},

	// Guardian correlation identifiers are per-execution identifiers. As metric
	// series labels they explode cardinality (one series per task) and make
	// broad queries load hundreds of MB, so they are excluded from series
	// identity. Both the raw dotted form and the normalized form are listed.
	"guardian.task.id":             {},
	"guardian.task.correlation_id": {},
	"guardian.task.correlation.id": {},
	"guardian_task_id":             {},
	"guardian_task_correlation_id": {},
	"guardian_correlation_id":      {},
}

// IsUnboundedMetricLabel reports whether name identifies a per-record value that
// must be excluded from metric series labels.
func IsUnboundedMetricLabel(name string) bool {
	_, ok := unboundedMetricLabels[name]
	return ok
}

type MetricMatchType string

const (
	MetricMatchEqual     MetricMatchType = "equal"
	MetricMatchNotEqual  MetricMatchType = "not_equal"
	MetricMatchRegexp    MetricMatchType = "regexp"
	MetricMatchNotRegexp MetricMatchType = "not_regexp"
)

type MetricLabelMatcher struct {
	Name  string          `json:"name"`
	Value string          `json:"value"`
	Type  MetricMatchType `json:"type"`
}

type MetricQuery struct {
	MetricName    string               `json:"metric_name,omitempty"`
	Service       string               `json:"service,omitempty"`
	LabelMatchers []MetricLabelMatcher `json:"label_matchers,omitempty"`
}

func NewID() string {
	return fmt.Sprintf("%d", time.Now().UTC().UnixNano())
}

// Guardian event kinds emitted by the Guardian control plane.
type GuardianEventKind string

const (
	EventStateTransition GuardianEventKind = "state_transition"
	EventDriftDetected   GuardianEventKind = "drift_detected"
	EventApplySucceeded  GuardianEventKind = "apply_succeeded"
	EventApplyFailed     GuardianEventKind = "apply_failed"
	EventCheckFailed     GuardianEventKind = "check_failed"
	EventRollback        GuardianEventKind = "rollback"
	EventHealthCheck     GuardianEventKind = "health_check"
	EventReconcile       GuardianEventKind = "reconcile"
)

// GuardianEvent captures a single event from the Guardian control plane.
type GuardianEvent struct {
	Tenant        string            `json:"tenant"`
	Partition     string            `json:"partition"`
	Intent        string            `json:"intent"`
	Asset         string            `json:"asset,omitempty"`
	Kind          GuardianEventKind `json:"kind"`
	Timestamp     time.Time         `json:"timestamp"`
	Status        string            `json:"status"`
	PreviousState string            `json:"previous_state,omitempty"`
	TargetPusher  string            `json:"target_pusher,omitempty"`
	TaskID        string            `json:"task_id,omitempty"`
	Revision      string            `json:"revision,omitempty"`
	DriftSummary  string            `json:"drift_summary,omitempty"`
	ErrorMessage  string            `json:"error_message,omitempty"`
	Outputs       map[string]string `json:"outputs,omitempty"`
	Attributes    map[string]string `json:"attributes,omitempty"`
}

// DeploymentNode is a vertex in the deployment topology graph.
type DeploymentNode struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"` // "partition", "intent", "asset"
	Name         string            `json:"name"`
	Partition    string            `json:"partition"`
	Intent       string            `json:"intent,omitempty"`
	Status       string            `json:"status"`
	TargetPusher string            `json:"target_pusher,omitempty"`
	Revision     string            `json:"revision,omitempty"`
	LastEventAt  time.Time         `json:"last_event_at"`
	Health       HealthScore       `json:"health"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

// DeploymentEdge is a directed edge in the deployment topology graph.
type DeploymentEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Relation string `json:"relation"` // "contains", "depends_on", "joins"
}

// DeploymentTopology is the full deployment graph for a partition scope.
type DeploymentTopology struct {
	Tenant    string           `json:"tenant"`
	Partition string           `json:"partition,omitempty"`
	Nodes     []DeploymentNode `json:"nodes"`
	Edges     []DeploymentEdge `json:"edges"`
	BuiltAt   time.Time        `json:"built_at"`
}

// HealthScore represents the computed health of a deployment node.
type HealthScore struct {
	Score   float64   `json:"score"`  // 0.0 (dead) to 1.0 (fully healthy)
	Level   string    `json:"level"`  // "healthy", "degraded", "unhealthy", "unknown"
	Reason  string    `json:"reason"` // human-readable summary
	CheckAt time.Time `json:"check_at"`
}

// HealthFeedback is the response Doctor sends back to Guardian for
// steering deployments, rollbacks, and remediation.
type HealthFeedback struct {
	Tenant         string        `json:"tenant"`
	Partition      string        `json:"partition"`
	Intent         string        `json:"intent"`
	OverallHealth  HealthScore   `json:"overall_health"`
	RecentFailures int           `json:"recent_failures"`
	RecentDrifts   int           `json:"recent_drifts"`
	Recommendation string        `json:"recommendation"` // "proceed", "hold", "rollback"
	Window         time.Duration `json:"-"`
	WindowStr      string        `json:"window"`
	ComputedAt     time.Time     `json:"computed_at"`
}
