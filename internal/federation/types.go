// Package federation implements multi-account AWS telemetry federation for
// Doctor. It owns the read-optimized target catalog, query planning, bounded
// fan-out, and result merging for CloudWatch/X-Ray pushers.
package federation

import (
	"context"
	"fmt"
	"time"

	"github.com/radryc/doctor/internal/proxy"
)

// Signal is one telemetry signal exposed by a federation target.
type Signal string

const (
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
	SignalTraces  Signal = "traces"
)

// Health is the last known state of a target. Heartbeats are maintained by
// Guardian; the catalog only carries a snapshot used for ranking/UI.
type Health struct {
	State         string    `json:"state,omitempty" yaml:"state,omitempty"`
	LastHeartbeat time.Time `json:"last_heartbeat,omitempty" yaml:"last_heartbeat,omitempty"`
	LastError     string    `json:"last_error,omitempty" yaml:"last_error,omitempty"`
	ErrorCount    int       `json:"error_count,omitempty" yaml:"error_count,omitempty"`
}

// Target is one AWS account/region pusher. A target may serve several Guardian
// partitions; partitions are labels/indexes, not target identity.
type Target struct {
	ID         string   `json:"id" yaml:"id"`
	Account    string   `json:"account" yaml:"account"`
	Region     string   `json:"region" yaml:"region"`
	Partitions []string `json:"partitions,omitempty" yaml:"partitions,omitempty"`
	// ServicePartitions maps an X-Ray service/function name to a Guardian
	// partition. Use it when an account hosts more than one partition and the
	// AWS resource does not carry a guardian.io/partition tag.
	ServicePartitions map[string]string `json:"service_partitions,omitempty" yaml:"service_partitions,omitempty"`
	Tenant            string            `json:"tenant,omitempty" yaml:"tenant,omitempty"`
	Endpoint          string            `json:"endpoint" yaml:"endpoint"`
	TokenEnv          string            `json:"token_env,omitempty" yaml:"token_env,omitempty"`
	TokenRef          string            `json:"token_ref,omitempty" yaml:"token_ref,omitempty"`
	Token             string            `json:"-" yaml:"-"` // resolved; never serialized
	Provider          string            `json:"provider,omitempty" yaml:"provider,omitempty"`
	Capabilities      []Signal          `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Labels            map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Health            Health            `json:"health,omitempty" yaml:"health,omitempty"`
	Version           string            `json:"version,omitempty" yaml:"version,omitempty"`
	Priority          int               `json:"priority,omitempty" yaml:"priority,omitempty"`

	// MetadataFetchedAt records when /v1/aws/meta was last refreshed. A fresh
	// timestamp lets query replicas use cached empty discovery results instead
	// of re-querying every pusher on each request.
	MetadataFetchedAt time.Time `json:"metadata_fetched_at,omitempty" yaml:"metadata_fetched_at,omitempty"`

	// Cached discovery metadata. These are derived from /v1/aws/meta and are
	// optional in a static catalog.
	MetricNames  []string `json:"metric_names,omitempty" yaml:"metric_names,omitempty"`
	LogGroups    []string `json:"log_groups,omitempty" yaml:"log_groups,omitempty"`
	XrayServices []string `json:"xray_services,omitempty" yaml:"xray_services,omitempty"`
}

// Snapshot is the serializable catalog document.
type Snapshot struct {
	Version     string    `json:"version,omitempty" yaml:"version,omitempty"`
	GeneratedAt time.Time `json:"generated_at,omitempty" yaml:"generated_at,omitempty"`
	Targets     []Target  `json:"targets" yaml:"targets"`
}

// Predicates are the scope constraints extracted from a query.
type Predicates struct {
	Accounts    []string
	AccountRE   []string
	Regions     []string
	RegionRE    []string
	Partitions  []string
	PartitionRE []string
	LogGroups   []string
	LogGroupRE  []string
	Services    []string
	ServiceRE   []string
	Metrics     []string
	MetricRE    []string
	TraceID     string
	From        time.Time
	To          time.Time
	Limit       int
}

// Query is a normalized federation request.
type Query struct {
	Signal    Signal
	Expr      string
	LogGroups []string
	Service   string
	TraceID   string
	Partition string
	Account   string
	Region    string
	From      time.Time
	To        time.Time
	Limit     int
}

// PlanMode describes how a query should be served.
type PlanMode string

const (
	PlanPull    PlanMode = "pull"
	PlanCentral PlanMode = "central"
	PlanReject  PlanMode = "reject"
)

// Plan is the result of target resolution.
type Plan struct {
	Mode     PlanMode
	Targets  []Target
	Matched  int
	Pruned   int
	Reason   string
	Warnings []string
}

// ScopeChecker is implemented by the federation manager so query handlers can
// reject broad live-AWS requests before entering the router fan-out.
type ScopeChecker interface {
	CheckScope(ctx context.Context, q Query) error
}

// ClientFactory builds a per-target AWS read client. It is injectable for
// tests; production uses awsstore.NewClient.
type ClientFactory func(Target) (proxy.AWSClient, error)

// ErrScopeRequired is returned when a live AWS query has no account, region,
// partition, log group, service, metric, or trace-ID scope.
var ErrScopeRequired = proxy.ErrScopeRequired

// ErrTooBroad is returned when a scoped query still matches more than the
// configured maximum number of targets.
var ErrTooBroad = proxy.ErrTooBroad

// ErrNoTargets is returned when no catalog target matches a scoped query.
var ErrNoTargets = proxy.ErrNoTargets

// ScopeError carries a structured reason for HTTP handlers.
type ScopeError struct {
	Err       error
	Matched   int
	MaxFanout int
}

func (e *ScopeError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "scope error"
}

func (e *ScopeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func invalidTarget(id, field string) error {
	return fmt.Errorf("federation target %q: %s is required", id, field)
}
