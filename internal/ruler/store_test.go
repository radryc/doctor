package ruler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rydzu/ainfra/doctor/internal/store"
)

func TestStorePutGetListDelete(t *testing.T) {
	memory := NewMemoryStore()
	ruleStore := NewStore(memory, "token", "/doctor/v1/rules")
	ctx := context.Background()

	file, err := ParseFile([]byte(validGroupYAML))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if err := ruleStore.Put(ctx, "default", "shard-health", file, true); err != nil {
		t.Fatalf("Put: %v", err)
	}

	loaded, err := ruleStore.Get(ctx, "default", "shard-health")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.Groups[0].Tenant != "default" {
		t.Fatalf("tenant = %q", loaded.Groups[0].Tenant)
	}

	refs, err := ruleStore.List(ctx)
	if err != nil || len(refs) != 1 || refs[0].Group != "shard-health" {
		t.Fatalf("refs = %#v, err = %v", refs, err)
	}

	// Updating an existing group works (version precondition), but create-only
	// fails with a conflict.
	if err := ruleStore.Put(ctx, "default", "shard-health", file, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := ruleStore.Put(ctx, "default", "shard-health", file, true); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("create-only err = %v, want conflict", err)
	}

	if err := ruleStore.Delete(ctx, "default", "shard-health"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ruleStore.Get(ctx, "default", "shard-health"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete err = %v, want ErrNotFound", err)
	}
}

func TestStoreGetMissing(t *testing.T) {
	ruleStore := NewStore(NewMemoryStore(), "token", "")
	if _, err := ruleStore.Get(context.Background(), "default", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAPIListPutGetDelete(t *testing.T) {
	memory := NewMemoryStore()
	ruleStore := NewStore(memory, "token", "")
	api := &API{Store: ruleStore, Evaluator: &Evaluator{Querier: &fakeQuerier{data: vector()}, Writer: &fakeWriter{}}}
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	// PUT a group.
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/v1/rules/default/shard-health", strings.NewReader(validGroupYAML))
	resp, err := server.Client().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %v, err = %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// GET list.
	resp, err = server.Client().Get(server.URL + "/v1/rules")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET list status = %v, err = %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// GET group.
	resp, err = server.Client().Get(server.URL + "/v1/rules/default/shard-health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET group status = %v, err = %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Evaluate.
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/rules/evaluate?tenant=default&group=shard-health", nil)
	resp, err = server.Client().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("evaluate status = %v, err = %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// DELETE.
	req, _ = http.NewRequest(http.MethodDelete, server.URL+"/v1/rules/default/shard-health", nil)
	resp, err = server.Client().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %v, err = %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	resp, _ = server.Client().Get(server.URL + "/v1/rules/default/shard-health")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAPIRejectsInvalidRules(t *testing.T) {
	api := &API{Store: NewStore(NewMemoryStore(), "token", "")}
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/v1/rules/default/bad", strings.NewReader("not: rules"))
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAPIHonorsAuthorizer(t *testing.T) {
	api := &API{
		Store:      NewStore(NewMemoryStore(), "token", ""),
		Authorizer: denyAuthorizer{},
	}
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPut, server.URL+"/v1/rules/default/g", strings.NewReader(validGroupYAML))
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

type denyAuthorizer struct{}

func (denyAuthorizer) CanModify(context.Context, string, string) bool { return false }

func TestLeaseTakeover(t *testing.T) {
	memory := NewMemoryStore()
	path := "/doctor/v1/rules/.lease/leader.json"
	now := time.Now().UTC()
	managerA := NewLeaseManager(&FileLeaseStore{Files: memory, Token: "t"}, path, "a", 100*time.Millisecond, 20*time.Millisecond, nil)
	managerA.Now = func() time.Time { return now }
	if !managerA.TryAcquire(context.Background()) {
		t.Fatal("manager A failed to acquire")
	}

	managerB := NewLeaseManager(&FileLeaseStore{Files: memory, Token: "t"}, path, "b", 100*time.Millisecond, 20*time.Millisecond, nil)
	managerB.Now = func() time.Time { return now }
	if managerB.TryAcquire(context.Background()) {
		t.Fatal("manager B acquired a live lease")
	}

	// Advance past expiry: B takes over.
	expired := now.Add(200 * time.Millisecond)
	managerB.Now = func() time.Time { return expired }
	if !managerB.TryAcquire(context.Background()) {
		t.Fatal("manager B failed to take over the expired lease")
	}

	// A cannot renew after B holds the live lease.
	managerA.Now = func() time.Time { return expired }
	if managerA.TryAcquire(context.Background()) {
		t.Fatal("manager A renewed a lease owned by B")
	}
}

func TestLeaseStoreMissingReadsAsEmpty(t *testing.T) {
	leaseStore := &FileLeaseStore{Files: NewMemoryStore(), Token: "t"}
	data, version, err := leaseStore.Read(context.Background(), "/missing")
	if err != nil || data != nil || version != "" {
		t.Fatalf("data=%v version=%q err=%v", data, version, err)
	}
}
