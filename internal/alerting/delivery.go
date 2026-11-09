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
	"sync"
	"time"
)

const (
	defaultDispatchInterval = 2 * time.Second
	defaultWebhookTimeout   = 10 * time.Second
	defaultMaxAttempts      = 6
	maxWebhookBody          = 1 << 20
	defaultPagerDutyURL     = "https://events.pagerduty.com/v2/enqueue"
	// defaultOutboxClaimTTL bounds how long one replica owns a record. It must
	// exceed the slowest single send so a live dispatcher does not lose its
	// claim mid-delivery.
	defaultOutboxClaimTTL = 2 * time.Minute
	// defaultDispatchWorkers bounds concurrent webhook sends per scan.
	defaultDispatchWorkers = 8
)

// Dispatcher delivers pending outbox records.
type Dispatcher struct {
	Store    *Store
	Logger   *log.Logger
	Client   *http.Client
	Interval time.Duration
	Now      func() time.Time
	// Owner identifies this dispatcher replica in outbox claims. It defaults
	// to hostname/pid so replicas do not steal each other's in-flight sends.
	Owner string

	reconcileMu sync.Mutex
	reconciled  bool
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

// ensurePendingIndex backfills markers for records written before the pending
// index existed. It runs once per dispatcher process.
func (d *Dispatcher) ensurePendingIndex(ctx context.Context) {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()
	if d.reconciled {
		return
	}
	if err := d.Store.ReconcileOutboxMarkers(ctx); err != nil {
		// Keep trying on later ticks; new records already write their markers.
		d.logf("reconcile outbox markers: %v", err)
		return
	}
	d.reconciled = true
}

func (d *Dispatcher) owner() string {
	if d != nil && strings.TrimSpace(d.Owner) != "" {
		return strings.TrimSpace(d.Owner)
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		hostname = "doctor-sentinel"
	}
	return fmt.Sprintf("%s/%d", hostname, os.Getpid())
}

// DispatchOnce attempts every due outbox record once.
func (d *Dispatcher) DispatchOnce(ctx context.Context) error {
	d.ensurePendingIndex(ctx)
	records, err := d.Store.ListOutboxPending(ctx)
	if err != nil {
		return err
	}
	var routing sync.Map // outboxRoutingKey -> outboxRoutingEntry
	loadRouting := func(tenant, partition string) (outboxRoutingEntry, error) {
		key := outboxRoutingKey{tenant: tenant, partition: partition}
		if entry, ok := routing.Load(key); ok {
			return entry.(outboxRoutingEntry), nil
		}
		cfg, err := d.Store.GetPartitionRouting(ctx, tenant, partition)
		if err != nil {
			return outboxRoutingEntry{}, err
		}
		entry := outboxRoutingEntry{cfg: cfg, receivers: make(map[string]Receiver, len(cfg.Receivers))}
		for _, receiver := range cfg.Receivers {
			entry.receivers[receiver.Name] = receiver
		}
		actual, _ := routing.LoadOrStore(key, entry)
		return actual.(outboxRoutingEntry), nil
	}

	now := d.now()
	owner := d.owner()
	// Deliver independent records concurrently. A single slow webhook must not
	// head-of-line-block every other alert in the outbox.
	sem := make(chan struct{}, defaultDispatchWorkers)
	var wg sync.WaitGroup
	for _, record := range records {
		if record.Status == "failed" || record.Status == "delivered" {
			continue
		}
		if !record.NextAttempt.IsZero() && record.NextAttempt.After(now) {
			continue
		}
		if record.ClaimedUntil.After(now) && record.ClaimedBy != owner {
			continue
		}
		wg.Add(1)
		go func(record OutboxRecord) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			d.deliverRecord(ctx, record, now, owner, loadRouting)
		}(record)
	}
	wg.Wait()
	return nil
}

type outboxRoutingKey struct{ tenant, partition string }

type outboxRoutingEntry struct {
	cfg       *PartitionRouting
	receivers map[string]Receiver
}

