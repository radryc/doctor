package alerting

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	doctorstore "github.com/radryc/doctor/internal/store"
)

// GuardianAlert mirrors guardian/internal/notify.Alert.
type GuardianAlert struct {
	Event      string            `json:"event"`
	Partition  string            `json:"partition"`
	Status     string            `json:"status"`
	Severity   string            `json:"severity"`
	Summary    string            `json:"summary"`
	Details    map[string]string `json:"details,omitempty"`
	FiredAt    time.Time         `json:"firedAt"`
	ResolvedAt *time.Time        `json:"resolvedAt,omitempty"`
}

// Service coordinates intake, state transitions, and notification routing.
type Service struct {
	Store           *Store
	DefaultTenant   string
	GuardianHMACKey []byte
	DeliveryEnabled bool
	Querier         Querier
	Logger          *log.Logger
	Now             func() time.Time
}

// IngestGuardian validates and normalizes a Guardian alert.
func (s *Service) IngestGuardian(ctx context.Context, tenant string, payload GuardianAlert, _ string) (*Alert, error) {
	if tenant == "" {
		tenant = s.DefaultTenant
	}
	if tenant == "" {
		tenant = "default"
	}
	if strings.TrimSpace(payload.Partition) == "" {
		return nil, fmt.Errorf("alerting: guardian alert partition is required")
	}
	status := StatusFiring
	if payload.Status == "resolved" || strings.HasSuffix(payload.Event, ".recovered") {
		status = StatusResolved
	}
	severity := strings.TrimSpace(payload.Severity)
	if severity == "" {
		severity = "warning"
	}
	firedAt := payload.FiredAt
	if firedAt.IsZero() {
		firedAt = s.now()
	}
	alertName := payload.Event
	if status == StatusResolved {
		if resolvedEvent := strings.TrimSpace(payload.Details["resolvedEvent"]); resolvedEvent != "" {
			alertName = resolvedEvent
		}
	}
	labels := map[string]string{
		"alertname": alertName,
		"partition": payload.Partition,
		"source":    "guardian",
	}
	annotations := map[string]string{"summary": payload.Summary, "event": payload.Event}
	for key, value := range payload.Details {
		annotations["guardian."+key] = value
	}
	alert := &Alert{
		Fingerprint: Fingerprint("guardian", tenant, payload.Partition, labels),
		Source:      "guardian",
		Tenant:      tenant,
		Partition:   payload.Partition,
		Status:      status,
		Severity:    severity,
		Labels:      labels,
		Annotations: annotations,
		StartsAt:    firedAt,
		Delivery:    DeliverySentinel,
	}
	alert.ID = alert.Fingerprint
	if payload.ResolvedAt != nil {
		alert.EndsAt = payload.ResolvedAt
	} else if status == StatusResolved {
		now := s.now()
		alert.EndsAt = &now
	}
	return s.Apply(ctx, *alert)
}

