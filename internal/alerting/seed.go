package alerting

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	defaultProbeRuleGroup      = "probe-health"
	defaultPlatformRuleGroup   = "platform-health"
	defaultDeploymentRuleGroup = "deployment-health"
	defaultPusherRuleGroup     = "pusher-health"
	defaultPlatformPartition   = "doctor"
)

// DefaultRuleSeeder installs the built-in probe-health rule pack for every
// partition that currently exports Guardian probe metrics. Rules are only
// created when absent, so an operator can edit them (or their notification
// routes) without the seeder overwriting the change.
type DefaultRuleSeeder struct {
	Querier          Querier
	Store            *Store
	Tenant           string
	Interval         time.Duration
	Logger           *log.Logger
	PusherPartitions map[string]string
}

// Run reconciles the default rule pack immediately and then on an interval.
func (s *DefaultRuleSeeder) Run(ctx context.Context) {
	if s == nil || s.Querier == nil || s.Store == nil {
		return
	}
	interval := s.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	s.ensure(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.ensure(ctx)
		}
	}
}

func (s *DefaultRuleSeeder) ensure(ctx context.Context) {
	tenant := strings.TrimSpace(s.Tenant)
	if tenant == "" {
		tenant = "default"
	}
	s.ensurePlatformHealthGroups(ctx, tenant)
	data, partial, err := s.Querier.PromInstant(ctx, "count by (guardian_partition) (guardian_probe_info)", time.Now())
	if err != nil {
		s.logf("probe rule seed: query partitions: %v", err)
		return
	}
	if partial {
		s.logf("probe rule seed: partition query returned partial data; skipping")
		return
	}
	for _, item := range prometheusResultSlice(data) {
		mapItem, ok := item.(map[string]any)
		if !ok {
			continue
		}
		metric, _ := mapItem["metric"].(map[string]any)
		partition, _ := metric["guardian_partition"].(string)
		partition = strings.TrimSpace(partition)
		if partition == "" {
			continue
		}
		s.ensureDeploymentHealthGroup(ctx, tenant, partition)
		if _, err := s.Store.GetRuleGroup(ctx, tenant, partition, defaultProbeRuleGroup); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			s.logf("probe rule seed: read %s/%s: %v", tenant, partition, err)
			continue
		}
		file := &RuleFile{
			Version: 1,
			Groups: []RuleGroup{{
				Name:      defaultProbeRuleGroup,
				Tenant:    tenant,
				Partition: partition,
				Interval:  Duration(30 * time.Second),
				Rules: []AlertRule{{
					Alert: "GuardianProbeDown",
					Expr:  fmt.Sprintf(`guardian_probe_up{guardian_partition=%q} == 0`, partition),
					For:   Duration(time.Minute),
					Labels: map[string]string{
						"severity": "critical",
						"owner":    "platform",
					},
					Annotations: map[string]string{
						"summary":     "A Guardian probe for this partition is down.",
						"description": "One or more probes report guardian_probe_up=0. Check the Doctor Probes page for the failing target.",
						"runbook":     "https://strata.local/runbooks/guardian-probe-down",
					},
				}},
			}},
		}
		if err := s.Store.PutRuleGroup(ctx, tenant, partition, defaultProbeRuleGroup, file); err != nil {
			s.logf("probe rule seed: create %s/%s: %v", tenant, partition, err)
			continue
		}
		s.logf("probe rule seed: installed %s/%s/%s", tenant, partition, defaultProbeRuleGroup)
	}
}

func (s *DefaultRuleSeeder) ensureDeploymentHealthGroup(ctx context.Context, tenant, partition string) {
	if _, err := s.Store.GetRuleGroup(ctx, tenant, partition, defaultDeploymentRuleGroup); err == nil {
		return
	} else if !errors.Is(err, ErrNotFound) {
		s.logf("deployment rule seed: read %s/%s: %v", tenant, partition, err)
		return
	}
	file := &RuleFile{
		Version: 1,
		Groups: []RuleGroup{{
			Name:      defaultDeploymentRuleGroup,
			Tenant:    tenant,
			Partition: partition,
			Interval:  Duration(30 * time.Second),
			Rules:     deploymentHealthRules(partition),
		}},
	}
	if err := s.Store.PutRuleGroup(ctx, tenant, partition, defaultDeploymentRuleGroup, file); err != nil {
		s.logf("deployment rule seed: create %s/%s: %v", tenant, partition, err)
		return
	}
	s.logf("deployment rule seed: installed %s/%s/%s", tenant, partition, defaultDeploymentRuleGroup)
}

