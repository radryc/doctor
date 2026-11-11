package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGCPSourceSelectionAndNormalization(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: false}

	alerts := []GCPAlert{
		{
			Name:        "projects/demo/alertPolicies/1",
			DisplayName: "guardian-probe-payments-down",
			State:       "firing",
			Severity:    "warning",
			Summary:     "probe failed",
			Partition:   "payments",
			Project:     "demo",
			Region:      "europe-west1",
			StartedAt:   time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
			Labels:      map[string]string{"sentinel.io/import": "true", "sentinel.io/severity": "critical"},
		},
		{
			Name:        "projects/demo/alertPolicies/2",
			DisplayName: "user-dev-policy",
			State:       "firing",
			Project:     "demo",
			Labels:      map[string]string{},
		},
		{
			Name:        "projects/demo/alertPolicies/3",
			DisplayName: "guardian-probe-checkout-down",
			State:       "firing",
			Partition:   "checkout",
			Project:     "demo",
			Labels:      map[string]string{"sentinel.io/import": "false"},
		},
		{
			Name:        "projects/demo/alertPolicies/4",
			DisplayName: "disabled-policy",
			State:       "disabled",
			Partition:   "payments",
			Project:     "demo",
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(gcpAlertEnvelope{Alerts: alerts})
	}))
	defer server.Close()

	source := &GCPSource{
		BaseURL:     server.URL,
		Project:     "demo",
		Region:      "europe-west1",
		Tenant:      "default",
		Service:     service,
		Store:       store,
		DefaultMode: GCPPolicyImportAll,
		Now:         func() time.Time { return time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC) },
	}
	summary, err := source.PollOnce(ctx)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	// discovered=4, excluded=2 (import=false + disabled), unscoped=1 (no partition), imported=1
	if summary.Discovered != 4 || summary.Included != 1 || summary.Excluded != 2 || summary.Unscoped != 1 || summary.Imported != 1 {
		t.Fatalf("summary = %+v", summary)
	}

	imported, err := store.ListAlerts(ctx, "default", "payments")
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("alerts = %+v", imported)
	}
	alert := imported[0]
	if alert.Source != "gcp" || alert.Status != StatusFiring || alert.Severity != "warning" {
		t.Fatalf("alert = %+v", alert)
	}
	if alert.Labels["alertname"] != "guardian-probe-payments-down" || alert.Labels["project"] != "demo" {
		t.Fatalf("labels = %#v", alert.Labels)
	}
	if alert.Annotations["console_url"] == "" {
		t.Fatalf("annotations = %#v", alert.Annotations)
	}
}

func TestGCPSourceResolved(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(gcpAlertEnvelope{Alerts: []GCPAlert{{
			Name:        "projects/demo/alertPolicies/1",
			DisplayName: "guardian-probe-payments-down",
			State:       "ok",
			Severity:    "warning",
			Partition:   "payments",
			Project:     "demo",
		}}})
	}))
	defer server.Close()

	source := &GCPSource{BaseURL: server.URL, Tenant: "default", Service: service, Store: store}
	if _, err := source.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	imported, err := store.ListAlerts(ctx, "default", "payments")
	if err != nil || len(imported) != 1 {
		t.Fatalf("alerts = %+v err=%v", imported, err)
	}
	if imported[0].Status != StatusResolved || imported[0].EndsAt == nil {
		t.Fatalf("alert = %+v", imported[0])
	}
}

func TestGCPPolicy(t *testing.T) {
	alert := GCPAlert{DisplayName: "guardian-probe-payments-down", Severity: "critical", Labels: map[string]string{"team": "payments"}}

	policy := DefaultGCPPolicy()
	if !policy.Select(alert).Included {
		t.Fatal("default policy should import")
	}

	policy = &GCPPolicy{Default: GCPPolicyIgnoreAll, Include: []GCPPolicyRule{{NameRegex: "^guardian-"}}}
	if !policy.Select(alert).Included {
		t.Fatal("include regex should match")
	}
	if policy.Select(GCPAlert{DisplayName: "user-policy"}).Included {
		t.Fatal("ignore-all should exclude unmatched policies")
	}

	policy = &GCPPolicy{Default: GCPPolicyImportAll, Exclude: []GCPPolicyRule{{Severity: "critical"}}}
	if policy.Select(alert).Included {
		t.Fatal("exclude should win")
	}

	policy = &GCPPolicy{Default: "bogus"}
	if err := policy.Validate(); err == nil {
		t.Fatal("expected an invalid mode error")
	}
}

func TestGCPPolicyLabelImportOverrides(t *testing.T) {
	policy := &GCPPolicy{Default: GCPPolicyIgnoreAll}
	if policy.Select(GCPAlert{Labels: map[string]string{"sentinel.io/import": "true"}}).Included != true {
		t.Fatal("import=true should override the default")
	}
	if policy.Select(GCPAlert{Labels: map[string]string{"sentinel.io/import": "false"}}).Included {
		t.Fatal("import=false should never be included")
	}
}
