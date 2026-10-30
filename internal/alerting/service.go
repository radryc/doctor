package alerting

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
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
	Logger          *log.Logger
	Now             func() time.Time
}

// IngestGuardian validates and normalizes a Guardian alert.
func (s *Service) IngestGuardian(ctx context.Context, tenant string, payload GuardianAlert, signature string) (*Alert, error) {
	if !strings.HasPrefix(signature, "sha256=") {
		if len(s.GuardianHMACKey) > 0 {
			return nil, fmt.Errorf("alerting: guardian signature is required")
		}
	} else if len(s.GuardianHMACKey) > 0 {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		mac := hmac.New(sha256.New, s.GuardianHMACKey)
		_, _ = mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(strings.ToLower(signature)), []byte(strings.ToLower(want))) {
			return nil, fmt.Errorf("alerting: guardian signature mismatch")
		}
	}
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
	now := s.now()
	if incoming.CreatedAt.IsZero() {
		incoming.CreatedAt = now
	}
	incoming.UpdatedAt = now
	previous, err := s.Store.GetAlert(ctx, incoming.Tenant, incoming.Partition, incoming.Fingerprint)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	enqueue := s.shouldNotify(previous, &incoming)
	if previous != nil {
		incoming.CreatedAt = previous.CreatedAt
		if incoming.Status == StatusFiring && previous.Status == StatusFiring {
			incoming.StartsAt = previous.StartsAt
		}
		incoming.Acked = previous.Acked
		incoming.AckedBy = previous.AckedBy
		incoming.AckedAt = previous.AckedAt
	}
	if incoming.Status == StatusResolved && incoming.EndsAt == nil {
		incoming.EndsAt = &now
	}
	if enqueue {
		silences, _ := s.Store.ListSilences(ctx, incoming.Tenant, incoming.Partition)
		matched := matchSilences(silences, incoming, now)
		incoming.SilenceIDs = matched
		if len(matched) == 0 && !incoming.Acked && incoming.Delivery == DeliverySentinel {
			outbox, err := s.buildOutbox(ctx, incoming)
			if err != nil {
				return nil, err
			}
			if len(outbox) > 0 {
				notified := now
				incoming.LastNotifiedAt = &notified
			}
			if err := s.Store.SaveAlert(ctx, incoming, outbox); err != nil {
				return nil, err
			}
			return &incoming, nil
		}
	}
	if err := s.Store.SaveAlert(ctx, incoming, nil); err != nil {
		return nil, err
	}
	return &incoming, nil
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
	cfg, err := s.Store.GetReceiverConfig(ctx)
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

func matchingReceivers(cfg *ReceiverConfig, alert Alert) []string {
	if cfg == nil {
		return nil
	}
	if len(cfg.Routes) == 0 {
		out := make([]string, 0, len(cfg.Receivers))
		for _, receiver := range cfg.Receivers {
			out = append(out, receiver.Name)
		}
		return out
	}
	selected := map[string]struct{}{}
	for _, route := range cfg.Routes {
		if !routeMatches(route, alert.Labels) {
			continue
		}
		for _, name := range route.Receivers {
			selected[name] = struct{}{}
		}
		if !route.Continue {
			break
		}
	}
	out := make([]string, 0, len(selected))
	for name := range selected {
		out = append(out, name)
	}
	return out
}

func routeMatches(route Route, labels map[string]string) bool {
	for key, want := range route.Matchers {
		if labels[key] != want {
			return false
		}
	}
	return true
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

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