// deliverRecord claims and sends one outbox record. The claim is a CAS write,
// so when several Sentinel replicas poll concurrently exactly one of them sends.
func (d *Dispatcher) deliverRecord(ctx context.Context, record OutboxRecord, now time.Time, owner string, loadRouting func(string, string) (outboxRoutingEntry, error)) {
	claimed, ok, err := d.Store.ClaimOutbox(ctx, record, owner, now, now.Add(defaultOutboxClaimTTL))
	if err != nil {
		d.logf("claim outbox %s: %v", record.ID, err)
		return
	}
	if !ok {
		return
	}
	record = claimed

	alert, err := d.Store.GetAlert(ctx, record.Tenant, record.Partition, record.Fingerprint)
	if err != nil {
		record.Status = "failed"
		record.LastError = "alert not found: " + err.Error()
		record.UpdatedAt = now
		record.ClaimedBy = ""
		record.ClaimedUntil = time.Time{}
		if putErr := d.Store.PutOutbox(ctx, record); putErr != nil {
			d.logf("mark outbox %s failed: %v", record.ID, putErr)
		}
		return
	}
	if alert.Acked || len(alert.SilenceIDs) > 0 {
		if err := d.Store.DeleteOutbox(ctx, record); err != nil {
			d.logf("delete acked outbox %s: %v", record.ID, err)
		}
		return
	}
	routing, err := loadRouting(record.Tenant, record.Partition)
	if err != nil {
		record.LastError = "routing config: " + err.Error()
		record.UpdatedAt = now
		record.ClaimedBy = ""
		record.ClaimedUntil = time.Time{}
		if putErr := d.Store.PutOutbox(ctx, record); putErr != nil {
			d.logf("update outbox %s: %v", record.ID, putErr)
		}
		return
	}
	receiver, ok := routing.receivers[record.Channel]
	if !ok {
		// The destination was removed or renamed after the notification was
		// queued. Drop the stale record instead of retrying forever.
		d.logf("drop stale notification alert=%s receiver=%s", alert.Fingerprint, record.Channel)
		if err := d.Store.DeleteOutbox(ctx, record); err != nil {
			d.logf("delete stale outbox %s: %v", record.ID, err)
		}
		return
	}
	err = d.send(ctx, receiver, *alert)
	record.Attempts++
	record.UpdatedAt = d.now()
	if err == nil {
		if err := d.Store.DeleteOutbox(ctx, record); err != nil {
			d.logf("delete delivered outbox %s: %v", record.ID, err)
		}
		d.logf("delivered alert %s via %s", alert.Fingerprint, receiver.Name)
		return
	}
	record.LastError = err.Error()
	record.ClaimedBy = ""
	record.ClaimedUntil = time.Time{}
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
	if putErr := d.Store.PutOutbox(ctx, record); putErr != nil {
		d.logf("record delivery failure for %s: %v", record.ID, putErr)
	}
	d.logf("delivery failed alert=%s channel=%s attempt=%d: %v", alert.Fingerprint, receiver.Name, record.Attempts, err)
}

func (d *Dispatcher) send(ctx context.Context, receiver Receiver, alert Alert) error {
	switch strings.ToLower(strings.TrimSpace(receiver.Type)) {
	case "", "webhook":
		return d.sendWebhook(ctx, receiver, alert)
	case "slack":
		return d.sendSlack(ctx, receiver, alert)
	case "goalert":
		return d.sendGoAlert(ctx, receiver, alert)
	case "pagerduty", "pagerduty-events", "pd":
		return d.sendPagerDuty(ctx, receiver, alert)
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
	endpoint := receiver.URL
	// GoAlert's generic incoming webhook authenticates with the token as a
	// query parameter, not a form field.
	if token := receiverSecret(receiver); token != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return fmt.Errorf("alerting: goalert receiver %q has invalid url: %w", receiver.Name, err)
		}
		query := parsed.Query()
		query.Set("token", token)
		parsed.RawQuery = query.Encode()
		endpoint = parsed.String()
	}
	body := values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
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

func (d *Dispatcher) sendPagerDuty(ctx context.Context, receiver Receiver, alert Alert) error {
	routingKey := receiverSecret(receiver)
	if routingKey == "" {
		return fmt.Errorf("alerting: pagerduty receiver %q has no routing key (set secretEnv)", receiver.Name)
	}
	endpoint := strings.TrimSpace(receiver.URL)
	if endpoint == "" {
		endpoint = defaultPagerDutyURL
	}
	eventAction := "trigger"
	if alert.Status == StatusResolved {
		eventAction = "resolve"
	}
	payload := map[string]any{
		"routing_key":  routingKey,
		"event_action": eventAction,
		"dedup_key":    alert.Fingerprint,
		"payload": map[string]any{
			"summary":   firstNonEmpty(alert.Annotations["summary"], alert.Labels["alertname"], "Alert"),
			"source":    fmt.Sprintf("%s/%s/%s", alert.Tenant, alert.Partition, firstNonEmpty(alert.Source, "sentinel")),
			"severity":  pagerDutySeverity(alert.Severity),
			"timestamp": alert.StartsAt.UTC().Format(time.RFC3339),
			"custom_details": map[string]any{
				"alertname":   alert.Labels["alertname"],
				"partition":   alert.Partition,
				"tenant":      alert.Tenant,
				"status":      alert.Status,
				"value":       alert.Value,
				"labels":      alert.Labels,
				"annotations": alert.Annotations,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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

func pagerDutySeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "fatal", "emergency":
		return "critical"
	case "warning", "warn":
		return "warning"
	default:
		return "info"
	}
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
