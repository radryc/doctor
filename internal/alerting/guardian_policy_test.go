package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGuardianPolicyStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	policy := GuardianPolicy{
		AlertOnLocked: true,
		Alerts: map[string]GuardianAlertPolicy{
			GuardianPartitionAttentionAlert: {For: Duration(30 * time.Minute)},
			GuardianPartitionErrorAlert:     {For: Duration(5 * time.Minute)},
		},
	}
	if err := store.PutGuardianPolicy(ctx, "default", "payments", policy); err != nil {
		t.Fatalf("PutGuardianPolicy: %v", err)
	}
	got, err := store.GetGuardianPolicy(ctx, "default", "payments")
	if err != nil {
		t.Fatalf("GetGuardianPolicy: %v", err)
	}
	if !got.AlertOnLocked {
		t.Fatal("AlertOnLocked = false, want true")
	}
	if got.Version != 1 || got.Tenant != "default" || got.Partition != "payments" {
		t.Fatalf("policy metadata = %+v", got)
	}
	if got.For(GuardianPartitionAttentionAlert) != 30*time.Minute {
		t.Fatalf("attention for = %s", got.For(GuardianPartitionAttentionAlert))
	}
	if got.For(GuardianPartitionErrorAlert) != 5*time.Minute {
		t.Fatalf("error for = %s", got.For(GuardianPartitionErrorAlert))
	}
	if got.For("UnknownAlert") != 0 {
		t.Fatalf("unknown alert for = %s, want 0", got.For("UnknownAlert"))
	}
}

func TestGuardianPolicyDefaultsWhenMissing(t *testing.T) {
	ctx := context.Background()
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/alerts")
	got, err := store.GetGuardianPolicy(ctx, "default", "payments")
	if err != nil {
		t.Fatalf("GetGuardianPolicy: %v", err)
	}
	if got.AlertOnLocked {
		t.Fatal("AlertOnLocked = true, want false by default")
	}
	if got.For(GuardianPartitionAttentionAlert) != 0 {
		t.Fatal("default attention for is not zero")
	}
}

func TestGuardianPolicyRejectsNegativeDuration(t *testing.T) {
	policy := GuardianPolicy{
		Alerts: map[string]GuardianAlertPolicy{
			GuardianPartitionAttentionAlert: {For: Duration(-time.Minute)},
		},
	}
	if err := policy.Validate(); err == nil {
		t.Fatal("Validate accepted a negative duration")
	}
}

func TestGuardianPolicyAPIRoundTrip(t *testing.T) {
	api, _ := testAPI(t)
	policy := GuardianPolicy{
		Alerts: map[string]GuardianAlertPolicy{
			GuardianPartitionAttentionAlert: {For: Duration(15 * time.Minute)},
		},
	}
	body, _ := json.Marshal(policy)
	req := httptest.NewRequest(http.MethodPut, "/v1/alerts/sources/guardian/policy?tenant=default&partition=payments", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/alerts/sources/guardian/policy?tenant=default&partition=payments", nil)
	rec = httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got GuardianPolicy
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.For(GuardianPartitionAttentionAlert) != 15*time.Minute {
		t.Fatalf("attention for = %s", got.For(GuardianPartitionAttentionAlert))
	}
}

func TestGuardianPolicyAPIRequiresPartition(t *testing.T) {
	api, _ := testAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/alerts/sources/guardian/policy?tenant=default", nil)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
