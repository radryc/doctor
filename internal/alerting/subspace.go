package alerting

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SubspacePartition is the reserved state partition for platform alerts.
const SubspacePartition = "subspace"

// SubspaceRequest is the API payload for a platform alert.
type SubspaceRequest struct {
	Tenant          string            `json:"tenant,omitempty"`
	Source          string            `json:"source,omitempty"`
	SourcePartition string            `json:"sourcePartition,omitempty"`
	Severity        string            `json:"severity,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	Audience        Audience          `json:"audience"`
	DedupeKey       string            `json:"dedupeKey"`
	StartsAt        *time.Time        `json:"startsAt,omitempty"`
	ExpiresAt       *time.Time        `json:"expiresAt,omitempty"`
}

// Validate checks a subspace request.
func (r *SubspaceRequest) Validate() error {
	if strings.TrimSpace(r.DedupeKey) == "" {
		return fmt.Errorf("alerting: subspace dedupeKey is required")
	}
	if r.Audience.Type == "" {
		return fmt.Errorf("alerting: subspace audience type is required")
	}
	switch r.Audience.Type {
	case "tenant-admins", "partition-owners", "partitions", "groups", "all":
	default:
		return fmt.Errorf("alerting: invalid subspace audience type %q", r.Audience.Type)
	}
	if (r.Audience.Type == "partitions" || r.Audience.Type == "partition-owners") && len(r.Audience.Partitions) == 0 {
		return fmt.Errorf("alerting: subspace audience type %q requires partitions", r.Audience.Type)
	}
	if r.Audience.Type == "groups" && len(r.Audience.Groups) == 0 {
		return fmt.Errorf("alerting: subspace audience type groups requires groups")
	}
	return nil
}

// UpsertSubspace creates or updates a platform alert by dedupe key.
func (s *Service) UpsertSubspace(ctx context.Context, req SubspaceRequest, tenant, user string) (*Alert, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if tenant == "" {
		tenant = req.Tenant
	}
	if tenant == "" {
		tenant = s.DefaultTenant
	}
	if tenant == "" {
		tenant = "default"
	}
	labels := map[string]string{
		"alertname": firstNonEmpty(req.Labels["alertname"], "SubspaceEvent"),
		"source":    firstNonEmpty(req.Source, "sentinel"),
		"scope":     "subspace",
		"dedupeKey": req.DedupeKey,
	}
	for key, value := range req.Labels {
		if value != "" && key != "partition" {
			labels[key] = value
		}
	}
	annotations := map[string]string{}
	for key, value := range req.Annotations {
		annotations[key] = value
	}
	if annotations["summary"] == "" {
		annotations["summary"] = labels["alertname"]
	}
	severity := req.Severity
	if severity == "" {
		severity = "warning"
	}
	startsAt := s.now()
	if req.StartsAt != nil && !req.StartsAt.IsZero() {
		startsAt = req.StartsAt.UTC()
	}
	fingerprintLabels := map[string]string{"dedupeKey": req.DedupeKey}
	alert := Alert{
		Fingerprint:     Fingerprint("subspace", tenant, SubspacePartition, fingerprintLabels),
		Scope:           "subspace",
		Source:          firstNonEmpty(req.Source, "sentinel"),
		Tenant:          tenant,
		Partition:       SubspacePartition,
		SourcePartition: req.SourcePartition,
		Status:          StatusFiring,
		Severity:        severity,
		Labels:          labels,
		Annotations:     annotations,
		StartsAt:        startsAt,
		Delivery:        DeliverySentinel,
		Audience:        &req.Audience,
		DedupeKey:       req.DedupeKey,
		ExpiresAt:       req.ExpiresAt,
	}
	alert.ID = alert.Fingerprint
	if _, err := s.Apply(ctx, alert); err != nil {
		return nil, err
	}
	return &alert, nil
}

// ResolveSubspace resolves a platform alert by dedupe key.
func (s *Service) ResolveSubspace(ctx context.Context, tenant, dedupeKey string) (*Alert, error) {
	fingerprint := Fingerprint("subspace", tenant, SubspacePartition, map[string]string{"dedupeKey": dedupeKey})
	alert, err := s.Store.GetAlert(ctx, tenant, SubspacePartition, fingerprint)
	if err != nil {
		return nil, err
	}
	now := s.now()
	alert.Status = StatusResolved
	alert.EndsAt = &now
	return s.Apply(ctx, *alert)
}

// ListSubspace returns subspace alerts visible to the identity.
func (s *Service) ListSubspace(ctx context.Context, tenant string, identity Identity) ([]Alert, error) {
	alerts, err := s.Store.ListAlerts(ctx, tenant, SubspacePartition)
	if err != nil {
		return nil, err
	}
	out := make([]Alert, 0, len(alerts))
	for _, alert := range alerts {
		if visibleToAudience(alert, identity) {
			out = append(out, alert)
		}
	}
	return out, nil
}

func visibleToAudience(alert Alert, identity Identity) bool {
	if identity.Admin || identity.AllowAll {
		return true
	}
	if alert.Audience == nil {
		return false
	}
	switch alert.Audience.Type {
	case "all":
		return len(identity.Partitions) > 0
	case "tenant-admins":
		return false
	case "partitions", "partition-owners":
		for _, partition := range alert.Audience.Partitions {
			if identity.canSeePartition(partition) {
				return true
			}
		}
		return false
	case "groups":
		for _, want := range alert.Audience.Groups {
			for _, actual := range identity.Groups {
				if strings.EqualFold(want, actual) {
					return true
				}
			}
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
