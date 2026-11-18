package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TeamsSender posts MessageCard or Adaptive Card notifications to Microsoft
// Teams. It accepts both Office connector webhooks and Power Automate workflow
// URLs; the format can be pinned per event kind.
type TeamsSender struct{}

// Type implements Sender.
func (TeamsSender) Type() string { return "teams" }

// Kinds implements Sender.
func (TeamsSender) Kinds() []string { return []string{KindAlert, KindIncident} }

// Validate implements Sender.
func (TeamsSender) Validate(receiver Receiver) error {
	// Endpoint presence is checked at send time so operators can define a
	// receiver before its environment variable is available.
	return nil
}

// Send implements Sender.
func (TeamsSender) Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error {
	endpoint := receiver.URLValue()
	if err := runtime.checkURL(endpoint); err != nil {
		return fmt.Errorf("delivery: receiver %q: %w", receiver.Name, err)
	}
	format := strings.ToLower(strings.TrimSpace(receiver.Format[message.Kind]))
	if format == "" {
		if strings.Contains(endpoint, "logic.azure.com") || strings.Contains(endpoint, "powerautomate") {
			format = "adaptive"
		} else {
			format = "messagecard"
		}
	}
	var payload []byte
	var err error
	if format == "adaptive" {
		payload, err = json.Marshal(teamsAdaptiveCard(message))
	} else {
		payload, err = json.Marshal(teamsMessageCard(message))
	}
	if err != nil {
		return err
	}
	return runtime.postJSON(ctx, receiver, payload)
}

func teamsMessageCard(message Message) map[string]any {
	facts := make([]map[string]string, 0, len(message.Fields))
	for _, field := range message.Fields {
		if strings.TrimSpace(field.Value) == "" {
			continue
		}
		facts = append(facts, map[string]string{"name": field.Label, "value": field.Value})
	}
	textLines := message.FieldLines()
	for _, section := range message.Sections {
		if len(section.Lines) == 0 {
			continue
		}
		textLines = append(textLines, "**"+section.Title+"**")
		for _, line := range section.Lines {
			textLines = append(textLines, "- "+line)
		}
	}
	section := map[string]any{"activityTitle": message.Title, "activitySubtitle": message.Summary}
	if len(facts) > 0 {
		section["facts"] = facts
	}
	if len(textLines) > 0 {
		section["text"] = strings.Join(textLines, "\n\n")
	}
	card := map[string]any{
		"@type":      "MessageCard",
		"@context":   "https://schema.org/extensions",
		"themeColor": teamsThemeColor(message.Severity),
		"summary":    message.Title,
		"title":      message.Title,
		"sections":   []map[string]any{section},
	}
	if message.URL != "" {
		card["potentialAction"] = []map[string]any{{
			"@type":   "OpenUri",
			"name":    "Open in Doctor",
			"targets": []map[string]string{{"os": "default", "uri": message.URL}},
		}}
	}
	return card
}

func teamsAdaptiveCard(message Message) map[string]any {
	body := []map[string]any{{
		"type":   "TextBlock",
		"text":   message.Title,
		"weight": "Bolder",
		"size":   "Medium",
		"wrap":   true,
	}}
	if summary := strings.TrimSpace(message.Summary); summary != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": summary, "wrap": true})
	}
	if facts := message.FieldLines(); len(facts) > 0 {
		body = append(body, map[string]any{"type": "TextBlock", "text": strings.Join(facts, "\n"), "wrap": true})
	}
	for _, section := range message.Sections {
		if len(section.Lines) == 0 {
			continue
		}
		body = append(body, map[string]any{
			"type": "TextBlock",
			"text": "**" + section.Title + "**\n" + strings.Join(section.Lines, "\n"),
			"wrap": true,
		})
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
	if message.URL != "" {
		card["actions"] = []map[string]any{{
			"type":  "Action.OpenUrl",
			"title": "Open in Doctor",
			"url":   message.URL,
		}}
	}
	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"contentUrl":  nil,
			"content":     card,
		}},
	}
}

func teamsThemeColor(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "page":
		return "D93F0B"
	case "warning", "warn":
		return "F2C744"
	case "mitigated", "resolved", "healthy":
		return "2EA043"
	default:
		return "1F6FEB"
	}
}
