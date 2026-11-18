package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAlertDefinitionsIncludesRulesGuardianAndUnmanaged(t *testing.T) {
	api, store := testAPI(t)
	ctx := context.Background()

	spec := testSpec(SignalErrors)
	spec.Severity = "warning"
	spec.Summary = "checkout error budget"
	file := &RuleFile{Version: 1, Groups: []RuleGroup{{
		Name:      "checkout",
		Tenant:    "default",
		Partition: "payments",
		Interval:  Duration(time.Minute),
		Rules: []AlertRule{{
			Alert: "CheckoutErrors",
			Mode:  ModeGuided,
			Spec:  spec,
		}},
	}}}
	if err := store.PutRuleGroup(ctx, "default", "payments", "checkout", file); err != nil {
		t.Fatalf("PutRuleGroup: %v", err)
	}

	now := time.Now().UTC()
	alerts := []Alert{
		{
			ID: "g1", Fingerprint: "fp-guardian", Source: "guardian", Tenant: "default", Partition: "payments",
			Status: StatusResolved, Severity: "critical",
			Labels:      map[string]string{"alertname": "GuardianPartitionError", "partition": "payments", "source": "guardian"},
			Annotations: map[string]string{"summary": "recovered"},
			StartsAt:    now.Add(-time.Hour), EndsAt: &now, Delivery: "sentinel", CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "t1", Fingerprint: "fp-smoke", Source: "test", Tenant: "default", Partition: "payments",
			Status: StatusSilenced, Severity: "critical",
			Labels:      map[string]string{"alertname": "DoctorAndroidSmoke2", "partition": "payments", "source": "test"},
			Annotations: map[string]string{"summary": "Guardian FCM smoke test"},
			StartsAt:    now, Delivery: "sentinel", CreatedAt: now, UpdatedAt: now,
		},
	}
	for _, alert := range alerts {
		if err := store.SaveAlert(ctx, alert, nil); err != nil {
			t.Fatalf("SaveAlert(%s): %v", alert.ID, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/alerts/inventory?tenant=default&partition=payments", nil)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Definitions []AlertDefinition `json:"definitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}

	byName := map[string]AlertDefinition{}
	for _, def := range response.Definitions {
		byName[def.Name] = def
	}

	rule, ok := byName["CheckoutErrors"]
	if !ok {
		t.Fatalf("missing CheckoutErrors in %+v", response.Definitions)
	}
	if !rule.Defined || rule.Source != "sentinel" || rule.Engine == "" || rule.Expr == "" {
		t.Fatalf("rule definition = %+v", rule)
	}
	if rule.Instances.Total != 0 {
		t.Fatalf("rule instances = %+v, want none", rule.Instances)
	}

	guardian, ok := byName["GuardianPartitionError"]
	if !ok || !guardian.Defined || guardian.Source != "guardian" {
		t.Fatalf("guardian definition = %+v", guardian)
	}
	if guardian.Instances.Resolved != 1 || guardian.Instances.Total != 1 {
		t.Fatalf("guardian instances = %+v", guardian.Instances)
	}

	if _, ok := byName["aws-cloudwatch"]; !ok {
		t.Fatal("missing aws-cloudwatch definition")
	}
	if _, ok := byName["gcp-monitoring"]; !ok {
		t.Fatal("missing gcp-monitoring definition")
	}

	unmanaged, ok := byName["DoctorAndroidSmoke2"]
	if !ok {
		t.Fatalf("missing unmanaged test alert in %+v", response.Definitions)
	}
	if unmanaged.Defined || unmanaged.Source != "test" || unmanaged.Instances.Silenced != 1 {
		t.Fatalf("unmanaged definition = %+v", unmanaged)
	}
}

func TestAlertDefinitionsEnforcesPartitionAccess(t *testing.T) {
	api, _ := testAPI(t)
	api.AllowAll = false

	req := httptest.NewRequest(http.MethodGet, "/v1/alerts/inventory?tenant=default&partition=payments", nil)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("denied status = %d, want 404", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/alerts/inventory?tenant=default&partition=payments", nil)
	req.Header.Set("X-Doctor-User", "usera")
	req.Header.Set("X-Doctor-Partition-Access", "payments")
	rec = httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed status = %d body=%s", rec.Code, rec.Body.String())
	}
}
