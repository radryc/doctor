// Package incident implements Doctor's operator-facing incident lifecycle.
//
// Alerts are signals; incidents are the unit responders acknowledge, mitigate,
// and resolve. Incidents are grouped from AlertSet selectors, follow a
// state machine, and produce an agent-assisted postmortem on resolution.
package incident

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/radryc/doctor/internal/alerting"
)

// Incident states.
const (
	StateOpen         = "open"
	StateAcknowledged = "acknowledged"
	StateMitigated    = "mitigated"
	StateResolved     = "resolved"
)

// Timeline kinds.
const (
	EventOpened              = "opened"
	EventAlertFired          = "alert_fired"
	EventAlertResolved       = "alert_resolved"
	EventMembersCleared      = "members_cleared"
	EventAcknowledged        = "acknowledged"
	EventUnacknowledged      = "unacknowledged"
	EventMitigated           = "mitigated"
	EventRefired             = "refired"
	EventResolved            = "resolved"
	EventReopened            = "reopened"
	EventNote                = "note"
	EventNotified            = "notified"
	EventAnalysisStarted     = "analysis_started"
	EventAnalysisCompleted   = "analysis_completed"
	EventPostmortemPublished = "postmortem_published"
	EventPolicyChanged       = "policy_changed"
)

// Postmortem statuses.
const (
	PostmortemDraft      = "draft"
	PostmortemAnalyzing  = "analyzing"
	PostmortemAnalyzed   = "analyzed"
	PostmortemPublished  = "published"
	PostmortemFailed     = "failed"
	PostmortemSuperseded = "superseded"
)

