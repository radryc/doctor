# Sentinel receiver integration spec

Status: current as of Doctor `v0.6.0`
Audience: teams building an HTTP endpoint that receives Doctor/Sentinel alerts.

This document is the contract between Doctor Sentinel (producer) and a receiver
(consumer). It is enough to implement a receiver without reading Doctor code.

---

## 1. What you are building

A receiver is an HTTP endpoint that accepts a JSON `Alert` document, verifies
its signature, and returns `2xx`.

Doctor ships eight built-in receiver types (`webhook`, `slack`, `goalert`, `pagerduty`, `teams`, `zoom`, `android`, `fcb`); the primary alert-webhook and push transports are:

| `type` | Behaviour |
|---|---|
| `webhook` (default) | `POST` the full `Alert` JSON to `url` |
| `slack` | `POST` `{"text": "..."}` to a Slack incoming webhook |
| `goalert` | form-encoded GoAlert generic webhook |
| `pagerduty` | PagerDuty Events v2 |
| `fcb` | `doctor.android/v1` to an `fcm-bridge`; `target` names the FCM topic, and device acks return through `/v1/alerts/intake/fcb/ack` |

For a custom protocol, use `type: webhook` and translate on your side. If you
own the Sentinel code and need a native transport, see §9.

---

## 2. Registration

Receivers are configured per partition. The canonical document lives at:

```text
{store}/config/routing/{tenant}/{partition}.yaml
```

Example (partition `memebank`, tenant `default`):

```yaml
receivers:
  - name: example-webhook
    type: webhook
    url: https://alerts.example.internal/sentinel
    secretEnv: EXAMPLE_RECEIVER_SECRET   # name of an env var in the sentinel pod
    headers:
      X-Team: platform
partitionReceiver: example-webhook
```

Or via the API (admin required):

```http
PUT /v1/alerts/receivers?tenant=default&partition=memebank
Content-Type: application/json

{
  "receivers": [
    {
      "name": "example-webhook",
      "type": "webhook",
      "url": "https://alerts.example.internal/sentinel",
      "secretEnv": "EXAMPLE_RECEIVER_SECRET",
      "headers": {"X-Team": "platform"}
    }
  ],
  "routes": [],
  "defaultReceiver": "example-webhook"
}
```

### Receiver fields

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | string | yes | Unique within the routing document |
| `type` | string | no | `webhook` when empty |
| `url` | string | yes for webhook/slack/goalert | Destination |
| `target` | string | no | Transport-specific destination; for `fcb` an FCM topic or FCB target. Empty uses the backend default. |
| `secretEnv` | string | recommended | **Name of an env var** in the Sentinel process holding the shared secret |
| `headers` | map[string]string | no | Static request headers |
| `description` | string | no | Documentation only |
| `timeout` | string | no | Reserved. Currently ignored; the dispatcher timeout applies (§7) |

### Destination precedence

For any alert, Sentinel chooses destinations in this order and stops at the
first match:

```text
1. explicit rule destination (alert.notify)
2. alertRoutes (source + alertname)
3. partitionReceiver
4. label routes (first non-continue match, or the union of continue matches)
5. defaultReceiver
6. legacy: if none of 2-5 are configured, send to all receivers
```

Steps 1-3 win outright. The route `continue` flag only affects step 4.

---

## 3. Webhook wire contract

### Request

```http
POST <url>
Content-Type: application/json
User-Agent: doctor-sentinel
X-Sentinel-Signature: sha256=<hex>
X-Doctor-Signature: sha256=<hex>
<static headers from the receiver config>

<Alert JSON body>
```

- Body size is bounded on the producer side (1 MiB read limit for responses);
  alert bodies are small.
- The request body is **not** gzipped, wrapped in an Alertmanager envelope, or
  batched. One HTTP request per alert per receiver.

### Body: `Alert`

```json
{
  "id": "9c2f…",
  "fingerprint": "6f1a…",
  "source": "guardian",
  "tenant": "default",
  "partition": "memebank",
  "status": "firing",
  "severity": "critical",
  "labels": {
    "alertname": "GuardianPartitionError",
    "partition": "memebank",
    "severity": "critical"
  },
  "annotations": {
    "summary": "Guardian partition memebank needs action (Failing).",
    "description": "…"
  },
  "startsAt": "2026-11-15T12:00:00Z",
  "endsAt": null,
  "value": "",
  "delivery": "sentinel",
  "createdAt": "2026-11-15T12:00:01Z",
  "updatedAt": "2026-11-15T12:00:01Z"
}
```

