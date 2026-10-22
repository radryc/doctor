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
