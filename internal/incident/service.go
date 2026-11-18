package incident

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/alerting"
	doctorstore "github.com/radryc/doctor/internal/store"
)

// Errors returned by lifecycle operations.
var (
	ErrInvalidTransition = errors.New("incident: invalid state transition")
	ErrAlertsFiring      = errors.New("incident: member alerts are still firing; pass override to mitigate")
	ErrResolveFromOpen   = errors.New("incident: mitigate the incident before resolving it")
)

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func safeName(value string) string {
	value = unsafeName.ReplaceAllString(strings.TrimSpace(value), "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "receiver"
	}
	if len(value) > 60 {
		value = value[:60]
	}
	return value
}

// Analyzer produces the postmortem narrative from an incident snapshot.
type Analyzer interface {
	Name() string
	Analyze(ctx context.Context, in AnalysisInput) (Analysis, error)
}

// AnalysisInput is deliberately small: the agent fetches telemetry itself
// under its partition-scoped tool allowlist.
type AnalysisInput struct {
	Incident Incident
	Set      AlertSet
	Alerts   []MemberAlert
}

// Service coordinates reconciliation, transitions, notifications, and
// postmortems.
type Service struct {
	Store        *Store
	Alerts       *alerting.Store
	AlertService *alerting.Service
	Engines      *EngineRegistry
	Analyzers    map[string]Analyzer
	Logger       *log.Logger
	Now          func() time.Time
	BaseURL      string
	Delivery     bool
	// AsyncAnalysis runs the agent call in the background. Tests set it false
	// and call AnalyzeNow directly.
	AsyncAnalysis bool
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) logf(format string, args ...any) {
	if s != nil && s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

func severityRank(value string) int {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical", "page":
		return 3
	case "warning", "warn":
		return 2
	case "info":
		return 1
	default:
		return 1
	}
}

func matchesMatchers(matchers []alerting.Matcher, labels map[string]string) bool {
	for _, matcher := range matchers {
		if !matcher.Matches(labels) {
			return false
		}
	}
	return true
}

func setMatches(set AlertSet, alert alerting.Alert) bool {
	if alert.Partition != set.Partition {
		return false
	}
	if alert.Status != alerting.StatusFiring && alert.Status != alerting.StatusPending {
		return false
	}
	if severityRank(alert.Severity) < severityRank(set.SeverityFloor) {
		return false
	}
	if !matchesMatchers(set.Match, alert.Labels) {
		return false
	}
	for _, matcher := range set.Exclude {
		if matcher.Matches(alert.Labels) {
			return false
		}
	}
	if set.RespectsSilences() && len(alert.SilenceIDs) > 0 {
		return false
	}
	if set.RespectsAcks() && alert.Acked {
		return false
	}
	return true
}

type alertGroup struct {
	Key    string
	Labels map[string]string
	Alerts []alerting.Alert
}

func groupKey(labels map[string]string, groupBy []string) (string, map[string]string) {
	chosen := make(map[string]string, len(groupBy))
	parts := make([]string, 0, len(groupBy))
	for _, label := range groupBy {
		value := strings.TrimSpace(labels[label])
		if value == "" {
			value = "_"
		}
		chosen[label] = value
		parts = append(parts, label+"="+value)
	}
	return strings.Join(parts, ","), chosen
}

func groupAlerts(set AlertSet, alerts map[string]alerting.Alert) map[string]*alertGroup {
	out := map[string]*alertGroup{}
	for _, alert := range alerts {
		if !setMatches(set, alert) {
			continue
		}
		key, labels := groupKey(alert.Labels, set.GroupBy)
		group := out[key]
		if group == nil {
			group = &alertGroup{Key: key, Labels: labels}
			out[key] = group
		}
		group.Alerts = append(group.Alerts, alert)
	}
	for _, group := range out {
		sort.Slice(group.Alerts, func(i, j int) bool {
			return group.Alerts[i].StartsAt.Before(group.Alerts[j].StartsAt)
		})
	}
	return out
}

