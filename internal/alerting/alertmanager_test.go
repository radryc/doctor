package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAlertmanagerStatusAndAlerts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default"}
	api := &API{Store: store, Service: service, AllowAll: true, Now: func() time.Time { return now }}
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	if err := store.PutPartitionRouting(ctx, "default", "payments", PartitionRouting{
		Version:         1,
		Receivers:       []Receiver{{Name: "payments-pager", Type: "webhook"}},
		DefaultReceiver: "payments-pager",
	}); err != nil {
		t.Fatal(err)
	}

	seedAlertmanagerAlert(t, store, Alert{
		ID:          "fp-payments",
		Fingerprint: "fp-payments",
		Source:      "guardian",
		Tenant:      "default",
		Partition:   "payments",
		Status:      StatusFiring,
		Severity:    "critical",
		Labels:      map[string]string{"alertname": GuardianPartitionErrorAlert, "partition": "payments"},
		Annotations: map[string]string{"summary": "payments is failing"},
		StartsAt:    now.Add(-10 * time.Minute),
		CreatedAt:   now.Add(-10 * time.Minute),
		UpdatedAt:   now.Add(-1 * time.Minute),
	})
	seedAlertmanagerAlert(t, store, Alert{
		ID:          "fp-billing",
		Fingerprint: "fp-billing",
		Source:      "guardian",
		Tenant:      "default",
		Partition:   "billing",
		Status:      StatusPending,
		Severity:    "warning",
		Labels:      map[string]string{"alertname": GuardianPartitionAttentionAlert, "partition": "billing"},
		Annotations: map[string]string{"summary": "billing needs attention"},
		StartsAt:    now.Add(-2 * time.Minute),
		CreatedAt:   now.Add(-2 * time.Minute),
		UpdatedAt:   now.Add(-2 * time.Minute),
	})
	seedAlertmanagerAlert(t, store, Alert{
		ID:          "fp-stable",
		Fingerprint: "fp-stable",
		Source:      "guardian",
		Tenant:      "default",
		Partition:   "stable",
		Status:      StatusResolved,
		Severity:    "warning",
		Labels:      map[string]string{"alertname": GuardianPartitionAttentionAlert, "partition": "stable"},
		StartsAt:    now.Add(-30 * time.Minute),
		CreatedAt:   now.Add(-30 * time.Minute),
		UpdatedAt:   now.Add(-5 * time.Minute),
	})
	if err := store.PutSilence(ctx, Silence{
		ID:        "sil-payments",
		Tenant:    "default",
		Partition: "payments",
		Matchers:  []Matcher{{Name: "partition", Operator: "=", Value: "payments"}},
		StartsAt:  now.Add(-time.Hour),
		EndsAt:    now.Add(time.Hour),
		CreatedBy: "oncall",
		Comment:   "maintenance",
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	var status amStatus
	getJSON(t, server.URL+"/api/v2/status", &status)
	if status.Cluster.Status != "disabled" || status.VersionInfo.GoVersion == "" {
		t.Fatalf("status = %+v", status)
	}
	if status.Config.Original == "" || !strings.Contains(status.Config.Original, "payments-pager") {
		t.Fatalf("status config = %q", status.Config.Original)
	}
	if strings.Contains(status.Config.Original, "Receivers:") || !strings.Contains(status.Config.Original, "receivers:") {
		t.Fatalf("status config keys are not Alertmanager YAML: %q", status.Config.Original)
	}

	var alerts []amAlert
	getJSON(t, server.URL+"/api/v2/alerts", &alerts)
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v, want firing and pending alerts", alerts)
	}
	byFingerprint := map[string]amAlert{}
	for _, alert := range alerts {
		byFingerprint[alert.Fingerprint] = alert
	}
	payments := byFingerprint["fp-payments"]
	if payments.Status.State != "suppressed" || len(payments.Status.SilencedBy) != 1 || payments.Status.SilencedBy[0] != "sil-payments" {
		t.Fatalf("payments status = %+v", payments.Status)
	}
	if len(payments.Receivers) != 1 || payments.Receivers[0].Name != "payments-pager" {
		t.Fatalf("payments receivers = %+v", payments.Receivers)
	}
	if payments.Labels["partition"] != "payments" || payments.Labels["tenant"] != "default" {
		t.Fatalf("payments labels = %+v", payments.Labels)
	}
	billing := byFingerprint["fp-billing"]
	if billing.Status.State != "unprocessed" {
		t.Fatalf("billing status = %+v", billing.Status)
	}

	getJSON(t, server.URL+`/api/v2/alerts?filter=partition%3D%22payments%22`, &alerts)
	if len(alerts) != 1 || alerts[0].Fingerprint != "fp-payments" {
		t.Fatalf("filtered alerts = %+v", alerts)
	}

	getJSON(t, server.URL+`/api/v2/alerts?silenced=false`, &alerts)
	if len(alerts) != 1 || alerts[0].Fingerprint != "fp-billing" {
		t.Fatalf("silenced=false alerts = %+v", alerts)
	}

	getJSON(t, server.URL+`/api/v2/alerts?receiver=payments-pager`, &alerts)
	if len(alerts) != 1 || alerts[0].Fingerprint != "fp-payments" {
		t.Fatalf("receiver-filtered alerts = %+v", alerts)
	}

	var groups []amAlertGroup
	getJSON(t, server.URL+"/api/v2/alerts/groups", &groups)
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}

	var silences []amSilence
	getJSON(t, server.URL+"/api/v2/silences", &silences)
	if len(silences) != 1 || silences[0].Status.State != "active" || !silences[0].Matchers[0].IsEqual {
		t.Fatalf("silences = %+v", silences)
	}
	var silence amSilence
	getJSON(t, server.URL+"/api/v2/silence/sil-payments", &silence)
	if silence.ID != "sil-payments" {
		t.Fatalf("silence = %+v", silence)
	}
}

func TestAlertmanagerRespectsPartitionAccess(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default"}
	api := &API{Store: store, Service: service, Now: func() time.Time { return now }}
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	seedAlertmanagerAlert(t, store, Alert{
		ID: "fp-payments", Fingerprint: "fp-payments", Source: "guardian", Tenant: "default", Partition: "payments",
		Status: StatusFiring, Severity: "critical", Labels: map[string]string{"alertname": "PaymentsDown"},
		StartsAt: now, CreatedAt: now, UpdatedAt: now,
	})
	seedAlertmanagerAlert(t, store, Alert{
		ID: "fp-billing", Fingerprint: "fp-billing", Source: "guardian", Tenant: "default", Partition: "billing",
		Status: StatusFiring, Severity: "warning", Labels: map[string]string{"alertname": "BillingDown"},
		StartsAt: now, CreatedAt: now, UpdatedAt: now,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v2/alerts", nil)
	req.Header.Set("X-Doctor-User", "operator@example.com")
	req.Header.Set("X-Doctor-Partition-Access", "payments")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var alerts []amAlert
	if err := json.Unmarshal(rec.Body.Bytes(), &alerts); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Fingerprint != "fp-payments" {
		t.Fatalf("alerts = %+v, want payments only", alerts)
	}
}

func seedAlertmanagerAlert(t *testing.T, store *Store, alert Alert) {
	t.Helper()
	if err := store.SaveAlert(context.Background(), alert, nil); err != nil {
		t.Fatal(err)
	}
}

func getJSON(t *testing.T, url string, target any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}
