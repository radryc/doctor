package federation

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// Planner resolves a query to a bounded set of targets using catalog indexes.
type Planner struct {
	catalog   *Catalog
	maxFanout int
}

// NewPlanner builds a planner. maxFanout <= 0 defaults to 32.
func NewPlanner(catalog *Catalog, maxFanout int) *Planner {
	if maxFanout <= 0 {
		maxFanout = 32
	}
	return &Planner{catalog: catalog, maxFanout: maxFanout}
}

// MaxFanout returns the configured fan-out cap.
func (p *Planner) MaxFanout() int {
	if p == nil || p.maxFanout <= 0 {
		return 32
	}
	return p.maxFanout
}

// Plan resolves a query. It never scans the full catalog for a scoped query:
// only the relevant indexes are consulted.
func (p *Planner) Plan(ctx context.Context, q Query) (Plan, error) {
	if p == nil || p.catalog == nil || p.catalog.Len() == 0 {
		return Plan{Mode: PlanReject, Reason: "federation catalog is empty"}, nil
	}
	_ = ctx

	pred := p.predicates(q)
	if pred.empty() {
		// The native traces page asks for recent traces without a service
		// filter. For a small catalog (the demo), fan out so X-Ray traces are
		// visible without forcing a service selection. Fleet-scale catalogs
		// still require a scope or a central trace store.
		if q.Signal == SignalTraces && p.catalog.Len() <= p.MaxFanout() {
			targets := p.catalog.Targets()
			return Plan{Mode: PlanPull, Targets: targets, Matched: len(targets)}, nil
		}
		return Plan{Mode: PlanReject, Reason: ErrScopeRequired.Error()}, nil
	}

	candidates, reason := p.resolve(pred)
	if reason != "" {
		return Plan{Mode: PlanReject, Reason: reason}, nil
	}
	if len(candidates) == 0 {
		return Plan{Mode: PlanReject, Reason: ErrNoTargets.Error()}, nil
	}

	matched := len(candidates)
	if matched > p.MaxFanout() {
		return Plan{
			Mode:    PlanReject,
			Matched: matched,
			Reason:  fmt.Sprintf("%s (matched=%d max=%d)", ErrTooBroad.Error(), matched, p.MaxFanout()),
		}, nil
	}
	return Plan{Mode: PlanPull, Targets: candidates, Matched: matched}, nil
}

// CheckScope implements ScopeChecker.
func (p *Planner) CheckScope(ctx context.Context, q Query) error {
	plan, err := p.Plan(ctx, q)
	if err != nil {
		return err
	}
	if plan.Mode == PlanReject {
		return &ScopeError{Err: scopeError(plan.Reason), Matched: plan.Matched, MaxFanout: p.MaxFanout()}
	}
	return nil
}

func scopeError(reason string) error {
	switch {
	case strings.Contains(reason, ErrTooBroad.Error()):
		return ErrTooBroad
	case strings.Contains(reason, ErrNoTargets.Error()):
		return ErrNoTargets
	default:
		return ErrScopeRequired
	}
}

