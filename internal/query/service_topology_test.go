package query

import (
	"testing"
	"time"
)

func TestDoctorNodeHealthFromGuardian(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		node      guardianTopologyNode
		wantScore float64
		wantLevel string
	}{
		{name: "flow healthy overrides", node: guardianTopologyNode{Health: "attention", FlowState: "healthy"}, wantScore: 1.0, wantLevel: "healthy"},
		{name: "flow busy overrides", node: guardianTopologyNode{Health: "healthy", FlowState: "busy"}, wantScore: 0.6, wantLevel: "degraded"},
		{name: "flow failing overrides", node: guardianTopologyNode{Health: "healthy", FlowState: "failing"}, wantScore: 0.2, wantLevel: "unhealthy"},
		{name: "legacy health fallback", node: guardianTopologyNode{Health: "attention"}, wantScore: 0.5, wantLevel: "attention"},
		{name: "empty fallback unknown", node: guardianTopologyNode{}, wantScore: 0.5, wantLevel: "unknown"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			score, level := doctorNodeHealthFromGuardian(tc.node)
			if score != tc.wantScore || level != tc.wantLevel {
				t.Fatalf("health = (%v,%q), want (%v,%q)", score, level, tc.wantScore, tc.wantLevel)
			}
		})
	}
}

func TestFormatRFC3339OrEmpty(t *testing.T) {
	t.Parallel()

	if got := formatRFC3339OrEmpty(time.Time{}); got != "" {
		t.Fatalf("zero time = %q, want empty", got)
	}
	ts := time.Date(2026, 8, 5, 10, 11, 12, 0, time.FixedZone("UTC+2", 2*3600))
	if got, want := formatRFC3339OrEmpty(ts), "2026-08-05T08:11:12Z"; got != want {
		t.Fatalf("formatted = %q, want %q", got, want)
	}
}
