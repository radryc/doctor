package alerting

import (
	"context"
	"testing"
	"time"
)

type fakeQuerier struct {
	data    map[string]any
	partial bool
	err     error
}

func (f *fakeQuerier) PromInstant(context.Context, string, time.Time) (map[string]any, bool, error) {
	return f.data, f.partial, f.err
}

func ruleVector(value string) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"result": []any{map[string]any{
				"metric": map[string]any{"service": "checkout"},
				"value":  []any{float64(1), value},
			}},
		},
	}
}

func TestRuleEvaluationFiresAndResolves(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	service := &Service{
		Store: store, DefaultTenant: "default", DeliveryEnabled: false,
		Querier: &fakeQuerier{data: ruleVector("0.12")},
		Now:     func() time.Time { return now },
	}
	group := RuleGroup{
		Name: "checkout", Tenant: "default", Partition: "payments", Interval: Duration(time.Minute),
		Rules: []AlertRule{{
			Alert:  "HighErrorRate",
			Expr:   `sum(rate(http_requests_total{status=~"5.."}[5m])) > 0.05`,
			Labels: map[string]string{"severity": "critical"},
		}},
	}
	results := service.EvaluateRuleGroup(ctx, group)
	if len(results) != 1 || results[0].Error != "" || results[0].Samples != 1 {
		t.Fatalf("results = %+v", results)
	}
	alerts, err := store.ListAlerts(ctx, "default", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Status != StatusFiring || alerts[0].Labels["rule"] != "HighErrorRate" {
		t.Fatalf("alerts = %+v", alerts)
	}

	service.Querier = &fakeQuerier{data: map[string]any{"data": map[string]any{"result": []any{}}}}
	service.Now = func() time.Time { return now.Add(time.Minute) }
	service.EvaluateRuleGroup(ctx, group)
	alerts, err = store.ListAlerts(ctx, "default", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Status != StatusResolved {
		t.Fatalf("resolved alerts = %+v", alerts)
	}
}

func TestRuleForMovesPendingToFiring(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	service := &Service{
		Store: store, DefaultTenant: "default", DeliveryEnabled: false,
		Querier: &fakeQuerier{data: ruleVector("1")},
		Now:     func() time.Time { return now },
	}
	group := RuleGroup{
		Name: "checkout", Tenant: "default", Partition: "payments",
		Rules: []AlertRule{{Alert: "PendingAlert", Expr: "vector(1)", For: Duration(5 * time.Minute)}},
	}
	service.EvaluateRuleGroup(ctx, group)
	alerts, _ := store.ListAlerts(ctx, "default", "payments")
	if len(alerts) != 1 || alerts[0].Status != StatusPending {
		t.Fatalf("first eval = %+v", alerts)
	}
	service.Now = func() time.Time { return now.Add(6 * time.Minute) }
	service.EvaluateRuleGroup(ctx, group)
	alerts, _ = store.ListAlerts(ctx, "default", "payments")
	if len(alerts) != 1 || alerts[0].Status != StatusFiring {
		t.Fatalf("second eval = %+v", alerts)
	}
}

func TestRulePartialDoesNotResolve(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	service := &Service{
		Store: store, DefaultTenant: "default", DeliveryEnabled: false,
		Querier: &fakeQuerier{data: ruleVector("1")},
		Now:     func() time.Time { return now },
	}
	group := RuleGroup{Name: "g", Tenant: "default", Partition: "payments", Rules: []AlertRule{{Alert: "A", Expr: "vector(1)"}}}
	service.EvaluateRuleGroup(ctx, group)
	service.Querier = &fakeQuerier{data: map[string]any{}, partial: true}
	service.EvaluateRuleGroup(ctx, group)
	alerts, _ := store.ListAlerts(ctx, "default", "payments")
	if len(alerts) != 1 || alerts[0].Status != StatusFiring {
		t.Fatalf("partial resolved alert: %+v", alerts)
	}
}

func TestRuleStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	file, err := ParseRuleFile([]byte(`
version: 1
groups:
  - name: checkout
    tenant: default
    partition: payments
    interval: 1m
    rules:
      - alert: HighErrorRate
        expr: vector(1)
        for: 5m
        labels:
          severity: critical
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRuleGroup(ctx, "default", "payments", "checkout", file); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetRuleGroup(ctx, "default", "payments", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Groups[0].Rules[0].Alert != "HighErrorRate" {
		t.Fatalf("loaded = %+v", loaded)
	}
	refs, err := store.ListRuleGroups(ctx, "default")
	if err != nil || len(refs) != 1 {
		t.Fatalf("refs = %+v err=%v", refs, err)
	}
}
