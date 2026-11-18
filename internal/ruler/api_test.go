package ruler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/radryc/monofs/pkg/authz"
)

func TestAuthenticatedAuthorizer(t *testing.T) {
	authorizer := AuthenticatedAuthorizer{}
	if authorizer.CanModify(context.Background(), "default", "group") {
		t.Fatal("anonymous caller must not modify rules")
	}
	ctx := authz.ContextWithIdentity(context.Background(), authz.Identity{Subject: "doctor-rules-api"})
	if !authorizer.CanModify(ctx, "default", "group") {
		t.Fatal("authenticated caller should modify rules")
	}
}

func TestStatusReportsLeaseAndScheduler(t *testing.T) {
	lease := NewLeaseManager(nil, "/doctor/v1/rules/.lease/leader.json", "ruler-1", 30*time.Second, 10*time.Second, nil)
	api := &API{Scheduler: &Scheduler{}, Lease: lease}

	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/rules/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Enabled bool   `json:"enabled"`
		RulerID string `json:"rulerId"`
		Leader  bool   `json:"leader"`
		Lease   struct {
			RulerID string `json:"rulerId"`
			TTL     string `json:"ttl"`
			Renew   string `json:"renew"`
		} `json:"lease"`
		Scheduler struct {
			Groups int `json:"groups"`
		} `json:"scheduler"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Enabled || payload.RulerID != "ruler-1" || payload.Lease.RulerID != "ruler-1" {
		t.Fatalf("unexpected payload: %s", rec.Body.String())
	}
	if payload.Lease.TTL != "30s" || payload.Lease.Renew != "10s" {
		t.Fatalf("unexpected lease timing: %s", rec.Body.String())
	}
}

func TestAPIEnforcesPerPartitionRulesAuthorizer(t *testing.T) {
	store := NewStore(NewMemoryStore(), "token", "")
	api := &API{Store: store, Authorizer: ScopedRulesAuthorizer{}}
	body := `version: 1
groups:
  - name: shard-health
    interval: 1m
    rules:
      - record: doctor:up:sum
        expr: sum(up)
`
	viewer := authz.ContextWithIdentity(context.Background(), authz.Identity{
		Subject: "rules:viewer",
		Groups:  []string{RulesReadGroupPrefix + "a"},
	})
	req := httptest.NewRequest(http.MethodPut, "/v1/rules/a/shard-health", strings.NewReader(body)).WithContext(viewer)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT status = %d, want 403", rec.Code)
	}

	maintainer := authz.ContextWithIdentity(context.Background(), authz.Identity{
		Subject: "rules:maintainer",
		Groups:  []string{RulesWriteGroupPrefix + "a"},
	})
	req = httptest.NewRequest(http.MethodPut, "/v1/rules/a/shard-health", strings.NewReader(body)).WithContext(maintainer)
	rec = httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("maintainer PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/rules/evaluate?partition=a&group=shard-health", nil).WithContext(viewer)
	rec = httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer evaluate status = %d, want 403", rec.Code)
	}
}

func TestAPIBackfillReplaysTicks(t *testing.T) {
	store := NewStore(NewMemoryStore(), "token", "")
	file, err := ParseFile([]byte(validGroupYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "default", "shard-health", file, false); err != nil {
		t.Fatal(err)
	}
	querier := &fakeQuerier{data: vector(sample(map[string]string{"job": "api"}, "1"))}
	writer := &fakeWriter{}
	evaluator := &Evaluator{Querier: querier, Writer: writer, PartialRetries: 1, RetryDelay: 0}
	api := &API{Store: store, Evaluator: evaluator}

	from := time.Unix(1780000000, 0).UTC()
	to := from.Add(2 * time.Minute)
	target := "/v1/rules/backfill?partition=default&group=shard-health&step=1m&from=" +
		from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodPost, target, nil)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Ticks   int          `json:"ticks"`
		Results []EvalResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Ticks != 3 || len(payload.Results) != 3 {
		t.Fatalf("payload = %#v", payload)
	}
	if len(writer.records) != 3 {
		t.Fatalf("records = %d, want 3", len(writer.records))
	}
}