func (p *Planner) predicates(q Query) Predicates {
	pred := Predicates{From: q.From, To: q.To, Limit: q.Limit}
	addExact := func(dst *[]string, value string) {
		if value = strings.TrimSpace(value); value != "" {
			*dst = append(*dst, value)
		}
	}
	addExact(&pred.Accounts, q.Account)
	addExact(&pred.Regions, q.Region)
	addExact(&pred.Partitions, q.Partition)
	addExact(&pred.Services, q.Service)
	for _, group := range q.LogGroups {
		addExact(&pred.LogGroups, group)
	}
	if q.TraceID != "" {
		pred.TraceID = q.TraceID
	}

	switch q.Signal {
	case SignalMetrics:
		prometheusPredicates(q.Expr, &pred)
	case SignalLogs:
		logQLPredicates(q.Expr, &pred)
	case SignalTraces:
		traceQLPredicates(q.Expr, &pred)
	default:
		// Best-effort: try PromQL first, then LogQL/TraceQL label syntax.
		if !prometheusPredicates(q.Expr, &pred) {
			logQLPredicates(q.Expr, &pred)
		}
	}

	pred.Accounts = dedupeStrings(pred.Accounts)
	pred.AccountRE = dedupeStrings(pred.AccountRE)
	pred.Regions = dedupeStrings(pred.Regions)
	pred.RegionRE = dedupeStrings(pred.RegionRE)
	pred.Partitions = dedupeStrings(pred.Partitions)
	pred.PartitionRE = dedupeStrings(pred.PartitionRE)
	pred.LogGroups = dedupeStrings(pred.LogGroups)
	pred.LogGroupRE = dedupeStrings(pred.LogGroupRE)
	pred.Services = dedupeStrings(pred.Services)
	pred.ServiceRE = dedupeStrings(pred.ServiceRE)
	pred.Metrics = dedupeStrings(pred.Metrics)
	pred.MetricRE = dedupeStrings(pred.MetricRE)
	return pred
}

func (p *Planner) resolve(pred Predicates) ([]Target, string) {
	if pred.TraceID != "" {
		if target, ok := p.catalog.ByTrace(pred.TraceID); ok {
			return []Target{target}, ""
		}
		// No trace index: only fall back to a full try when the catalog is
		// small enough for a bounded fan-out.
		if p.catalog.Len() <= p.MaxFanout() {
			return p.catalog.Targets(), ""
		}
		return nil, ErrScopeRequired.Error()
	}

	// CloudWatch log groups encode account/region/partition. If the catalog
	// has no log-group cache, use those segments as indexes.
	if len(pred.LogGroups) > 0 && len(p.catalog.ByLogGroups(pred.LogGroups)) == 0 {
		for _, group := range pred.LogGroups {
			if account, region, partition, ok := parseGuardianLogGroup(group); ok {
				pred.Accounts = append(pred.Accounts, account)
				pred.Regions = append(pred.Regions, region)
				pred.Partitions = append(pred.Partitions, partition)
			}
		}
		pred.Accounts = dedupeStrings(pred.Accounts)
		pred.Regions = dedupeStrings(pred.Regions)
		pred.Partitions = dedupeStrings(pred.Partitions)
	}

	var candidates []Target
	hasNarrowing := false
	addSet := func(set []Target) {
		hasNarrowing = true
		if candidates == nil {
			candidates = set
			return
		}
		candidates = intersectTargets(candidates, set)
	}

	if len(pred.Accounts) > 0 {
		addSet(p.catalog.ByAccounts(pred.Accounts))
	}
	if len(pred.AccountRE) > 0 {
		addSet(p.catalog.byRegex(p.catalog.byAccount, pred.AccountRE))
	}
	if len(pred.Regions) > 0 {
		addSet(p.catalog.ByRegions(pred.Regions))
	}
	if len(pred.RegionRE) > 0 {
		addSet(p.catalog.byRegex(p.catalog.byRegion, pred.RegionRE))
	}
	if len(pred.Partitions) > 0 {
		addSet(p.catalog.ByPartitions(pred.Partitions))
	}
	if len(pred.PartitionRE) > 0 {
		addSet(p.catalog.byRegex(p.catalog.byPartition, pred.PartitionRE))
	}
	if len(pred.LogGroups) > 0 {
		if set := p.catalog.ByLogGroups(pred.LogGroups); len(set) > 0 {
			addSet(set)
		} else if len(pred.Accounts) == 0 && len(pred.Regions) == 0 && len(pred.Partitions) == 0 {
			// Not cached and not parseable: no target can serve it.
			addSet(set)
		}
	}
	if len(pred.LogGroupRE) > 0 {
		addSet(p.catalog.byRegex(p.catalog.byLogGroup, pred.LogGroupRE))
	}
	if len(pred.Services) > 0 {
		set := p.catalog.ByServices(pred.Services)
		if len(set) == 0 && p.catalog.Len() <= p.MaxFanout() {
			set = p.catalog.Targets()
		}
		addSet(set)
	}
	if len(pred.ServiceRE) > 0 {
		addSet(p.catalog.byRegex(p.catalog.byService, pred.ServiceRE))
	}
	if len(pred.Metrics) > 0 || len(pred.MetricRE) > 0 {
		var metricSet []Target
		for _, metric := range pred.Metrics {
			metricSet = append(metricSet, p.catalog.ByMetric(metric)...)
		}
		if len(pred.MetricRE) > 0 {
			metricSet = append(metricSet, p.catalog.byRegex(p.catalog.byMetric, pred.MetricRE)...)
		}
		metricSet = dedupeTargets(metricSet)
		if len(metricSet) == 0 {
			if p.catalog.Len() > p.MaxFanout() {
				// A metric-only query with no index match cannot be safely
				// scoped at fleet scale.
				return nil, ErrScopeRequired.Error()
			}
			metricSet = p.catalog.Targets()
		}
		addSet(metricSet)
	}

	if !hasNarrowing {
		return nil, ErrScopeRequired.Error()
	}
	if len(candidates) == 0 {
		return nil, ErrNoTargets.Error()
	}
	sortTargets(candidates)
	return candidates, ""
}