Fields a receiver should rely on:

| Field | Notes |
|---|---|
| `fingerprint` | **Stable dedupe key.** Same alert = same fingerprint across retries. Use it as your idempotency key. |
| `status` | `firing`, `pending`, or `resolved`. Act on `firing` and `resolved`. |
| `severity` | Free-form string; conventionally `critical`, `warning`, `info`. |
| `source` | `guardian`, `sentinel`, `rules`, `cloudwatch`, `gcp`, `subspace`, `intake`. |
| `partition` / `tenant` | Routing scope. |
| `labels.alertname` | Alert identity. |
| `annotations.summary` / `description` | Human-readable text. |
| `startsAt` / `endsAt` | RFC3339 UTC. `endsAt` is set on resolve. |
| `acked` / `silenceIds` | Normally absent from delivered alerts (§6). |

Treat unknown JSON fields as forward-compatible: ignore, do not reject.

### Signature

When `secretEnv` resolves to a non-empty value, Sentinel signs the **exact raw
request body bytes**:

```text
signature = hex(HMAC-SHA256(secret, raw_body))
```

Both headers carry the same value with a `sha256=` prefix:

```text
X-Sentinel-Signature: sha256=5f3c…
X-Doctor-Signature:   sha256=5f3c…
```

Verification rules:

1. Read the raw body bytes before JSON decoding.
2. Recompute the HMAC with the shared secret.
3. Compare in constant time (`hmac.Equal`).
4. Reject with `401` on mismatch, `400` on malformed body.

If `secretEnv` is empty or the env var is unset, no signature headers are sent.
Production receivers should require them.

### Response

| Response | Producer behaviour |
|---|---|
| Any `2xx` | Delivery succeeded; the outbox record is deleted |
| Non-`2xx` | Retried per §7; response body (up to 1 MiB) is logged |
| Timeout / connection error | Retried per §7 |

Respond within the timeout (§7) and **ack quickly**; do your own async work after
returning `204`.

---

## 4. Delivery semantics

- **At-least-once.** A receiver may see the same `fingerprint` more than once
  (retry or replica takeover). Make handling idempotent.
- **No ordering guarantee.** Records are delivered concurrently (up to 8 per
  scan). A `resolved` may arrive before or after an unrelated `firing`.
- **No periodic re-notify.** The `RepeatInterval` route field is parsed but not
  used today. A receiver gets one notification when an alert enters `firing`
  and one when it enters `resolved`.
- **Transitions only.** No notification is produced for:
  - `pending` → `pending`
  - `resolved` → `resolved`
  - alerts with `delivery != "sentinel"`
  - alerts that are acked or silenced at send time (the record is dropped)
- **Dropped receiver.** If the receiver is renamed/removed after a record is
  queued, the stale record is dropped, not retried.

### Retry schedule

| Setting | Value |
|---|---|
| Dispatch scan | every 2 s |
| Concurrent sends per scan | 8 |
| Max attempts | 6 |
| Backoff | `2^attempt` seconds, capped at 5 min |
| Per-request timeout | 10 s (dispatcher HTTP client default) |
| Attempt accounting | incremented after each send; the first retry waits 2 s |

After 6 failed attempts the record is marked `failed` and is not retried.
There is no dead-letter endpoint; the failure is logged by Sentinel.

### Replica safety

Sentinel replicas claim outbox records with a CAS lease (2 min). Exactly one
replica sends a given record at a time, but a crashed replica can cause a
re-delivery after the lease expires — again, be idempotent.

---

## 5. Security requirements

- Use **HTTPS** for `url`. Sentinel does not enforce the scheme, so this is on
  the receiver owner.
- Set `secretEnv` and verify the HMAC on every request.
- Static `headers` are fine for routing metadata; **do not** put secrets in
  `headers` (they are stored in the routing document). Use `secretEnv`.
- Never treat `labels` or `annotations` as trusted authorization input.
- Rotate the secret by updating both the Sentinel env var and the receiver;
  there is no key ID header, so rotate atomically.

---

