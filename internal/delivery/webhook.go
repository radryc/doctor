package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// WebhookSender is the generic, no-code extension path: it POSTs the alert or
// event payload and supports per-kind body templates.
//
// Alert payloads remain the historical raw Alert JSON so existing integrations
// are not broken. Incident payloads are doctor.incident/v1 envelopes.
type WebhookSender struct{}

// Type implements Sender.
func (WebhookSender) Type() string { return "webhook" }

// Kinds implements Sender.
func (WebhookSender) Kinds() []string { return []string{KindAlert, KindIncident} }

// Validate implements Sender.
func (WebhookSender) Validate(receiver Receiver) error {
	// Endpoint presence is checked at send time so operators can define a
	// receiver before its environment variable is available.
	return nil
}

// Send implements Sender.
func (WebhookSender) Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error {
	var payload []byte
	if body := strings.TrimSpace(receiver.Templates[message.Kind]); body != "" {
		tmpl, err := template.New("event").Parse(body)
		if err != nil {
			return fmt.Errorf("receiver %q template: %w", receiver.Name, err)
		}
		var rendered bytes.Buffer
		if err := tmpl.Execute(&rendered, message.AsEvent()); err != nil {
			return fmt.Errorf("receiver %q template: %w", receiver.Name, err)
		}
		payload = rendered.Bytes()
	} else if message.Kind == KindAlert && len(message.Raw) > 0 {
		payload = message.Raw
	} else {
		var err error
		payload, err = json.Marshal(message.AsEvent())
		if err != nil {
			return err
		}
	}
	headers := map[string]string{
		"X-Doctor-Event-Kind": message.Kind,
		"X-Doctor-Event":      message.Event,
	}
	if message.Sequence > 0 {
		headers["X-Doctor-Idempotency-Key"] = fmt.Sprintf("%s/%d", message.ID, message.Sequence)
	}
	return runtime.postSigned(ctx, receiver, payload, headers)
}
