package alerting

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
	"sync"
	"time"

	doctorstore "github.com/radryc/doctor/internal/store"
	"sigs.k8s.io/yaml"
)

// ErrNotFound reports a missing alert, silence, or config.
var ErrNotFound = errors.New("alerting: not found")

// FileStore is the CAS-capable file surface Sentinel needs. It is implemented
// by internal/store/monofs.GRPCClient and by MemoryStore for tests.
type FileStore interface {
	ReadFile(ctx context.Context, logicalPath string) ([]byte, error)
	ListDir(ctx context.Context, logicalDir string) ([]doctorstore.DirEntry, error)
	UpsertPaths(ctx context.Context, token string, writes []doctorstore.PathWrite, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error)
	DeletePaths(ctx context.Context, token string, deletes []doctorstore.PathDelete, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error)
	ListVersions(ctx context.Context, token, logicalPath string) ([]doctorstore.FileVersion, error)
}

// Store persists alerts, silences, outbox records, and receiver config.
type Store struct {
	Files     FileStore
	Token     string
	Root      string
	Principal string
}

// NewStore builds a store rooted at root (default /doctor/v1/alerts).
func NewStore(files FileStore, token, root string) *Store {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" {
		root = "doctor/v1/alerts"
	}
	return &Store{Files: files, Token: token, Root: "/" + root, Principal: "doctor-sentinel"}
}

func (s *Store) statePath(tenant, partition, fingerprint string) string {
	return fmt.Sprintf("%s/state/%s/%s/%s.json", s.Root, tenant, partition, fingerprint)
}

func (s *Store) silencePath(tenant, partition, id string) string {
	return fmt.Sprintf("%s/silences/%s/%s/%s.yaml", s.Root, tenant, partition, id)
}

func (s *Store) outboxPath(tenant, partition, id string) string {
	return fmt.Sprintf("%s/outbox/%s/%s/%s.json", s.Root, tenant, partition, id)
}

// outboxMarkerDir is a flat index of records that still need delivery. The
// dispatcher lists this one directory instead of walking every tenant and
// partition on every tick.
func (s *Store) outboxMarkerDir() string {
	return s.Root + "/outbox-pending"
}

type outboxMarker struct {
	Tenant    string `json:"tenant"`
	Partition string `json:"partition"`
	ID        string `json:"id"`
}

func (s *Store) outboxMarkerPath(tenant, partition, id string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + partition + "\x00" + id))
	return s.outboxMarkerDir() + "/" + hex.EncodeToString(sum[:]) + ".json"
}

func (s *Store) outboxMarkerWrite(record OutboxRecord) doctorstore.PathWrite {
	data, _ := json.Marshal(outboxMarker{Tenant: record.Tenant, Partition: record.Partition, ID: record.ID})
	return doctorstore.PathWrite{
		LogicalPath: s.outboxMarkerPath(record.Tenant, record.Partition, record.ID),
		Content:     data,
	}
}

func (s *Store) deleteOutboxMarker(ctx context.Context, tenant, partition, id string) error {
	return s.deleteOutboxMarkerAt(ctx, s.outboxMarkerPath(tenant, partition, id))
}

func (s *Store) deleteOutboxMarkerAt(ctx context.Context, path string) error {
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return nil
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
		LogicalPath:       path,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "outbox marker cleanup"})
	return err
}