## 6. Receiver acceptance checklist

A receiver is ready when all of these pass:

- [ ] Returns `204` (or any `2xx`) for a valid `firing` payload.
- [ ] Returns `401` for a bad/missing signature and `400` for invalid JSON.
- [ ] Verifies the signature over raw bytes, not re-marshalled JSON.
- [ ] Is idempotent per `fingerprint`: a duplicate does not open a second
      incident/ticket.
- [ ] Handles `status=resolved` by resolving the incident/ticket.
- [ ] Responds within 10 s; slow downstream work is asynchronous.
- [ ] Ignores unknown JSON fields.
- [ ] Uses `fingerprint` (not `id`) as the dedupe key.
- [ ] Logs `fingerprint`, `status`, `severity`, `partition`, `alertname` for
      audit.
- [ ] Handles `severity` case-insensitively and tolerates new values.

---

## 7. Reference implementation (Go)

```go
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

type alert struct {
	Fingerprint string            `json:"fingerprint"`
	Status      string            `json:"status"`
	Severity    string            `json:"severity"`
	Partition   string            `json:"partition"`
	Labels      map[string]string `json:"labels"`
}

var (
	secret = []byte(os.Getenv("EXAMPLE_RECEIVER_SECRET"))
	seen   = struct {
		sync.Mutex
		m map[string]string // fingerprint -> last status
	}{m: map[string]string{}}
)

func handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(secret) > 0 {
		got := strings.TrimPrefix(r.Header.Get("X-Sentinel-Signature"), "sha256=")
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(got), []byte(want)) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
	}
	var a alert
	if err := json.Unmarshal(body, &a); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	seen.Lock()
	prev := seen.m[a.Fingerprint]
	seen.m[a.Fingerprint] = a.Status
	seen.Unlock()
	if prev == a.Status {
		w.WriteHeader(http.StatusNoContent) // duplicate; already handled
		return
	}
	log.Printf("alert %s status=%s severity=%s partition=%s name=%s",
		a.Fingerprint, a.Status, a.Severity, a.Partition, a.Labels["alertname"])
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	http.HandleFunc("/sentinel", handle)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Smoke test:

```bash
BODY='{"fingerprint":"test-1","status":"firing","severity":"critical","partition":"memebank","labels":{"alertname":"Test"}}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$EXAMPLE_RECEIVER_SECRET" -hex | awk '{print $2}')"
curl -i -X POST http://localhost:8080/sentinel \
  -H 'Content-Type: application/json' \
  -H "X-Sentinel-Signature: $SIG" \
  --data "$BODY"
```

---

## 8. Adding a native receiver type (Sentinel-side only)

If your team owns Sentinel and needs a transport other than `webhook`:

1. Add a case in `internal/alerting/delivery.go`:

   ```go
   func (d *Dispatcher) send(ctx context.Context, receiver Receiver, alert Alert) error {
       switch strings.ToLower(strings.TrimSpace(receiver.Type)) {
       // …
       case "myproto":
           return d.sendMyProto(ctx, receiver, alert)
       }
   }
   ```

2. Implement:

   ```go
   func (d *Dispatcher) sendMyProto(ctx context.Context, receiver Receiver, alert Alert) error
   ```

   Use `receiverSecret(receiver)` for the secret, `d.do(req)` for the request so
   timeout, body limits, and error formatting stay consistent.

3. Add the type to `docs/alerts.md` and a unit test alongside the other
   `sendX` tests.

The receiver-side contract for a new type is whatever `sendMyProto` emits; the
rest of this spec (retry, signatures where applicable, at-least-once) still
applies.

---

## 9. Current limitations

- No batching, no ordering, no replay endpoint.
- No periodic re-notify; `repeatInterval` is accepted but unused.
- `Receiver.timeout` is reserved and currently ignored (10 s dispatcher timeout).
- Signature has no key ID; rotate secrets atomically.
- At-least-once delivery, including after replica lease expiry.

---

## 10. Questions to settle with the platform team

1. Which tenant/partition routes to this receiver, and is it
   `partitionReceiver`, `alertRoutes`, or a label route?
2. Shared secret delivery method and rotation window.
3. Expected alert volume and peak (per partition, per minute).
4. Whether `resolved` must close incidents or only annotate them.
5. Retention/audit requirements for raw payloads.
