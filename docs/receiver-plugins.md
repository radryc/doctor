# Receiver plugins

Notification receivers are transport adapters. Each channel lives in its own
file under `internal/delivery/` and implements the `delivery.Sender` interface.
Alerts and incidents share the same outbox and dispatcher, but senders receive a
kind-aware `Message`, so incident information is never forced into alert labels.

## Layout

```text
internal/delivery/
  receiver.go     Receiver config, kind support, URL/secret resolution
  registry.go     Sender interface, Registry, built-in registration
  message.go      Message/Event/AlertPayload
  http.go         HTTP helpers, URL policy, HMAC signing
  webhook.go      generic webhook
  slack.go
  teams.go
  zoom.go
  goalert.go
  pagerduty.go
  android.go      worked example for an app/bridge receiver
  fcb.go          first-class fcm-bridge / FCM topic receiver
```

## Interface

```go
type Sender interface {
    Type() string
    Kinds() []string
    Validate(receiver Receiver) error
    Send(ctx context.Context, runtime Runtime, receiver Receiver, message Message) error
}
```

- `Type` is the persisted `receiver.type` value.
- `Kinds` lists `alert` and/or `incident`. A receiver without `incident` is
  filtered out of alert-set notification pickers and delivery.
- `Validate` runs when routing is saved. Keep endpoint checks at send time if
  the value comes from an environment variable.
- `Send` receives the resolved receiver and a neutral `Message`.

`Message` exposes ordered `Fields`, `Sections`, status/severity/owner, a deep
link, and the original alert payload (`Raw`, `Alert`) for alert-specific
transports. Generic sends should use `message.AsEvent()`.

## Adding a receiver

1. Create `internal/delivery/my_receiver.go`.
2. Implement the interface.
3. Register it. `android.go` is registered in `DefaultRegistry()`; a new
   receiver can be added there or self-register from an `init()`:

   ```go
   func init() { Register(MyReceiver{}) }
   ```

4. Add a preset to `frontend/src/components/alerts/RoutingBoard.tsx` if it
   should appear in the destination editor.
5. Add a test under `internal/delivery/`.

External packages can register a sender from `cmd/doctor-sentinel`:

```go
import "github.com/radryc/doctor/internal/delivery"

func main() {
    delivery.Register(myreceiver.New())
    // ... start Sentinel
}
```

## Webhook receiver extension

The generic webhook is the no-code extension path. It sends:

- alert events: the historical raw `Alert` JSON (integration compatibility),
- incident events: `doctor.incident/v1`,
- optional per-kind body templates via `receiver.templates`.

A custom webhook receiver is usually a copy of `webhook.go` with a different
payload:

```go
type MyWebhookSender struct{}

func (MyWebhookSender) Type() string    { return "my-webhook" }
func (MyWebhookSender) Kinds() []string { return []string{KindAlert, KindIncident} }

func (MyWebhookSender) Validate(Receiver) error { return nil }

func (MyWebhookSender) Send(ctx context.Context, rt Runtime, receiver Receiver, message Message) error {
    payload, err := json.Marshal(map[string]any{
        "kind":    message.Kind,
        "event":   message.Event,
        "title":   message.Title,
        "body":    message.Summary,
        "state":   message.Status,
        "link":    message.URL,
        "fields":  message.Fields,
    })
    if err != nil {
        return err
    }
    return rt.postSigned(ctx, receiver, payload, map[string]string{
        "X-Doctor-Event-Kind": message.Kind,
    })
}
```

The receiver config still uses the shared fields:

```yaml
type: my-webhook
urlEnv: MY_WEBHOOK_URL       # keeps the token out of config
secretEnv: MY_WEBHOOK_SECRET # optional HMAC
events: [incident]           # optional allowlist
headers:
  X-Tenant: platform
```

## Android receiver

`internal/delivery/android.go` is a complete example of a webhook-shaped
receiver for an Android app or an ntfy/FCM bridge. It posts:

```json
{
  "schemaVersion": "doctor.android/v1",
  "kind": "incident",
  "event": "opened",
  "id": "platform-critical-...",
  "title": "CheckoutDown",
  "body": "1 alert(s) fired",
  "state": "open",
  "severity": "critical",
  "priority": "high",
  "tags": ["incident", "open", "critical"],
  "url": "https://doctor/#/incidents/...",
  "occurredAt": "2026-11-15T12:00:00Z",
  "data": { "State": "open", "Owner": "unassigned" }
}
```

Configure it like any other receiver:

```yaml
- name: android-oncall
  type: android
  urlEnv: ANDROID_ONCALL_WEBHOOK_URL
  secretEnv: ANDROID_ONCALL_SECRET  # optional HMAC
```

To use Firebase Cloud Messaging instead, keep `AndroidNotification` and replace
`postSigned` with an FCM `POST /v1/projects/{project}/messages:send` call. No
other Doctor component changes: the outbox, retries, delivery log, and incident
lifecycle remain the same.