func intersectTargets(left, right []Target) []Target {
	if len(left) == 0 {
		return nil
	}
	if right == nil {
		sortTargets(left)
		return left
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, target := range right {
		rightSet[target.ID] = struct{}{}
	}
	out := make([]Target, 0, len(left))
	for _, target := range left {
		if _, ok := rightSet[target.ID]; ok {
			out = append(out, target)
		}
	}
	sortTargets(out)
	return out
}

func (pred Predicates) empty() bool {
	return len(pred.Accounts) == 0 && len(pred.AccountRE) == 0 &&
		len(pred.Regions) == 0 && len(pred.RegionRE) == 0 &&
		len(pred.Partitions) == 0 && len(pred.PartitionRE) == 0 &&
		len(pred.LogGroups) == 0 && len(pred.LogGroupRE) == 0 &&
		len(pred.Services) == 0 && len(pred.ServiceRE) == 0 &&
		len(pred.Metrics) == 0 && len(pred.MetricRE) == 0 &&
		pred.TraceID == ""
}

// prometheusPredicates extracts vector selectors from a PromQL expression.
func prometheusPredicates(expr string, pred *Predicates) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return false
	}
	parsed, err := parser.NewParser(parser.Options{}).ParseExpr(expr)
	if err != nil {
		return false
	}
	found := false
	parser.Inspect(parsed, func(node parser.Node, _ []parser.Node) error {
		selector, ok := node.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		found = true
		if selector.Name != "" {
			pred.Metrics = append(pred.Metrics, selector.Name)
		}
		for _, matcher := range selector.LabelMatchers {
			addMatcher(pred, matcher)
		}
		return nil
	})
	return found
}

func addMatcher(pred *Predicates, matcher *labels.Matcher) {
	if matcher == nil || matcher.Name == "" {
		return
	}
	name := normalizeLabelName(matcher.Name)
	if name == "" {
		return
	}
	switch matcher.Type {
	case labels.MatchEqual:
		addExactPredicate(pred, name, matcher.Value)
	case labels.MatchRegexp:
		addRegexPredicate(pred, name, matcher.Value)
	case labels.MatchNotEqual, labels.MatchNotRegexp:
		// Negative matchers cannot narrow a target set. Ignore them; the
		// backend still applies them to the returned data.
	}
}

func addExactPredicate(pred *Predicates, name, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	switch name {
	case "account":
		pred.Accounts = append(pred.Accounts, value)
	case "region":
		pred.Regions = append(pred.Regions, value)
	case "partition":
		pred.Partitions = append(pred.Partitions, value)
	case "log_group":
		pred.LogGroups = append(pred.LogGroups, value)
	case "service":
		pred.Services = append(pred.Services, value)
	}
}

