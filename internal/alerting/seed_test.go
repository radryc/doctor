package alerting

import (
	"context"
	"testing"
	"time"
)

type seedQuerier struct {
	data map[string]any
}

func (q seedQuerier) PromInstant(context.Context, string, time.Time) (map[string]any, bool, error) {
	return q.data, false, nil
}

func seedProbeData(partitions ...string) map[string]any {
	result := make([]any, 0, len(partitions))
	for _, partition := range partitions {
		result = append(result, map[string]any{
			"metric": map[string]any{"guardian_partition": partition},
			"value":  []any{float64(1), "1"},
		})
	}
	return map[string]any{"result": result}
}

func ruleNames(file *RuleFile) map[string]bool {
	out := map[string]bool{}
	if file == nil {
		return out
	}
	for _, group := range file.Groups {
		for _, rule := range group.Rules {
			out[rule.Alert] = true
		}
	}
	return out
}

func TestDefaultRuleSeederInstallsDeploymentHealthRules(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "")
	seeder := &DefaultRuleSeeder{
		Querier: seedQuerier{data: seedProbeData("doctor", "div-a-orders", "goalert")},
		Store:   store,
		Tenant:  "default",
		PusherPartitions: map[string]string{
			"k8s-main":             "doctor",
			"aws-div-780603797542": "div-a-orders",
		},
	}

	seeder.ensure(ctx)

	doctor, err := store.GetRuleGroup(ctx, "default", "doctor", defaultDeploymentRuleGroup)
	if err != nil {
		t.Fatalf("doctor deployment-health: %v", err)
	}
	doctorRules := ruleNames(doctor)
	for _, want := range []string{
		"GuardianProbeSlow",
		"GuardianProbeStale",
		"GuardianApplyFailed",
		"DoctorFederationCatalogStale",
		"DoctorFederationNoTargets",
		"DoctorIngestBacklog",
		"DoctorQueryErrorRateHigh",
		"DoctorQueryLatencyHigh",
	} {
		if !doctorRules[want] {
			t.Errorf("doctor deployment-health missing %s (have %v)", want, doctorRules)
		}
	}

	orders, err := store.GetRuleGroup(ctx, "default", "div-a-orders", defaultDeploymentRuleGroup)
	if err != nil {
		t.Fatalf("div-a-orders deployment-health: %v", err)
	}
	ordersRules := ruleNames(orders)
	for _, want := range []string{
		"GuardianProbeSlow",
		"GuardianProbeStale",
		"GuardianApplyFailed",
		"K8sClientErrorRateHigh",
		"K8sClientLatencyHigh",
	} {
		if !ordersRules[want] {
			t.Errorf("div-a-orders deployment-health missing %s (have %v)", want, ordersRules)
		}
	}
	if ordersRules["DoctorFederationCatalogStale"] {
		t.Error("div-a-orders unexpectedly contains doctor-specific federation rules")
	}

	goalert, err := store.GetRuleGroup(ctx, "default", "goalert", defaultDeploymentRuleGroup)
	if err != nil {
		t.Fatalf("goalert deployment-health: %v", err)
	}
	goalertRules := ruleNames(goalert)
	if !goalertRules["GuardianApplyFailed"] || goalertRules["K8sClientErrorRateHigh"] {
		t.Errorf("goalert deployment-health rules = %v", goalertRules)
	}

	for _, partition := range []string{"doctor", "div-a-orders", "goalert"} {
		if _, err := store.GetRuleGroup(ctx, "default", partition, defaultProbeRuleGroup); err != nil {
			t.Errorf("%s probe-health: %v", partition, err)
		}
	}

	for _, partition := range []string{"doctor", "div-a-orders"} {
		file, err := store.GetRuleGroup(ctx, "default", partition, defaultPusherRuleGroup)
		if err != nil {
			t.Fatalf("%s pusher-health: %v", partition, err)
		}
		if !ruleNames(file)["GuardianPusherTaskFailures"] {
			t.Errorf("%s pusher-health missing GuardianPusherTaskFailures", partition)
		}
	}

	// The seeder is idempotent: a second pass must not add or duplicate rules.
	before := len(doctor.Groups[0].Rules)
	seeder.ensure(ctx)
	after, err := store.GetRuleGroup(ctx, "default", "doctor", defaultDeploymentRuleGroup)
	if err != nil {
		t.Fatalf("doctor deployment-health after second pass: %v", err)
	}
	if got := len(after.Groups[0].Rules); got != before {
		t.Fatalf("doctor deployment-health rule count = %d, want %d", got, before)
	}
}