// GetOutbox loads one outbox record.
func (s *Store) GetOutbox(ctx context.Context, tenant, partition, id string) (*OutboxRecord, error) {
	data, err := s.Files.ReadFile(ctx, s.outboxPath(tenant, partition, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var record OutboxRecord
	if err := yaml.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("alerting: decode outbox: %w", err)
	}
	return &record, nil
}

// ListOutboxPending returns the records referenced by the pending index and
// cleans markers whose records are gone or already terminal.
func (s *Store) ListOutboxPending(ctx context.Context) ([]OutboxRecord, error) {
	dir := s.outboxMarkerDir()
	entries, err := s.Files.ListDir(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]OutboxRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		data, err := s.Files.ReadFile(ctx, dir+"/"+entry.Name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var marker outboxMarker
		if err := json.Unmarshal(data, &marker); err != nil || marker.ID == "" {
			_ = s.deleteOutboxMarkerAt(ctx, dir+"/"+entry.Name)
			continue
		}
		record, err := s.GetOutbox(ctx, marker.Tenant, marker.Partition, marker.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				_ = s.deleteOutboxMarker(ctx, marker.Tenant, marker.Partition, marker.ID)
				continue
			}
			return nil, err
		}
		if record.Status == "failed" || record.Status == "delivered" {
			_ = s.deleteOutboxMarker(ctx, marker.Tenant, marker.Partition, marker.ID)
			continue
		}
		out = append(out, *record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ReconcileOutboxMarkers backfills the pending index for records created
// before the marker index existed. Call it once when a dispatcher starts.
func (s *Store) ReconcileOutboxMarkers(ctx context.Context) error {
	records, err := s.ListOutboxAll(ctx)
	if err != nil {
		return err
	}
	writes := make([]doctorstore.PathWrite, 0, len(records))
	for _, record := range records {
		if record.Status == "failed" || record.Status == "delivered" {
			continue
		}
		writes = append(writes, s.outboxMarkerWrite(record))
	}
	if len(writes) == 0 {
		return nil
	}
	_, err = s.Files.UpsertPaths(ctx, s.Token, writes, doctorstore.MutationContext{
		PrincipalID: s.Principal,
		Reason:      "outbox marker reconcile",
	})
	return err
}

func (s *Store) receiverPath() string {
	return s.Root + "/config/receivers.yaml"
}

func (s *Store) policyPath() string {
	return s.Root + "/config/sources/aws.yaml"
}

// GetAlert loads one alert state.
func (s *Store) GetAlert(ctx context.Context, tenant, partition, fingerprint string) (*Alert, error) {
	data, err := s.Files.ReadFile(ctx, s.statePath(tenant, partition, fingerprint))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var alert Alert
	if err := yaml.Unmarshal(data, &alert); err != nil {
		return nil, fmt.Errorf("alerting: decode alert: %w", err)
	}
	return &alert, nil
}

// ListAlerts loads every alert for a tenant/partition.
func (s *Store) ListAlerts(ctx context.Context, tenant, partition string) ([]Alert, error) {
	dir := fmt.Sprintf("%s/state/%s/%s", s.Root, tenant, partition)
	entries, err := s.Files.ListDir(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Alert, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		fingerprint := strings.TrimSuffix(entry.Name, ".json")
		alert, err := s.GetAlert(ctx, tenant, partition, fingerprint)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, *alert)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].Fingerprint < out[j].Fingerprint
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// alertStateKey identifies one stored alert inside a tenant snapshot.
func alertStateKey(partition, fingerprint string) string {
	return partition + "\x00" + fingerprint
}

// ListTenantAlerts loads every alert for one tenant in one pass. Only the
// partitions that actually have state are touched, so a fleet with mostly
// healthy partitions does not issue two reads per partition.
func (s *Store) ListTenantAlerts(ctx context.Context, tenant string) (map[string]Alert, error) {
	if tenant == "" {
		tenant = "default"
	}
	root := fmt.Sprintf("%s/state/%s", s.Root, tenant)
	entries, err := s.Files.ListDir(ctx, root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]Alert{}, nil
		}
		return nil, err
	}

	out := make(map[string]Alert)
	for _, partitionEntry := range entries {
		if !partitionEntry.IsDir {
			continue
		}
		partition := partitionEntry.Name
		dir := root + "/" + partition
		files, err := s.Files.ListDir(ctx, dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, file := range files {
			if file.IsDir || !strings.HasSuffix(file.Name, ".json") {
				continue
			}
			data, err := s.Files.ReadFile(ctx, dir+"/"+file.Name)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, err
			}
			var alert Alert
			if err := yaml.Unmarshal(data, &alert); err != nil {
				continue
			}
			if alert.Partition == "" {
				alert.Partition = partition
			}
			if alert.Fingerprint == "" {
				alert.Fingerprint = strings.TrimSuffix(file.Name, ".json")
			}
			out[alertStateKey(alert.Partition, alert.Fingerprint)] = alert
		}
	}
	return out, nil
}

// ListAlertSummaries rolls up alert state per partition for one tenant.
func (s *Store) ListAlertSummaries(ctx context.Context, tenant string) ([]PartitionAlertSummary, error) {
	if tenant == "" {
		tenant = "default"
	}
	root := fmt.Sprintf("%s/state/%s", s.Root, tenant)
	entries, err := s.Files.ListDir(ctx, root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]PartitionAlertSummary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir {
			continue
		}
		alerts, err := s.ListAlerts(ctx, tenant, entry.Name)
		if err != nil {
			continue
		}
		summary := PartitionAlertSummary{Tenant: tenant, Partition: entry.Name, Total: len(alerts)}
		for _, alert := range alerts {
			switch alert.Status {
			case StatusFiring:
				summary.Firing++
			case StatusPending:
				summary.Pending++
			}
			if alert.Severity == "critical" && alert.Status != StatusResolved {
				summary.Critical++
			}
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Partition < out[j].Partition })
	return out, nil
}

