package alerting

import (
	"context"
	"errors"
	"testing"
	"time"

	doctorstore "github.com/radryc/doctor/internal/store"
)

func TestSaveAlertExpectedRejectsStaleVersion(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	service := &Service{Store: store, DefaultTenant: "default"}
	service.Now = func() time.Time { return now }

	if _, err := service.IngestGuardian(ctx, "default", GuardianAlert{
		Event: "GuardianPartitionError", Partition: "payments", Status: "firing",
		Severity: "critical", Summary: "failed", FiredAt: now,
	}, ""); err != nil {
		t.Fatal(err)
	}
	alerts, err := store.ListAlerts(ctx, "default", "payments")
	if err != nil || len(alerts) != 1 {
		t.Fatalf("ListAlerts() = %+v err=%v", alerts, err)
	}
	alert := &alerts[0]

	// A decision made before the alert existed must not overwrite the newer
	// state: the expected version "absent" is now stale.
	stale := *alert
	stale.Status = StatusResolved
	if err := store.SaveAlertExpected(ctx, stale, nil, "absent"); !errors.Is(err, doctorstore.ErrConflict) {
		t.Fatalf("SaveAlertExpected(stale) error = %v, want ErrConflict", err)
	}
}

func TestClaimOutboxIsExclusiveUntilExpiry(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	record := OutboxRecord{
		ID: "record-1", Tenant: "default", Partition: "payments", Channel: "pd",
		Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.PutOutbox(ctx, record); err != nil {
		t.Fatal(err)
	}

	claimed, ok, err := store.ClaimOutbox(ctx, record, "replica-a", now, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("first claim = %+v ok=%v err=%v", claimed, ok, err)
	}
	if claimed.ClaimedBy != "replica-a" {
		t.Fatalf("claimedBy = %q", claimed.ClaimedBy)
	}

	if _, ok, err := store.ClaimOutbox(ctx, record, "replica-b", now, now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("second claim while owned = ok=%v err=%v, want false/nil", ok, err)
	}
	if _, ok, err := store.ClaimOutbox(ctx, record, "replica-b", now.Add(2*time.Minute), now.Add(3*time.Minute)); err != nil || !ok {
		t.Fatalf("claim after expiry = ok=%v err=%v, want true/nil", ok, err)
	}
}