// Analysis providers.
const (
	AnalysisAgent = "agent"
	AnalysisNone  = "none"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Duration returns the concrete duration for a configured value.
func duration(value alerting.Duration) time.Duration {
	return time.Duration(value)
}

// AlertSet is a stable selector that turns matching alerts into incidents.
type AlertSet struct {
	Tenant          string             `json:"tenant"`
	Partition       string             `json:"partition"`
	Name            string             `json:"name"`
	Description     string             `json:"description,omitempty"`
	Enabled         *bool              `json:"enabled,omitempty"`
	SeverityFloor   string             `json:"severityFloor,omitempty"`
	Match           []alerting.Matcher `json:"match"`
	Exclude         []alerting.Matcher `json:"exclude,omitempty"`
	RespectSilences *bool              `json:"respectSilences,omitempty"`
	RespectAcks     *bool              `json:"respectAcks,omitempty"`
	GroupBy         []string           `json:"groupBy,omitempty"`
	Notify          NotifySpec         `json:"notify,omitempty"`
	Postmortem      PostmortemSpec     `json:"postmortem,omitempty"`
	CreatedAt       time.Time          `json:"createdAt,omitempty"`
	UpdatedAt       time.Time          `json:"updatedAt,omitempty"`
}

// NotifySpec selects receivers and update cadence for a set.
type NotifySpec struct {
	Receivers        []string          `json:"receivers,omitempty"`
	UpdateInterval   alerting.Duration `json:"updateInterval,omitempty"`
	Reminders        *bool             `json:"reminders,omitempty"`
	UpdateWhileAcked bool              `json:"updateWhileAcked"`
}

// PostmortemSpec controls analysis and publishing for a set.
type PostmortemSpec struct {
	Engine           string            `json:"engine,omitempty"`
	Options          map[string]string `json:"options,omitempty"`
	AutoPublish      bool              `json:"autoPublish"`
	AnalysisProvider string            `json:"analysisProvider,omitempty"`
	AnalysisTimeout  alerting.Duration `json:"analysisTimeout,omitempty"`
}

// IsEnabled reports whether the set participates in reconciliation. Sets
// created before the field existed default to enabled.
func (s AlertSet) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// RespectsSilences reports whether silenced alerts are excluded from incidents.
func (s AlertSet) RespectsSilences() bool {
	return s.RespectSilences == nil || *s.RespectSilences
}

// RespectsAcks reports whether acked alerts are excluded from incidents.
func (s AlertSet) RespectsAcks() bool {
	return s.RespectAcks != nil && *s.RespectAcks
}

// Normalize fills defaults in place.
func (s *AlertSet) Normalize() {
	s.Name = strings.TrimSpace(s.Name)
	s.Tenant = strings.TrimSpace(s.Tenant)
	s.Partition = strings.TrimSpace(s.Partition)
	if s.SeverityFloor == "" {
		s.SeverityFloor = "critical"
	}
	if len(s.GroupBy) == 0 {
		s.GroupBy = []string{"partition"}
	}
	if s.Notify.UpdateInterval <= 0 {
		s.Notify.UpdateInterval = alerting.Duration(30 * time.Minute)
	}
}

// Validate normalizes and checks an alert set.
func (s *AlertSet) Validate() error {
	s.Normalize()
	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("incident: alert set name %q must match %s", s.Name, namePattern)
	}
	if s.Tenant == "" || s.Partition == "" {
		return fmt.Errorf("incident: alert set tenant and partition are required")
	}
	if len(s.Match) == 0 {
		return fmt.Errorf("incident: alert set %q needs at least one match matcher", s.Name)
	}
	for i, matcher := range s.Match {
		if strings.TrimSpace(matcher.Name) == "" {
			return fmt.Errorf("incident: alert set %q match[%d] has no name", s.Name, i)
		}
	}
	for i, matcher := range s.Exclude {
		if strings.TrimSpace(matcher.Name) == "" {
			return fmt.Errorf("incident: alert set %q exclude[%d] has no name", s.Name, i)
		}
	}
	switch strings.ToLower(strings.TrimSpace(s.SeverityFloor)) {
	case "critical", "warning", "info":
	default:
		return fmt.Errorf("incident: alert set %q has unknown severityFloor %q", s.Name, s.SeverityFloor)
	}
	for _, label := range s.GroupBy {
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("incident: alert set %q has an empty groupBy label", s.Name)
		}
	}
	switch strings.ToLower(strings.TrimSpace(s.Postmortem.AnalysisProvider)) {
	case "", AnalysisAgent, AnalysisNone:
	default:
		return fmt.Errorf("incident: alert set %q has unknown analysis provider %q", s.Name, s.Postmortem.AnalysisProvider)
	}
	return nil
}

// Policy is the per-partition incident lifecycle configuration.
type Policy struct {
	Version           int               `json:"version"`
	MitigateDelay     alerting.Duration `json:"mitigateDelay,omitempty"`
	AutoMitigate      *bool             `json:"autoMitigate,omitempty"`
	AutoResolve       *bool             `json:"autoResolve,omitempty"`
	AutoResolveDelay  alerting.Duration `json:"autoResolveDelay,omitempty"`
	ResumeWindow      alerting.Duration `json:"resumeWindow,omitempty"`
	ReopenWindow      alerting.Duration `json:"reopenWindow,omitempty"`
	MaxUpdatesPerHour int               `json:"maxUpdatesPerHour,omitempty"`
	AnalysisProvider  string            `json:"analysisProvider,omitempty"`
	AnalysisTimeout   alerting.Duration `json:"analysisTimeout,omitempty"`
	PostmortemEngine  string            `json:"postmortemEngine,omitempty"`
	AutoPublish       bool              `json:"autoPublish"`
}