// GetAlertVersioned loads one alert and the version token that must be used to
// update it. A missing alert returns (nil, "absent", nil).
func (s *Store) GetAlertVersioned(ctx context.Context, tenant, partition, fingerprint string) (*Alert, string, error) {
	path := s.statePath(tenant, partition, fingerprint)
	// List versions before reading so the version token is never newer than the
	// content we decoded.
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
	var alert Alert
	if err := yaml.Unmarshal(data, &alert); err != nil {
		return nil, "", fmt.Errorf("alerting: decode alert: %w", err)
	}
	return &alert, version, nil
}

// SaveAlert writes alert state, optionally with outbox records in the same CAS
// batch.
func (s *Store) SaveAlert(ctx context.Context, alert Alert, outbox []OutboxRecord) error {
	return s.SaveAlertExpected(ctx, alert, outbox, s.latestVersion(ctx, s.statePath(alert.Tenant, alert.Partition, alert.Fingerprint)))
}

// SaveAlertExpected writes alert state only when expected is still the current
// version of the state file. Callers that made a decision from a previously
// read state must use this form to avoid overwriting a concurrent transition.
func (s *Store) SaveAlertExpected(ctx context.Context, alert Alert, outbox []OutboxRecord, expected string) error {
	alert.UpdatedAt = time.Now().UTC()
	data, err := yaml.Marshal(alert)
	if err != nil {
		return fmt.Errorf("alerting: encode alert: %w", err)
	}
	if expected == "" {
		expected = "absent"
	}
	writes := []doctorstore.PathWrite{{
		LogicalPath:       s.statePath(alert.Tenant, alert.Partition, alert.Fingerprint),
		Content:           data,
		ExpectedVersionID: expected,
	}}
	for i := range outbox {
		record := &outbox[i]
		if record.ID == "" {
			record.ID = fmt.Sprintf("%s-%d", alert.Fingerprint, time.Now().UTC().UnixNano())
		}
		if record.CreatedAt.IsZero() {
			record.CreatedAt = time.Now().UTC()
		}
		record.UpdatedAt = time.Now().UTC()
		payload, err := yaml.Marshal(record)
		if err != nil {
			return fmt.Errorf("alerting: encode outbox: %w", err)
		}
		writes = append(writes, doctorstore.PathWrite{
			LogicalPath:       s.outboxPath(record.Tenant, record.Partition, record.ID),
			Content:           payload,
			ExpectedVersionID: s.latestVersion(ctx, s.outboxPath(record.Tenant, record.Partition, record.ID)),
		})
		// The marker is written in the same batch as the record, so the
		// dispatcher can find new work with one directory listing.
		writes = append(writes, s.outboxMarkerWrite(*record))
	}
	_, err = s.Files.UpsertPaths(ctx, s.Token, writes, doctorstore.MutationContext{
		PrincipalID: s.Principal,
		Reason:      "alert state transition",
	})
	return err
}

// DeleteAlert removes alert state.
func (s *Store) DeleteAlert(ctx context.Context, alert Alert) error {
	path := s.statePath(alert.Tenant, alert.Partition, alert.Fingerprint)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return nil
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
		LogicalPath:       path,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "alert resolved"})
	return err
}