// Apply merges an incoming alert with stored state and enqueues notifications
// for meaningful transitions.
func (s *Service) Apply(ctx context.Context, incoming Alert) (*Alert, error) {
	if err := incoming.Validate(); err != nil {
		return nil, err
	}
	if incoming.Fingerprint == "" {
		incoming.Fingerprint = Fingerprint(incoming.Source, incoming.Tenant, incoming.Partition, incoming.Labels)
	}
	if incoming.ID == "" {
		incoming.ID = incoming.Fingerprint
	}

	// The notification decision depends on the previous state, so the write
	// must be conditional on the version that was read. Without this, two
	// concurrent Apply calls both observe ErrNotFound and both enqueue.
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		next := incoming
		now := s.now()
		if next.CreatedAt.IsZero() {
			next.CreatedAt = now
		}
		next.UpdatedAt = now

		previous, version, err := s.Store.GetAlertVersioned(ctx, next.Tenant, next.Partition, next.Fingerprint)
		if err != nil {
			return nil, err
		}
		enqueue := s.shouldNotify(previous, &next)
		if previous != nil {
			next.CreatedAt = previous.CreatedAt
			if next.Status == StatusFiring && previous.Status == StatusFiring {
				next.StartsAt = previous.StartsAt
			}
			next.Acked = previous.Acked
			next.AckedBy = previous.AckedBy
			next.AckedAt = previous.AckedAt
			// Sources re-publish alert state on every poll. Preserve delivery
			// metadata so "last notified" survives a poll and so routing
			// reconciliation does not re-send an alert that was already
			// delivered.
			next.LastNotifiedAt = previous.LastNotifiedAt
			next.SilenceIDs = previous.SilenceIDs
		}
		if next.Status == StatusResolved && next.EndsAt == nil {
			next.EndsAt = &now
		}
		var outbox []OutboxRecord
		if enqueue {
			silences, _ := s.Store.ListSilences(ctx, next.Tenant, next.Partition)
			next.SilenceIDs = matchSilences(silences, next, now)
			if len(next.SilenceIDs) == 0 && !next.Acked && next.Delivery == DeliverySentinel {
				outbox, err = s.buildOutbox(ctx, next)
				if err != nil {
					return nil, err
				}
				if len(outbox) > 0 {
					notified := now
					next.LastNotifiedAt = &notified
				}
			}
		}
		err = s.Store.SaveAlertExpected(ctx, next, outbox, version)
		if err == nil {
			return &next, nil
		}
		if !errors.Is(err, doctorstore.ErrConflict) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// ReconcileRouting delivers firing alerts that have not been notified yet.
// Routing changes do not create an alert transition, so without this an alert
// that fired before a destination existed (for example a Guardian partition
// attention alert) would stay silent until it resolved and fired again. It is
// called after a partition routing document is saved.
func (s *Service) ReconcileRouting(ctx context.Context, tenant, partition string) (int, error) {
	if !s.DeliveryEnabled {
		return 0, nil
	}
	alerts, err := s.Store.ListAlerts(ctx, tenant, partition)
	if err != nil {
		return 0, err
	}
	if len(alerts) == 0 {
		return 0, nil
	}
	silences, err := s.Store.ListSilences(ctx, tenant, partition)
	if err != nil {
		return 0, err
	}
	now := s.now()
	enqueued := 0
	for i := range alerts {
		alert := &alerts[i]
		if alert.Status != StatusFiring || alert.Delivery != DeliverySentinel || alert.Acked {
			continue
		}
		if alert.LastNotifiedAt != nil {
			continue
		}
		if len(matchSilences(silences, *alert, now)) > 0 {
			continue
		}
		outbox, err := s.buildOutbox(ctx, *alert)
		if err != nil {
			return enqueued, err
		}
		if len(outbox) == 0 {
			continue
		}
		alert.LastNotifiedAt = &now
		alert.UpdatedAt = now
		if err := s.Store.SaveAlert(ctx, *alert, outbox); err != nil {
			return enqueued, err
		}
		enqueued++
	}
	return enqueued, nil
}

func (s *Service) shouldNotify(previous *Alert, incoming *Alert) bool {
	if !s.DeliveryEnabled || incoming.Delivery != DeliverySentinel {
		return false
	}
	if previous == nil {
		return incoming.Status == StatusFiring
	}
	switch {
	case incoming.Status == StatusFiring && previous.Status != StatusFiring:
		return true
	case incoming.Status == StatusResolved && previous.Status == StatusFiring:
		return true
	case incoming.Status == StatusResolved && previous.Status != StatusFiring:
		return false
	default:
		return false
	}
}

func (s *Service) buildOutbox(ctx context.Context, alert Alert) ([]OutboxRecord, error) {
	cfg, err := s.Store.GetPartitionRouting(ctx, alert.Tenant, alert.Partition)
	if err != nil {
		return nil, err
	}
	names := matchingReceivers(cfg, alert)
	if len(names) == 0 {
		return nil, nil
	}
	now := s.now()
	out := make([]OutboxRecord, 0, len(names))
	for _, name := range names {
		record := OutboxRecord{
			AlertID:     alert.ID,
			Fingerprint: alert.Fingerprint,
			Tenant:      alert.Tenant,
			Partition:   alert.Partition,
			Channel:     name,
			Status:      "pending",
			NextAttempt: now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := record.MarshalPayload(alert); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

func matchingReceivers(cfg *PartitionRouting, alert Alert) []string {
	if cfg == nil {
		return nil
	}
	known := make(map[string]struct{}, len(cfg.Receivers))
	for _, receiver := range cfg.Receivers {
		known[receiver.Name] = struct{}{}
	}
	// 1. An explicit per-alert destination wins over every route.
	if explicit := filterKnownReceivers(alert.Notify, known); len(explicit) > 0 {
		return explicit
	}
	// 2. A source-specific destination. These let source-generated alerts
	// (Guardian partition health, CloudWatch alarms, ...) be routed explicitly
	// even though they have no rule on which to store Notify.
	for _, route := range cfg.AlertRoutes {
		if route.Source != alert.Source || route.Alert != alert.Labels["alertname"] {
			continue
		}
		if explicit := filterKnownReceivers(route.Receivers, known); len(explicit) > 0 {
			return explicit
		}
	}
	// 3. A partition-wide destination receives everything else.
	if cfg.PartitionReceiver != "" {
		if _, ok := known[cfg.PartitionReceiver]; ok {
			return []string{cfg.PartitionReceiver}
		}
	}
	// 4. Label routes.
	selected := map[string]struct{}{}
	matched := false
	for _, route := range cfg.Routes {
		if !route.Applies(alert) {
			continue
		}
		matched = true
		for _, name := range route.Receivers {
			if _, ok := known[name]; ok {
				selected[name] = struct{}{}
			}
		}
		if !route.Continue {
			break
		}
	}
	if matched {
		out := make([]string, 0, len(selected))
		for name := range selected {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}
	// 5. Default receiver.
	if cfg.DefaultReceiver != "" {
		if _, ok := known[cfg.DefaultReceiver]; ok {
			return []string{cfg.DefaultReceiver}
		}
	}
	// 6. Legacy behaviour: a config with no routes or fallbacks sends to every
	// receiver. This keeps older global configs working.
	if len(cfg.Routes) == 0 && len(cfg.AlertRoutes) == 0 && cfg.DefaultReceiver == "" && cfg.PartitionReceiver == "" {
		out := make([]string, 0, len(cfg.Receivers))
		for _, receiver := range cfg.Receivers {
			out = append(out, receiver.Name)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

func filterKnownReceivers(names []string, known map[string]struct{}) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	seen := map[string]struct{}{}
	for _, name := range names {
		if _, ok := known[name]; !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func matchSilences(silences []Silence, alert Alert, now time.Time) []string {
	out := make([]string, 0)
	for _, silence := range silences {
		if silence.Active(now) && silence.Matches(alert) {
			out = append(out, silence.ID)
		}
	}
	return out
}

// Ack sets or clears the acknowledgement flag for one alert.
func (s *Service) Ack(ctx context.Context, tenant, partition, fingerprint, user string, acked bool) (*Alert, error) {
	alert, err := s.Store.GetAlert(ctx, tenant, partition, fingerprint)
	if err != nil {
		return nil, err
	}
	now := s.now()
	alert.Acked = acked
	if acked {
		alert.AckedBy = user
		alert.AckedAt = &now
	} else {
		alert.AckedBy = ""
		alert.AckedAt = nil
	}
	if err := s.Store.SaveAlert(ctx, *alert, nil); err != nil {
		return nil, err
	}
	return alert, nil
}

// CreateSilence validates and stores a silence.
func (s *Service) CreateSilence(ctx context.Context, silence Silence, user string) (*Silence, error) {
	now := s.now()
	if silence.ID == "" {
		silence.ID = fmt.Sprintf("sil-%d", now.UnixNano())
	}
	if silence.CreatedBy == "" {
		silence.CreatedBy = user
	}
	if err := silence.Validate(now); err != nil {
		return nil, err
	}
	if err := s.Store.PutSilence(ctx, silence); err != nil {
		return nil, err
	}
	return &silence, nil
}

// VerifyGuardianSignature verifies an HMAC over the exact Guardian request
// body. The API should call this before decoding the JSON.
func VerifyGuardianSignature(key, body []byte, signature string) bool {
	if len(key) == 0 {
		return true
	}
	signature = strings.TrimSpace(signature)
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(strings.ToLower(signature)), []byte(strings.ToLower(want)))
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