// DefaultPolicy is the authoritative default: mitigate 30 minutes after the
// last member alert clears.
func DefaultPolicy() Policy {
	autoMitigate := true
	autoResolve := false
	return Policy{
		Version:           1,
		MitigateDelay:     alerting.Duration(30 * time.Minute),
		AutoMitigate:      &autoMitigate,
		AutoResolve:       &autoResolve,
		AutoResolveDelay:  alerting.Duration(30 * time.Minute),
		ResumeWindow:      alerting.Duration(2 * time.Hour),
		ReopenWindow:      alerting.Duration(1 * time.Hour),
		MaxUpdatesPerHour: 12,
		AnalysisProvider:  AnalysisAgent,
		AnalysisTimeout:   alerting.Duration(120 * time.Second),
		PostmortemEngine:  EngineBuiltin,
	}
}

// Normalize fills missing policy fields with defaults.
func (p *Policy) Normalize() {
	defaults := DefaultPolicy()
	if p.Version <= 0 {
		p.Version = defaults.Version
	}
	if p.MitigateDelay <= 0 {
		p.MitigateDelay = defaults.MitigateDelay
	}
	if p.AutoMitigate == nil {
		p.AutoMitigate = defaults.AutoMitigate
	}
	if p.AutoResolve == nil {
		p.AutoResolve = defaults.AutoResolve
	}
	if p.AutoResolveDelay <= 0 {
		p.AutoResolveDelay = defaults.AutoResolveDelay
	}
	if p.ResumeWindow <= 0 {
		p.ResumeWindow = defaults.ResumeWindow
	}
	if p.ReopenWindow <= 0 {
		p.ReopenWindow = defaults.ReopenWindow
	}
	if p.MaxUpdatesPerHour <= 0 {
		p.MaxUpdatesPerHour = defaults.MaxUpdatesPerHour
	}
	if strings.TrimSpace(p.AnalysisProvider) == "" {
		p.AnalysisProvider = defaults.AnalysisProvider
	}
	if p.AnalysisTimeout <= 0 {
		p.AnalysisTimeout = defaults.AnalysisTimeout
	}
	if strings.TrimSpace(p.PostmortemEngine) == "" {
		p.PostmortemEngine = defaults.PostmortemEngine
	}
}

// AutoMitigateEnabled reports the effective auto-mitigate setting.
func (p Policy) AutoMitigateEnabled() bool { return p.AutoMitigate == nil || *p.AutoMitigate }

// AutoResolveEnabled reports the effective auto-resolve setting.
func (p Policy) AutoResolveEnabled() bool { return p.AutoResolve != nil && *p.AutoResolve }

// NotifyReminders reports the effective reminder setting for a set.
func (s AlertSet) NotifyReminders() bool {
	return s.Notify.Reminders == nil || *s.Notify.Reminders
}

