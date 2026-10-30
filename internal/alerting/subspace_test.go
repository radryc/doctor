package alerting

import (
	"context"
	"testing"
	"time"
)

func TestSubspaceAudienceAndResolve(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: false}
	req := SubspaceRequest{
		Source:          "sentinel",
		SourcePartition: "doctor-telemetry",
		Severity:        "warning",
		DedupeKey:       "sentinel.delivery.pagerduty-unavailable",
		Labels:          map[string]string{"alertname": "DeliveryChannelUnavailable", "component": "pagerduty"},
		Annotations:     map[string]string{"summary": "PagerDuty unavailable"},
		Audience:        Audience{Type: "partitions", Partitions: []string{"payments", "checkout"}},
	}
	alert, err := service.UpsertSubspace(ctx, req, "default", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if alert.Scope != "subspace" || alert.Partition != SubspacePartition {
		t.Fatalf("alert = %+v", alert)
	}
	visible := Identity{User: "dev", Partitions: map[string]bool{"payments": true}}
	alerts, err := service.ListSubspace(ctx, "default", visible)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("visible alerts = %d", len(alerts))
	}
	other := Identity{User: "other", Partitions: map[string]bool{"orders": true}}
	alerts, err = service.ListSubspace(ctx, "default", other)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("other partition saw %d subspace alerts", len(alerts))
	}
	admin := Identity{User: "admin", Admin: true, Partitions: map[string]bool{}}
	alerts, err = service.ListSubspace(ctx, "default", admin)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("admin alerts = %d, err=%v", len(alerts), err)
	}

	resolved, err := service.ResolveSubspace(ctx, "default", req.DedupeKey)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != StatusResolved {
		t.Fatalf("resolved status = %s", resolved.Status)
	}
}

func TestSubspaceNotSuppressedByPartitionSilence(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: true}
	now := time.Now().UTC()
	if _, err := service.CreateSilence(ctx, Silence{
		Tenant: "default", Partition: "payments",
		Matchers: []Matcher{{Name: "alertname", Operator: "=", Value: "DeliveryChannelUnavailable"}},
		StartsAt: now, EndsAt: now.Add(time.Hour), Scope: "rule",
	}, "tester"); err != nil {
		t.Fatal(err)
	}
	alert, err := service.UpsertSubspace(ctx, SubspaceRequest{
		DedupeKey: "sentinel.delivery.pagerduty-unavailable",
		Audience:  Audience{Type: "partitions", Partitions: []string{"payments"}},
		Labels:    map[string]string{"alertname": "DeliveryChannelUnavailable"},
	}, "default", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if len(alert.SilenceIDs) != 0 {
		t.Fatalf("subspace alert was suppressed by partition silence: %#v", alert.SilenceIDs)
	}
}
