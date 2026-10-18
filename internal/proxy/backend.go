// Package proxy contains the backend model, upstream adapters, and result
// merging used to present many observability sources as one dataspace.
//
// This package deliberately has no dependency on internal/query: query handlers
// construct upstreams and call the merge helpers here, and the full registry and
// fan-out router (phase P1) build on the same primitives.
package proxy

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Signal identifies one telemetry signal.
type Signal string

const (
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
	SignalTraces  Signal = "traces"
)

// Mode describes how a backend answers queries.
type Mode string

const (
	// ModeProxy forwards the query language to the backend unchanged.
	ModeProxy Mode = "proxy"
	// ModeStore fetches raw records and lets Doctor evaluate the query locally.
	ModeStore Mode = "store"
)

// Backend kinds supported by the adapter registry.
const (
	KindPrometheus    = "prometheus"
	KindLoki          = "loki"
	KindTempo         = "tempo"
	KindElasticsearch = "elasticsearch"
	KindBigQuery      = "bigquery"
	KindMonoFS        = "monofs"
	KindAWS           = "aws"
)

// Label names injected by the proxy.
const (
	// LabelSource carries provenance ("doctor", "aws", "recording", ...).
	LabelSource = "source"
	// LabelShard disambiguates records that would otherwise collide across
	// backends. It is only added when a collision is detected.
	LabelShard = "shard"
)

// Well-known source values.
const (
	SourceDoctor    = "doctor"
	SourceAWS       = "aws"
	SourceRecording = "recording"
)

// QueryPolicy controls when a backend participates in a query.
type QueryPolicy string

const (
	// PolicyAlways queries the backend on every request (default for proxy
	// backends). Results are merged with the local dataspace.
	PolicyAlways QueryPolicy = "always"
	// PolicyFallback queries the backend only when the local result is empty,
	// or when the query explicitly selects the backend's source. This preserves
	// the legacy AWS federation behavior.
	PolicyFallback QueryPolicy = "fallback"
)

var backendNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Backend describes one configured upstream source.
type Backend struct {
	Name       string // stable id; becomes the shard label
	Kind       string // prometheus|loki|tempo|elasticsearch|bigquery|monofs|aws
	BaseURL    string
	Token      string
	Tenant     string
	Labels     map[string]string // static labels stamped on every record
	Signals    []Signal
	OwnersPath string        // MonoFS path whose .guardian/OWNERS governs access
	Public     bool          // metrics are public by default
	Timeout    time.Duration // per-request timeout; 0 means the router default
	Priority   int           // merge order (lower first); ties broken by Name
	Modes      map[Signal]Mode
	Policy     QueryPolicy // always (default) or fallback
	Origin     string      // "config" | "guardian-pusher" | "guardian-partition"

	// Pusher metadata surfaced by /v1/proxy/endpoints.
	Provider      string
	Version       string
	Cluster       string
	Account       string
	Region        string
	Capabilities  []string
	Status        string
	LastHeartbeat time.Time

	// Kind-specific configuration.
	Index   string            // elasticsearch index pattern
	Schema  map[string]string // field mapping overrides (elasticsearch/bigquery)
	Project string            // bigquery project
	Dataset string            // bigquery dataset
	Table   string            // bigquery table (logs)
}

// Validate reports whether the backend is well-formed.
func (b Backend) Validate() error {
	if !backendNamePattern.MatchString(b.Name) {
		return fmt.Errorf("proxy: backend name %q must match %s", b.Name, backendNamePattern)
	}
	if strings.TrimSpace(b.Kind) == "" {
		return fmt.Errorf("proxy: backend %q has no kind", b.Name)
	}
	if len(b.Signals) == 0 {
		return fmt.Errorf("proxy: backend %q declares no signals", b.Name)
	}
	for _, signal := range b.Signals {
		if !signal.Valid() {
			return fmt.Errorf("proxy: backend %q has invalid signal %q", b.Name, signal)
		}
		if mode := b.Mode(signal); mode != ModeProxy && mode != ModeStore {
			return fmt.Errorf("proxy: backend %q has invalid mode %q for signal %q", b.Name, mode, signal)
		}
	}
	switch b.QueryPolicy() {
	case PolicyAlways, PolicyFallback:
	default:
		return fmt.Errorf("proxy: backend %q has invalid query policy %q", b.Name, b.Policy)
	}
	switch b.Kind {
	case KindPrometheus, KindLoki, KindTempo, KindElasticsearch:
		baseURL := strings.TrimSpace(b.BaseURL)
		if baseURL == "" {
			return fmt.Errorf("proxy: backend %q kind %q requires a baseUrl", b.Name, b.Kind)
		}
		if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
			return fmt.Errorf("proxy: backend %q baseUrl %q must be http(s)", b.Name, baseURL)
		}
	}
	return nil
}

// QueryPolicy returns the configured query policy, defaulting to PolicyAlways.
func (b Backend) QueryPolicy() QueryPolicy {
	if b.Policy == "" {
		return PolicyAlways
	}
	return b.Policy
}

// Valid reports whether the signal is recognized.
func (s Signal) Valid() bool {
	switch s {
	case SignalMetrics, SignalLogs, SignalTraces:
		return true
	default:
		return false
	}
}

// Supports reports whether the backend serves the signal.
func (b Backend) Supports(signal Signal) bool {
	for _, candidate := range b.Signals {
		if candidate == signal {
			return true
		}
	}
	return false
}

// Mode returns the configured mode for a signal, defaulting to ModeProxy.
func (b Backend) Mode(signal Signal) Mode {
	if b.Modes != nil {
		if mode, ok := b.Modes[signal]; ok {
			return mode
		}
	}
	return ModeProxy
}

// SortBackends orders backends deterministically: priority first, then name.
func SortBackends(backends []Backend) {
	// Insertion sort keeps the helper dependency-free and stable for the small
	// slices it is used with; the registry sorts larger slices on load.
	for i := 1; i < len(backends); i++ {
		for j := i; j > 0 && backendLess(backends[j], backends[j-1]); j-- {
			backends[j], backends[j-1] = backends[j-1], backends[j]
		}
	}
}

func backendLess(a, b Backend) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.Name < b.Name
}
