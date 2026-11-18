package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SlackSender posts alert and incident messages as Block Kit cards.
type SlackSender struct{}

// Type implements Sender.
func (SlackSender) Type() string { return "slack" }

// Kinds implements Sender.
func (SlackSender) Kinds() []string { return []string{KindAlert, KindIncident} }

// Validate implements Sender.
func (SlackSender) Validate(receiver Receiver) error {
	// Endpoint presence is checked at send time so operators can define a
	// receiver before its environment variable is available.
	return nil
}

// Send implements Sender.
func (SlackSender) Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error {
	// Preserve the historical plain-text alert payload; incidents use Block
	// Kit because they carry state, ownership, and a timeline.
	if message.Kind == KindAlert {
		text := slackAlertText(message)
		payload, err := json.Marshal(map[string]any{"text": text})
		if err != nil {
			return err
		}
		return runtime.postJSON(ctx, receiver, payload)
	}
	fields := make([]map[string]any, 0, len(message.Fields))
	for _, field := range message.Fields {
		if strings.TrimSpace(field.Value) == "" {
			continue
		}
		fields = append(fields, map[string]any{
			"type": "mrkdwn",
			"text": fmt.Sprintf("*%s:*\n%s", field.Label, field.Value),
		})
	}
	text := fmt.Sprintf("[%s] %s", strings.ToUpper(firstNonEmpty(message.Severity, message.Kind)), message.Title)
	blocks := []map[string]any{
		{"type": "header", "text": map[string]string{"type": "plain_text", "text": text}},
	}
	if summary := strings.TrimSpace(message.Summary); summary != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]string{"type": "mrkdwn", "text": summary},
		})
	}
	if len(fields) > 0 {
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	}
	if message.URL != "" {
		blocks = append(blocks, map[string]any{
			"type": "actions",
			"elements": []map[string]any{{
				"type": "button",
				"text": map[string]string{"type": "plain_text", "text": "Open in Doctor"},
				"url":  message.URL,
			}},
		})
	}
	payload, err := json.Marshal(map[string]any{"text": text, "blocks": blocks})
	if err != nil {
		return err
	}
	return runtime.postJSON(ctx, receiver, payload)
}

func slackAlertText(message Message) string {
	if message.Alert == nil {
		return fmt.Sprintf("[%s] %s", strings.ToUpper(firstNonEmpty(message.Severity, message.Kind)), message.Title)
	}
	alert := message.Alert
	summary := firstNonEmpty(alert.Annotations["summary"], alert.Alertname)
	return fmt.Sprintf("[%s] %s (%s/%s) — %s", strings.ToUpper(alert.Severity), alert.Alertname, alert.Tenant, alert.Partition, summary)
}
