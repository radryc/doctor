package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestGuardianSourceEmitsErrorAndAttentionAlerts(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default"}
	server := guardianPartitionServer(t, []GuardianPartitionSummary{
		{Name: "payments", Status: "Failing", DisplayStatus: "Needs action", FailedIntents: 2},
		{Name: "billing", Status: "Attention", DisplayStatus: "Attention", DriftedIntents: 1},
		{Name: "stable", Status: "Healthy", DisplayStatus: "Stable"},
	})
	defer server.Close()

	source := &GuardianSource{
		BaseURL: server.URL,
		Tenant:  "default",
		Service: service,
		Store:   store,
		Now:     func() time.Time { return time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) },
	}
	summary, err := source.PollOnce(ctx)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if summary.Discovered != 3 || summary.Included != 2 || summary.Healthy != 1 || summary.Imported != 2 {
		t.Fatalf("summary = %+v", summary)
	}

	payments, err := store.ListAlerts(ctx, "default", "payments")
	if err != nil || len(payments) != 1 {
		t.Fatalf("payments alerts = %+v err=%v", payments, err)
	}
	if payments[0].Source != "guardian" || payments[0].Status != StatusFiring || payments[0].Severity != "critical" {
		t.Fatalf("payments alert = %+v", payments[0])
	}
	if payments[0].Labels["alertname"] != GuardianPartitionErrorAlert || payments[0].Labels["guardian_partition"] != "payments" {
		t.Fatalf("payments labels = %#v", payments[0].Labels)
	}
	if payments[0].Annotations["guardian.failed_intents"] != "2" {
		t.Fatalf("payments annotations = %#v", payments[0].Annotations)
	}

	billing, err := store.ListAlerts(ctx, "default", "billing")
	if err != nil || len(billing) != 1 {
		t.Fatalf("billing alerts = %+v err=%v", billing, err)
	}
	if billing[0].Severity != "warning" || billing[0].Labels["alertname"] != GuardianPartitionAttentionAlert {
		t.Fatalf("billing alert = %+v", billing[0])
	}

	stable, err := store.ListAlerts(ctx, "default", "stable")
	if err != nil || len(stable) != 0 {
		t.Fatalf("stable alerts = %+v err=%v", stable, err)
	}
}

func TestGuardianSourceResolvesRecoveredPartition(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default"}

	var mu sync.Mutex
	partitions := []GuardianPartitionSummary{{Name: "payments", Status: "Attention", DisplayStatus: "Attention"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(partitions)
	}))
	defer server.Close()

	source := &GuardianSource{BaseURL: server.URL, Tenant: "default", Service: service, Store: store}
	if _, err := source.PollOnce(ctx); err != nil {
		t.Fatalf("first PollOnce: %v", err)
	}
	alerts, err := store.ListAlerts(ctx, "default", "payments")
	if err != nil || len(alerts) != 1 || alerts[0].Status != StatusFiring {
		t.Fatalf("alerts after attention = %+v err=%v", alerts, err)
	}

	mu.Lock()
	partitions = []GuardianPartitionSummary{{Name: "payments", Status: "Healthy", DisplayStatus: "Stable"}}
	mu.Unlock()
	summary, err := source.PollOnce(ctx)
	if err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	if summary.Resolved != 1 {
		t.Fatalf("summary = %+v, want one resolved alert", summary)
	}
	alerts, err = store.ListAlerts(ctx, "default", "payments")
	if err != nil || len(alerts) != 1 {
		t.Fatalf("alerts after recovery = %+v err=%v", alerts, err)
	}
	if alerts[0].Status != StatusResolved || alerts[0].EndsAt == nil {
		t.Fatalf("alert after recovery = %+v", alerts[0])
	}
}

func TestGuardianSourceAttentionToFailingReplacesAlert(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default"}

	var mu sync.Mutex
	partitions := []GuardianPartitionSummary{{Name: "payments", Status: "Attention", DisplayStatus: "Attention"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(partitions)
	}))
	defer server.Close()

	source := &GuardianSource{BaseURL: server.URL, Tenant: "default", Service: service, Store: store}
	if _, err := source.PollOnce(ctx); err != nil {
		t.Fatalf("first PollOnce: %v", err)
	}

	mu.Lock()
	partitions = []GuardianPartitionSummary{{Name: "payments", Status: "Failing", DisplayStatus: "Needs action"}}
	mu.Unlock()
	summary, err := source.PollOnce(ctx)
	if err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	if summary.Imported != 1 || summary.Resolved != 1 {
		t.Fatalf("summary = %+v, want error import and attention resolve", summary)
	}
	alerts, err := store.ListAlerts(ctx, "default", "payments")
	if err != nil || len(alerts) != 2 {
		t.Fatalf("alerts = %+v err=%v", alerts, err)
	}
	statuses := map[string]string{}
	for _, alert := range alerts {
		statuses[alert.Labels["alertname"]] = alert.Status
	}
	if statuses[GuardianPartitionErrorAlert] != StatusFiring || statuses[GuardianPartitionAttentionAlert] != StatusResolved {
		t.Fatalf("alert statuses = %#v", statuses)
	}
}

func TestGuardianSourceErrorsTakePrecedence(t *testing.T) {
	partition := GuardianPartitionSummary{
		Name:          "payments",
		Status:        "Healthy",
		DisplayStatus: "Stable",
		Errors:        []string{"invalid manifest"},
	}
	if got := guardianPartitionAlertName(partition); got != GuardianPartitionErrorAlert {
		t.Fatalf("alert name = %q, want %q", got, GuardianPartitionErrorAlert)
	}
}

func TestGuardianSourceFallsBackToM2MPartitions(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default"}
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/api/partitions" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/api/m2m/partitions" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-Guardian-Internal-Token"); got != "internal" {
			t.Errorf("internal token = %q", got)
		}
		_ = json.NewEncoder(w).Encode(guardianPartitionEnvelope{Partitions: []GuardianPartitionSummary{
			{Name: "payments", Status: "Attention"},
		}})
	}))
	defer server.Close()

	source := &GuardianSource{BaseURL: server.URL, Token: "internal", Tenant: "default", Service: service, Store: store}
	summary, err := source.PollOnce(ctx)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if summary.Imported != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(paths) != 2 || paths[0] != "/api/partitions" || paths[1] != "/api/m2m/partitions" {
		t.Fatalf("paths = %#v", paths)
	}
}

func guardianPartitionServer(t *testing.T, partitions []GuardianPartitionSummary) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(partitions)
	}))
}
