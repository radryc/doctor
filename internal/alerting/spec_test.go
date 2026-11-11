package alerting

import (
	"strings"
	"testing"
	"time"
)

func testSpec(signal string) *AlertSpec {
	return &AlertSpec{
		Signal: signal,
		Primary: SignalQuery{
			Metric:   "http_server_request_duration_count",
			Filters:  []Matcher{{Name: "partition", Operator: "=", Value: "payments"}, {Name: "service", Operator: "=~", Value: "checkout"}},
			Function: "rate",
			Reduce:   "sum",
			By:       []string{"service"},
		},
		Condition: Condition{Op: "gt", Value: 5, Unit: "percent"},
		Window:    Duration(5 * time.Minute),
		For:       Duration(10 * time.Minute),
		Severity:  "critical",
		Summary:   "checkout is failing",
	}
}

func TestCompileErrorRatio(t *testing.T) {
	spec := testSpec(SignalErrors)
	spec.Denominator = &SignalQuery{
		Metric:   "http_server_request_duration_count",
		Filters:  []Matcher{{Name: "partition", Operator: "=", Value: "payments"}},
		Function: "rate",
		Reduce:   "sum",
	}
	expr, err := CompileSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := `100 * (sum by (service) (rate(http_server_request_duration_count{partition="payments",service=~"checkout"}[5m])) / clamp_min(sum(rate(http_server_request_duration_count{partition="payments"}[5m])), 0.001)) > 5`
	if expr != want {
		t.Fatalf("expr =\n%s\nwant\n%s", expr, want)
	}
}

func TestCompileLatency(t *testing.T) {
	spec := testSpec(SignalLatency)
	spec.Primary.Metric = "http_server_request_duration_bucket"
	spec.Primary.Quantile = 0.95
	spec.Condition = Condition{Op: "gt", Value: 1000, Unit: "ms"}
	expr, err := CompileSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := `histogram_quantile(0.95, sum by (le, service) (rate(http_server_request_duration_bucket{partition="payments",service=~"checkout"}[5m]))) * 1000 > 1000`
	if expr != want {
		t.Fatalf("expr =\n%s\nwant\n%s", expr, want)
	}
}

func TestCompileTrafficDrop(t *testing.T) {
	spec := testSpec(SignalTraffic)
	spec.Condition = Condition{Op: "lt", Value: 0.01, Unit: "rps"}
	expr, err := CompileSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expr, `sum by (service) (rate(`) || !strings.HasSuffix(expr, ` < 0.01`) {
		t.Fatalf("expr = %s", expr)
	}
}

func TestCompileSaturationRatio(t *testing.T) {
	spec := testSpec(SignalSaturation)
	spec.Primary = SignalQuery{Metric: "container_cpu_usage_millicores", Reduce: "sum", By: []string{"container_name"}}
	spec.Denominator = &SignalQuery{Metric: "container_cpu_limit_millicores", Reduce: "sum", By: []string{"container_name"}}
	spec.Condition = Condition{Op: "gt", Value: 85, Unit: "percent"}
	expr, err := CompileSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := `100 * (sum by (container_name) (container_cpu_usage_millicores) / clamp_min(sum by (container_name) (container_cpu_limit_millicores), 0.001)) > 85`
	if expr != want {
		t.Fatalf("expr =\n%s\nwant\n%s", expr, want)
	}
}

func TestCompileAvailability(t *testing.T) {
	spec := testSpec(SignalAvailability)
	spec.Primary = SignalQuery{Metric: "guardian_probe_up", Filters: []Matcher{{Name: "partition", Operator: "=", Value: "payments"}}, Reduce: "min"}
	spec.Condition = Condition{Op: "eq", Value: 0}
	expr, err := CompileSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := `min(guardian_probe_up{partition="payments"}) == 0`
	if expr != want {
		t.Fatalf("expr =\n%s\nwant\n%s", expr, want)
	}
}

func TestGuidedRuleCompilesAndInfersMode(t *testing.T) {
	rule := AlertRule{Alert: "CheckoutErrors", Spec: testSpec(SignalErrors)}
	if err := rule.Prepare(); err != nil {
		t.Fatal(err)
	}
	if rule.Mode != ModeGuided {
		t.Fatalf("mode = %s", rule.Mode)
	}
	if rule.Expr == "" || !strings.Contains(rule.Expr, "> 5") {
		t.Fatalf("expr = %s", rule.Expr)
	}
	if rule.Labels["severity"] != "critical" {
		t.Fatalf("labels = %#v", rule.Labels)
	}
	if rule.For != Duration(10*time.Minute) {
		t.Fatalf("for = %s", time.Duration(rule.For))
	}
}

func TestPromQLRuleNeedsExpr(t *testing.T) {
	rule := AlertRule{Alert: "Raw", Mode: ModePromQL}
	if err := rule.Prepare(); err == nil {
		t.Fatal("expected error for empty expr")
	}
	rule.Expr = "up == 0"
	if err := rule.Prepare(); err != nil {
		t.Fatal(err)
	}
}

func TestSpecRejectsUnsafeMetricAndLabel(t *testing.T) {
	spec := testSpec(SignalTraffic)
	spec.Primary.Metric = `sum(rate(up[5m]))`
	if _, err := CompileSpec(spec); err == nil {
		t.Fatal("expected unsafe metric to be rejected")
	}
	spec = testSpec(SignalTraffic)
	spec.Primary.Filters[0].Name = `x} or vector(1`
	if _, err := CompileSpec(spec); err == nil {
		t.Fatal("expected unsafe label to be rejected")
	}
}
