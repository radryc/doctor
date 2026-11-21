package alerting

import "github.com/radryc/doctor/internal/delivery"

// EventKindAlert and EventKindIncident are re-exported so existing outbox and
// incident code keeps a stable API while renderers live in internal/delivery.
const (
	EventKindAlert    = delivery.KindAlert
	EventKindIncident = delivery.KindIncident
)

// Event is the neutral non-alert payload carried by the outbox.
type Event = delivery.Event

// EventField is one ordered display row.
type EventField = delivery.EventField

// EventSection groups related lines.
type EventSection = delivery.EventSection

// IncidentCapable reports whether a receiver type can deliver incidents.
func IncidentCapable(receiverType string) bool {
	return delivery.IncidentCapable(receiverType)
}

// ReceiverSupports reports whether a configured receiver accepts a kind.
func ReceiverSupports(receiver Receiver, kind string) bool {
	return delivery.ReceiverSupports(receiver, kind)
}
