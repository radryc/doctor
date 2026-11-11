package query

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/radryc/doctor/internal/quota"
	"github.com/radryc/monofs/pkg/authz"
	"github.com/radryc/monofs/pkg/authz/policy"
)

func TestHandleQuotaReturnsSubjectAndPartitionViews(t *testing.T) {
	bundle := policy.Bundle{
		FormatVersion: 1,
		Quotas: &policy.QuotaBundle{
			Defaults: policy.Quota{
				Query: &policy.QueryQuota{RequestsPerSecond: 20, MaxConcurrent: 4},
			},
			Partitions: map[string]policy.Quota{
				"checkout": {
					Profile: "standard",
					Query:   &policy.QueryQuota{RequestsPerSecond: 100, MaxConcurrent: 20},
				},
			},
			Subjects: map[string]policy.Quota{
				"user:alice": {Query: &policy.QueryQuota{RequestsPerSecond: 50, MaxConcurrent: 8}},
			},
			KnownPartitions: []string{"checkout", "payments"},
		},
	}
	pol := policy.New(bundle)
	provider := quota.NewProvider(quota.ProviderOptions{
		PolicyFunc: func() *policy.Policy { return pol },
	})
	svc := &Service{quotaProvider: provider}

	req := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
	req = req.WithContext(authz.ContextWithIdentity(req.Context(), authz.Identity{Subject: "alice"}))
	rec := httptest.NewRecorder()
	svc.handleQuota(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload quotaDashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Enabled {
		t.Fatal("expected quota to be enabled")
	}
	if len(payload.Partitions) != 2 {
		t.Fatalf("partitions = %d, want 2", len(payload.Partitions))
	}
	if payload.Partitions[0].Name != "checkout" {
		t.Fatalf("first partition = %q, want checkout", payload.Partitions[0].Name)
	}
	if payload.Partitions[0].Quota.Query.RequestsPerSecond != 100 {
		t.Fatalf("checkout rps = %v, want 100", payload.Partitions[0].Quota.Query.RequestsPerSecond)
	}
	if len(payload.Subject) != 1 || payload.Subject[0].Key != "user:alice" {
		t.Fatalf("subject = %#v", payload.Subject)
	}
	if payload.Mode != "observe" {
		t.Fatalf("mode = %q, want observe", payload.Mode)
	}
}
