package alerting

import (
	"context"
	"testing"
)

func TestQuotaBreachCriticalIsCappedAtWarning(t *testing.T) {
	in := Alert{
		Source:    "guardian",
		Tenant:    "default",
		Partition: "memebank",
		Status:    StatusFiring,
		Severity:  "critical",
		Labels: map[string]string{
			"alertname": "PartitionQuotaExceeded",
			"severity":  "critical",
			"partition": "memebank",
		},
		Annotations: map[string]string{
			"summary": "partition memebank exceeded its ingest quota",
		},
	}

	got := capQuotaBreachSeverity(in)
	if got.Severity != "warning" {
		t.Fatalf("severity = %q, want warning", got.Severity)
	}
	if got.Labels["severity"] != "warning" {
		t.Fatalf("label severity = %q, want warning", got.Labels["severity"])
	}
	if got.Labels["quota_breach"] != "true" {
		t.Fatalf("quota_breach label = %q, want true", got.Labels["quota_breach"])
	}
	// The caller's maps must not be mutated.
	if in.Labels["severity"] != "critical" {
		t.Fatalf("input labels mutated: %#v", in.Labels)
	}
}

func TestNonQuotaCriticalIsUnchanged(t *testing.T) {
	in := Alert{
		Partition: "memebank",
		Severity:  "critical",
		Labels:    map[string]string{"alertname": "GuardianProbeDown"},
	}
	got := capQuotaBreachSeverity(in)
	if got.Severity != "critical" {
		t.Fatalf("severity = %q, want critical", got.Severity)
	}
}

func TestQuotaWarningIsUnchanged(t *testing.T) {
	in := Alert{
		Partition: "memebank",
		Severity:  "warning",
		Labels:    map[string]string{"alertname": "PartitionQuotaExceeded"},
	}
	got := capQuotaBreachSeverity(in)
	if got.Severity != "warning" {
		t.Fatalf("severity = %q, want warning", got.Severity)
	}
	if _, ok := got.Labels["quota_breach"]; ok {
		t.Fatal("already-warning quota alerts should not be rewritten")
	}
}

func TestApplyPersistsQuotaBreachAsWarning(t *testing.T) {
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default"}
	ctx := context.Background()

	got, err := service.Apply(ctx, Alert{
		Source:    "guardian",
		Tenant:    "default",
		Partition: "memebank",
		Status:    StatusFiring,
		Severity:  "critical",
		Labels: map[string]string{
			"alertname": "PartitionQuotaExceeded",
			"severity":  "critical",
			"partition": "memebank",
		},
		Annotations: map[string]string{"summary": "partition memebank exceeded its quota"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Severity != "warning" {
		t.Fatalf("applied severity = %q, want warning", got.Severity)
	}

	stored, err := store.GetAlert(ctx, "default", "memebank", got.Fingerprint)
	if err != nil {
		t.Fatalf("GetAlert: %v", err)
	}
	if stored.Severity != "warning" {
		t.Fatalf("stored severity = %q, want warning", stored.Severity)
	}
}
