package dashboards

import (
	"context"
	"errors"
	"testing"

	"github.com/radryc/doctor/internal/ruler"
)

func newTestStore() *Store {
	return NewStore(ruler.NewMemoryStore(), "", "/doctor/v1/dashboards")
}

func TestListIncludesBuiltInPresets(t *testing.T) {
	s := newTestStore()
	docs, err := s.List(context.Background(), "default", "", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(docs) < 5 {
		t.Fatalf("List returned %d docs, want at least 5 presets", len(docs))
	}
	for _, preset := range builtInPresets() {
		found := false
		for _, doc := range docs {
			if doc.ID == preset.ID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("preset %q missing from list", preset.ID)
		}
	}
}

func TestStoreCRUDAndVersions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore()
	doc := &DashboardDocument{
		Version: Version,
		ID:      "team-red",
		Title:   "Team RED",
		Tenant:  "default",
		Layout:  []DashboardWidget{},
	}
	if err := s.Put(ctx, "default", doc.ID, doc, ""); err != nil {
		t.Fatalf("Put create: %v", err)
	}

	got, err := s.Get(ctx, "default", doc.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != doc.Title || got.Meta == nil || got.Meta.VersionID == "" {
		t.Fatalf("Get returned %#v", got)
	}

	versions, err := s.ListVersions(ctx, "default", doc.ID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}

	doc.Title = "Team RED v2"
	if err := s.Put(ctx, "default", doc.ID, doc, versions[0].VersionID); err != nil {
		t.Fatalf("Put update: %v", err)
	}
	versions, err = s.ListVersions(ctx, "default", doc.ID)
	if err != nil {
		t.Fatalf("ListVersions after update: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions after update = %d, want 2", len(versions))
	}

	if err := s.Put(ctx, "default", doc.ID, doc, versions[1].VersionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Put error = %v, want ErrConflict", err)
	}

	if err := s.Delete(ctx, "default", doc.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "default", doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
}

func TestBuiltInPresetsAreReadOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore()
	preset := builtInPresets()[0]
	if err := s.Put(ctx, "default", preset.ID, &preset, ""); !errors.Is(err, ErrBuiltIn) {
		t.Fatalf("Put built-in error = %v, want ErrBuiltIn", err)
	}
	if err := s.Delete(ctx, "default", preset.ID); !errors.Is(err, ErrBuiltIn) {
		t.Fatalf("Delete built-in error = %v, want ErrBuiltIn", err)
	}
}

func TestParseDocumentAcceptsJSONAndYAML(t *testing.T) {
	jsonDoc := []byte(`{"version":"doctor.dashboard/v1","id":"x","title":"X","tenant":"default"}`)
	if _, err := ParseDocument(jsonDoc); err != nil {
		t.Fatalf("ParseDocument JSON: %v", err)
	}
	yamlDoc := []byte("version: doctor.dashboard/v1\nid: x\ntitle: X\ntenant: default\n")
	if _, err := ParseDocument(yamlDoc); err != nil {
		t.Fatalf("ParseDocument YAML: %v", err)
	}
	if _, err := ParseDocument([]byte(`{"version":"wrong","id":"x","title":"X"}`)); err == nil {
		t.Fatal("ParseDocument accepted an unsupported version")
	}
}
