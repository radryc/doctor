package delivery

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GoAlertSender opens and closes alerts through GoAlert's generic incoming
// webhook. Incidents are intentionally not routed as GoAlert alerts.
type GoAlertSender struct{}

// Type implements Sender.
func (GoAlertSender) Type() string { return "goalert" }

// Kinds implements Sender.
func (GoAlertSender) Kinds() []string { return []string{KindAlert} }

// Validate implements Sender.
func (GoAlertSender) Validate(receiver Receiver) error {
	// Endpoint presence is checked at send time so operators can define a
	// receiver before its environment variable is available.
	return nil
}

// Send implements Sender.
func (GoAlertSender) Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error {
	alert := message.Alert
	if alert == nil {
		return fmt.Errorf("goalert receiver %q only accepts alert events", receiver.Name)
	}
	values := url.Values{
		"summary": {firstNonEmpty(alert.Annotations["summary"], alert.Alertname, "Alert")},
		"details": {goAlertDetails(alert)},
		"dedup":   {alert.Fingerprint},
	}
	if alert.Status == "resolved" {
		values["action"] = []string{"close"}
	}
	endpoint := receiver.URLValue()
	if token := receiver.SecretValue(); token != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return fmt.Errorf("goalert receiver %q has invalid url: %w", receiver.Name, err)
		}
		query := parsed.Query()
		query.Set("token", token)
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
	}
	if err := runtime.checkURL(endpoint); err != nil {
		return fmt.Errorf("delivery: receiver %q: %w", receiver.Name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "doctor-sentinel")
	for key, value := range receiver.Headers {
		req.Header.Set(key, value)
	}
	return runtime.do(req)
}

func goAlertDetails(alert *AlertPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Severity: %s\nTenant: %s\nPartition: %s\n", alert.Severity, alert.Tenant, alert.Partition)
	if alert.Source != "" {
		fmt.Fprintf(&b, "Source: %s\n", alert.Source)
	}
	if alert.Value != "" {
		fmt.Fprintf(&b, "Value: %s\n", alert.Value)
	}
	for key, value := range alert.Labels {
		if key == "alertname" || key == "severity" || key == "partition" {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", key, value)
	}
	return b.String()
}