func deploymentHealthRules(partition string) []AlertRule {
	rules := []AlertRule{
		probeSlowRule(partition),
		probeStaleRule(partition),
		guardianApplyFailedRule(partition),
	}
	switch partition {
	case defaultPlatformPartition:
		rules = append(rules,
			doctorFederationCatalogStaleRule(),
			doctorFederationNoTargetsRule(),
			doctorIngestBacklogRule(),
			doctorQueryErrorRateRule(),
			doctorQueryLatencyRule(),
		)
	case "div-a-orders":
		rules = append(rules,
			k8sClientErrorRateRule(),
			k8sClientLatencyRule(),
		)
	}
	return rules
}

func probeSlowRule(partition string) AlertRule {
	return AlertRule{
		Alert: "GuardianProbeSlow",
		Expr:  fmt.Sprintf(`max by (guardian_partition, guardian_asset) (guardian_probe_duration_seconds{guardian_partition=%q}) > 5`, partition),
		For:   Duration(2 * time.Minute),
		Labels: map[string]string{
			"severity": "warning",
		},
		Annotations: map[string]string{
			"summary": "A Guardian probe in this partition is responding slowly.",
			"runbook": "https://strata.local/runbooks/guardian-probe-slow",
		},
	}
}

func probeStaleRule(partition string) AlertRule {
	return AlertRule{
		Alert: "GuardianProbeStale",
		Expr:  fmt.Sprintf(`time() - max by (guardian_partition, guardian_asset) (guardian_probe_last_success_timestamp_seconds{guardian_partition=%q}) > 300`, partition),
		For:   Duration(2 * time.Minute),
		Labels: map[string]string{
			"severity": "warning",
		},
		Annotations: map[string]string{
			"summary":     "A Guardian probe has not succeeded recently.",
			"description": "The probe may be stuck or unable to reach its target. Check the Doctor Probes page.",
			"runbook":     "https://strata.local/runbooks/guardian-probe-stale",
		},
	}
}

func guardianApplyFailedRule(partition string) AlertRule {
	return AlertRule{
		Alert: "GuardianApplyFailed",
		Expr:  fmt.Sprintf(`increase(guardian_results_failures{guardian_status="Failed", guardian_target_partition=%q}[10m]) > 0`, partition),
		For:   Duration(time.Minute),
		Labels: map[string]string{
			"severity": "critical",
		},
		Annotations: map[string]string{
			"summary":     "Guardian failed to apply an intent in this partition.",
			"description": "A Guardian result reported a failed apply. Check the partition intent status and Guardian events.",
			"runbook":     "https://strata.local/runbooks/guardian-apply-failed",
		},
	}
}

func doctorFederationCatalogStaleRule() AlertRule {
	return AlertRule{
		Alert: "DoctorFederationCatalogStale",
		Expr:  `doctor_federation_catalog_stale == 1`,
		For:   Duration(2 * time.Minute),
		Labels: map[string]string{
			"severity": "warning",
		},
		Annotations: map[string]string{
			"summary": "The Doctor federation catalog is stale.",
			"runbook": "https://strata.local/runbooks/doctor-federation-stale",
		},
	}
}

func doctorFederationNoTargetsRule() AlertRule {
	return AlertRule{
		Alert: "DoctorFederationNoTargets",
		Expr:  `doctor_federation_catalog_targets == 0 and on() (doctor_federation_catalog_partitions > 0)`,
		For:   Duration(2 * time.Minute),
		Labels: map[string]string{
			"severity": "critical",
		},
		Annotations: map[string]string{
			"summary": "Doctor federation has partitions but no usable targets.",
			"runbook": "https://strata.local/runbooks/doctor-federation-no-targets",
		},
	}
}

func doctorIngestBacklogRule() AlertRule {
	return AlertRule{
		Alert: "DoctorIngestBacklog",
		Expr:  `sum by (signal) (doctor_ingest_buffer_pending_records) > 10000`,
		For:   Duration(5 * time.Minute),
		Labels: map[string]string{
			"severity": "warning",
		},
		Annotations: map[string]string{
			"summary":     "Doctor ingest is holding a large pending buffer.",
			"description": "A signal buffer has more than 10,000 pending records. Check MonoFS logengine health and ingest logs.",
			"runbook":     "https://strata.local/runbooks/doctor-ingest-backlog",
		},
	}
}

