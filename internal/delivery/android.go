package delivery

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// AndroidSender is a worked example of an extension receiver built on the
// webhook transport. It posts a compact, stable JSON payload that an Android
// app (or an ntfy/FCM bridge) can consume.
//
// To adapt it to Firebase Cloud Messaging, replace the HTTP call with an FCM
// send request and keep the AndroidNotification payload in the message body.
type AndroidSender struct{}

// Type implements Sender.
func (AndroidSender) Type() string { return "android" }

// Kinds implements Sender.
func (AndroidSender) Kinds() []string { return []string{KindAlert, KindIncident} }

// Validate implements Sender.
func (AndroidSender) Validate(receiver Receiver) error {
	// Endpoint presence is checked at send time so operators can define a
	// receiver before its environment variable is available.
	return nil
}

// AndroidNotification is the payload consumed by the companion app.
type AndroidNotification struct {
	SchemaVersion string            `json:"schemaVersion"`
	Kind          string            `json:"kind"`
	Event         string            `json:"event"`
	ID            string            `json:"id"`
	Title         string            `json:"title"`
	Body          string            `json:"body"`
	State         string            `json:"state,omitempty"`
	Severity      string            `json:"severity,omitempty"`
	Priority      string            `json:"priority"`
	Tags          []string          `json:"tags,omitempty"`
	URL           string            `json:"url,omitempty"`
	Actor         string            `json:"actor,omitempty"`
	OccurredAt    time.Time         `json:"occurredAt"`
	Data          map[string]string `json:"data,omitempty"`
}

// Send implements Sender.
func (AndroidSender) Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error {
	payload, err := json.Marshal(androidNotification(message))
	if err != nil {
		return err
	}
	return runtime.postWithHMAC(ctx, receiver, payload)
}

// androidNotification renders the doctor.android/v1 payload shared by the
// android and fcb receivers.
func androidNotification(message Message) AndroidNotification {
	notification := AndroidNotification{
		SchemaVersion: "doctor.android/v1",
		Kind:          message.Kind,
		Event:         message.Event,
		ID:            message.ID,
		Title:         message.Title,
		Body:          firstNonEmpty(message.Summary, strings.Join(message.FieldLines(), "\n")),
		State:         message.Status,
		Severity:      message.Severity,
		Priority:      androidPriority(message),
		Tags:          androidTags(message),
		URL:           message.URL,
		Actor:         message.Actor,
		OccurredAt:    message.OccurredAt,
		Data:          map[string]string{},
	}
	for _, field := range message.Fields {
		if strings.TrimSpace(field.Value) == "" {
			continue
		}
		notification.Data[field.Label] = field.Value
	}
	return notification
}

func androidPriority(message Message) string {
	severity := strings.ToLower(strings.TrimSpace(message.Severity))
	if severity == "critical" || severity == "page" || strings.EqualFold(message.Status, "open") {
		return "high"
	}
	return "normal"
}

func androidTags(message Message) []string {
	tags := []string{message.Kind}
	if message.Status != "" {
		tags = append(tags, message.Status)
	}
	if message.Severity != "" {
		tags = append(tags, message.Severity)
	}
	return tags
}
