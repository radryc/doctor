// Package quota implements partition-scoped admission control for the Doctor
// data plane. Quotas are compiled by Guardian into the shared policy bundle;
// this package resolves the effective quota and enforces it locally.
package quota

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
)

// ProviderOptions configures quota policy loading.
type ProviderOptions struct {
	// PolicyURL and PolicyToken point at Guardian's compiled access policy.
	PolicyURL   string
	PolicyToken string
	// PolicyFunc lets a caller share an existing loader (doctor-query already
	// has a PolicyGate). When set, PolicyURL is ignored.
	PolicyFunc func() *policy.Policy
	// Interval is the policy refresh period.
	Interval time.Duration
	// Defaults are applied before the bundle values so an unconfigured
	// deployment still has a conservative envelope.
	Defaults policy.Quota
	// Enforce is the deployment default. A partition/profile may override it.
	Enforce bool
	Logger  *log.Logger
}

// Provider resolves effective quotas for partitions and subjects.
type Provider struct {
	loader   *policy.Loader
	policyFn func() *policy.Policy
	defaults policy.Quota
	enforce  bool
	logger   *log.Logger

	knownMu      sync.Mutex
	knownVersion string
	knownSet     map[string]struct{}
}

// NewProvider builds a provider. A zero PolicyURL with no PolicyFunc yields a
// provider that only serves Defaults.
func NewProvider(opts ProviderOptions) *Provider {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	p := &Provider{
		defaults: opts.Defaults.Clone(),
		enforce:  opts.Enforce,
		logger:   logger,
	}
	if opts.PolicyFunc != nil {
		p.policyFn = opts.PolicyFunc
		return p
	}
	if strings.TrimSpace(opts.PolicyURL) == "" {
		return p
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	logf := func(format string, args ...any) { logger.Printf(format, args...) }
	p.loader = policy.NewLoader(policy.HTTPSource{
		URL:   strings.TrimSpace(opts.PolicyURL),
		Token: strings.TrimSpace(opts.PolicyToken),
	}, interval, logf)
	p.policyFn = p.loader.Get
	return p
}

// Run polls the policy source until ctx is cancelled.
func (p *Provider) Run(ctx context.Context) {
	if p == nil || p.loader == nil {
		return
	}
	p.loader.Run(ctx)
}

// Ready reports whether a compiled policy is loaded.
func (p *Provider) Ready() bool {
	if p == nil {
		return false
	}
	if p.loader != nil {
		return p.loader.Ready()
	}
	return p.policyFn != nil && p.policyFn() != nil
}

// Policy returns the current compiled policy, if any.
func (p *Provider) Policy() *policy.Policy {
	if p == nil || p.policyFn == nil {
		return nil
	}
	return p.policyFn()
}

// Defaults returns the configured base quota.
func (p *Provider) Defaults() policy.Quota {
	if p == nil {
		return policy.Quota{}
	}
	return p.defaults.Clone()
}

// EffectiveDefaults merges the environment defaults with the bundle defaults.
func (p *Provider) EffectiveDefaults() policy.Quota {
	if p == nil {
		return policy.Quota{}
	}
	out := p.defaults.Clone()
	if pol := p.Policy(); pol != nil {
		out = policy.MergeQuota(out, pol.QuotaDefaults())
	}
	return out
}

// PartitionQuota resolves the effective quota for a partition.
func (p *Provider) PartitionQuota(name string) policy.Quota {
	if p == nil {
		return policy.Quota{}
	}
	out := p.defaults.Clone()
	if pol := p.Policy(); pol != nil {
		out = policy.MergeQuota(out, pol.QuotaForPartition(name))
	}
	return out
}

// SubjectQuotas returns every scoped quota that applies to the identity.
func (p *Provider) SubjectQuotas(id authz.Identity) []policy.ScopedQuota {
	if p == nil {
		return nil
	}
	if pol := p.Policy(); pol != nil {
		return pol.SubjectQuotas(id)
	}
	return nil
}

// KnownPartitions returns the catalog carried by the bundle.
func (p *Provider) KnownPartitions() []string {
	if p == nil {
		return nil
	}
	if pol := p.Policy(); pol != nil {
		return pol.KnownPartitions()
	}
	return nil
}

// IsKnownPartition reports whether Guardian knows the partition. An empty
// catalog is treated as "unknown is allowed" so a partially rolled-out policy
// does not block all ingest.
func (p *Provider) IsKnownPartition(name string) bool {
	if p == nil {
		return true
	}
	set := p.knownSetFor()
	if len(set) == 0 {
		return true
	}
	name = strings.TrimSpace(name)
	if name == "" || name == UnknownPartition {
		return false
	}
	_, ok := set[name]
	return ok
}

func (p *Provider) knownSetFor() map[string]struct{} {
	pol := p.Policy()
	version := ""
	if pol != nil {
		version = pol.Version()
	}
	p.knownMu.Lock()
	defer p.knownMu.Unlock()
	if p.knownVersion == version && p.knownSet != nil {
		return p.knownSet
	}
	known := p.KnownPartitions()
	set := make(map[string]struct{}, len(known))
	for _, name := range known {
		set[name] = struct{}{}
	}
	p.knownVersion = version
	p.knownSet = set
	return set
}

// Enforce reports whether this partition should reject over-quota requests.
func (p *Provider) Enforce(partition string) bool {
	if p == nil {
		return false
	}
	q := p.PartitionQuota(partition)
	switch strings.ToLower(strings.TrimSpace(q.Enforcement)) {
	case "enforce":
		return true
	case "observe":
		return false
	}
	return p.enforce
}

// QueryQuota returns the effective query quota for a partition. When the
// partition is empty the bundle defaults are used.
func (p *Provider) QueryQuota(partition string) policy.QueryQuota {
	q := p.PartitionQuota(partition)
	if q.Query == nil {
		return policy.QueryQuota{}
	}
	return *q.Query
}

// QueryLimits returns the strictest query limits that apply to a request. It
// combines the default, every touched partition, and every subject bucket so a
// cross-partition query cannot escape a tighter limit.
func (p *Provider) QueryLimits(id authz.Identity, partitions []string) policy.QueryQuota {
	if p == nil {
		return policy.QueryQuota{}
	}
	limits := policy.QueryQuota{}
	apply := func(q policy.QueryQuota) {
		limits.RequestsPerSecond = minPositiveFloat(limits.RequestsPerSecond, q.RequestsPerSecond)
		limits.MaxConcurrent = minPositiveInt(limits.MaxConcurrent, q.MaxConcurrent)
		limits.BudgetPerSecond = minPositiveInt64(limits.BudgetPerSecond, q.BudgetPerSecond)
		limits.MaxSamples = minPositiveInt64(limits.MaxSamples, q.MaxSamples)
		limits.MaxSeries = minPositiveInt64(limits.MaxSeries, q.MaxSeries)
		limits.MaxPartitions = minPositiveInt(limits.MaxPartitions, q.MaxPartitions)
		limits.MaxRange = minDurationString(limits.MaxRange, q.MaxRange)
	}
	apply(p.QueryQuota(""))
	for _, partition := range partitions {
		apply(p.QueryQuota(partition))
	}
	for _, scoped := range p.SubjectQuotas(id) {
		if scoped.Quota.Query != nil {
			apply(*scoped.Quota.Query)
		}
	}
	return limits
}

func minPositiveFloat(current, candidate float64) float64 {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

func minPositiveInt(current, candidate int) int {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

func minPositiveInt64(current, candidate int64) int64 {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

func minDurationString(current, candidate string) string {
	if candidate == "" {
		return current
	}
	candidateDuration, err := policy.ParseQuotaDuration(candidate)
	if err != nil || candidateDuration <= 0 {
		return current
	}
	if current == "" {
		return candidate
	}
	currentDuration, err := policy.ParseQuotaDuration(current)
	if err != nil || currentDuration <= 0 || candidateDuration < currentDuration {
		return candidate
	}
	return current
}

// IngestQuota returns the partition ingest quota.
func (p *Provider) IngestQuota(partition string) policy.IngestQuota {
	q := p.PartitionQuota(partition)
	if q.Ingest == nil {
		return policy.IngestQuota{}
	}
	return *q.Ingest
}

// RulesQuota returns the partition rules quota.
func (p *Provider) RulesQuota(partition string) policy.RulesQuota {
	q := p.PartitionQuota(partition)
	if q.Rules == nil {
		return policy.RulesQuota{}
	}
	return *q.Rules
}

// StorageQuota returns the partition storage quota.
func (p *Provider) StorageQuota(partition string) policy.StorageQuota {
	q := p.PartitionQuota(partition)
	if q.Storage == nil {
		return policy.StorageQuota{}
	}
	return *q.Storage
}

// Blocked reports whether a partition is administratively muted.
func (p *Provider) Blocked(partition string) bool {
	return p.PartitionQuota(partition).Blocked
}
