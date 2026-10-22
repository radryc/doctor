package federation

import (
	"context"
	"errors"
	"testing"
)

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := NewCatalog([]Target{
		{ID: "aws-a", Account: "111111111111", Region: "us-east-1", Partitions: []string{"div-a"}, Endpoint: "http://a", Token: "a", MetricNames: []string{"Invocations"}, LogGroups: []string{"guardian/111111111111/us-east-1/div-a/app"}},
		{ID: "aws-b", Account: "222222222222", Region: "us-east-1", Partitions: []string{"div-b"}, Endpoint: "http://b", Token: "b", LogGroups: []string{"guardian/222222222222/us-east-1/div-b/app"}},
		{ID: "aws-c", Account: "333333333333", Region: "us-west-2", Partitions: []string{"div-c"}, Endpoint: "http://c", Token: "c", LogGroups: []string{"guardian/333333333333/us-west-2/div-c/app"}},
	}, "test")
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func TestPlannerResolvesScopedQueries(t *testing.T) {
	planner := NewPlanner(testCatalog(t), 32)
	cases := []struct {
		name  string
		query Query
		want  string
	}{
		{
			name:  "account",
			query: Query{Signal: SignalMetrics, Expr: `Invocations{account="111111111111"}`},
			want:  "aws-a",
		},
		{
			name:  "partition",
			query: Query{Signal: SignalMetrics, Expr: `{guardian_partition="div-b"}`},
			want:  "aws-b",
		},
		{
			name:  "partition aws tag alias",
			query: Query{Signal: SignalMetrics, Expr: `{"@aws.tag.guardian.io/partition"="div-c"}`},
			want:  "aws-c",
		},
		{
			name:  "log group without cache",
			query: Query{Signal: SignalLogs, LogGroups: []string{"guardian/111111111111/us-east-1/div-a/app"}},
			want:  "aws-a",
		},
		{
			name:  "metric index fallback",
			query: Query{Signal: SignalMetrics, Expr: `Invocations`},
			want:  "aws-a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planner.Plan(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("Plan() error = %v", err)
			}
			if plan.Mode != PlanPull {
				t.Fatalf("Plan() mode = %s reason=%s", plan.Mode, plan.Reason)
			}
			if len(plan.Targets) != 1 || plan.Targets[0].ID != tc.want {
				t.Fatalf("Plan() targets = %#v, want %s", plan.Targets, tc.want)
			}
		})
	}
}

func TestPlannerWildcardLogGroupUsesCatalogAndCap(t *testing.T) {
	catalog := testCatalog(t)
	planner := NewPlanner(catalog, 32)
	plan, err := planner.Plan(context.Background(), Query{
		Signal: SignalLogs,
		Expr:   `{source="aws", aws_log_group=~".*"} |= "error"`,
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.Mode != PlanPull || len(plan.Targets) != 3 {
		t.Fatalf("Plan() = %#v, want pull with 3 targets", plan)
	}

	capped := NewPlanner(catalog, 2)
	plan, err = capped.Plan(context.Background(), Query{
		Signal: SignalLogs,
		Expr:   `{source="aws", aws_log_group=~".*"}`,
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.Mode != PlanReject {
		t.Fatalf("Plan() = %#v, want reject", plan)
	}
}

func TestPlannerRejectsUnscopedAndTooBroadQueries(t *testing.T) {
	planner := NewPlanner(testCatalog(t), 2)
	for _, query := range []Query{
		{Signal: SignalMetrics, Expr: `sum(UnknownMetric)`},
		{Signal: SignalLogs, Expr: `{source="aws"} |= "error"`},
		{Signal: SignalTraces, Expr: `{resource.service.name=~".*"}`},
	} {
		plan, err := planner.Plan(context.Background(), query)
		if err != nil {
			t.Fatalf("Plan(%v) error = %v", query, err)
		}
		if plan.Mode != PlanReject {
			t.Fatalf("Plan(%v) mode = %s, want reject", query, plan.Mode)
		}
	}

	tooBroad := Query{Signal: SignalMetrics, Expr: `{region="us-east-1"}`}
	broadPlanner := NewPlanner(testCatalog(t), 1)
	plan, err := broadPlanner.Plan(context.Background(), tooBroad)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.Mode != PlanReject || plan.Matched != 2 {
		t.Fatalf("Plan() = %#v, want reject with matched=2", plan)
	}
	if err := broadPlanner.CheckScope(context.Background(), tooBroad); err == nil {
		t.Fatal("CheckScope() expected an error")
	} else if !errors.Is(err, ErrTooBroad) {
		t.Fatalf("CheckScope() error = %v, want ErrTooBroad", err)
	}
}