// ListSilences returns silences for a partition.
func (s *Store) ListSilences(ctx context.Context, tenant, partition string) ([]Silence, error) {
	dir := fmt.Sprintf("%s/silences/%s/%s", s.Root, tenant, partition)
	entries, err := s.Files.ListDir(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Silence, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".yaml") {
			continue
		}
		data, err := s.Files.ReadFile(ctx, dir+"/"+entry.Name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var silence Silence
		if err := yaml.Unmarshal(data, &silence); err != nil {
			continue
		}
		out = append(out, silence)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// GetSilence loads one silence.
func (s *Store) GetSilence(ctx context.Context, tenant, partition, id string) (*Silence, error) {
	data, err := s.Files.ReadFile(ctx, s.silencePath(tenant, partition, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var silence Silence
	if err := yaml.Unmarshal(data, &silence); err != nil {
		return nil, err
	}
	return &silence, nil
}

// PutSilence creates or updates a silence.
func (s *Store) PutSilence(ctx context.Context, silence Silence) error {
	silence.UpdatedAt = time.Now().UTC()
	if silence.CreatedAt.IsZero() {
		silence.CreatedAt = silence.UpdatedAt
	}
	data, err := yaml.Marshal(silence)
	if err != nil {
		return err
	}
	path := s.silencePath(silence.Tenant, silence.Partition, silence.ID)
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "silence update"})
	return err
}

// DeleteSilence removes a silence.
func (s *Store) DeleteSilence(ctx context.Context, tenant, partition, id string) error {
	path := s.silencePath(tenant, partition, id)
	version := s.latestVersion(ctx, path)
	if version == "absent" {
		return ErrNotFound
	}
	_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
		LogicalPath:       path,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "silence delete"})
	return err
}

// GetReceiverConfig loads notification receivers/routes, returning a default
// local config when none is stored.
func (s *Store) GetReceiverConfig(ctx context.Context) (*ReceiverConfig, error) {
	data, err := s.Files.ReadFile(ctx, s.receiverPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultReceiverConfig(), nil
		}
		return nil, err
	}
	var cfg ReceiverConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// PutReceiverConfig stores notification receivers/routes.
func (s *Store) PutReceiverConfig(ctx context.Context, cfg ReceiverConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	path := s.receiverPath()
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "receiver config update"})
	return err
}

// routingPath is the per-partition notification configuration.
func (s *Store) routingPath(tenant, partition string) string {
	return fmt.Sprintf("%s/config/routing/%s/%s.yaml", s.Root, tenant, partition)
}

// GetPartitionRouting loads a partition's notification configuration. When no
// per-partition file exists it falls back to the legacy global receiver config
// so existing installations keep delivering alerts.
func (s *Store) GetPartitionRouting(ctx context.Context, tenant, partition string) (*PartitionRouting, error) {
	data, err := s.Files.ReadFile(ctx, s.routingPath(tenant, partition))
	if err == nil {
		var cfg PartitionRouting
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, err
		}
		if cfg.Tenant == "" {
			cfg.Tenant = tenant
		}
		if cfg.Partition == "" {
			cfg.Partition = partition
		}
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		return &cfg, nil
	}
	if !isNotExist(err) && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	legacy, legacyErr := s.GetReceiverConfig(ctx)
	if legacyErr != nil {
		return nil, legacyErr
	}
	return &PartitionRouting{
		Version:         1,
		Tenant:          tenant,
		Partition:       partition,
		Receivers:       legacy.Receivers,
		Routes:          legacy.Routes,
		DefaultReceiver: legacy.DefaultReceiver,
	}, nil
}

// PutPartitionRouting stores one partition's notification configuration.
func (s *Store) PutPartitionRouting(ctx context.Context, tenant, partition string, cfg PartitionRouting) error {
	cfg.Tenant = tenant
	cfg.Partition = partition
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	path := s.routingPath(tenant, partition)
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "partition routing update"})
	return err
}

