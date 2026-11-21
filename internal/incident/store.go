package incident

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/alerting"
	doctorstore "github.com/radryc/doctor/internal/store"
	"sigs.k8s.io/yaml"
)

// ErrNotFound reports a missing set, incident, policy, or postmortem.
var ErrNotFound = errors.New("incident: not found")

// ActiveRef maps one set + grouping key to the incident that owns it.
type ActiveRef struct {
	IncidentID string     `json:"incidentId"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
}

// Store persists alert sets, incidents, policies, postmortems, and active
// grouping indexes. Incidents and their notifications share the alert store's
// root so the existing outbox dispatcher delivers both domains.
type Store struct {
	Files  alerting.FileStore
	Outbox *alerting.Store
	Token  string
	Root   string

	Principal string
}

// NewStore builds an incident store rooted at the Sentinel store root.
func NewStore(files alerting.FileStore, outbox *alerting.Store, token, root string) *Store {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" {
		root = "doctor/v1/alerts"
	}
	return &Store{
		Files:     files,
		Outbox:    outbox,
		Token:     token,
		Root:      "/" + root,
		Principal: "doctor-sentinel",
	}
}

func (s *Store) setPath(tenant, partition, name string) string {
	return fmt.Sprintf("%s/alert-sets/%s/%s/%s.yaml", s.Root, tenant, partition, name)
}

func (s *Store) setsDir(tenant string) string {
	return fmt.Sprintf("%s/alert-sets/%s", s.Root, tenant)
}

func (s *Store) incidentPath(tenant, partition, id string) string {
	return fmt.Sprintf("%s/incidents/%s/%s/%s.yaml", s.Root, tenant, partition, id)
}

func (s *Store) incidentsDir(tenant, partition string) string {
	return fmt.Sprintf("%s/incidents/%s/%s", s.Root, tenant, partition)
}

func (s *Store) activePath(tenant, partition, set, groupKey string) string {
	hash := sha256.Sum256([]byte(set + "\x00" + groupKey))
	return fmt.Sprintf("%s/incident-active/%s/%s/%s.json", s.Root, tenant, partition, hex.EncodeToString(hash[:])[:24])
}

func (s *Store) policyPath(tenant, partition string) string {
	return fmt.Sprintf("%s/config/incident-policy/%s/%s.yaml", s.Root, tenant, partition)
}

func (s *Store) postmortemPath(tenant, partition, incidentID string) string {
	return fmt.Sprintf("%s/postmortems/%s/%s/%s.json", s.Root, tenant, partition, incidentID)
}

func (s *Store) latestVersion(ctx context.Context, logicalPath string) string {
	versions, err := s.Files.ListVersions(ctx, s.Token, logicalPath)
	if err != nil || len(versions) == 0 {
		return "absent"
	}
	return versions[0].VersionID
}

// ListAlertSets returns every set for a tenant.
func (s *Store) ListAlertSets(ctx context.Context, tenant string) ([]AlertSet, error) {
	partitions, err := s.Files.ListDir(ctx, s.setsDir(tenant))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []AlertSet
	for _, partition := range partitions {
		if !partition.IsDir {
			continue
		}
		dir := s.setsDir(tenant) + "/" + partition.Name
		entries, err := s.Files.ListDir(ctx, dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir || !strings.HasSuffix(entry.Name, ".yaml") {
				continue
			}
			data, err := s.Files.ReadFile(ctx, dir+"/"+entry.Name)
			if err != nil {
				continue
			}
			var set AlertSet
			if err := yaml.Unmarshal(data, &set); err != nil {
				continue
			}
			set.Normalize()
			out = append(out, set)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Partition != out[j].Partition {
			return out[i].Partition < out[j].Partition
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// GetAlertSet loads one set and its version token.
func (s *Store) GetAlertSet(ctx context.Context, tenant, partition, name string) (*AlertSet, string, error) {
	path := s.setPath(tenant, partition, name)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return nil, "absent", nil
	}
	data, err := s.Files.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "absent", nil
		}
		return nil, "", err
	}
	var set AlertSet
	if err := yaml.Unmarshal(data, &set); err != nil {
		return nil, "", fmt.Errorf("incident: decode alert set: %w", err)
	}
	set.Normalize()
	return &set, version, nil
}

// PutAlertSet creates or replaces a set. expected is the CAS token read by the
// caller ("absent" for a new set).
func (s *Store) PutAlertSet(ctx context.Context, set AlertSet, expected string) error {
	now := time.Now().UTC()
	if set.CreatedAt.IsZero() {
		set.CreatedAt = now
	}
	set.UpdatedAt = now
	data, err := yaml.Marshal(set)
	if err != nil {
		return err
	}
	if expected == "" {
		expected = "absent"
	}
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       s.setPath(set.Tenant, set.Partition, set.Name),
		Content:           data,
		ExpectedVersionID: expected,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "alert set update"})
	return err
}

// DeleteAlertSet removes a set.
func (s *Store) DeleteAlertSet(ctx context.Context, tenant, partition, name string) error {
	path := s.setPath(tenant, partition, name)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return nil
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
		LogicalPath:       path,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "alert set delete"})
	return err
}

// ListIncidents returns incidents for one partition.
func (s *Store) ListIncidents(ctx context.Context, tenant, partition string) ([]Incident, error) {
	dir := s.incidentsDir(tenant, partition)
	entries, err := s.Files.ListDir(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Incident, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".yaml") {
			continue
		}
		data, err := s.Files.ReadFile(ctx, dir+"/"+entry.Name)
		if err != nil {
			continue
		}
		var incident Incident
		if err := yaml.Unmarshal(data, &incident); err != nil {
			continue
		}
		out = append(out, incident)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.After(out[j].OpenedAt) })
	return out, nil
}

// ListTenantIncidents walks every partition for a tenant.
func (s *Store) ListTenantIncidents(ctx context.Context, tenant string) ([]Incident, error) {
	partitions, err := s.Files.ListDir(ctx, fmt.Sprintf("%s/incidents/%s", s.Root, tenant))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Incident
	for _, partition := range partitions {
		if !partition.IsDir {
			continue
		}
		incidents, err := s.ListIncidents(ctx, tenant, partition.Name)
		if err == nil {
			out = append(out, incidents...)
		}
	}
	return out, nil
}

// FindIncident locates one incident by ID across every partition of a tenant.
func (s *Store) FindIncident(ctx context.Context, tenant, id string) (*Incident, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	incidents, err := s.ListTenantIncidents(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for i := range incidents {
		if incidents[i].ID == id {
			return &incidents[i], nil
		}
	}
	return nil, nil
}

// GetIncident loads one incident and its version token.
func (s *Store) GetIncident(ctx context.Context, tenant, partition, id string) (*Incident, string, error) {
	path := s.incidentPath(tenant, partition, id)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return nil, "absent", nil
	}
	data, err := s.Files.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "absent", nil
		}
		return nil, "", err
	}
	var incident Incident
	if err := yaml.Unmarshal(data, &incident); err != nil {
		return nil, "", fmt.Errorf("incident: decode: %w", err)
	}
	return &incident, version, nil
}

// SaveIncident writes the incident, optional active index, and notifications in
// one CAS batch. expected is the version token read by the caller.
func (s *Store) SaveIncident(ctx context.Context, incident Incident, expected string, active *ActiveRef, outbox []alerting.OutboxRecord) error {
	incident.UpdatedAt = time.Now().UTC()
	data, err := yaml.Marshal(incident)
	if err != nil {
		return err
	}
	if expected == "" {
		expected = "absent"
	}
	writes := []doctorstore.PathWrite{{
		LogicalPath:       s.incidentPath(incident.Tenant, incident.Partition, incident.ID),
		Content:           data,
		ExpectedVersionID: expected,
	}}
	if active != nil {
		payload, err := json.Marshal(active)
		if err != nil {
			return err
		}
		path := s.activePath(incident.Tenant, incident.Partition, incident.Set, incident.GroupKey)
		writes = append(writes, doctorstore.PathWrite{
			LogicalPath:       path,
			Content:           payload,
			ExpectedVersionID: s.latestVersion(ctx, path),
		})
	}
	if s.Outbox != nil && len(outbox) > 0 {
		outboxWrites, err := s.Outbox.OutboxWrites(ctx, outbox)
		if err != nil {
			return err
		}
		writes = append(writes, outboxWrites...)
	}
	_, err = s.Files.UpsertPaths(ctx, s.Token, writes, doctorstore.MutationContext{
		PrincipalID: s.Principal,
		Reason:      "incident transition",
	})
	return err
}

// GetActive loads the active incident reference for a set + grouping key.
func (s *Store) GetActive(ctx context.Context, tenant, partition, set, groupKey string) (*ActiveRef, error) {
	path := s.activePath(tenant, partition, set, groupKey)
	data, err := s.Files.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var ref ActiveRef
	if err := json.Unmarshal(data, &ref); err != nil {
		return nil, err
	}
	return &ref, nil
}

// DeleteActive removes the grouping index for a set + grouping key.
func (s *Store) DeleteActive(ctx context.Context, tenant, partition, set, groupKey string) error {
	path := s.activePath(tenant, partition, set, groupKey)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return nil
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
		LogicalPath:       path,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "incident resolved"})
	return err
}

// GetPolicy returns the partition policy, or the 30-minute default.
func (s *Store) GetPolicy(ctx context.Context, tenant, partition string) (Policy, error) {
	path := s.policyPath(tenant, partition)
	data, err := s.Files.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			policy := DefaultPolicy()
			return policy, nil
		}
		return Policy{}, err
	}
	var policy Policy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return Policy{}, fmt.Errorf("incident: decode policy: %w", err)
	}
	policy.Normalize()
	return policy, nil
}

// PutPolicy stores a partition policy.
func (s *Store) PutPolicy(ctx context.Context, tenant, partition string, policy Policy) error {
	policy.Normalize()
	data, err := yaml.Marshal(policy)
	if err != nil {
		return err
	}
	path := s.policyPath(tenant, partition)
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "incident policy update"})
	return err
}

// GetPostmortem loads a postmortem and its version token.
func (s *Store) GetPostmortem(ctx context.Context, tenant, partition, incidentID string) (*Postmortem, string, error) {
	path := s.postmortemPath(tenant, partition, incidentID)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return nil, "absent", nil
	}
	data, err := s.Files.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "absent", nil
		}
		return nil, "", err
	}
	var doc Postmortem
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, "", err
	}
	return &doc, version, nil
}

// SavePostmortem writes a postmortem document.
func (s *Store) SavePostmortem(ctx context.Context, doc Postmortem, expected string) error {
	doc.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if expected == "" {
		expected = "absent"
	}
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       s.postmortemPath(doc.Tenant, doc.Partition, doc.IncidentID),
		Content:           data,
		ExpectedVersionID: expected,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "postmortem update"})
	return err
}
