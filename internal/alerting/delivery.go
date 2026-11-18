package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/radryc/doctor/internal/delivery"
	"github.com/radryc/doctor/internal/netguard"
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
	// URLPolicy rejects SSRF targets for operator-supplied webhook URLs.
	URLPolicy *netguard.Policy
	// FCB configures the optional fcm-bridge backend used by fcb receivers.
	FCB delivery.FCBConfig
	// Receivers overrides the process-wide receiver registry (tests/extensions).
	Receivers *delivery.Registry

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
	if strings.EqualFold(strings.TrimSpace(record.Kind), EventKindIncident) {
		d.deliverClaimedEvent(ctx, record, now, loadRouting)
		return
	}

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

func (d *Dispatcher) registry() *delivery.Registry {
	if d.Receivers != nil {
		return d.Receivers
	}
	return delivery.DefaultRegistry()
}

func (d *Dispatcher) runtime() delivery.Runtime {
	return delivery.Runtime{Client: d.Client, URLPolicy: d.URLPolicy, FCB: d.FCB}
}

func (d *Dispatcher) send(ctx context.Context, receiver Receiver, alert Alert) error {
	return d.sendMessage(ctx, receiver, alertMessage(alert))
}

func (d *Dispatcher) sendEvent(ctx context.Context, receiver Receiver, event Event) error {
	return d.sendMessage(ctx, receiver, delivery.FromEvent(event))
}

func (d *Dispatcher) sendMessage(ctx context.Context, receiver Receiver, message delivery.Message) error {
	return d.registry().Send(ctx, d.runtime(), receiver, message)
}

// alertMessage adapts an Alert to the neutral sender model. Raw keeps the
// historical Alert JSON for generic webhook compatibility.
func alertMessage(alert Alert) delivery.Message {
	raw, _ := json.Marshal(alert)
	fields := []delivery.EventField{
		{Label: "State", Value: alert.Status},
		{Label: "Severity", Value: alert.Severity},
		{Label: "Partition", Value: alert.Partition},
		{Label: "Source", Value: alert.Source},
	}
	if alert.Value != "" {
		fields = append(fields, delivery.EventField{Label: "Value", Value: alert.Value})
	}
	for key, value := range alert.Labels {
		if key == "alertname" || key == "severity" || key == "partition" {
			continue
		}
		fields = append(fields, delivery.EventField{Label: key, Value: value})
	}
	return delivery.Message{
		SchemaVersion: "doctor.alert/v1",
		Kind:          delivery.KindAlert,
		Event:         alert.Status,
		ID:            alert.Fingerprint,
		Title:         firstNonEmpty(alert.Labels["alertname"], alert.Annotations["summary"], "Alert"),
		Summary:       alert.Annotations["summary"],
		Status:        alert.Status,
		Severity:      alert.Severity,
		URL:           alert.Annotations["guardian.url"],
		OccurredAt:    alert.StartsAt,
		Fields:        fields,
		Raw:           raw,
		Alert: &delivery.AlertPayload{
			Fingerprint: alert.Fingerprint,
			Alertname:   alert.Labels["alertname"],
			Source:      alert.Source,
			Tenant:      alert.Tenant,
			Partition:   alert.Partition,
			Status:      alert.Status,
			Severity:    alert.Severity,
			Value:       alert.Value,
			Labels:      alert.Labels,
			Annotations: alert.Annotations,
			StartsAt:    alert.StartsAt,
			EndsAt:      alert.EndsAt,
		},
	}
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