// ListPartitionRouting returns every stored partition routing document.
func (s *Store) ListPartitionRouting(ctx context.Context, tenant string) ([]PartitionRouting, error) {
	root := s.Root + "/config/routing"
	tenants, err := s.Files.ListDir(ctx, root)
	if err != nil {
		if isNotExist(err) || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []PartitionRouting
	for _, tenantEntry := range tenants {
		if !tenantEntry.IsDir || (tenant != "" && tenantEntry.Name != tenant) {
			continue
		}
		partitions, err := s.Files.ListDir(ctx, root+"/"+tenantEntry.Name)
		if err != nil {
			continue
		}
		for _, partitionEntry := range partitions {
			if partitionEntry.IsDir || !strings.HasSuffix(partitionEntry.Name, ".yaml") {
				continue
			}
			partition := strings.TrimSuffix(partitionEntry.Name, ".yaml")
			cfg, err := s.GetPartitionRouting(ctx, tenantEntry.Name, partition)
			if err != nil {
				continue
			}
			out = append(out, *cfg)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Partition < out[j].Partition
	})
	return out, nil
}

// ListOutbox returns pending outbox records for a partition.
func (s *Store) ListOutbox(ctx context.Context, tenant, partition string) ([]OutboxRecord, error) {
	dir := fmt.Sprintf("%s/outbox/%s/%s", s.Root, tenant, partition)
	entries, err := s.Files.ListDir(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]OutboxRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || !strings.HasSuffix(entry.Name, ".json") {
			continue
		}
		data, err := s.Files.ReadFile(ctx, dir+"/"+entry.Name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var record OutboxRecord
		if err := yaml.Unmarshal(data, &record); err != nil {
			continue
		}
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ListOutboxAll walks tenants and partitions. It is used by the delivery loop.
func (s *Store) ListOutboxAll(ctx context.Context) ([]OutboxRecord, error) {
	root := s.Root + "/outbox"
	tenants, err := s.Files.ListDir(ctx, root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []OutboxRecord
	for _, tenant := range tenants {
		if !tenant.IsDir {
			continue
		}
		partitions, err := s.Files.ListDir(ctx, root+"/"+tenant.Name)
		if err != nil {
			continue
		}
		for _, partition := range partitions {
			if !partition.IsDir {
				continue
			}
			records, err := s.ListOutbox(ctx, tenant.Name, partition.Name)
			if err == nil {
				out = append(out, records...)
			}
		}
	}
	return out, nil
}

// PutOutbox updates one outbox record.
func (s *Store) PutOutbox(ctx context.Context, record OutboxRecord) error {
	record.UpdatedAt = time.Now().UTC()
	data, err := yaml.Marshal(record)
	if err != nil {
		return err
	}
	path := s.outboxPath(record.Tenant, record.Partition, record.ID)
	writes := []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}, s.outboxMarkerWrite(record)}
	_, err = s.Files.UpsertPaths(ctx, s.Token, writes, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "outbox update"})
	return err
}

// ClaimOutbox marks a due record as owned by one dispatcher until the given
// time. It returns false when another dispatcher already holds an unexpired
// claim or the record disappeared. The write is guarded by a CAS precondition
// so two replicas cannot both win the same claim.
func (s *Store) ClaimOutbox(ctx context.Context, record OutboxRecord, owner string, now, until time.Time) (OutboxRecord, bool, error) {
	path := s.outboxPath(record.Tenant, record.Partition, record.ID)
	versions, err := s.Files.ListVersions(ctx, s.Token, path)
	if err != nil {
		return OutboxRecord{}, false, err
	}
	if len(versions) == 0 {
		return OutboxRecord{}, false, nil
	}
	version := versions[0].VersionID
	data, err := s.Files.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return OutboxRecord{}, false, nil
		}
		return OutboxRecord{}, false, err
	}
	var current OutboxRecord
	if err := yaml.Unmarshal(data, &current); err != nil {
		return OutboxRecord{}, false, fmt.Errorf("alerting: decode outbox: %w", err)
	}
	if current.ClaimedUntil.After(now) && current.ClaimedBy != owner {
		return OutboxRecord{}, false, nil
	}
	record.ClaimedBy = owner
	record.ClaimedUntil = until
	record.UpdatedAt = time.Now().UTC()
	payload, err := yaml.Marshal(record)
	if err != nil {
		return OutboxRecord{}, false, err
	}
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           payload,
		ExpectedVersionID: version,
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "outbox claim"})
	if err != nil {
		if errors.Is(err, doctorstore.ErrConflict) {
			return OutboxRecord{}, false, nil
		}
		return OutboxRecord{}, false, err
	}
	return record, true, nil
}

// DeleteOutbox removes a delivered record.
func (s *Store) DeleteOutbox(ctx context.Context, record OutboxRecord) error {
	path := s.outboxPath(record.Tenant, record.Partition, record.ID)
	version := s.latestVersion(ctx, path)
	if version != "absent" {
		_, err := s.Files.DeletePaths(ctx, s.Token, []doctorstore.PathDelete{{
			LogicalPath:       path,
			ExpectedVersionID: version,
		}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "outbox delivered"})
		if err != nil {
			return err
		}
	}
	return s.deleteOutboxMarker(ctx, record.Tenant, record.Partition, record.ID)
}

