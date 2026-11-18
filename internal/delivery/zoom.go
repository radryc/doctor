package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ZoomSender posts to a Zoom Team Chat incoming webhook.
type ZoomSender struct{}

// Type implements Sender.
func (ZoomSender) Type() string { return "zoom" }

// Kinds implements Sender.
func (ZoomSender) Kinds() []string { return []string{KindAlert, KindIncident} }

// Validate implements Sender.
func (ZoomSender) Validate(receiver Receiver) error {
	// Endpoint presence is checked at send time so operators can define a
	// receiver before its environment variable is available.
	return nil
}

// Send implements Sender.
func (ZoomSender) Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error {
	lines := []string{fmt.Sprintf("[%s] %s", strings.ToUpper(firstNonEmpty(message.Severity, message.Kind)), message.Title)}
	if summary := strings.TrimSpace(message.Summary); summary != "" {
		lines = append(lines, summary)
	}
	lines = append(lines, message.FieldLines()...)
	for _, section := range message.Sections {
		if len(section.Lines) == 0 {
			continue
		}
		lines = append(lines, section.Title+": "+strings.Join(section.Lines, "; "))
	}
	if message.URL != "" {
		lines = append(lines, "Open in Doctor: "+message.URL)
	}
	payload, err := json.Marshal(map[string]any{
		"content": map[string]any{
			"head": map[string]any{
				"text":     fmt.Sprintf("%s · %s", message.Kind, message.Status),
				"sub_head": map[string]any{"text": message.Title},
			},
			"body": []map[string]any{{
				"type": "message",
				"text": strings.Join(lines, "\n"),
			}},
		},
	})
	if err != nil {
		return err
	}
	return runtime.postJSON(ctx, receiver, payload)
}
