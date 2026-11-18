package delivery

import (
	"encoding/json"
	"time"
)

// Event is the normalized incident/postmortem payload sent by Doctor.
type Event struct {
	SchemaVersion string          `json:"schemaVersion"`
	Kind          string          `json:"kind"`
	Event         string          `json:"event"`
	Sequence      int64           `json:"sequence,omitempty"`
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Summary       string          `json:"summary,omitempty"`
	Status        string          `json:"status,omitempty"`
	Severity      string          `json:"severity,omitempty"`
	URL           string          `json:"url,omitempty"`
	Actor         string          `json:"actor,omitempty"`
	OccurredAt    time.Time       `json:"occurredAt"`
	Fields        []EventField    `json:"fields,omitempty"`
	Sections      []EventSection  `json:"sections,omitempty"`
	Raw           json.RawMessage `json:"raw,omitempty"`
}

// EventField is one ordered display row.
type EventField struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// EventSection groups related lines.
type EventSection struct {
	Title string   `json:"title"`
	Lines []string `json:"lines,omitempty"`
}

// AlertPayload is the alert-specific subset used by GoAlert and PagerDuty.
type AlertPayload struct {
	Fingerprint string            `json:"fingerprint"`
	Alertname   string            `json:"alertname,omitempty"`
	Source      string            `json:"source,omitempty"`
	Tenant      string            `json:"tenant,omitempty"`
	Partition   string            `json:"partition,omitempty"`
	Status      string            `json:"status,omitempty"`
	Severity    string            `json:"severity,omitempty"`
	Value       string            `json:"value,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    time.Time         `json:"startsAt,omitempty"`
	EndsAt      *time.Time        `json:"endsAt,omitempty"`
}

// Message is the kind-aware input every sender receives. Alert webhooks keep
// their historical raw Alert JSON through Raw; incident senders use the
// Event fields.
type Message struct {
	SchemaVersion string
	Kind          string
	Event         string
	Sequence      int64
	ID            string
	Title         string
	Summary       string
	Status        string
	Severity      string
	URL           string
	Actor         string
	OccurredAt    time.Time
	Fields        []EventField
	Sections      []EventSection
	Alert         *AlertPayload
	Raw           []byte
}

// FromEvent builds a Message from an incident or postmortem event.
func FromEvent(event Event) Message {
	return Message{
		SchemaVersion: event.SchemaVersion,
		Kind:          event.Kind,
		Event:         event.Event,
		Sequence:      event.Sequence,
		ID:            event.ID,
		Title:         event.Title,
		Summary:       event.Summary,
		Status:        event.Status,
		Severity:      event.Severity,
		URL:           event.URL,
		Actor:         event.Actor,
		OccurredAt:    event.OccurredAt,
		Fields:        event.Fields,
		Sections:      event.Sections,
	}
}

// AsEvent returns the normalized event representation.
func (m Message) AsEvent() Event {
	event := Event{
		SchemaVersion: m.SchemaVersion,
		Kind:          m.Kind,
		Event:         m.Event,
		Sequence:      m.Sequence,
		ID:            m.ID,
		Title:         m.Title,
		Summary:       m.Summary,
		Status:        m.Status,
		Severity:      m.Severity,
		URL:           m.URL,
		Actor:         m.Actor,
		OccurredAt:    m.OccurredAt,
		Fields:        m.Fields,
		Sections:      m.Sections,
	}
	if len(m.Raw) > 0 {
		event.Raw = append(json.RawMessage(nil), m.Raw...)
	}
	return event
}

// FieldLines renders non-empty fields as "Label: Value".
func (m Message) FieldLines() []string {
	lines := make([]string, 0, len(m.Fields))
	for _, field := range m.Fields {
		if field.Value == "" {
			continue
		}
		lines = append(lines, field.Label+": "+field.Value)
	}
	return lines
}
