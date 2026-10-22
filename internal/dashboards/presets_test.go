package dashboards

import (
	"strings"
	"testing"
)

// The AWS federation dashboard advertises source=aws scoping. The X-Ray panel
// must therefore use a trace query that filters by the canonical source label
// instead of the unscoped recent-traces endpoint (which also returns Guardian
// and MonoFS traces).
func TestAWSFederationPresetScopesXRayTraces(t *testing.T) {
	var preset DashboardDocument
	for _, candidate := range builtInPresets() {
		if candidate.ID == "preset-aws-federation" {
			preset = candidate
			break
		}
	}
	if preset.ID == "" {
		t.Fatal("preset-aws-federation not found")
	}

	for _, widget := range preset.Layout {
		if widget.ID != "aws-xray-traces" {
			continue
		}
		if len(widget.Queries) == 0 {
			t.Fatal("aws-xray-traces has no queries")
		}
		query := widget.Queries[0]
		if query.Datasource != DatasourceTempo {
			t.Fatalf("x-ray datasource = %q, want %q", query.Datasource, DatasourceTempo)
		}
		if !strings.Contains(query.Expr, `source = "aws"`) {
			t.Fatalf("x-ray query %q must scope to source=aws", query.Expr)
		}
		return
	}
	t.Fatal("aws-xray-traces widget not found")
}

// The Service RED "Top Failing Operations" panel must use TraceQL metrics
// syntax; a LogQL-style pipeline filter is rejected by the metrics parser.
func TestServiceREDPresetTopFailingOperationsIsTraceQL(t *testing.T) {
	var preset DashboardDocument
	for _, candidate := range builtInPresets() {
		if candidate.ID == "preset-service-red" {
			preset = candidate
			break
		}
	}
	if preset.ID == "" {
		t.Fatal("preset-service-red not found")
	}

	for _, widget := range preset.Layout {
		if widget.ID != "red-top-failing-ops" {
			continue
		}
		if len(widget.Queries) == 0 {
			t.Fatal("red-top-failing-ops has no queries")
		}
		query := widget.Queries[0]
		if query.Datasource != DatasourceTempo {
			t.Fatalf("datasource = %q, want %q", query.Datasource, DatasourceTempo)
		}
		if strings.Contains(query.Expr, "| status = error |") {
			t.Fatalf("query %q uses a LogQL-style filter stage", query.Expr)
		}
		if !strings.Contains(query.Expr, "status = error") ||
			!strings.Contains(query.Expr, "rate() by (span.name)") {
			t.Fatalf("query %q must filter status=error and group by span.name", query.Expr)
		}
		return
	}
	t.Fatal("red-top-failing-ops widget not found")
}

// The Active Series stat must match both a short service name and its
// canonical doctor.<intent>.<asset> variants.
func TestServiceREDPresetActiveSeriesMatchesCanonicalServices(t *testing.T) {
	var preset DashboardDocument
	for _, candidate := range builtInPresets() {
		if candidate.ID == "preset-service-red" {
			preset = candidate
			break
		}
	}
	if preset.ID == "" {
		t.Fatal("preset-service-red not found")
	}
	for _, widget := range preset.Layout {
		if widget.ID != "red-active-series" {
			continue
		}
		if len(widget.Queries) == 0 {
			t.Fatal("red-active-series has no queries")
		}
		if expr := widget.Queries[0].Expr; !strings.Contains(expr, `$service(\\..*)?`) {
			t.Fatalf("active-series query %q must include canonical service variants", expr)
		}
		return
	}
	t.Fatal("red-active-series widget not found")
}