## FCB (fcm-bridge) receiver

`type: fcb` is the first-class receiver for the `notfify/fcm-bridge` service. It
posts the same `doctor.android/v1` payload as the Android receiver, but resolves
the bridge URL and HMAC secret from one optional Sentinel-wide configuration.
Each receiver may name its **target** (an FCM topic or an on-call device
group). Leave `target` empty to use the backend default (the on-call rotation
for `notfify/fcm-bridge`, or its configured default topic):

```yaml
receivers:
  - name: mobile-payments
    type: fcb
    target: doctor-partition-payments
    events: [alert, incident]

  - name: mobile-platform
    type: fcb
    target: doctor-partition-platform
    events: [alert, incident]

partitionReceiver: mobile-payments
```

Configure the backend once in `doctor-sentinel`:

```bash
DOCTOR_SENTINEL_FCB_BACKEND_URL=http://doctor-fcm-bridge.internal:8080/webhook
DOCTOR_SENTINEL_FCB_WEBHOOK_SECRET=your-shared-hmac-secret
# Optional: applied when a receiver has no target.
# Leave empty to let the backend choose (on-call rotation / FCM_TOPIC).
DOCTOR_SENTINEL_FCB_DEFAULT_TOPIC=
DOCTOR_SENTINEL_FCB_TIMEOUT=10s
```

- `DOCTOR_SENTINEL_FCB_ENABLED` defaults to `true` when the backend URL is set.
  An empty URL leaves the integration disabled; existing receivers are
  unaffected.
- The shorter `DOCTOR_FCB_BACKEND_URL`, `DOCTOR_FCB_WEBHOOK_SECRET`,
  `DOCTOR_FCB_ENABLED`, `DOCTOR_FCB_DEFAULT_TOPIC`, and `DOCTOR_FCB_TIMEOUT`
  aliases are also accepted.
- `target` is sent as both `X-FCB-Target` and `X-FCM-Topic`.
  - The legacy topic path uses `X-FCM-Topic`.
  - The `notfify/fcm-bridge` on-call backend resolves `X-FCB-Target` to a named
    target (a group of assigned devices) before falling back to its rotation.
    Create the target and assign devices in the On-Call Console or via
    `/api/targets`. Use the target ID (not a name with spaces) so the value is
    also a valid FCM topic for the simple relay.
- A receiver may still override the backend with `url`/`urlEnv` and the secret
  with `secretEnv`. `X-FCM-Token` in `headers` targets one device directly.
- FCM topics do not need server-side creation. The topic exists when a device
  subscribes (`FirebaseMessaging.subscribeToTopic`) or when the first message
  is sent. Devices are owned by the FCB backend / companion app, so Doctor never
  stores device tokens. For an on-call target, assign the device in FCB instead
  of subscribing it in the app.
- The `notfify/fcm-bridge` HMAC verification (`X-Doctor-Signature`) is used
  automatically when a secret is configured.
- The Sentinel `netguard` policy still applies to the resolved bridge URL. Set
  `DOCTOR_SENTINEL_WEBHOOK_ALLOW_PRIVATE=true` (and loopback in local dev) when
  the bridge is on an internal network.

Routing works exactly as before: point `partitionReceiver`, `defaultReceiver`,
`alertRoutes`, or an individual route at different `fcb` receivers to give each
routing target its own topic.

### Device acknowledgements

`notfify/fcm-bridge` posts `doctor.android.ack/v1` back to Sentinel when a paged
device acks or nacks a notification. Sentinel exposes the callback at:

```text
POST /v1/alerts/intake/fcb/ack
```

Point the bridge at it and give both sides the same HMAC secret:

```bash
# fcm-bridge
DOCTOR_ACK_WEBHOOK_URL=http://doctor-sentinel.internal:18082/v1/alerts/intake/fcb/ack
DOCTOR_ACK_SECRET=your-shared-hmac-secret

# doctor-sentinel (falls back to DOCTOR_SENTINEL_FCB_WEBHOOK_SECRET)
DOCTOR_SENTINEL_FCB_ACK_SECRET=your-shared-hmac-secret
```

Sentinel verifies `X-Doctor-Signature` over the raw body, then resolves the
callback's `alertId` to the alert fingerprint or incident ID in the default
tenant. An `ack` sets the alert/incident to acknowledged on behalf of the
device owner; a `nack` clears a previous acknowledgement so Doctor stays in
sync with the page. The callback endpoint is disabled when no ack secret is
configured (unless `DOCTOR_SENTINEL_AUTH_ALLOW_ALL=true` for local development).

## Security

- Prefer `urlEnv` for webhook URLs; they are credentials.
- Set `secretEnv` to enable `X-Doctor-Signature` HMAC-SHA256.
- `Runtime.URLPolicy` applies the Sentinel `netguard` policy to every receiver.
- A receiver that only renders alerts must return `["alert"]` from `Kinds()` so
  incident routing cannot select it.
