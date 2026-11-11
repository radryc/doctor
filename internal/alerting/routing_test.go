package alerting

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMatchingReceiversPrecedence(t *testing.T) {
	cfg := &PartitionRouting{
		Receivers: []Receiver{
			{Name: "default", Type: "slack", URL: "https://example.invalid"},
			{Name: "payments-pd", Type: "pagerduty"},
			{Name: "platform-pd", Type: "pagerduty"},
		},
		DefaultReceiver: "default",
		Routes: []Route{{
			Name:      "critical",
			Match:     []Matcher{{Name: "severity", Operator: "=", Value: "critical"}},
			Receivers: []string{"payments-pd"},
		}},
	}
	alert := Alert{
		Tenant:    "default",
		Partition: "payments",
		Labels:    map[string]string{"alertname": "HighErrorRate", "severity": "critical"},
	}
	if got := matchingReceivers(cfg, alert); len(got) != 1 || got[0] != "payments-pd" {
		t.Fatalf("label route = %#v", got)
	}
	cfg.PartitionReceiver = "platform-pd"
	if got := matchingReceivers(cfg, alert); len(got) != 1 || got[0] != "platform-pd" {
		t.Fatalf("partition receiver should beat label route: %#v", got)
	}
	alert.Notify = []string{"payments-pd"}
	if got := matchingReceivers(cfg, alert); len(got) != 1 || got[0] != "payments-pd" {
		t.Fatalf("explicit notify should win: %#v", got)
	}
	alert.Notify = nil
	alert.Partition = "checkout"
	alert.Labels["severity"] = "info"
	cfg.PartitionReceiver = ""
	if got := matchingReceivers(cfg, alert); len(got) != 1 || got[0] != "default" {
		t.Fatalf("unmatched alert should use default: %#v", got)
	}
}

func TestRouteRegexAndContinue(t *testing.T) {
	cfg := &PartitionRouting{
		Receivers: []Receiver{{Name: "a"}, {Name: "b"}},
		Routes: []Route{
			{Name: "critical", Match: []Matcher{{Name: "severity", Operator: "=~", Value: "critical|page"}}, Receivers: []string{"a"}, Continue: true},
			{Name: "payments", Receivers: []string{"b"}},
		},
	}
	alert := Alert{Tenant: "default", Partition: "payments", Labels: map[string]string{"severity": "critical"}}
	got := matchingReceivers(cfg, alert)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("continue route = %#v", got)
	}
	alert.Labels["severity"] = "info"
	if got := matchingReceivers(cfg, alert); len(got) != 1 || got[0] != "b" {
		t.Fatalf("non-critical route = %#v", got)
	}
}

func TestAlertRoutesBeatPartitionDestination(t *testing.T) {
	cfg := &PartitionRouting{
		Receivers: []Receiver{
			{Name: "default", Type: "slack"},
			{Name: "platform-pd", Type: "pagerduty"},
			{Name: "guardian-pd", Type: "pagerduty"},
		},
		PartitionReceiver: "platform-pd",
		AlertRoutes: []AlertRoute{{
			Source:    "guardian",
			Alert:     GuardianPartitionAttentionAlert,
			Receivers: []string{"guardian-pd"},
		}},
		DefaultReceiver: "default",
	}
	alert := Alert{
		Tenant:    "default",
		Partition: "payments",
		Source:    "guardian",
		Labels:    map[string]string{"alertname": GuardianPartitionAttentionAlert, "source": "guardian"},
	}
	if got := matchingReceivers(cfg, alert); len(got) != 1 || got[0] != "guardian-pd" {
		t.Fatalf("source alert route = %#v", got)
	}
	alert.Labels["alertname"] = GuardianPartitionErrorAlert
	if got := matchingReceivers(cfg, alert); len(got) != 1 || got[0] != "platform-pd" {
		t.Fatalf("unrouted source alert should use partition destination: %#v", got)
	}
}

