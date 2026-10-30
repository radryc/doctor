package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAWSSourceSelectionAndNormalization(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: false}
	alarms := []CloudWatchAlarm{
		{
			Name: "guardian-probe-payments-down", ARN: "arn:aws:cloudwatch:eu-west-1:1:alarm:guardian-probe-payments-down",
			Account: "1", Region: "eu-west-1", Type: "metric", State: "ALARM",
			StateReason: "probe failed", StateUpdatedAt: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
			Tags: map[string]string{
				"guardian.io/managed-by": "guardian",
				"guardian.io/partition":  "payments",
				"sentinel.io/import":     "true",
				"sentinel.io/severity":   "critical",
			},
		},
		{
			Name: "user-dev-alarm", ARN: "arn:aws:cloudwatch:eu-west-1:1:alarm:user-dev-alarm",
			Account: "1", Region: "eu-west-1", Type: "metric", State: "ALARM",
			Tags: map[string]string{},
		},
		{
			Name: "guardian-probe-checkout-down", ARN: "arn:aws:cloudwatch:eu-west-1:1:alarm:guardian-probe-checkout-down",
			Account: "1", Region: "eu-west-1", Type: "metric", State: "ALARM",
			Tags: map[string]string{
				"guardian.io/managed-by": "guardian",
				"guardian.io/partition":  "checkout",
				"sentinel.io/import":     "false",
			},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(alarmEnvelope{Alarms: alarms, Count: len(alarms)})
	}))
	defer server.Close()

	source := &AWSSource{
		BaseURL: server.URL, Tenant: "default", Service: service, Store: store,
		Now: func() time.Time { return time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC) },
	}
	summary, err := source.PollOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Discovered != 3 || summary.Included != 1 || summary.Excluded != 2 || summary.Imported != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	alerts, err := store.ListAlerts(ctx, "default", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v", alerts)
	}
	if alerts[0].Source != "aws" || alerts[0].Status != StatusFiring || alerts[0].Severity != "critical" {
		t.Fatalf("alert = %+v", alerts[0])
	}
	if alerts[0].Labels["alertname"] != "guardian-probe-payments-down" {
		t.Fatalf("labels = %#v", alerts[0].Labels)
	}

	// State OK must resolve the same fingerprint.
	alarms[0].State = "OK"
	alarms[0].StateUpdatedAt = time.Date(2026, 10, 30, 10, 5, 0, 0, time.UTC)
	if _, err := source.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	alerts, err = store.ListAlerts(ctx, "default", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Status != StatusResolved {
		t.Fatalf("resolved alerts = %+v", alerts)
	}
}

func TestAWSPolicyImportAllStillHonorsExcludeTag(t *testing.T) {
	policy := &AWSPolicy{Default: AWSPolicyImportAll}
	meta := AlarmMeta{
		Name: "user-alarm",
		Tags: map[string]string{
			"sentinel.io/import":    "false",
			"sentinel.io/partition": "payments",
		},
	}
	if selection := policy.Select(meta); selection.Included {
		t.Fatalf("exclude tag did not win: %+v", selection)
	}
}
