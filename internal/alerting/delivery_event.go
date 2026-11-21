package alerting

import (
	"context"
	"encoding/json"
	"time"
)

// deliverClaimedEvent delivers one non-alert outbox record. It mirrors the
// alert retry policy but never loads alert state, because incident events are
// self-contained in the record payload.
func (d *Dispatcher) deliverClaimedEvent(ctx context.Context, record OutboxRecord, now time.Time, loadRouting func(string, string) (outboxRoutingEntry, error)) {
	routing, err := loadRouting(record.Tenant, record.Partition)
	if err != nil {
		d.failEvent(ctx, record, "routing config: "+err.Error())
		return
	}
	receiver, ok := routing.receivers[record.Channel]
	if !ok {
		d.logf("drop stale %s notification subject=%s receiver=%s", record.Kind, record.SubjectID, record.Channel)
		if err := d.Store.DeleteOutbox(ctx, record); err != nil {
			d.logf("delete stale outbox %s: %v", record.ID, err)
		}
		return
	}
	var event Event
	if err := json.Unmarshal(record.Payload, &event); err != nil {
		d.failEvent(ctx, record, "decode event: "+err.Error())
		return
	}
	if !d.registry().Supports(receiver, event.Kind) {
		d.logf("drop %s notification subject=%s receiver=%s: receiver does not accept this kind", event.Kind, event.ID, receiver.Name)
		if err := d.Store.DeleteOutbox(ctx, record); err != nil {
			d.logf("delete unsupported outbox %s: %v", record.ID, err)
		}
		return
	}
	err = d.sendEvent(ctx, receiver, event)
	record.Attempts++
	record.UpdatedAt = d.now()
	if err == nil {
		if err := d.Store.DeleteOutbox(ctx, record); err != nil {
			d.logf("delete delivered outbox %s: %v", record.ID, err)
		}
		d.logf("delivered %s %s via %s", event.Kind, event.ID, receiver.Name)
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
	d.logf("delivery failed %s=%s channel=%s attempt=%d: %v", event.Kind, event.ID, receiver.Name, record.Attempts, err)
}

func (d *Dispatcher) failEvent(ctx context.Context, record OutboxRecord, message string) {
	record.Status = "failed"
	record.LastError = message
	record.UpdatedAt = d.now()
	record.ClaimedBy = ""
	record.ClaimedUntil = time.Time{}
	if err := d.Store.PutOutbox(ctx, record); err != nil {
		d.logf("mark outbox %s failed: %v", record.ID, err)
	}
}
