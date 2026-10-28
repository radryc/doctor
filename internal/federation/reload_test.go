package federation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/radryc/doctor/internal/proxy"
)

func TestManagerReloadSwapsStateAndPrunesChangedClients(t *testing.T) {
	catalogV1, err := NewCatalog(testSnapshot("v1").Targets, "v1")
	if err != nil {
		t.Fatalf("NewCatalog(v1) error = %v", err)
	}
	fakes := map[string]*fakeAWSClient{}
	var mu sync.Mutex
	manager := NewManager(catalogV1, Options{
		ClientFactory: func(target Target) (proxy.AWSClient, error) {
			mu.Lock()
			defer mu.Unlock()
			if fakes[target.ID] == nil {
				fakes[target.ID] = &fakeAWSClient{}
			}
			return fakes[target.ID], nil
		},
	})
	if _, err := manager.clientFor(catalogV1.Targets()[0]); err != nil {
		t.Fatalf("clientFor(v1) error = %v", err)
	}

	catalogV2, err := NewCatalog(testSnapshot("v2").Targets, "v2")
	if err != nil {
		t.Fatalf("NewCatalog(v2) error = %v", err)
	}
	if err := manager.Reload(catalogV2, "monofs:v2"); err != nil {
		t.Fatalf("Reload(v2) error = %v", err)
	}
	if got := manager.Version(); got != "v2" {
		t.Fatalf("version = %q, want v2", got)
	}
	if status := manager.Status(); status["source"] != "monofs:v2" || status["version"] != "v2" {
		t.Fatalf("status = %#v", status)
	}

	// A changed endpoint must not reuse the old client.
	targetsV3 := testSnapshot("v3").Targets
	targetsV3[0].Endpoint = "http://changed"
	catalogV3, err := NewCatalog(targetsV3, "v3")
	if err != nil {
		t.Fatalf("NewCatalog(v3) error = %v", err)
	}
	if err := manager.Reload(catalogV3, "monofs:v3"); err != nil {
		t.Fatalf("Reload(v3) error = %v", err)
	}
	if _, stillCached := manager.pool.get(targetsV3[0]); stillCached {
		t.Fatal("changed target kept its old client")
	}
}

func TestCatalogReloaderReloadsNewVersion(t *testing.T) {
	store := &MemorySnapshotStore{}
	if _, err := store.Save(context.Background(), testSnapshot("v1"), ""); err != nil {
		t.Fatalf("Save(v1) error = %v", err)
	}
	catalogV1, err := NewCatalog(testSnapshot("v1").Targets, "v1")
	if err != nil {
		t.Fatalf("NewCatalog(v1) error = %v", err)
	}
	manager := NewManager(catalogV1, Options{
		ClientFactory: func(target Target) (proxy.AWSClient, error) { return &fakeAWSClient{}, nil },
	})
	reloader := NewCatalogReloader(store, manager, "monofs", time.Minute, nil)
	if err := reloader.Reload(context.Background()); err != nil {
		t.Fatalf("Reload(same) error = %v", err)
	}
	if manager.Version() != "v1" {
		t.Fatalf("version = %q, want v1", manager.Version())
	}

	if _, err := store.Save(context.Background(), testSnapshot("v2"), "v1"); err != nil {
		t.Fatalf("Save(v2) error = %v", err)
	}
	if err := reloader.Reload(context.Background()); err != nil {
		t.Fatalf("Reload(v2) error = %v", err)
	}
	if got := manager.Version(); got != "v2" {
		t.Fatalf("version = %q, want v2", got)
	}

	// Corrupt the next version by making the target invalid. The old catalog
	// must remain active.
	invalid := testSnapshot("v3")
	invalid.Targets[0].Endpoint = ""
	if _, err := store.Save(context.Background(), invalid, "v2"); err != nil {
		t.Fatalf("Save(v3) error = %v", err)
	}
	if err := reloader.Reload(context.Background()); err == nil {
		t.Fatal("expected reload validation error")
	}
	if got := manager.Version(); got != "v2" {
		t.Fatalf("version after failed reload = %q, want v2", got)
	}
}

func TestCatalogReloaderMissingSnapshotKeepsCurrent(t *testing.T) {
	store := &MemorySnapshotStore{}
	catalog, err := NewCatalog(testSnapshot("v1").Targets, "v1")
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	manager := NewManager(catalog, Options{
		ClientFactory: func(target Target) (proxy.AWSClient, error) { return &fakeAWSClient{}, nil },
	})
	reloader := NewCatalogReloader(store, manager, "monofs", time.Minute, nil)
	if err := reloader.Reload(context.Background()); err != nil {
		t.Fatalf("Reload(empty) error = %v", err)
	}
	if got := manager.Version(); got != "v1" {
		t.Fatalf("version = %q, want v1", got)
	}
}