// GetAWSPolicy loads the CloudWatch alarm-selection policy.
func (s *Store) GetAWSPolicy(ctx context.Context) (*AWSPolicy, error) {
	data, err := s.Files.ReadFile(ctx, s.policyPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultAWSPolicy(), nil
		}
		return nil, err
	}
	var policy AWSPolicy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return nil, err
	}
	if policy.Default == "" {
		policy.Default = AWSPolicyManagedOnly
	}
	return &policy, nil
}

// PutAWSPolicy stores the CloudWatch alarm-selection policy.
func (s *Store) PutAWSPolicy(ctx context.Context, policy AWSPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(policy)
	if err != nil {
		return err
	}
	path := s.policyPath()
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "aws alarm selection policy update"})
	return err
}

func (s *Store) gcpPolicyPath() string {
	return s.Root + "/config/sources/gcp.yaml"
}

// GetGCPPolicy loads the Cloud Monitoring policy-selection policy.
func (s *Store) GetGCPPolicy(ctx context.Context) (*GCPPolicy, error) {
	data, err := s.Files.ReadFile(ctx, s.gcpPolicyPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultGCPPolicy(), nil
		}
		return nil, err
	}
	var policy GCPPolicy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return nil, err
	}
	if policy.Default == "" {
		policy.Default = GCPPolicyImportAll
	}
	return &policy, nil
}

// PutGCPPolicy stores the Cloud Monitoring policy-selection policy.
func (s *Store) PutGCPPolicy(ctx context.Context, policy GCPPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(policy)
	if err != nil {
		return err
	}
	path := s.gcpPolicyPath()
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "gcp alert selection policy update"})
	return err
}

func (s *Store) guardianPolicyPath(tenant, partition string) string {
	return fmt.Sprintf("%s/config/sources/guardian/%s/%s.yaml", s.Root, tenant, partition)
}

// GetGuardianPolicy loads one partition's Guardian source policy. A missing
// document uses the defaults so locked drift stays quiet and alerts fire
// immediately.
func (s *Store) GetGuardianPolicy(ctx context.Context, tenant, partition string) (*GuardianPolicy, error) {
	data, err := s.Files.ReadFile(ctx, s.guardianPolicyPath(tenant, partition))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			policy := DefaultGuardianPolicy()
			policy.Tenant = tenant
			policy.Partition = partition
			return policy, nil
		}
		return nil, err
	}
	var policy GuardianPolicy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		return nil, fmt.Errorf("alerting: decode guardian policy: %w", err)
	}
	if policy.Version == 0 {
		policy.Version = 1
	}
	if policy.Tenant == "" {
		policy.Tenant = tenant
	}
	if policy.Partition == "" {
		policy.Partition = partition
	}
	if policy.Alerts == nil {
		policy.Alerts = map[string]GuardianAlertPolicy{}
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &policy, nil
}

// PutGuardianPolicy stores one partition's Guardian source policy.
func (s *Store) PutGuardianPolicy(ctx context.Context, tenant, partition string, policy GuardianPolicy) error {
	policy.Tenant = tenant
	policy.Partition = partition
	if policy.Version == 0 {
		policy.Version = 1
	}
	if policy.Alerts == nil {
		policy.Alerts = map[string]GuardianAlertPolicy{}
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(policy)
	if err != nil {
		return err
	}
	path := s.guardianPolicyPath(tenant, partition)
	_, err = s.Files.UpsertPaths(ctx, s.Token, []doctorstore.PathWrite{{
		LogicalPath:       path,
		Content:           data,
		ExpectedVersionID: s.latestVersion(ctx, path),
	}}, doctorstore.MutationContext{PrincipalID: s.Principal, Reason: "guardian alert policy update"})
	return err
}

func (s *Store) latestVersion(ctx context.Context, logicalPath string) string {
	versions, err := s.Files.ListVersions(ctx, s.Token, logicalPath)
	if err != nil || len(versions) == 0 {
		return "absent"
	}
	return versions[0].VersionID
}

// DefaultReceiverConfig returns a no-op config with a local webhook placeholder.
func DefaultReceiverConfig() *ReceiverConfig {
	return &ReceiverConfig{}
}

// MemoryStore is an in-memory FileStore for tests and local development.
type MemoryStore struct {
	mu       sync.Mutex
	files    map[string][]byte
	versions map[string][]doctorstore.FileVersion
	seq      int
}

// NewMemoryStore creates an empty memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{files: map[string][]byte{}, versions: map[string][]doctorstore.FileVersion{}}
}

