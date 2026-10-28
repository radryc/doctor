package dashboards

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/ruler"
	"github.com/radryc/doctor/internal/store"
	"sigs.k8s.io/yaml"
)

// ErrNotFound is returned when a dashboard does not exist.
var ErrNotFound = errors.New("dashboards: not found")

// ErrBuiltIn is returned when attempting to mutate a built-in preset.
var ErrBuiltIn = errors.New("dashboards: cannot modify a built-in preset")

// ErrConflict is returned when a CAS precondition fails.
var ErrConflict = errors.New("dashboards: version conflict")

// Store persists dashboard documents in a FileStore (MonoFS CAS or MemoryStore).
// The layout mirrors internal/ruler/store.go exactly.
type Store struct {
	Files     ruler.FileStore
	Token     string
	Root      string
	Principal string
}

// NewStore creates a dashboard store rooted at root.
// root defaults to "/doctor/v1/dashboards" when empty.
func NewStore(files ruler.FileStore, token, root string) *Store {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" {
		root = "doctor/v1/dashboards"
	}
	return &Store{
		Files:     files,
		Token:     token,
		Root:      "/" + root,
		Principal: "doctor-query",
	}
}

func (s *Store) docPath(tenant, id string) string {
	return fmt.Sprintf("%s/%s/%s.yaml", s.Root, tenant, id)
}

// Get loads one dashboard. Built-in presets take priority over stored overrides.
func (s *Store) Get(ctx context.Context, tenant, id string) (*DashboardDocument, error) {
	// Check built-ins first.
	if doc := builtInByID(id); doc != nil {
		return doc, nil
	}
	data, err := s.Files.ReadFile(ctx, s.docPath(tenant, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	doc, err := ParseDocument(data)
	if err != nil {
		return nil, err
	}
	// Populate meta.VersionID from the latest stored version.
	versions, _ := s.Files.ListVersions(ctx, s.Token, s.docPath(tenant, id))
	if len(versions) > 0 && doc.Meta == nil {
		doc.Meta = &DashboardMeta{}
	}
	if len(versions) > 0 {
		doc.Meta.VersionID = versions[0].VersionID
		if !versions[0].CommittedAt.IsZero() {
			doc.Meta.UpdatedAt = versions[0].CommittedAt.UTC().Format(time.RFC3339)
		}
		doc.Meta.UpdatedBy = versions[0].PrincipalID
	}
	return doc, nil
}

// List returns all dashboards for a tenant: built-in presets first, then stored shared dashboards.
// Filtering by tags and/or a search query is applied when non-empty.
func (s *Store) List(ctx context.Context, tenant, tag, search string) ([]DashboardDocument, error) {
	out := make([]DashboardDocument, 0)

	// Collect built-in presets.
	for _, preset := range builtInPresets() {
		if matchDashboard(&preset, tenant, tag, search) {
			out = append(out, preset)
		}
	}

	// Collect stored shared dashboards.
	tenants, err := s.Files.ListDir(ctx, s.Root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, nil // return presets even if store is unavailable
	}
	for _, t := range tenants {
		if !t.IsDir {
			continue
		}
		// Match requested tenant or wildcard.
		if tenant != "" && t.Name != tenant && t.Name != "*" {
			continue
		}
		files, _ := s.Files.ListDir(ctx, s.Root+"/"+t.Name)
		for _, entry := range files {
			if entry.IsDir || !strings.HasSuffix(entry.Name, ".yaml") {
				continue
			}
			id := strings.TrimSuffix(entry.Name, ".yaml")
			// Skip built-ins already in the list.
			if builtInByID(id) != nil {
				continue
			}
			doc, err := s.Get(ctx, t.Name, id)
			if err != nil {
				continue
			}
			if matchDashboard(doc, tenant, tag, search) {
				out = append(out, *doc)
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].BuiltIn != out[j].BuiltIn {
			return out[i].BuiltIn
		}
		return out[i].Title < out[j].Title
	})
	return out, nil
}

// Put creates or updates a shared dashboard. It refuses to overwrite built-in presets.
// When ifMatch is non-empty it is used as the CAS ExpectedVersionID.
func (s *Store) Put(ctx context.Context, tenant, id string, doc *DashboardDocument, ifMatch string) error {
	if builtInByID(id) != nil {
		return ErrBuiltIn
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("dashboards: encode: %w", err)
	}
	expected := ifMatch
	if expected == "" {
		expected = s.latestVersion(ctx, s.docPath(tenant, id))
	}
	_, err = s.Files.UpsertPaths(ctx, s.Token, []store.PathWrite{{
		LogicalPath:       s.docPath(tenant, id),
		Content:           data,
		ExpectedVersionID: expected,
	}}, store.MutationContext{PrincipalID: s.Principal, Reason: "dashboard update"})
	if err != nil {
		if strings.Contains(err.Error(), "conflict") || strings.Contains(err.Error(), "version") {
			return ErrConflict
		}
		return err
	}
	return nil
}

// Delete removes a shared dashboard. Returns ErrBuiltIn for presets and ErrNotFound if absent.
func (s *Store) Delete(ctx context.Context, tenant, id string) error {
	if builtInByID(id) != nil {
		return ErrBuiltIn
	}
	expected := s.latestVersion(ctx, s.docPath(tenant, id))
	if expected == "absent" {
		return ErrNotFound
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []store.PathDelete{{
		LogicalPath:       s.docPath(tenant, id),
		ExpectedVersionID: expected,
	}}, store.MutationContext{PrincipalID: s.Principal, Reason: "dashboard delete"})
	return err
}

// ListVersions returns the MonoFS revision history for a stored dashboard.
func (s *Store) ListVersions(ctx context.Context, tenant, id string) ([]DashboardVersion, error) {
	if builtInByID(id) != nil {
		return nil, nil
	}
	versions, err := s.Files.ListVersions(ctx, s.Token, s.docPath(tenant, id))
	if err != nil {
		return nil, err
	}
	out := make([]DashboardVersion, 0, len(versions))
	for _, v := range versions {
		dv := DashboardVersion{
			VersionID:       v.VersionID,
			BatchRevisionID: v.BatchRevisionID,
			ContentSHA256:   v.ContentSHA256,
			CommittedAt:     v.CommittedAt,
			PrincipalID:     v.PrincipalID,
			Reason:          v.Reason,
		}
		out = append(out, dv)
	}
	return out, nil
}

func (s *Store) latestVersion(ctx context.Context, logicalPath string) string {
	versions, err := s.Files.ListVersions(ctx, s.Token, logicalPath)
	if err != nil || len(versions) == 0 {
		return "absent"
	}
	return versions[0].VersionID
}

// --- helpers ---

func builtInByID(id string) *DashboardDocument {
	for _, p := range builtInPresets() {
		if p.ID == id {
			clone := p
			return &clone
		}
	}
	return nil
}

func matchDashboard(doc *DashboardDocument, tenant, tag, search string) bool {
	if tenant != "" && doc.Tenant != "*" && doc.Tenant != tenant {
		return false
	}
	if tag != "" {
		found := false
		for _, t := range doc.Tags {
			if t == tag {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if search != "" {
		q := strings.ToLower(search)
		if !strings.Contains(strings.ToLower(doc.Title), q) &&
			!strings.Contains(strings.ToLower(doc.Description), q) &&
			!strings.Contains(strings.ToLower(doc.ID), q) {
			return false
		}
	}
	return true
}
