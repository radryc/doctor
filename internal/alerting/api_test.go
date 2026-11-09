package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAPI(t *testing.T) (*API, *Store) {
	t.Helper()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: false}
	api := &API{Store: store, Service: service, AllowAll: true}
	return api, store
}

func TestRuleAPIRoundTripsGuidedSpec(t *testing.T) {
	api, _ := testAPI(t)
	spec := testSpec(SignalErrors)
	spec.Summary = "checkout is failing"
	rule := AlertRule{Alert: "CheckoutErrors", Mode: ModeGuided, Spec: spec}
	file := RuleFile{Version: 1, Groups: []RuleGroup{{
		Name: "checkout", Tenant: "default", Partition: "payments", Interval: Duration(time.Minute),
		Rules: []AlertRule{rule},
	}}}
	body, _ := json.Marshal(file)
	req := httptest.NewRequest(http.MethodPut, "/v1/alerts/rules/default/payments/checkout?tenant=default&partition=payments", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/alerts/rules/default/payments/checkout?tenant=default&partition=payments", nil)
	req.Header.Set("Accept", "application/json")
	rec = httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got RuleFile
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	ruleOut := got.Groups[0].Rules[0]
	if ruleOut.Mode != ModeGuided || ruleOut.Spec == nil {
		t.Fatalf("rule = %#v", ruleOut)
	}
	if !strings.Contains(ruleOut.Expr, "> 5") {
		t.Fatalf("compiled expr = %s", ruleOut.Expr)
	}
	if ruleOut.Expr != mustCompile(t, spec) {
		t.Fatalf("stored expr differs from compiled expr")
	}
}

func TestRuleCompileEndpoint(t *testing.T) {
	api, _ := testAPI(t)
	spec := testSpec(SignalLatency)
	spec.Primary.Metric = "http_server_request_duration_bucket"
	spec.Primary.Quantile = 0.95
	spec.Condition = Condition{Op: "gt", Value: 1000, Unit: "ms"}
	body, _ := json.Marshal(map[string]any{"spec": spec})
	req := httptest.NewRequest(http.MethodPost, "/v1/alerts/rules/compile?tenant=default&partition=payments", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Expr string `json:"expr"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response.Expr, "histogram_quantile") {
		t.Fatalf("expr = %s", response.Expr)
	}
}

func TestPartitionRoutingIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	payments := PartitionRouting{
		Receivers:       []Receiver{{Name: "payments-pd", Type: "pagerduty", SecretEnv: "PAYMENTS_KEY"}},
		DefaultReceiver: "payments-pd",
	}
	checkout := PartitionRouting{
		Receivers:       []Receiver{{Name: "checkout-slack", Type: "slack", URL: "https://example.invalid"}},
		DefaultReceiver: "checkout-slack",
	}
	if err := store.PutPartitionRouting(ctx, "default", "payments", payments); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPartitionRouting(ctx, "default", "checkout", checkout); err != nil {
		t.Fatal(err)
	}
	gotPayments, err := store.GetPartitionRouting(ctx, "default", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotPayments.Receivers) != 1 || gotPayments.Receivers[0].Name != "payments-pd" {
		t.Fatalf("payments routing = %#v", gotPayments)
	}
	gotCheckout, err := store.GetPartitionRouting(ctx, "default", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotCheckout.Receivers) != 1 || gotCheckout.Receivers[0].Name != "checkout-slack" {
		t.Fatalf("checkout routing = %#v", gotCheckout)
	}
}

func TestPartitionRoutingFallsBackToLegacy(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	if err := store.PutReceiverConfig(ctx, ReceiverConfig{
		Receivers:       []Receiver{{Name: "legacy", Type: "slack", URL: "https://example.invalid"}},
		DefaultReceiver: "legacy",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.GetPartitionRouting(ctx, "default", "payments")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Receivers) != 1 || cfg.Receivers[0].Name != "legacy" || cfg.DefaultReceiver != "legacy" {
		t.Fatalf("legacy fallback = %#v", cfg)
	}
}

func TestAlertSummaryEndpoint(t *testing.T) {
	api, _ := testAPI(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, partition := range []string{"payments", "checkout"} {
		if _, err := api.Service.IngestGuardian(ctx, "default", GuardianAlert{
			Event: "asset.failed", Partition: partition, Status: "firing", Severity: "critical", FiredAt: now,
		}, ""); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/alerts/summary?tenant=default", nil)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Partitions []PartitionAlertSummary `json:"partitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Partitions) != 2 {
		t.Fatalf("partitions = %#v", response.Partitions)
	}
	for _, summary := range response.Partitions {
		if summary.Firing != 1 || summary.Critical != 1 {
			t.Fatalf("summary = %#v", summary)
		}
	}
}

func TestReceiversAPIDeliversAlreadyFiringGuardianAlert(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	service := &Service{Store: store, DefaultTenant: "default", DeliveryEnabled: true}
	api := &API{Store: store, Service: service, AllowAll: true}
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	if _, err := service.IngestGuardian(ctx, "default", GuardianAlert{
		Event: GuardianPartitionAttentionAlert, Partition: "payments", Status: "firing",
		Severity: "warning", Summary: "needs attention", FiredAt: now,
	}, ""); err != nil {
		t.Fatal(err)
	}

	cfg := PartitionRouting{
		Receivers: []Receiver{{Name: "platform-pd", Type: "pagerduty", URL: "https://example.invalid"}},
		AlertRoutes: []AlertRoute{{
			Source:    "guardian",
			Alert:     GuardianPartitionAttentionAlert,
			Receivers: []string{"platform-pd"},
		}},
	}
	body, _ := json.Marshal(cfg)
	req := httptest.NewRequest(http.MethodPut, "/v1/alerts/receivers?tenant=default&partition=payments", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}
	outbox, err := store.ListOutbox(ctx, "default", "payments")
	if err != nil || len(outbox) != 1 || outbox[0].Channel != "platform-pd" {
		t.Fatalf("outbox = %+v err=%v", outbox, err)
	}
}

func mustCompile(t *testing.T, spec *AlertSpec) string {
	t.Helper()
	expr, err := CompileSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	return expr
}
