package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGuardianPollHealthyUsesTenantSnapshot(t *testing.T) {
	const partitions = 50
	summaries := make([]GuardianPartitionSummary, partitions)
	for i := range summaries {
		summaries[i] = GuardianPartitionSummary{Name: fmt.Sprintf("part-%03d", i), Status: "healthy"}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(summaries)
	}))
	defer server.Close()

	files := &countingFileStore{MemoryStore: NewMemoryStore()}
	store := NewStore(files, "token", "/doctor/v1/alerts")
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	source := &GuardianSource{
		BaseURL: server.URL,
		Tenant:  "default",
		Service: &Service{Store: store, DefaultTenant: "default", Now: func() time.Time { return now }},
		Store:   store,
		HTTP:    server.Client(),
		Now:     func() time.Time { return now },
	}
	ctx := context.Background()
	if _, err := source.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := files.reads.Load(); got != 0 {
		t.Fatalf("healthy poll reads = %d, want 0 (tenant snapshot should answer)", got)
	}
	if got := files.lists.Load(); got == 0 {
		t.Fatal("healthy poll did not list the tenant state directory")
	}

	// The second poll is served entirely from the cached snapshot.
	listsAfterFirst := files.lists.Load()
	if _, err := source.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := files.reads.Load(); got != 0 {
		t.Fatalf("cached healthy poll reads = %d, want 0", got)
	}
	if got := files.lists.Load(); got != listsAfterFirst {
		t.Fatalf("cached poll performed %d additional list calls", got-listsAfterFirst)
	}
}

func TestGuardianSnapshotRefreshesAfterTTL(t *testing.T) {
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	source := &GuardianSource{
		BaseURL: "http://guardian.invalid",
		Tenant:  "default",
		Service: &Service{Store: store, DefaultTenant: "default", Now: func() time.Time { return now }},
		Store:   store,
		Now:     func() time.Time { return now },
	}

	// Seed an alert after the first (empty) snapshot. A TTL refresh must pick it
	// up so a recovered partition can resolve it.
	if _, err := source.loadState(context.Background()); err != nil {
		t.Fatal(err)
	}
	alert := source.normalizePartitionAlert(GuardianPartitionSummary{Name: "payments", Status: "Failing"}, GuardianPartitionErrorAlert, "critical", now)
	if err := store.SaveAlert(context.Background(), alert, nil); err != nil {
		t.Fatal(err)
	}

	// Within the TTL the snapshot is intentionally stale.
	state, err := source.loadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state[alertStateKey("payments", alert.Fingerprint)]; ok {
		t.Fatal("snapshot refreshed before TTL")
	}

	now = now.Add(2 * time.Minute)
	state, err = source.loadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if existing, ok := state[alertStateKey("payments", alert.Fingerprint)]; !ok || existing.Status != StatusFiring {
		t.Fatalf("snapshot after TTL = %+v, want firing alert", existing)
	}
}