func TestReconcileRoutingDeliversExistingFiringAlerts(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: true}
	service.Now = func() time.Time { return now }

	if _, err := service.IngestGuardian(ctx, "default", GuardianAlert{
		Event: GuardianPartitionAttentionAlert, Partition: "payments", Status: "firing",
		Severity: "warning", Summary: "partition needs attention", FiredAt: now,
	}, ""); err != nil {
		t.Fatal(err)
	}
	// Without routing the alert is stored but nothing is queued.
	if outbox, err := store.ListOutbox(ctx, "default", "payments"); err != nil || len(outbox) != 0 {
		t.Fatalf("outbox before routing = %+v err=%v", outbox, err)
	}

	if err := store.PutPartitionRouting(ctx, "default", "payments", PartitionRouting{
		Receivers: []Receiver{{Name: "platform-pd", Type: "pagerduty", URL: "https://example.invalid"}},
		AlertRoutes: []AlertRoute{{
			Source:    "guardian",
			Alert:     GuardianPartitionAttentionAlert,
			Receivers: []string{"platform-pd"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	enqueued, err := service.ReconcileRouting(ctx, "default", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", enqueued)
	}
	outbox, err := store.ListOutbox(ctx, "default", "payments")
	if err != nil || len(outbox) != 1 || outbox[0].Channel != "platform-pd" {
		t.Fatalf("outbox after routing = %+v err=%v", outbox, err)
	}
	alert, err := store.GetAlert(ctx, "default", "payments", outbox[0].Fingerprint)
	if err != nil || alert.LastNotifiedAt == nil {
		t.Fatalf("alert after routing = %+v err=%v", alert, err)
	}

	// A second save must not duplicate the notification.
	enqueued, err = service.ReconcileRouting(ctx, "default", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 0 {
		t.Fatalf("second reconcile enqueued = %d, want 0", enqueued)
	}
}

func TestPagerDutyDeliveryTriggerAndResolve(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			payloads = append(payloads, payload)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("PAGERDUTY_TEST_KEY", "routing-key-123")
	if err := store.PutReceiverConfig(ctx, ReceiverConfig{
		Receivers: []Receiver{{
			Name:      "pagerduty",
			Type:      "pagerduty",
			URL:       server.URL,
			SecretEnv: "PAGERDUTY_TEST_KEY",
		}},
		DefaultReceiver: "pagerduty",
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: true}
	now := time.Now().UTC().Truncate(time.Second)
	service.Now = func() time.Time { return now }
	alert, err := service.IngestGuardian(ctx, "default", GuardianAlert{
		Event: "partition.failing", Partition: "payments", Status: "firing",
		Severity: "critical", Summary: "2 failing intents", FiredAt: now,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &Dispatcher{Store: store}
	if err := dispatcher.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 {
		t.Fatalf("payloads = %d", len(payloads))
	}
	if payloads[0]["routing_key"] != "routing-key-123" || payloads[0]["event_action"] != "trigger" {
		t.Fatalf("trigger payload = %#v", payloads[0])
	}
	if payloads[0]["dedup_key"] != alert.Fingerprint {
		t.Fatalf("dedup key = %#v", payloads[0]["dedup_key"])
	}
	body, _ := payloads[0]["payload"].(map[string]any)
	if body["severity"] != "critical" {
		t.Fatalf("severity = %#v", body["severity"])
	}

	resolvedAt := time.Now().UTC()
	now = resolvedAt
	if _, err := service.IngestGuardian(ctx, "default", GuardianAlert{
		Event: "partition.recovered", Partition: "payments", Status: "resolved",
		FiredAt: alert.StartsAt, ResolvedAt: &resolvedAt,
		Details: map[string]string{"resolvedEvent": "partition.failing"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 2 {
		t.Fatalf("payloads after resolve = %d", len(payloads))
	}
	if payloads[1]["event_action"] != "resolve" || payloads[1]["dedup_key"] != alert.Fingerprint {
		t.Fatalf("resolve payload = %#v", payloads[1])
	}
}