func doctorQueryErrorRateRule() AlertRule {
	return AlertRule{
		Alert: "DoctorQueryErrorRateHigh",
		Expr: `sum(rate(http_server_request_duration_count{service="doctor.query.query", http_response_status_code=~"5.."}[5m]))
/
sum(rate(http_server_request_duration_count{service="doctor.query.query"}[5m])) > 0.05`,
		For: Duration(5 * time.Minute),
		Labels: map[string]string{
			"severity": "critical",
		},
		Annotations: map[string]string{
			"summary": "Doctor query is returning more than 5% 5xx responses.",
			"runbook": "https://strata.local/runbooks/doctor-query-errors",
		},
	}
}

func doctorQueryLatencyRule() AlertRule {
	return AlertRule{
		Alert: "DoctorQueryLatencyHigh",
		Expr:  `histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_bucket{service="doctor.query.query"}[5m]))) > 5`,
		For:   Duration(5 * time.Minute),
		Labels: map[string]string{
			"severity": "warning",
		},
		Annotations: map[string]string{
			"summary": "Doctor query p95 latency is above 5s.",
			"runbook": "https://strata.local/runbooks/doctor-query-latency",
		},
	}
}

func k8sClientErrorRateRule() AlertRule {
	return AlertRule{
		Alert: "K8sClientErrorRateHigh",
		Expr: `sum(rate(http_server_request_duration_count{service="div-a-orders.app.k8s-client", http_response_status_code=~"5.."}[2m]))
/
sum(rate(http_server_request_duration_count{service="div-a-orders.app.k8s-client"}[2m])) > 0.05`,
		For: Duration(time.Minute),
		Labels: map[string]string{
			"severity": "critical",
		},
		Annotations: map[string]string{
			"summary": "The k8s client is returning more than 5% 5xx responses.",
			"runbook": "https://strata.local/runbooks/k8s-client-errors",
		},
	}
}

func k8sClientLatencyRule() AlertRule {
	return AlertRule{
		Alert: "K8sClientLatencyHigh",
		Expr:  `histogram_quantile(0.95, sum by (le) (rate(http_server_request_duration_bucket{service="div-a-orders.app.k8s-client"}[5m]))) > 2`,
		For:   Duration(5 * time.Minute),
		Labels: map[string]string{
			"severity": "warning",
		},
		Annotations: map[string]string{
			"summary": "The k8s client p95 latency is above 2s.",
			"runbook": "https://strata.local/runbooks/k8s-client-latency",
		},
	}
}

func (s *DefaultRuleSeeder) ensurePusherHealthGroup(ctx context.Context, tenant, partition, pusher string) {
	if _, err := s.Store.GetRuleGroup(ctx, tenant, partition, defaultPusherRuleGroup); err == nil {
		return
	} else if !errors.Is(err, ErrNotFound) {
		s.logf("pusher rule seed: read %s/%s: %v", tenant, partition, err)
		return
	}
	file := &RuleFile{
		Version: 1,
		Groups: []RuleGroup{{
			Name:      defaultPusherRuleGroup,
			Tenant:    tenant,
			Partition: partition,
			Interval:  Duration(30 * time.Second),
			Rules:     []AlertRule{pusherTaskFailureRule(pusher)},
		}},
	}
	if err := s.Store.PutRuleGroup(ctx, tenant, partition, defaultPusherRuleGroup, file); err != nil {
		s.logf("pusher rule seed: create %s/%s: %v", tenant, partition, err)
		return
	}
	s.logf("pusher rule seed: installed %s/%s/%s", tenant, partition, defaultPusherRuleGroup)
}

func pusherTaskFailureRule(pusher string) AlertRule {
	return AlertRule{
		Alert: "GuardianPusherTaskFailures",
		Expr:  fmt.Sprintf(`increase(guardian_pusher_task_failures{guardian_pusher=%q}[10m]) > 0`, pusher),
		For:   Duration(time.Minute),
		Labels: map[string]string{
			"severity":        "warning",
			"guardian_pusher": pusher,
		},
		Annotations: map[string]string{
			"summary":     fmt.Sprintf("The %s Guardian pusher reported task failures.", pusher),
			"description": "The pusher failed one or more tasks in the last 10 minutes. Check the Guardian task and event views.",
			"runbook":     "https://strata.local/runbooks/guardian-pusher-task-failures",
		},
	}
}

