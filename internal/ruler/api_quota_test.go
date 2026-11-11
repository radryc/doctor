package ruler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/radryc/monofs/pkg/authz/policy"
)

type staticLimits struct{ quota policy.RulesQuota }

func (s staticLimits) RulesQuota(string) policy.RulesQuota { return s.quota }

func TestAPIRuleCountQuota(t *testing.T) {
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/rules")
	api := &API{Store: store, Limits: staticLimits{quota: policy.RulesQuota{MaxRules: 2, MaxGroups: 1}}}
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	body := `
version: 1
groups:
  - name: g
    partition: checkout
    rules:
      - record: a
        expr: up
      - record: b
        expr: up
      - record: c
        expr: up
`
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/v1/rules/checkout/g", strings.NewReader(body))
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

func TestAPIRuleCountAllowsUpdateWithinQuota(t *testing.T) {
	store := NewStore(NewMemoryStore(), "token", "/doctor/v1/rules")
	api := &API{Store: store, Limits: staticLimits{quota: policy.RulesQuota{MaxRules: 2, MaxGroups: 1}}}
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	body := `
version: 1
groups:
  - name: g
    partition: checkout
    rules:
      - record: a
        expr: up
`
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPut, server.URL+"/v1/rules/checkout/g", strings.NewReader(body))
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("PUT %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %d status = %d, want 200", i, resp.StatusCode)
		}
	}
}