// ReadFile implements FileStore.
func (m *MemoryStore) ReadFile(_ context.Context, logicalPath string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[logicalPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

// ListDir implements FileStore.
func (m *MemoryStore) ListDir(_ context.Context, logicalDir string) ([]doctorstore.DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := strings.TrimRight(logicalDir, "/") + "/"
	seen := map[string]doctorstore.DirEntry{}
	for path := range m.files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		if rest == "" {
			continue
		}
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			seen[rest[:idx]] = doctorstore.DirEntry{Name: rest[:idx], IsDir: true}
		} else {
			seen[rest] = doctorstore.DirEntry{Name: rest}
		}
	}
	out := make([]doctorstore.DirEntry, 0, len(seen))
	for _, entry := range seen {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 {
		return nil, os.ErrNotExist
	}
	return out, nil
}

// UpsertPaths implements FileStore.
func (m *MemoryStore) UpsertPaths(_ context.Context, _ string, writes []doctorstore.PathWrite, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	revision := doctorstore.BatchRevision{BatchRevisionID: doctorstore.NewBatchRevisionID()}
	for _, write := range writes {
		if err := m.checkExpected(write.LogicalPath, write.ExpectedVersionID); err != nil {
			return doctorstore.BatchRevision{}, err
		}
		m.seq++
		version := doctorstore.FileVersion{
			LogicalPath:     write.LogicalPath,
			VersionID:       fmt.Sprintf("ver-%d", m.seq),
			BatchRevisionID: revision.BatchRevisionID,
			ContentSHA256:   doctorstore.ContentHash(write.Content),
			PrincipalID:     mutation.PrincipalID,
			Reason:          mutation.Reason,
			CommittedAt:     time.Now().UTC(),
		}
		m.files[write.LogicalPath] = append([]byte(nil), write.Content...)
		m.versions[write.LogicalPath] = append([]doctorstore.FileVersion{version}, m.versions[write.LogicalPath]...)
		revision.Files = append(revision.Files, version)
	}
	return revision, nil
}

// DeletePaths implements FileStore.
func (m *MemoryStore) DeletePaths(_ context.Context, _ string, deletes []doctorstore.PathDelete, mutation doctorstore.MutationContext) (doctorstore.BatchRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	revision := doctorstore.BatchRevision{BatchRevisionID: doctorstore.NewBatchRevisionID()}
	for _, del := range deletes {
		if err := m.checkExpected(del.LogicalPath, del.ExpectedVersionID); err != nil {
			return doctorstore.BatchRevision{}, err
		}
		m.seq++
		version := doctorstore.FileVersion{
			LogicalPath:     del.LogicalPath,
			VersionID:       fmt.Sprintf("ver-%d", m.seq),
			BatchRevisionID: revision.BatchRevisionID,
			Tombstone:       true,
			PrincipalID:     mutation.PrincipalID,
			Reason:          mutation.Reason,
			CommittedAt:     time.Now().UTC(),
		}
		delete(m.files, del.LogicalPath)
		m.versions[del.LogicalPath] = append([]doctorstore.FileVersion{version}, m.versions[del.LogicalPath]...)
		revision.Files = append(revision.Files, version)
	}
	return revision, nil
}

// ListVersions implements FileStore.
func (m *MemoryStore) ListVersions(_ context.Context, _, logicalPath string) ([]doctorstore.FileVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]doctorstore.FileVersion(nil), m.versions[logicalPath]...), nil
}

func (m *MemoryStore) checkExpected(logicalPath, expected string) error {
	switch expected {
	case "":
		return nil
	case "absent":
		if _, exists := m.files[logicalPath]; exists {
			return doctorstore.ErrConflict
		}
		return nil
	default:
		versions := m.versions[logicalPath]
		if len(versions) == 0 || versions[0].VersionID != expected {
			return doctorstore.ErrConflict
		}
		return nil
	}
}
