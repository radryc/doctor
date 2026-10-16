package access

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
	"github.com/rydzu/ainfra/doctor/internal/segment"
)

// PolicyOptions configures the repo-managed access policy gate.
type PolicyOptions struct {
	// URL of the compiled policy bundle (guardiand /api/access/policy).
	URL string
	// File is a local compiled bundle (development/tests). URL wins when both
	// are set.
	File string
	// Token authenticates the policy URL fetch.
	Token string
	// Interval is the refresh period (default 30s).
	Interval time.Duration
	// Enforce turns observe-only decisions into filtering. Metrics are never
	// enforced.
	Enforce bool
	// Logger receives refresh/observe messages.
	Logger *log.Logger
}

// PolicyGate loads the compiled access policy and applies it to telemetry
// reads: logs and traces are filtered per partition, metrics always pass.
// A nil gate means the feature is disabled.
type PolicyGate struct {
	loader  *policy.Loader
	enforce bool
	logger  *log.Logger
}

// NewPolicyGate builds a gate. It returns nil when neither URL nor File is
// configured, which keeps every caller on the pre-policy behavior.
func NewPolicyGate(opts PolicyOptions) *PolicyGate {
	url := strings.TrimSpace(opts.URL)
	file := strings.TrimSpace(opts.File)
	if url == "" && file == "" {
		return nil
	}
	var source policy.Source
	if url != "" {
		source = policy.HTTPSource{URL: url, Token: strings.TrimSpace(opts.Token)}
	} else {
		source = policy.FileSource{Path: file}
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	logf := func(format string, args ...any) { logger.Printf(format, args...) }
	return &PolicyGate{
		loader:  policy.NewLoader(source, interval, logf),
		enforce: opts.Enforce,
		logger:  logger,
	}
}

// Run polls the policy source until ctx is done.
func (g *PolicyGate) Run(ctx context.Context) {
	if g == nil {
		return
	}
	g.loader.Run(ctx)
}

// Refresh performs one fetch (used by tests and callers that want a sync load).
func (g *PolicyGate) Refresh(ctx context.Context) error {
	if g == nil {
		return nil
	}
	return g.loader.Refresh(ctx)
}

// Enabled reports whether the gate is configured.
func (g *PolicyGate) Enabled() bool { return g != nil }

// Ready reports whether a policy has been loaded.
func (g *PolicyGate) Ready() bool { return g != nil && g.loader.Ready() }

// Enforced reports whether filtering is active.
func (g *PolicyGate) Enforced() bool { return g != nil && g.enforce }

// Version returns the loaded policy version ("" when not ready).
func (g *PolicyGate) Version() string {
	if g == nil {
		return ""
	}
	return g.loader.Get().Version()
}

func (g *PolicyGate) policy() *policy.Policy {
	if g == nil {
		return nil
	}
	return g.loader.Get()
}

// IsAdmin reports whether the identity bypasses filtering. The break-glass
// admin subject (from the shared auth verifier) always bypasses, even while no
// policy is loaded.
func (g *PolicyGate) IsAdmin(id authz.Identity) bool {
	if id.Subject == "break-glass-admin" {
		return true
	}
	p := g.policy()
	return p != nil && p.IsAdmin(id)
}

// Allowed is the request-level gate: metrics always pass; disabled and
// observe modes pass; enforce fails closed for anonymous callers and while no
// policy has loaded. Record-level filtering still applies after this.
func (g *PolicyGate) Allowed(id authz.Identity, signal policy.Signal) bool {
	if g == nil || signal == policy.SignalMetrics {
		return true
	}
	if !g.enforce {
		return true
	}
	if g.IsAdmin(id) {
		return true
	}
	if id.IsAnonymous() {
		return false
	}
	return g.Ready()
}

// PartitionVisible decides one record. Under enforcement, records without a
// partition are denied unless the identity is an admin.
func (g *PolicyGate) PartitionVisible(id authz.Identity, partition string, signal policy.Signal) bool {
	if g == nil || signal == policy.SignalMetrics {
		return true
	}
	if !g.enforce {
		return true
	}
	if g.IsAdmin(id) {
		return true
	}
	p := g.policy()
	if p == nil {
		return false
	}
	if strings.TrimSpace(partition) == "" {
		return false
	}
	return p.CanReadSignal(id, partition, signal)
}

// CanonicalPartition maps AWS account/region to a partition name via the
// policy bindings, falling back to the current value. Region-only lookups use
// current when it looks like an AWS region (the pre-binding behavior stored
// the region as guardian_partition).
func (g *PolicyGate) CanonicalPartition(account, region, current string) string {
	p := g.policy()
	if p == nil {
		return current
	}
	account = strings.TrimSpace(account)
	region = strings.TrimSpace(region)
	if account != "" || region != "" {
		if partition, ok := p.PartitionForAWS(account, region); ok {
			return partition
		}
		return current
	}
	if current != "" {
		if partition, ok := p.PartitionForAWS("", current); ok {
			return partition
		}
	}
	return current
}

func (g *PolicyGate) canonicalize(current string, attrs map[string]string) string {
	account, region := "", ""
	if attrs != nil {
		account = attrs["aws_account"]
		region = attrs["aws_region"]
		if current == "" {
			current = attrs["guardian_partition"]
		}
	}
	return g.CanonicalPartition(account, region, current)
}

// FilterLogs applies the policy to log records. Disabled or observe modes
// return the input unchanged; enforcement keeps only visible partitions and
// rewrites AWS records to their bound partition name.
func (g *PolicyGate) FilterLogs(id authz.Identity, records []segment.LogRecord) []segment.LogRecord {
	if g == nil || len(records) == 0 {
		return records
	}
	if g.IsAdmin(id) && g.enforce {
		return records
	}
	if !g.enforce {
		g.observeDrops(id, "logs", len(records), countLogDrops(g, id, records))
		return records
	}
	kept := make([]segment.LogRecord, 0, len(records))
	dropped := 0
	for _, rec := range records {
		canonical := g.canonicalize(rec.Partition, rec.ResourceAttributes)
		if !g.PartitionVisible(id, canonical, policy.SignalLogs) {
			dropped++
			continue
		}
		if canonical != rec.Partition {
			rec = rec
			rec.Partition = canonical
		}
		kept = append(kept, rec)
	}
	g.observeDrops(id, "logs", len(records), dropped)
	return kept
}

// FilterSpans applies the policy to trace spans. A trace stays visible while
// at least one span remains after filtering.
func (g *PolicyGate) FilterSpans(id authz.Identity, spans []segment.TraceRecord) []segment.TraceRecord {
	if g == nil || len(spans) == 0 {
		return spans
	}
	if g.IsAdmin(id) && g.enforce {
		return spans
	}
	if !g.enforce {
		g.observeDrops(id, "traces", len(spans), countSpanDrops(g, id, spans))
		return spans
	}
	kept := make([]segment.TraceRecord, 0, len(spans))
	dropped := 0
	for _, span := range spans {
		canonical := g.canonicalize(span.Partition, span.ResourceAttributes)
		if !g.PartitionVisible(id, canonical, policy.SignalTraces) {
			dropped++
			continue
		}
		if canonical != span.Partition {
			span = span
			span.Partition = canonical
		}
		kept = append(kept, span)
	}
	g.observeDrops(id, "traces", len(spans), dropped)
	return kept
}

func countLogDrops(g *PolicyGate, id authz.Identity, records []segment.LogRecord) int {
	dropped := 0
	for _, rec := range records {
		if !g.PartitionVisible(id, g.canonicalize(rec.Partition, rec.ResourceAttributes), policy.SignalLogs) {
			dropped++
		}
	}
	return dropped
}

func countSpanDrops(g *PolicyGate, id authz.Identity, spans []segment.TraceRecord) int {
	dropped := 0
	for _, span := range spans {
		if !g.PartitionVisible(id, g.canonicalize(span.Partition, span.ResourceAttributes), policy.SignalTraces) {
			dropped++
		}
	}
	return dropped
}

// observeDrops logs observe-mode decisions without changing the payload.
func (g *PolicyGate) observeDrops(id authz.Identity, signal string, total, dropped int) {
	if dropped == 0 {
		return
	}
	if g.enforce {
		g.logger.Printf("access policy: principal=%s signal=%s records=%d filtered=%d version=%s",
			principalString(id), signal, total, dropped, g.Version())
		return
	}
	g.logger.Printf("access policy: observe mode: principal=%s signal=%s records=%d would_filter=%d version=%s",
		principalString(id), signal, total, dropped, g.Version())
}

func principalString(id authz.Identity) string {
	switch {
	case id.ClientID != "":
		return "client:" + id.ClientID
	case id.Subject != "":
		return "user:" + id.Subject
	case id.Email != "":
		return "user:" + id.Email
	}
	return "anonymous"
}

// SignalForPath maps an HTTP path to the telemetry signal it queries. Paths
// that mix signals (service discovery, proxy endpoints) or serve metrics
// return ok=false and are not request-gated; record filtering still applies.
func SignalForPath(path string) (policy.Signal, bool) {
	switch {
	case path == "/v1/logs", strings.HasPrefix(path, "/v1/logs/"),
		strings.HasPrefix(path, "/loki/"):
		return policy.SignalLogs, true
	case path == "/v1/traces", strings.HasPrefix(path, "/v1/traces/"),
		path == "/v1/spans", strings.HasPrefix(path, "/v1/spans/"),
		strings.HasPrefix(path, "/api/traces"), strings.HasPrefix(path, "/api/v2/traces"),
		path == "/api/search", strings.HasPrefix(path, "/api/search/"),
		strings.HasPrefix(path, "/api/metrics/"):
		return policy.SignalTraces, true
	}
	return "", false
}

// Middleware rejects whole requests that the policy denies (anonymous under
// enforcement, or no policy loaded yet). Record-level filtering happens in the
// backend decorator. It is a no-op unless the gate is enabled and enforcing;
// install it inside the authentication middleware so identity is available.
func (g *PolicyGate) Middleware(next http.Handler) http.Handler {
	if g == nil || !g.enforce {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signal, ok := SignalForPath(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		id, _ := authz.IdentityFromContext(r.Context())
		if !g.Allowed(id, signal) {
			reason := "access denied"
			if !g.Ready() {
				reason = "access policy unavailable"
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"` + reason + `"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
