package alerting

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	defaultDispatchInterval = 2 * time.Second
	defaultWebhookTimeout   = 10 * time.Second
	defaultMaxAttempts      = 6
	maxWebhookBody          = 1 << 20
)

// Dispatcher delivers pending outbox records.
type Dispatcher struct {
	Store    *Store
	Logger   *log.Logger
	Client   *http.Client
	Interval time.Duration
	Now      func() time.Time
}

// Run delivers pending notifications until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	interval := d.Interval
	if interval <= 0 {
		interval = defaultDispatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := d.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
			d.logf("dispatch: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DispatchOnce attempts every due outbox record once.
func (d *Dispatcher) DispatchOnce(ctx context.Context) error {
	cfg, err := d.Store.GetReceiverConfig(ctx)
	if err != nil {
		return err
	}
	receivers := map[string]Receiver{}
	for _, receiver := range cfg.Receivers {
		receivers[receiver.Name] = receiver
	}
	records, err := d.Store.ListOutboxAll(ctx)
	if err != nil {
		return err
	}
	now := d.now()
	for _, record := range records {
		if record.Status == "failed" || record.Status == "delivered" {
			continue
		}
		if !record.NextAttempt.IsZero() && record.NextAttempt.After(now) {
			continue
		}
		alert, err := d.Store.GetAlert(ctx, record.Tenant, record.Partition, record.Fingerprint)
		if err != nil {
			record.Status = "failed"
			record.LastError = "alert not found: " + err.Error()
			record.UpdatedAt = now
			_ = d.Store.PutOutbox(ctx, record)
			continue
		}
		if alert.Acked || len(alert.SilenceIDs) > 0 {
			_ = d.Store.DeleteOutbox(ctx, record)
			continue
		}
		receiver, ok := receivers[record.Channel]
		if !ok {
			record.Status = "failed"
			record.LastError = "receiver not found"
			record.UpdatedAt = now
			_ = d.Store.PutOutbox(ctx, record)
			continue
		}
		err = d.send(ctx, receiver, *alert)
		record.Attempts++
		record.UpdatedAt = d.now()
		if err == nil {
			_ = d.Store.DeleteOutbox(ctx, record)
			d.logf("delivered alert %s via %s", alert.Fingerprint, receiver.Name)
			continue
		}
		record.LastError = err.Error()
		if record.Attempts >= defaultMaxAttempts {
			record.Status = "failed"
			record.NextAttempt = time.Time{}
		} else {
			record.Status = "pending"
			backoff := time.Duration(1<<uint(record.Attempts)) * time.Second
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
			record.NextAttempt = record.UpdatedAt.Add(backoff)
		}
		_ = d.Store.PutOutbox(ctx, record)
		d.logf("delivery failed alert=%s channel=%s attempt=%d: %v", alert.Fingerprint, receiver.Name, record.Attempts, err)
	}
	return nil
}

func (d *Dispatcher) send(ctx context.Context, receiver Receiver, alert Alert) error {
	switch strings.ToLower(strings.TrimSpace(receiver.Type)) {
	case "", "webhook":
		return d.sendWebhook(ctx, receiver, alert)
	case "slack":
		return d.sendSlack(ctx, receiver, alert)
	case "goalert":
		return d.sendGoAlert(ctx, receiver, alert)
	default:
		return fmt.Errorf("alerting: unsupported receiver type %q", receiver.Type)
	}
}

func (d *Dispatcher) sendWebhook(ctx context.Context, receiver Receiver, alert Alert) error {
	payload, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, receiver.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "doctor-sentinel")
	for key, value := range receiver.Headers {
		req.Header.Set(key, value)
	}
	if secret := receiverSecret(receiver); secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(payload)
		req.Header.Set("X-Sentinel-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-Doctor-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return d.do(req)
}

func (d *Dispatcher) sendSlack(ctx context.Context, receiver Receiver, alert Alert) error {
	text := formatSlackAlert(alert)
	payload, err := json.Marshal(map[string]any{"text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, receiver.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "doctor-sentinel")
	for key, value := range receiver.Headers {
		req.Header.Set(key, value)
	}
	return d.do(req)
}

func (d *Dispatcher) sendGoAlert(ctx context.Context, receiver Receiver, alert Alert) error {
	values := urlValues{
		"summary": {goAlertSummary(alert)},
		"details": {goAlertDetails(alert)},
		"dedup":   {alert.Fingerprint},
	}
	if alert.Status == StatusResolved {
		values["action"] = []string{"close"}
	}
	if token := receiverSecret(receiver); token != "" {
		values["token"] = []string{token}
	}
	body := values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, receiver.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "doctor-sentinel")
	for key, value := range receiver.Headers {
		req.Header.Set(key, value)
	}
	return d.do(req)
}

// urlValues is a small alias to keep the GoAlert payload readable.
type urlValues map[string][]string

func (v urlValues) Encode() string {
	values := make(url.Values, len(v))
	for key, items := range v {
		values[key] = append([]string(nil), items...)
	}
	return values.Encode()
}

func goAlertSummary(alert Alert) string {
	name := firstNonEmpty(alert.Labels["alertname"], "Alert")
	if summary := alert.Annotations["summary"]; summary != "" {
		return fmt.Sprintf("[%s] %s: %s", strings.ToUpper(alert.Severity), name, summary)
	}
	return fmt.Sprintf("[%s] %s", strings.ToUpper(alert.Severity), name)
}

func goAlertDetails(alert Alert) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Source:** %s\n\n", alert.Source)
	fmt.Fprintf(&b, "**Partition:** %s\n\n", alert.Partition)
	fmt.Fprintf(&b, "**State:** %s\n\n", alert.Status)
	fmt.Fprintf(&b, "**Severity:** %s\n\n", alert.Severity)
	if summary := alert.Annotations["summary"]; summary != "" {
		fmt.Fprintf(&b, "%s\n\n", summary)
	}
	b.WriteString("**Labels**\n\n")
	keys := make([]string, 0, len(alert.Labels))
	for key := range alert.Labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "- `%s=%s`\n", key, alert.Labels[key])
	}
	return b.String()
}

func (d *Dispatcher) do(req *http.Request) error {
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: defaultWebhookTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxWebhookBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func formatSlackAlert(alert Alert) string {
	summary := alert.Annotations["summary"]
	if summary == "" {
		summary = alert.Labels["alertname"]
	}
	return fmt.Sprintf("[%s] %s (%s/%s) — %s", strings.ToUpper(alert.Severity), alert.Labels["alertname"], alert.Tenant, alert.Partition, summary)
}

func receiverSecret(receiver Receiver) string {
	if receiver.SecretEnv != "" {
		return strings.TrimSpace(os.Getenv(receiver.SecretEnv))
	}
	return ""
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d *Dispatcher) logf(format string, args ...any) {
	if d.Logger != nil {
		d.Logger.Printf("sentinel: "+format, args...)
	}
}