func memberAlerts(alerts []alerting.Alert) ([]MemberAlert, []string) {
	members := make([]MemberAlert, 0, len(alerts))
	fingerprints := make([]string, 0, len(alerts))
	for _, alert := range alerts {
		members = append(members, MemberAlert{
			Fingerprint: alert.Fingerprint,
			Alertname:   firstNonEmpty(alert.Labels["alertname"], alert.Annotations["summary"], alert.Source),
			Source:      alert.Source,
			Severity:    alert.Severity,
			Status:      alert.Status,
			StartsAt:    alert.StartsAt,
			Labels:      alert.Labels,
		})
		fingerprints = append(fingerprints, alert.Fingerprint)
	}
	return members, fingerprints
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func highestSeverity(alerts []alerting.Alert) string {
	severity := "info"
	rank := 0
	for _, alert := range alerts {
		if current := severityRank(alert.Severity); current > rank {
			rank = current
			severity = strings.ToLower(alert.Severity)
		}
	}
	return severity
}

func (s *Service) loadPolicy(ctx context.Context, tenant, partition string) Policy {
	policy, err := s.Store.GetPolicy(ctx, tenant, partition)
	if err != nil {
		s.logf("incident: load policy %s/%s: %v", tenant, partition, err)
		return DefaultPolicy()
	}
	return policy
}

func (s *Service) loadSet(ctx context.Context, tenant, partition, name string) *AlertSet {
	set, _, err := s.Store.GetAlertSet(ctx, tenant, partition, name)
	if err != nil || set == nil {
		return nil
	}
	return set
}

func (s *Service) incidentURL(incident Incident) string {
	base := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	if base == "" {
		return "/#/incidents/" + incident.ID
	}
	return base + "/#/incidents/" + incident.ID
}

func (s *Service) resolveReceivers(ctx context.Context, set AlertSet) (alerting.PartitionRouting, []alerting.Receiver) {
	if s.Alerts == nil {
		return alerting.PartitionRouting{}, nil
	}
	routing, err := s.Alerts.GetPartitionRouting(ctx, set.Tenant, set.Partition)
	if err != nil {
		return alerting.PartitionRouting{}, nil
	}
	names := set.Notify.Receivers
	if len(names) == 0 {
		names = []string{routing.PartitionReceiver, routing.DefaultReceiver}
	}
	byName := make(map[string]alerting.Receiver, len(routing.Receivers))
	for _, receiver := range routing.Receivers {
		byName[receiver.Name] = receiver
	}
	var out []alerting.Receiver
	seen := map[string]bool{}
	for _, name := range names {
		if strings.TrimSpace(name) == "" || seen[name] {
			continue
		}
		seen[name] = true
		receiver, ok := byName[name]
		if !ok {
			continue
		}
		if !alerting.ReceiverSupports(receiver, alerting.EventKindIncident) {
			continue
		}
		out = append(out, receiver)
	}
	return *routing, out
}

func (s *Service) notifications(ctx context.Context, set AlertSet, incident Incident, event, actor, message string) []alerting.OutboxRecord {
	if !s.Delivery {
		return nil
	}
	_, receivers := s.resolveReceivers(ctx, set)
	if len(receivers) == 0 {
		return nil
	}
	now := s.now()
	fields := []alerting.EventField{
		{Label: "State", Value: incident.State},
		{Label: "Severity", Value: incident.Severity},
		{Label: "Partition", Value: incident.Partition},
		{Label: "Alert set", Value: incident.Set},
		{Label: "Group", Value: incident.GroupKey},
		{Label: "Owner", Value: firstNonEmpty(incident.Owner, "unassigned")},
		{Label: "Opened", Value: incident.OpenedAt.Format(time.RFC3339)},
		{Label: "Member alerts", Value: fmt.Sprintf("%d", len(incident.MemberAlerts))},
	}
	if incident.MitigatedAt != nil {
		fields = append(fields, alerting.EventField{Label: "Mitigated", Value: incident.MitigatedAt.Format(time.RFC3339)})
	}
	if incident.ResolvedAt != nil {
		fields = append(fields, alerting.EventField{Label: "Resolved", Value: incident.ResolvedAt.Format(time.RFC3339)})
	}
	sections := []alerting.EventSection{}
	if len(incident.Timeline) > 0 {
		start := len(incident.Timeline) - 6
		if start < 0 {
			start = 0
		}
		lines := make([]string, 0, len(incident.Timeline)-start)
		for _, entry := range incident.Timeline[start:] {
			lines = append(lines, fmt.Sprintf("%s %s %s", entry.At.Format(time.RFC3339), entry.Kind, entry.Message))
		}
		sections = append(sections, alerting.EventSection{Title: "Timeline", Lines: lines})
	}
	if len(incident.MemberAlerts) > 0 {
		lines := make([]string, 0, len(incident.MemberAlerts))
		for i, alert := range incident.MemberAlerts {
			if i >= 6 {
				break
			}
			lines = append(lines, fmt.Sprintf("%s (%s)", alert.Alertname, alert.Severity))
		}
		sections = append(sections, alerting.EventSection{Title: "Member alerts", Lines: lines})
	}
	payload, err := json.Marshal(alerting.Event{
		SchemaVersion: "doctor.incident/v1",
		Kind:          alerting.EventKindIncident,
		Event:         event,
		Sequence:      incident.Seq,
		ID:            incident.ID,
		Title:         incident.TitleText(),
		Summary:       firstNonEmpty(message, incident.Resolution, incident.Mitigation),
		Status:        incident.State,
		Severity:      incident.Severity,
		URL:           s.incidentURL(incident),
		Actor:         actor,
		OccurredAt:    now,
		Fields:        fields,
		Sections:      sections,
	})
	if err != nil {
		return nil
	}
	out := make([]alerting.OutboxRecord, 0, len(receivers))
	for _, receiver := range receivers {
		out = append(out, alerting.OutboxRecord{
			ID:          fmt.Sprintf("%s-%d-%s", incident.ID, incident.Seq, safeName(receiver.Name)),
			Kind:        alerting.EventKindIncident,
			SubjectID:   incident.ID,
			EventSeq:    incident.Seq,
			AlertID:     incident.ID,
			Fingerprint: incident.ID,
			Tenant:      incident.Tenant,
			Partition:   incident.Partition,
			Channel:     receiver.Name,
			Status:      "pending",
			Payload:     payload,
			NextAttempt: now,
		})
	}
	return out
}

// ReconcileTenant evaluates every enabled alert set for one tenant.
func (s *Service) ReconcileTenant(ctx context.Context, tenant string) error {
	if s.Store == nil || s.Alerts == nil {
		return nil
	}
	if tenant == "" {
		tenant = "default"
	}
	sets, err := s.Store.ListAlertSets(ctx, tenant)
	if err != nil {
		return err
	}
	if len(sets) == 0 {
		return nil
	}
	alerts, err := s.Alerts.ListTenantAlerts(ctx, tenant)
	if err != nil {
		return err
	}
	incidents, err := s.Store.ListTenantIncidents(ctx, tenant)
	if err != nil {
		return err
	}
	bySet := map[string][]Incident{}
	byID := map[string]Incident{}
	for _, inc := range incidents {
		key := inc.Partition + "\x00" + inc.Set
		bySet[key] = append(bySet[key], inc)
		byID[inc.ID] = inc
	}
	for _, set := range sets {
		if !set.IsEnabled() {
			continue
		}
		policy := s.loadPolicy(ctx, tenant, set.Partition)
		grouped := groupAlerts(set, alerts)
		handled := map[string]bool{}
		for _, inc := range bySet[set.Partition+"\x00"+set.Name] {
			if handled[inc.GroupKey] {
				continue
			}
			group := grouped[inc.GroupKey]
			if group != nil && len(group.Alerts) > 0 {
				if inc.State == StateResolved {
					if inc.AutoResolved && inc.ResolvedAt != nil && s.now().Before(inc.ResolvedAt.Add(duration(policy.ReopenWindow))) {
						s.reopen(ctx, &inc, set, "sentinel", "reopened: member alert fired within reopen window")
						handled[inc.GroupKey] = true
					}
					continue
				}
				s.refreshActive(ctx, &inc, set, policy, group.Alerts)
				handled[inc.GroupKey] = true
				continue
			}
			if inc.State == StateResolved {
				continue
			}
			handled[inc.GroupKey] = true
			s.handleEmptyGroup(ctx, &inc, set, policy)
		}
		for key, group := range grouped {
			if handled[key] || len(group.Alerts) == 0 {
				continue
			}
			ref, err := s.Store.GetActive(ctx, tenant, set.Partition, set.Name, key)
			if err != nil {
				continue
			}
			if ref != nil {
				if existing, ok := byID[ref.IncidentID]; ok && existing.State != StateResolved {
					continue
				}
			}
			s.openIncident(ctx, set, policy, group)
		}
	}
	return nil
}

func (s *Service) refreshActive(ctx context.Context, inc *Incident, set AlertSet, policy Policy, alerts []alerting.Alert) {
	now := s.now()
	members, fingerprints := memberAlerts(alerts)
	inc.MemberAlerts = members
	inc.MemberFingerprints = fingerprints
	inc.Title = firstNonEmpty(inc.Title, members[0].Alertname, set.Name)
	inc.Severity = highestSeverity(alerts)
	refired := false
	if inc.State == StateMitigated {
		// A manual mitigation may leave a noisy alert firing. Do not bounce the
		// incident back open for alerts that were already firing at mitigation
		// time; only a genuinely new alert (starts after mitigation) refires.
		refire := inc.MitigatedAt == nil
		for _, alert := range alerts {
			if inc.MitigatedAt != nil && alert.StartsAt.After(*inc.MitigatedAt) {
				refire = true
				break
			}
		}
		if !refire {
			inc.MemberAlerts = members
			inc.MemberFingerprints = fingerprints
			return
		}
		inc.State = StateOpen
		if inc.Owner != "" {
			inc.State = StateAcknowledged
		}
		inc.MitigatedAt = nil
		inc.MitigatedBy = ""
		inc.Mitigation = ""
		inc.AutoMitigated = false
		inc.ResolveDueAt = nil
		refired = true
		inc.Append(now, EventRefired, "sentinel", "member alert fired again; incident reopened from mitigated")
	} else if inc.MembersClearAt != nil {
		inc.MembersClearAt = nil
		inc.MitigateDueAt = nil
		refired = true
		inc.Append(now, EventRefired, "sentinel", "member alert fired again before mitigation")
	}
	if refired {
		inc.NextUpdateAt = nextUpdate(now, set)
		outbox := s.notifications(ctx, set, *inc, "incident.refired", "sentinel", fmt.Sprintf("incident %s is %s again", inc.ID, inc.State))
		version := s.latestIncidentVersion(ctx, *inc)
		if err := s.Store.SaveIncident(ctx, *inc, version, nil, outbox); err != nil {
			s.logf("incident: refire %s: %v", inc.ID, err)
			return
		}
	}
	s.maybePeriodicUpdate(ctx, inc, set, policy)
}

func nextUpdate(now time.Time, set AlertSet) *time.Time {
	if set.Notify.UpdateInterval <= 0 {
		return nil
	}
	next := now.Add(duration(set.Notify.UpdateInterval))
	return &next
}

func (s *Service) maybePeriodicUpdate(ctx context.Context, inc *Incident, set AlertSet, policy Policy) {
	if inc.State != StateOpen && inc.State != StateAcknowledged {
		return
	}
	if !set.NotifyReminders() {
		return
	}
	if inc.State == StateAcknowledged && !set.Notify.UpdateWhileAcked {
		return
	}
	now := s.now()
	if inc.NextUpdateAt == nil {
		inc.NextUpdateAt = nextUpdate(now, set)
		return
	}
	if now.Before(*inc.NextUpdateAt) {
		return
	}
	inc.Append(now, EventNotified, "sentinel", "periodic incident update")
	inc.NextUpdateAt = nextUpdate(now, set)
	outbox := s.notifications(ctx, set, *inc, "incident.update", "sentinel", "periodic incident update")
	version := s.latestIncidentVersion(ctx, *inc)
	if err := s.Store.SaveIncident(ctx, *inc, version, nil, outbox); err != nil {
		s.logf("incident: periodic update %s: %v", inc.ID, err)
	}
	_ = policy
}

func (s *Service) handleEmptyGroup(ctx context.Context, inc *Incident, set AlertSet, policy Policy) {
	now := s.now()
	if inc.State == StateMitigated {
		if policy.AutoResolveEnabled() && inc.ResolveDueAt != nil && !now.Before(*inc.ResolveDueAt) {
			if _, err := s.resolveIncident(ctx, inc, "sentinel", "auto-resolved after mitigation window", true); err != nil {
				s.logf("incident: auto-resolve %s: %v", inc.ID, err)
			}
		}
		return
	}
	if inc.MembersClearAt == nil {
		inc.MembersClearAt = &now
		due := now.Add(duration(policy.MitigateDelay))
		inc.MitigateDueAt = &due
		inc.Append(now, EventMembersCleared, "sentinel", fmt.Sprintf("all member alerts cleared; auto-mitigate in %s", duration(policy.MitigateDelay)))
		outbox := s.notifications(ctx, set, *inc, "incident.members_cleared", "sentinel", "all member alerts cleared")
		version := s.latestIncidentVersion(ctx, *inc)
		if err := s.Store.SaveIncident(ctx, *inc, version, nil, outbox); err != nil {
			s.logf("incident: members cleared %s: %v", inc.ID, err)
		}
		return
	}
	if policy.AutoMitigateEnabled() && inc.MitigateDueAt != nil && !now.Before(*inc.MitigateDueAt) {
		inc.State = StateMitigated
		inc.MitigatedAt = &now
		inc.MitigatedBy = "sentinel"
		inc.Mitigation = fmt.Sprintf("auto-mitigated %s after the last member alert cleared", duration(policy.MitigateDelay))
		inc.AutoMitigated = true
		inc.MembersClearAt = &now
		if policy.AutoResolveEnabled() {
			due := now.Add(duration(policy.AutoResolveDelay))
			inc.ResolveDueAt = &due
		}
		inc.Append(now, EventMitigated, "sentinel", inc.Mitigation)
		outbox := s.notifications(ctx, set, *inc, "incident.mitigated", "sentinel", inc.Mitigation)
		version := s.latestIncidentVersion(ctx, *inc)
		if err := s.Store.SaveIncident(ctx, *inc, version, nil, outbox); err != nil {
			s.logf("incident: mitigate %s: %v", inc.ID, err)
		}
	}
}

func (s *Service) openIncident(ctx context.Context, set AlertSet, policy Policy, group *alertGroup) {
	now := s.now()
	members, fingerprints := memberAlerts(group.Alerts)
	hash := sha256.Sum256([]byte(set.Tenant + "\x00" + set.Partition + "\x00" + set.Name + "\x00" + group.Key))
	id := fmt.Sprintf("%s-%s-%d", set.Name, hex.EncodeToString(hash[:])[:8], now.Unix())
	incident := Incident{
		ID:                 id,
		Tenant:             set.Tenant,
		Partition:          set.Partition,
		Set:                set.Name,
		GroupKey:           group.Key,
		GroupLabels:        group.Labels,
		Title:              firstNonEmpty(members[0].Alertname, set.Name),
		Severity:           highestSeverity(group.Alerts),
		State:              StateOpen,
		OpenedAt:           now,
		MemberAlerts:       members,
		MemberFingerprints: fingerprints,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	incident.Append(now, EventOpened, "sentinel", fmt.Sprintf("%d alert(s) matched set %s", len(group.Alerts), set.Name))
	incident.NextUpdateAt = nextUpdate(now, set)
	outbox := s.notifications(ctx, set, incident, "incident.opened", "sentinel", fmt.Sprintf("%d alert(s) fired", len(group.Alerts)))
	active := &ActiveRef{IncidentID: id}
	if err := s.Store.SaveIncident(ctx, incident, "absent", active, outbox); err != nil {
		if errors.Is(err, doctorstore.ErrConflict) {
			return
		}
		s.logf("incident: open %s: %v", id, err)
	}
}

// RefreshActive is exported for tests that drive one reconciliation step.
func (s *Service) RefreshActive(ctx context.Context, tenant, partition, id string) (*Incident, error) {
	inc, version, err := s.Store.GetIncident(ctx, tenant, partition, id)
	if err != nil || inc == nil {
		return nil, ErrNotFound
	}
	set := s.loadSet(ctx, tenant, partition, inc.Set)
	if set == nil {
		return inc, nil
	}
	policy := s.loadPolicy(ctx, tenant, partition)
	alerts, err := s.Alerts.ListAlerts(ctx, tenant, partition)
	if err != nil {
		return nil, err
	}
	var active []alerting.Alert
	byFingerprint := map[string]alerting.Alert{}
	for _, alert := range alerts {
		byFingerprint[alert.Fingerprint] = alert
	}
	for _, fingerprint := range inc.MemberFingerprints {
		if alert, ok := byFingerprint[fingerprint]; ok {
			active = append(active, alert)
		}
	}
	if len(active) > 0 {
		s.refreshActive(ctx, inc, *set, policy, active)
	} else {
		s.handleEmptyGroup(ctx, inc, *set, policy)
	}
	_ = version
	return inc, nil
}

// Ack acknowledges an incident.
func (s *Service) Ack(ctx context.Context, tenant, partition, id, actor string) (*Incident, error) {
	inc, version, err := s.Store.GetIncident(ctx, tenant, partition, id)
	if err != nil || inc == nil {
		return nil, ErrNotFound
	}
	if inc.State == StateResolved || inc.State == StateMitigated {
		return nil, ErrInvalidTransition
	}
	now := s.now()
	inc.State = StateAcknowledged
	inc.Owner = actor
	inc.AcknowledgedAt = &now
	inc.Append(now, EventAcknowledged, actor, "incident acknowledged")
	set := s.loadSet(ctx, tenant, partition, inc.Set)
	outbox := s.notifications(ctx, derefSet(set), *inc, "incident.acknowledged", actor, "incident acknowledged")
	if err := s.Store.SaveIncident(ctx, *inc, version, nil, outbox); err != nil {
		return nil, err
	}
	return inc, nil
}

// AckByID acknowledges or unacknowledges the incident with the given ID in a
// tenant. It is used by the fcm-bridge ack callback, which knows the incident
// ID but not its partition. The bool reports whether the incident existed.
func (s *Service) AckByID(ctx context.Context, tenant, id, actor string, acked bool) (bool, error) {
	inc, err := s.Store.FindIncident(ctx, tenant, id)
	if err != nil {
		return false, err
	}
	if inc == nil {
		return false, nil
	}
	if acked {
		if inc.State == StateAcknowledged {
			return true, nil
		}
		if _, err := s.Ack(ctx, inc.Tenant, inc.Partition, inc.ID, actor); err != nil {
			if errors.Is(err, ErrInvalidTransition) {
				// The incident moved to mitigated/resolved before the device
				// acked. Treat the callback as a no-op instead of failing the
				// bridge delivery.
				return true, nil
			}
			return true, err
		}
		return true, nil
	}
	if inc.State != StateAcknowledged {
		return true, nil
	}
	if _, err := s.Unack(ctx, inc.Tenant, inc.Partition, inc.ID, actor); err != nil {
		return true, err
	}
	return true, nil
}

// Unack returns an acknowledged incident to open.
func (s *Service) Unack(ctx context.Context, tenant, partition, id, actor string) (*Incident, error) {
	inc, version, err := s.Store.GetIncident(ctx, tenant, partition, id)
	if err != nil || inc == nil {
		return nil, ErrNotFound
	}
	if inc.State != StateAcknowledged {
		return nil, ErrInvalidTransition
	}
	now := s.now()
	inc.State = StateOpen
	inc.Owner = ""
	inc.AcknowledgedAt = nil
	inc.Append(now, EventUnacknowledged, actor, "incident unacknowledged")
	set := s.loadSet(ctx, tenant, partition, inc.Set)
	outbox := s.notifications(ctx, derefSet(set), *inc, "incident.unacknowledged", actor, "incident unacknowledged")
	if err := s.Store.SaveIncident(ctx, *inc, version, nil, outbox); err != nil {
		return nil, err
	}
	return inc, nil
}

// Mitigate marks the bleeding as stopped.
func (s *Service) Mitigate(ctx context.Context, tenant, partition, id, actor, note string, override bool) (*Incident, error) {
	inc, version, err := s.Store.GetIncident(ctx, tenant, partition, id)
	if err != nil || inc == nil {
		return nil, ErrNotFound
	}
	if inc.State == StateResolved || inc.State == StateMitigated {
		return nil, ErrInvalidTransition
	}
	if !override {
		alerts, err := s.Alerts.ListAlerts(ctx, tenant, partition)
		if err == nil {
			firing := map[string]bool{}
			for _, alert := range alerts {
				if alert.Status == alerting.StatusFiring || alert.Status == alerting.StatusPending {
					firing[alert.Fingerprint] = true
				}
			}
			for _, fingerprint := range inc.MemberFingerprints {
				if firing[fingerprint] {
					return nil, ErrAlertsFiring
				}
			}
		}
	}
	now := s.now()
	policy := s.loadPolicy(ctx, tenant, partition)
	inc.State = StateMitigated
	inc.MitigatedAt = &now
	inc.MitigatedBy = actor
	inc.Mitigation = note
	inc.AutoMitigated = false
	if policy.AutoResolveEnabled() {
		due := now.Add(duration(policy.AutoResolveDelay))
		inc.ResolveDueAt = &due
	}
	inc.Append(now, EventMitigated, actor, firstNonEmpty(note, "incident mitigated"))
	set := s.loadSet(ctx, tenant, partition, inc.Set)
	outbox := s.notifications(ctx, derefSet(set), *inc, "incident.mitigated", actor, inc.Mitigation)
	if err := s.Store.SaveIncident(ctx, *inc, version, nil, outbox); err != nil {
		return nil, err
	}
	return inc, nil
}

// Resolve closes a mitigated incident and starts postmortem analysis.
func (s *Service) Resolve(ctx context.Context, tenant, partition, id, actor, resolution string) (*Incident, error) {
	inc, _, err := s.Store.GetIncident(ctx, tenant, partition, id)
	if err != nil || inc == nil {
		return nil, ErrNotFound
	}
	if inc.State != StateMitigated {
		return nil, ErrResolveFromOpen
	}
	return s.resolveIncident(ctx, inc, actor, resolution, false)
}

func (s *Service) resolveIncident(ctx context.Context, inc *Incident, actor, resolution string, auto bool) (*Incident, error) {
	now := s.now()
	policy := s.loadPolicy(ctx, inc.Tenant, inc.Partition)
	set := s.loadSet(ctx, inc.Tenant, inc.Partition, inc.Set)
	inc.State = StateResolved
	inc.ResolvedAt = &now
	inc.ResolvedBy = actor
	inc.Resolution = resolution
	inc.AutoResolved = auto
	inc.ResolveDueAt = nil
	inc.MembersClearAt = nil
	inc.MitigateDueAt = nil
	inc.Append(now, EventResolved, actor, firstNonEmpty(resolution, "incident resolved"))
	var active *ActiveRef
	if auto {
		expires := now.Add(duration(policy.ReopenWindow))
		active = &ActiveRef{IncidentID: inc.ID, ExpiresAt: &expires}
	}
	outbox := s.notifications(ctx, derefSet(set), *inc, "incident.resolved", actor, resolution)
	version := s.latestIncidentVersion(ctx, *inc)
	if err := s.Store.SaveIncident(ctx, *inc, version, active, outbox); err != nil {
		return nil, err
	}
	if auto {
		// Auto-resolved incidents keep the grouping index for a reopen window.
	} else if err := s.Store.DeleteActive(ctx, inc.Tenant, inc.Partition, inc.Set, inc.GroupKey); err != nil {
		s.logf("incident: clear active index %s: %v", inc.ID, err)
	}
	s.createPostmortem(ctx, *inc, set, policy)
	return inc, nil
}

// Reopen returns a resolved incident to the open state.
func (s *Service) Reopen(ctx context.Context, tenant, partition, id, actor string) (*Incident, error) {
	inc, version, err := s.Store.GetIncident(ctx, tenant, partition, id)
	if err != nil || inc == nil {
		return nil, ErrNotFound
	}
	if inc.State != StateResolved {
		return nil, ErrInvalidTransition
	}
	policy := s.loadPolicy(ctx, tenant, partition)
	if inc.ResolvedAt != nil {
		window := duration(policy.ReopenWindow)
		if window > 0 && s.now().After(inc.ResolvedAt.Add(window)) {
			return nil, fmt.Errorf("incident: reopen window (%s) has passed", window)
		}
	}
	s.reopen(ctx, inc, derefSet(s.loadSet(ctx, tenant, partition, inc.Set)), actor, "incident reopened by operator")
	_ = version
	return inc, nil
}

func (s *Service) reopen(ctx context.Context, inc *Incident, set AlertSet, actor, message string) {
	now := s.now()
	inc.State = StateOpen
	if inc.Owner != "" {
		inc.State = StateAcknowledged
	}
	inc.ResolvedAt = nil
	inc.ResolvedBy = ""
	inc.Resolution = ""
	inc.AutoResolved = false
	inc.ReopenCount++
	inc.Append(now, EventReopened, actor, message)
	inc.NextUpdateAt = nextUpdate(now, set)
	outbox := s.notifications(ctx, set, *inc, "incident.reopened", actor, message)
	version := s.latestIncidentVersion(ctx, *inc)
	if err := s.Store.SaveIncident(ctx, *inc, version, &ActiveRef{IncidentID: inc.ID}, outbox); err != nil {
		s.logf("incident: reopen %s: %v", inc.ID, err)
		return
	}
	if doc, expected, err := s.Store.GetPostmortem(ctx, inc.Tenant, inc.Partition, inc.ID); err == nil && doc != nil {
		doc.Status = PostmortemSuperseded
		_ = s.Store.SavePostmortem(ctx, *doc, expected)
	}
}

// Note appends an operator note and notifies receivers.
func (s *Service) Note(ctx context.Context, tenant, partition, id, actor, message string) (*Incident, error) {
	if strings.TrimSpace(message) == "" {
		return nil, fmt.Errorf("incident: note is empty")
	}
	inc, version, err := s.Store.GetIncident(ctx, tenant, partition, id)
	if err != nil || inc == nil {
		return nil, ErrNotFound
	}
	now := s.now()
	inc.Append(now, EventNote, actor, message)
	set := s.loadSet(ctx, tenant, partition, inc.Set)
	outbox := s.notifications(ctx, derefSet(set), *inc, "incident.note", actor, message)
	if err := s.Store.SaveIncident(ctx, *inc, version, nil, outbox); err != nil {
		return nil, err
	}
	return inc, nil
}

// Preview returns matching alerts and proposed groups for the set editor.
func (s *Service) Preview(ctx context.Context, set AlertSet) (map[string]any, error) {
	set.Normalize()
	alerts, err := s.Alerts.ListTenantAlerts(ctx, set.Tenant)
	if err != nil {
		return nil, err
	}
	grouped := groupAlerts(set, alerts)
	alertsMatched := 0
	groups := make([]map[string]any, 0, len(grouped))
	for _, group := range grouped {
		alertsMatched += len(group.Alerts)
		names := make([]string, 0, len(group.Alerts))
		for _, alert := range group.Alerts {
			names = append(names, firstNonEmpty(alert.Labels["alertname"], alert.Fingerprint))
		}
		groups = append(groups, map[string]any{
			"key":    group.Key,
			"labels": group.Labels,
			"alerts": names,
			"count":  len(group.Alerts),
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i]["key"].(string) < groups[j]["key"].(string)
	})
	return map[string]any{
		"matchedAlerts": alertsMatched,
		"groups":        groups,
	}, nil
}

func (s *Service) latestIncidentVersion(ctx context.Context, inc Incident) string {
	if s.Store == nil {
		return "absent"
	}
	versions, err := s.Store.Files.ListVersions(ctx, s.Store.Token, s.Store.incidentPath(inc.Tenant, inc.Partition, inc.ID))
	if err != nil || len(versions) == 0 {
		return "absent"
	}
	return versions[0].VersionID
}

func derefSet(set *AlertSet) AlertSet {
	if set == nil {
		return AlertSet{}
	}
	return *set
}