func (s *DefaultRuleSeeder) ensurePlatformHealthGroups(ctx context.Context, tenant string) {
	partitions := s.PusherPartitions
	if len(partitions) == 0 {
		partitions = map[string]string{"k8s-main": defaultPlatformPartition}
	}
	for pusher, partition := range partitions {
		pusher = strings.TrimSpace(pusher)
		partition = strings.TrimSpace(partition)
		if pusher == "" || partition == "" {
			continue
		}
		s.ensurePusherHealthGroup(ctx, tenant, partition, pusher)
		if _, err := s.Store.GetRuleGroup(ctx, tenant, partition, defaultPlatformRuleGroup); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			s.logf("platform rule seed: read %s/%s: %v", tenant, partition, err)
			continue
		}
		rules := pusherAlertRules(pusher)
		if partition == defaultPlatformPartition {
			rules = append([]AlertRule{doctorComponentRule()}, rules...)
		}
		file := &RuleFile{
			Version: 1,
			Groups: []RuleGroup{{
				Name:      defaultPlatformRuleGroup,
				Tenant:    tenant,
				Partition: partition,
				Interval:  Duration(30 * time.Second),
				Rules:     rules,
			}},
		}
		if err := s.Store.PutRuleGroup(ctx, tenant, partition, defaultPlatformRuleGroup, file); err != nil {
			s.logf("platform rule seed: create %s/%s: %v", tenant, partition, err)
			continue
		}
		s.logf("platform rule seed: installed %s/%s/%s", tenant, partition, defaultPlatformRuleGroup)
	}
}

func doctorComponentRule() AlertRule {
	return AlertRule{
		Alert: "DoctorComponentDown",
		Expr:  "min(doctor_component_up) < 1",
		For:   Duration(2 * time.Minute),
		Labels: map[string]string{
			"severity": "critical",
		},
		Annotations: map[string]string{
			"summary": "A Doctor component is reporting down.",
			"runbook": "https://strata.local/runbooks/doctor-component-down",
		},
	}
}

func pusherAlertRules(pusher string) []AlertRule {
	selector := fmt.Sprintf(`guardian_pusher=%q`, pusher)
	rules := []AlertRule{
		{
			Alert: "GuardianPusherHeartbeatStale",
			Expr:  fmt.Sprintf(`max(guardian_registry_pusher_heartbeat_age_seconds{%s}) > 120`, selector),
			For:   Duration(2 * time.Minute),
			Labels: map[string]string{
				"severity":        "warning",
				"guardian_pusher": pusher,
			},
			Annotations: map[string]string{
				"summary": "A Guardian pusher has not sent a heartbeat recently.",
				"runbook": "https://strata.local/runbooks/guardian-pusher-stale",
			},
		},
		{
			Alert: "GuardianPusherUnhealthy",
			Expr:  fmt.Sprintf(`max(guardian_registry_pusher_info{%s, status!="healthy"}) > 0`, selector),
			For:   Duration(time.Minute),
			Labels: map[string]string{
				"severity":        "critical",
				"guardian_pusher": pusher,
			},
			Annotations: map[string]string{
				"summary": "A Guardian pusher reports an unhealthy status.",
				"runbook": "https://strata.local/runbooks/guardian-pusher-unhealthy",
			},
		},
	}
	// The federated AWS backends synthesize zero samples for selectors that do
	// not match their shards, so a missing-pusher rule would create false
	// positives for AWS pushers. Keep the missing-registration check for the
	// local k8s pusher only; registered pushers are covered by heartbeat and
	// status rules.
	if pusher == "k8s-main" {
		rules = append(rules, AlertRule{
			Alert: "GuardianPusherUnregistered",
			// absent() is shard-safe only when guarded by a metric that the
			// federated AWS backends do not synthesize. Without the guard,
			// every AWS shard reports a false zero for the k8s pusher.
			Expr: fmt.Sprintf(`absent(guardian_registry_pusher_info{%s}) and on() (count(guardian_registry_pushers) > 0)`, selector),
			For:  Duration(30 * time.Second),
			Labels: map[string]string{
				"severity":        "warning",
				"guardian_pusher": pusher,
			},
			Annotations: map[string]string{
				"summary":     fmt.Sprintf("The %s Guardian pusher is not registered in the pusher registry.", pusher),
				"description": "Tasks may still be processed, but partition assignment and pusher health views will be stale.",
				"runbook":     "https://strata.local/runbooks/guardian-pusher-unregistered",
			},
		})
	}
	return rules
}

func (s *DefaultRuleSeeder) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf("sentinel seed: "+format, args...)
	}
}
