package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const defaultPagerDutyURL = "https://events.pagerduty.com/v2/enqueue"

// PagerDutySender sends alerts through the PagerDuty Events API v2.
// Incidents are not routed as PagerDuty alerts.
type PagerDutySender struct{}

// Type implements Sender.
func (PagerDutySender) Type() string { return "pagerduty" }

// Kinds implements Sender.
func (PagerDutySender) Kinds() []string { return []string{KindAlert} }

// Validate implements Sender.
func (PagerDutySender) Validate(receiver Receiver) error {
	// Endpoint presence is checked at send time so operators can define a
	// receiver before its environment variable is available.
	return nil
}

// Send implements Sender.
func (PagerDutySender) Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error {
	alert := message.Alert
	if alert == nil {
		return fmt.Errorf("pagerduty receiver %q only accepts alert events", receiver.Name)
	}
	routingKey := receiver.SecretValue()
	if routingKey == "" {
		return fmt.Errorf("pagerduty receiver %q has no routing key (set secretEnv)", receiver.Name)
	}
	endpoint := receiver.URLValue()
	if endpoint == "" {
		endpoint = defaultPagerDutyURL
	}
	if err := runtime.checkURL(endpoint); err != nil {
		return fmt.Errorf("delivery: receiver %q: %w", receiver.Name, err)
	}
	eventAction := "trigger"
	if alert.Status == "resolved" {
		eventAction = "resolve"
	}
	payload, err := json.Marshal(map[string]any{
		"routing_key":  routingKey,
		"event_action": eventAction,
		"dedup_key":    alert.Fingerprint,
		"payload": map[string]any{
			"summary":   firstNonEmpty(alert.Annotations["summary"], alert.Alertname, "Alert"),
			"source":    fmt.Sprintf("%s/%s/%s", alert.Tenant, alert.Partition, firstNonEmpty(alert.Source, "sentinel")),
			"severity":  pagerDutySeverity(alert.Severity),
			"timestamp": alert.StartsAt.UTC().Format(time.RFC3339),
			"custom_details": map[string]any{
				"alertname": alert.Alertname,
				"partition": alert.Partition,
				"labels":    alert.Labels,
			},
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "doctor-sentinel")
	for key, value := range receiver.Headers {
		req.Header.Set(key, value)
	}
	return runtime.do(req)
}

func pagerDutySeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "page":
		return "critical"
	case "warning", "warn":
		return "warning"
	default:
		return "info"
	}
}