// MemberAlert is a compact snapshot of an alert that belongs to an incident.
type MemberAlert struct {
	Fingerprint string            `json:"fingerprint"`
	Alertname   string            `json:"alertname"`
	Source      string            `json:"source,omitempty"`
	Severity    string            `json:"severity,omitempty"`
	Status      string            `json:"status"`
	StartsAt    time.Time         `json:"startsAt,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// TimelineEntry is one append-only incident event.
type TimelineEntry struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Actor   string    `json:"actor,omitempty"`
	Message string    `json:"message,omitempty"`
}

// Incident is the operator-facing incident.
type Incident struct {
	ID          string            `json:"id"`
	Tenant      string            `json:"tenant"`
	Partition   string            `json:"partition"`
	Set         string            `json:"set"`
	GroupKey    string            `json:"groupKey"`
	GroupLabels map[string]string `json:"groupLabels,omitempty"`
	Title       string            `json:"title"`
	Severity    string            `json:"severity"`
	State       string            `json:"state"`

	Owner          string     `json:"owner,omitempty"`
	OpenedAt       time.Time  `json:"openedAt"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
	MitigatedAt    *time.Time `json:"mitigatedAt,omitempty"`
	ResolvedAt     *time.Time `json:"resolvedAt,omitempty"`
	MitigatedBy    string     `json:"mitigatedBy,omitempty"`
	Mitigation     string     `json:"mitigation,omitempty"`
	ResolvedBy     string     `json:"resolvedBy,omitempty"`
	Resolution     string     `json:"resolution,omitempty"`
	AutoMitigated  bool       `json:"autoMitigated,omitempty"`
	AutoResolved   bool       `json:"autoResolved,omitempty"`
	ReopenCount    int        `json:"reopenCount,omitempty"`

	MemberFingerprints []string      `json:"memberFingerprints,omitempty"`
	MemberAlerts       []MemberAlert `json:"memberAlerts,omitempty"`
	MembersClearAt     *time.Time    `json:"membersClearAt,omitempty"`
	MitigateDueAt      *time.Time    `json:"mitigateDueAt,omitempty"`
	ResolveDueAt       *time.Time    `json:"resolveDueAt,omitempty"`
	NextUpdateAt       *time.Time    `json:"nextUpdateAt,omitempty"`

	Timeline []TimelineEntry `json:"timeline,omitempty"`
	Seq      int64           `json:"seq"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Append adds a timeline entry and increments the sequence.
func (i *Incident) Append(now time.Time, kind, actor, message string) {
	i.Seq++
	i.Timeline = append(i.Timeline, TimelineEntry{
		Seq:     i.Seq,
		At:      now.UTC(),
		Kind:    kind,
		Actor:   actor,
		Message: message,
	})
}

// Active reports whether the incident still needs operator attention.
func (i Incident) Active() bool {
	return i.State != StateResolved
}

// EvidenceRef points at telemetry or topology used by an analysis.
type EvidenceRef struct {
	Kind    string `json:"kind"`
	Query   string `json:"query,omitempty"`
	Summary string `json:"summary,omitempty"`
	Source  string `json:"source,omitempty"`
}

// ActionItem is one postmortem follow-up.
type ActionItem struct {
	Title      string  `json:"title"`
	Category   string  `json:"category,omitempty"`
	Rationale  string  `json:"rationale,omitempty"`
	Target     string  `json:"target,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// Analysis is the structured agent output embedded in a postmortem.
type Analysis struct {
	Summary             string        `json:"summary"`
	Impact              string        `json:"impact,omitempty"`
	RootCause           string        `json:"rootCause,omitempty"`
	ContributingFactors []string      `json:"contributingFactors,omitempty"`
	ActionItems         []ActionItem  `json:"actionItems,omitempty"`
	Evidence            []EvidenceRef `json:"evidence,omitempty"`
	Confidence          float64       `json:"confidence,omitempty"`
	Provider            string        `json:"provider,omitempty"`
	Model               string        `json:"model,omitempty"`
	GeneratedAt         time.Time     `json:"generatedAt,omitempty"`
	Error               string        `json:"error,omitempty"`
}

// Postmortem is the document generated when an incident resolves.
type Postmortem struct {
	IncidentID       string            `json:"incidentId"`
	Tenant           string            `json:"tenant"`
	Partition        string            `json:"partition"`
	Revision         int               `json:"revision"`
	Status           string            `json:"status"`
	Engine           string            `json:"engine"`
	EngineOptions    map[string]string `json:"engineOptions,omitempty"`
	AnalysisProvider string            `json:"analysisProvider,omitempty"`
	Markdown         string            `json:"markdown,omitempty"`
	Analysis         *Analysis         `json:"analysis,omitempty"`
	URL              string            `json:"url,omitempty"`
	ExternalID       string            `json:"externalId,omitempty"`
	Error            string            `json:"error,omitempty"`
	Attempts         int               `json:"attempts,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	UpdatedAt        time.Time         `json:"updatedAt"`
}

// Title returns a human-readable incident title.
func (i Incident) TitleText() string {
	if strings.TrimSpace(i.Title) != "" {
		return i.Title
	}
	if len(i.MemberAlerts) > 0 && strings.TrimSpace(i.MemberAlerts[0].Alertname) != "" {
		return i.MemberAlerts[0].Alertname
	}
	return i.Set
}