func addRegexPredicate(pred *Predicates, name, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if value == ".*" || value == ".+" {
		// A wildcard log-group selector is still a scope for the small-catalog
		// demo (the manager resolves concrete groups from the catalog). Fleet
		// catalogs will hit the fan-out cap and return 422.
		if name == "log_group" {
			pred.LogGroupRE = append(pred.LogGroupRE, value)
		}
		return
	}
	if simple, ok := simpleAlternation(value); ok {
		for _, item := range simple {
			addExactPredicate(pred, name, item)
		}
		return
	}
	switch name {
	case "account":
		pred.AccountRE = append(pred.AccountRE, value)
	case "region":
		pred.RegionRE = append(pred.RegionRE, value)
	case "partition":
		pred.PartitionRE = append(pred.PartitionRE, value)
	case "log_group":
		pred.LogGroupRE = append(pred.LogGroupRE, value)
	case "service":
		pred.ServiceRE = append(pred.ServiceRE, value)
	}
}

func normalizeLabelName(name string) string {
	switch name {
	case "account", "aws_account", "@aws.account", "guardian.account":
		return "account"
	case "region", "aws_region", "@aws.region", "guardian.region":
		return "region"
	case "partition", "guardian_partition", "@aws.tag.guardian.io/partition", "@aws.tag.strata.io/partition":
		return "partition"
	case "aws_log_group", "log_group":
		return "log_group"
	case "service", "service.name", "resource.service.name":
		return "service"
	default:
		return ""
	}
}

func simpleAlternation(value string) ([]string, bool) {
	if !strings.Contains(value, "|") {
		return nil, false
	}
	parts := strings.Split(value, "|")
	if len(parts) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || strings.ContainsAny(part, `*+?[](){}^$\\`) {
			return nil, false
		}
		out = append(out, part)
	}
	return out, len(out) > 0
}

var (
	logQLMatcher = regexp.MustCompile(`(?m)([a-zA-Z_][a-zA-Z0-9_.@/-]*)\s*(=~|!=|!~|=)\s*"([^"]*)"`)
	traceMatcher = regexp.MustCompile(`(?m)([a-zA-Z_][a-zA-Z0-9_.@/-]*)\s*(=~|!=|!~|=)\s*"([^"]*)"`)
)

func logQLPredicates(expr string, pred *Predicates) {
	for _, match := range logQLMatcher.FindAllStringSubmatch(expr, -1) {
		name := normalizeLabelName(match[1])
		if name == "" {
			continue
		}
		switch match[2] {
		case "=", "!=":
			if match[2] == "=" {
				addExactPredicate(pred, name, match[3])
			}
		case "=~", "!~":
			if match[2] == "=~" {
				addRegexPredicate(pred, name, match[3])
			}
		}
	}
}

func traceQLPredicates(expr string, pred *Predicates) {
	for _, match := range traceMatcher.FindAllStringSubmatch(expr, -1) {
		name := normalizeLabelName(match[1])
		if name == "" {
			if strings.EqualFold(match[1], "trace_id") || strings.EqualFold(match[1], "traceID") {
				pred.TraceID = strings.TrimSpace(match[3])
			}
			continue
		}
		switch match[2] {
		case "=":
			addExactPredicate(pred, name, match[3])
		case "=~":
			addRegexPredicate(pred, name, match[3])
		}
	}
}

// parseGuardianLogGroup parses guardian/<account>/<region>/<partition>/<intent>.
func parseGuardianLogGroup(group string) (account, region, partition string, ok bool) {
	group = strings.TrimSpace(group)
	if idx := strings.Index(group, "guardian/"); idx >= 0 {
		group = group[idx:]
	}
	parts := strings.Split(strings.Trim(group, "/"), "/")
	if len(parts) < 4 || parts[0] != "guardian" {
		return "", "", "", false
	}
	return parts[1], parts[2], parts[3], true
}
