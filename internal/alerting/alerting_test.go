package alerting

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestFingerprintStableAndLabelSensitive(t *testing.T) {
	labels := map[string]string{"alertname": "HighErrorRate", "service": "checkout", "partition": "payments"}
	first := Fingerprint("sentinel", "default", "payments", labels)
	second := Fingerprint("sentinel", "default", "payments", labels)
	if first != second {
		t.Fatalf("fingerprint changed: %s != %s", first, second)
	}
	labels["service"] = "orders"
	third := Fingerprint("sentinel", "default", "payments", labels)
	if third == first {
		t.Fatal("fingerprint did not change when labels changed")
	}
}

func TestVerifyGuardianSignature(t *testing.T) {
	body := []byte(`{"event":"partition.failing","partition":"payments"}`)
	key := []byte("secret")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !VerifyGuardianSignature(key, body, signature) {
		t.Fatal("valid signature rejected")
	}
	if VerifyGuardianSignature(key, body, signature[:len(signature)-1]+"0") {
		t.Fatal("invalid signature accepted")
	}
	if VerifyGuardianSignature(nil, body, "") != true {
		t.Fatal("empty key should skip verification")
	}
}

func TestAWSPolicySelection(t *testing.T) {
	managed := AlarmMeta{
		Name: "guardian-probe-payments-down",
		Tags: map[string]string{"guardian.io/managed-by": "guardian", "sentinel.io/import": "true"},
	}
	user := AlarmMeta{Name: "my-dev-alarm", Tags: map[string]string{}}
	excluded := AlarmMeta{Name: "guardian-probe-payments-down", Tags: map[string]string{
		"guardian.io/managed-by": "guardian",
		"sentinel.io/import":     "false",
	}}

	policy := DefaultAWSPolicy()
	if got := policy.Select(user); got.Included {
		t.Fatalf("user alarm included by managed-only default: %+v", got)
	}
	if got := policy.Select(managed); !got.Included {
		t.Fatalf("managed alarm excluded: %+v", got)
	}
	if got := policy.Select(excluded); got.Included {
		t.Fatalf("exclude tag did not win: %+v", got)
	}

	all := &AWSPolicy{Default: AWSPolicyImportAll}
	if got := all.Select(user); !got.Included {
		t.Fatalf("import-all did not include user alarm: %+v", got)
	}
	withExclude := &AWSPolicy{
		Default: AWSPolicyImportAll,
		Exclude: []AWSPolicyRule{{NameRegex: "^my-"}},
	}
	if got := withExclude.Select(user); got.Included {
		t.Fatalf("rule exclude did not win: %+v", got)
	}
}

func TestGuardianIntakeDeliversAndSilenceSuppresses(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	received := make(chan Alert, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var alert Alert
		if err := json.Unmarshal(body, &alert); err == nil {
			received <- alert
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := store.PutReceiverConfig(ctx, ReceiverConfig{
		Receivers: []Receiver{{Name: "test", Type: "webhook", URL: server.URL}},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: true, Now: func() time.Time {
		return time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	}}
	payload := GuardianAlert{
		Event: "partition.failing", Partition: "payments", Status: "firing",
		Severity: "critical", Summary: "2 failing intents",
		FiredAt: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
	}
	alert, err := service.IngestGuardian(ctx, "default", payload, "")
	if err != nil {
		t.Fatal(err)
	}
	if alert.Status != StatusFiring {
		t.Fatalf("status = %s", alert.Status)
	}
	dispatcher := &Dispatcher{Store: store}
	if err := dispatcher.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got.Fingerprint != alert.Fingerprint {
			t.Fatalf("delivered fingerprint = %s, want %s", got.Fingerprint, alert.Fingerprint)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook was not delivered")
	}

	silence, err := service.CreateSilence(ctx, Silence{
		Tenant: "default", Partition: "payments",
		Matchers: []Matcher{{Name: "alertname", Operator: "=", Value: "partition.failing"}},
		StartsAt: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC),
		Scope:    "rule",
	}, "tester")
	if err != nil {
		t.Fatal(err)
	}
	resolvedAt := time.Date(2026, 10, 30, 10, 30, 0, 0, time.UTC)
	payload.Status = "resolved"
	payload.Event = "partition.recovered"
	payload.ResolvedAt = &resolvedAt
	payload.Details = map[string]string{"resolvedEvent": "partition.failing"}
	resolved, err := service.IngestGuardian(ctx, "default", payload, "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != StatusResolved {
		t.Fatalf("resolved status = %s", resolved.Status)
	}
	if len(resolved.SilenceIDs) != 1 || resolved.SilenceIDs[0] != silence.ID {
		t.Fatalf("silence ids = %#v", resolved.SilenceIDs)
	}
	if err := dispatcher.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		t.Fatalf("silenced resolve was delivered: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestGoAlertDelivery(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	var forms []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		forms = append(forms, r.PostForm)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("GOALERT_TEST_KEY", "key-123")
	if err := store.PutReceiverConfig(ctx, ReceiverConfig{
		Receivers: []Receiver{{Name: "goalert", Type: "goalert", URL: server.URL + "/api/v2/generic/incoming", SecretEnv: "GOALERT_TEST_KEY"}},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: true}
	alert, err := service.IngestGuardian(ctx, "default", GuardianAlert{
		Event: "partition.failing", Partition: "payments", Status: "firing",
		Severity: "critical", Summary: "2 failing intents", FiredAt: time.Now().UTC(),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &Dispatcher{Store: store}
	if err := dispatcher.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(forms) != 1 {
		t.Fatalf("forms = %d", len(forms))
	}
	if forms[0].Get("token") != "key-123" || forms[0].Get("dedup") != alert.Fingerprint {
		t.Fatalf("goalert firing form = %#v", forms[0])
	}
	if forms[0].Get("action") != "" {
		t.Fatalf("firing action = %q", forms[0].Get("action"))
	}

	now := time.Now().UTC()
	if _, err := service.IngestGuardian(ctx, "default", GuardianAlert{
		Event: "partition.recovered", Partition: "payments", Status: "resolved",
		FiredAt: alert.StartsAt, ResolvedAt: &now,
		Details: map[string]string{"resolvedEvent": "partition.failing"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(forms) != 2 {
		t.Fatalf("forms after resolve = %d", len(forms))
	}
	if forms[1].Get("action") != "close" || forms[1].Get("dedup") != alert.Fingerprint {
		t.Fatalf("goalert resolve form = %#v", forms[1])
	}
}

func TestAckStopsResolveDelivery(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: true}
	alert, err := service.IngestGuardian(ctx, "default", GuardianAlert{
		Event: "partition.failing", Partition: "payments", Status: "firing", FiredAt: time.Now().UTC(),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ack(ctx, "default", "payments", alert.Fingerprint, "tester", true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := service.IngestGuardian(ctx, "default", GuardianAlert{
		Event: "partition.recovered", Partition: "payments", Status: "resolved", FiredAt: alert.StartsAt, ResolvedAt: &now,
		Details: map[string]string{"resolvedEvent": "partition.failing"},
	}, ""); err != nil {
		t.Fatal(err)
	}
	records, err := store.ListOutboxAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("acked alert enqueued notifications: %#v", records)
	}
}
