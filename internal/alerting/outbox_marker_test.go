package alerting

import (
	"context"
	"testing"
	"time"

	doctorstore "github.com/radryc/doctor/internal/store"
	"sigs.k8s.io/yaml"
)

func TestListOutboxPendingUsesMarkerIndex(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	store := NewStore(mem, "token", "/doctor/v1/alerts")
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

	record := OutboxRecord{
		ID: "record-1", Tenant: "default", Partition: "payments",
		Channel: "pd", Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.PutOutbox(ctx, record); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListOutboxPending(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != record.ID {
		t.Fatalf("ListOutboxPending() = %+v err=%v", pending, err)
	}

	// A record written without a marker is invisible to the fast path until
	// reconciliation backfills the index.
	legacy := OutboxRecord{
		ID: "legacy", Tenant: "default", Partition: "billing",
		Channel: "pd", Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
	data, _ := yaml.Marshal(legacy)
	if _, err := mem.UpsertPaths(ctx, "token", []doctorstore.PathWrite{{
		LogicalPath: store.outboxPath(legacy.Tenant, legacy.Partition, legacy.ID),
		Content:     data,
	}}, doctorstore.MutationContext{}); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListOutboxPending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListOutboxPending() before reconcile = %+v err=%v", pending, err)
	}
	if err := store.ReconcileOutboxMarkers(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListOutboxPending(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListOutboxPending() after reconcile = %+v err=%v", pending, err)
	}
}

func TestDeleteOutboxRemovesPendingMarker(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	record := OutboxRecord{
		ID: "record-1", Tenant: "default", Partition: "payments",
		Channel: "pd", Status: "pending",
	}
	if err := store.PutOutbox(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteOutbox(ctx, record); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListOutboxPending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("ListOutboxPending() after delete = %+v err=%v", pending, err)
	}
}
