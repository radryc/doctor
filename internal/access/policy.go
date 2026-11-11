package access

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/segment"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
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

// CurrentPolicy returns the currently loaded bundle, or nil when no policy is
// loaded. It lets other features (quotas) share the same loader.
func (g *PolicyGate) CurrentPolicy() *policy.Policy {
	if g == nil {
		return nil
	}
	return g.policy()
}

// IsAdmin reports whether the identity bypasses filtering. The break-glass
// admin subject (from the shared auth verifier) always bypasses, even while no
// policy is loaded.
func (g *PolicyGate) IsAdmin(id authz.Identity) bool {
	if id.Subject == "break-glass-admin" || id.ClientID == "break-glass-admin" {
		return true
	}
	p := g.policy()
	return p != nil && p.IsAdmin(id)
}

// VisiblePartitions returns the partitions the caller may read for a signal.
// Unlike record filtering, this runs even in observe mode: the SQL console is
// a new read surface and fails closed while the policy is unavailable. Metrics
// are always visible under the Strata policy.
func (g *PolicyGate) VisiblePartitions(id authz.Identity, signal policy.Signal) ([]string, bool) {
	if g == nil || signal == policy.SignalMetrics {
		return nil, false
	}
	if g.IsAdmin(id) {
		return nil, false
	}
	p := g.policy()
	if p == nil {
		return []string{}, true
	}
	names, all := p.VisiblePartitions(id)
	if all {
		if p.CanReadSignal(id, "*", signal) {
			return nil, false
		}
		return []string{}, true
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if p.CanReadSignal(id, name, signal) {
			out = append(out, name)
		}
	}
	return out, true
}

// ReadPartitions returns the raw storage partition labels that may contain
// records the caller can read. It starts from VisiblePartitions and adds the
// AWS account/region aliases bound to each visible partition because legacy
// federated records can carry the region (or account) as guardian_partition.
// A storage pushdown using only the canonical partition names would hide those
// rows even though the record-level canonicalizer would keep them.
//
// The returned set is deliberately a superset: callers must still apply
// FilterLogs/FilterSpans after reading. It is sorted so pushdown requests and
// cache keys are deterministic.
func (g *PolicyGate) ReadPartitions(id authz.Identity, signal policy.Signal) ([]string, bool) {
	partitions, restricted := g.VisiblePartitions(id, signal)
	if !restricted {
		return nil, false
	}
	p := g.policy()
	if p == nil || len(partitions) == 0 {
		return partitions, true
	}

	visible := make(map[string]struct{}, len(partitions))
	for _, partition := range partitions {
		visible[partition] = struct{}{}
	}

	seen := make(map[string]struct{}, len(partitions))
	out := make([]string, 0, len(partitions))
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, partition := range partitions {
		add(partition)
	}
	for _, binding := range p.AWSBindings() {
		if _, ok := visible[binding.Partition]; !ok {
			continue
		}
		add(binding.Account)
		add(binding.Region)
	}
	sort.Strings(out)
	return out, true
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

// AccessiblePartitions returns the partition names the identity may see at
// all (the same partition scope CanSeePartition enforces). restricted is false
// when the gate does not restrict the identity: enforcement is disabled, the
// identity is an admin or has a wildcard grant, or the policy has not loaded
// yet.
func (g *PolicyGate) AccessiblePartitions(id authz.Identity) ([]string, bool) {
	if g == nil || !g.enforce {
		return nil, false
	}
	if g.IsAdmin(id) {
		return nil, false
	}
	p := g.policy()
	if p == nil {
		return nil, false
	}
	names, all := p.VisiblePartitions(id)
	if all {
		return nil, false
	}
	if names == nil {
		names = []string{}
	}
	sort.Strings(names)
	return names, true
}

// CanSeePartition reports whether the identity may see the partition in
// catalog/list views such as APM Services, Synthetics, and Fleet. Metrics stay
// globally readable; this gate is for partition identities, not metric data.
// Observe mode and a disabled gate pass through.
func (g *PolicyGate) CanSeePartition(id authz.Identity, partition string) bool {
	if g == nil || !g.enforce {
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
	return p.CanSeePartition(id, partition)
}

// CanModifyPartition reports whether the identity may create, update, or
// delete partition-scoped control-plane objects (dashboards, saved graphs)
// for the partition. Maintainer and admin roles allow modification; viewer and
// ingester do not. Observe mode and a disabled gate pass through.
func (g *PolicyGate) CanModifyPartition(id authz.Identity, partition string) bool {
	if g == nil || !g.enforce {
		return true
	}
	partition = strings.TrimSpace(partition)
	if partition == "" || id.IsAnonymous() {
		return false
	}
	if g.IsAdmin(id) {
		return true
	}
	p := g.policy()
	if p == nil {
		return false
	}
	return p.Role(id, partition).Allows(authz.ActionModify)
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
	if g.policy() == nil {
		return false
	}
	return g.partitionReadable(id, partition, signal)
}

// partitionReadable evaluates the loaded policy without applying the
// observe/enforce short-circuit. Observe mode uses it to report what would be
// filtered, while PartitionVisible uses it only after enforcement is enabled.
func (g *PolicyGate) partitionReadable(id authz.Identity, partition string, signal policy.Signal) bool {
	if g == nil || signal == policy.SignalMetrics {
		return true
	}
	if g.IsAdmin(id) {
		return true
	}
	p := g.policy()
	if p == nil {
		// No policy is loaded yet. Observe mode cannot predict a decision;
		// enforcement fails closed in PartitionVisible instead.
		return true
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
			// OTel resource attributes keep the dotted form, while the
			// Loki/AWS label paths use the underscore form.
			if current == "" {
				current = attrs["guardian.partition"]
			}
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
		canonical := g.canonicalize(rec.Partition, mergedAttributes(rec.ResourceAttributes, rec.Attributes))
		if !g.PartitionVisible(id, canonical, policy.SignalLogs) {
			dropped++
			continue
		}
		if canonical != rec.Partition {
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
		canonical := g.canonicalize(span.Partition, mergedAttributes(span.ResourceAttributes, span.Attributes))
		if !g.PartitionVisible(id, canonical, policy.SignalTraces) {
			dropped++
			continue
		}
		if canonical != span.Partition {
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
		attrs := mergedAttributes(rec.ResourceAttributes, rec.Attributes)
		if !g.partitionReadable(id, g.canonicalize(rec.Partition, attrs), policy.SignalLogs) {
			dropped++
		}
	}
	return dropped
}

func countSpanDrops(g *PolicyGate, id authz.Identity, spans []segment.TraceRecord) int {
	dropped := 0
	for _, span := range spans {
		attrs := mergedAttributes(span.ResourceAttributes, span.Attributes)
		if !g.partitionReadable(id, g.canonicalize(span.Partition, attrs), policy.SignalTraces) {
			dropped++
		}
	}
	return dropped
}

// mergedAttributes combines resource-level and record-level attributes. The
// partition label can arrive in either map depending on the OTel pipeline.
func mergedAttributes(resource, attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return resource
	}
	out := make(map[string]string, len(resource)+len(attributes))
	for key, value := range resource {
		out[key] = value
	}
	for key, value := range attributes {
		out[key] = value
	}
	return out
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
