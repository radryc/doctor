# Cross-Boundary Distributed Tracing Design

- **Status:** Proposed
- **Date:** 2026-10-23
- **Scope:** Preserve one trace ID and parent chain across k8s, AWS, and
  future backends while keeping customer applications on standard
  OpenTelemetry.
- **Related:**
  - [50k production readiness](../../../docs/federation/50k-production-readiness.md)
  - [Implementation status](./IMPLEMENTATION_STATUS.md)
  - [ClickHouse traces plan](../../../docs/federation/clickhouse-traces-plan.md)

---

## 1. Summary

The customer contract is:

> Instrument the application with normal OpenTelemetry. The platform makes the
> trace work across k8s and AWS boundaries.

Doctor currently has query-side federation: it can search native OTel spans
and AWS X-Ray spans, and it can merge results by trace ID. That is not yet
enough for a reliable cross-boundary trace. This design adds:

1. A canonical trace identity and context contract.
2. W3C ↔ X-Ray context bridging at every boundary.
3. Placement-aware partitions, so one partition can own multiple backends.
4. Correct span merging for the same trace across backends.
5. Span-level partial-trace and RBAC behavior.
6. A trace locator so fleet-scale trace lookup does not fan out to every
   partition.

The first implementation slice is deliberately small: make a synthetic
k8s-parent + AWS-child trace work end to end in Doctor, then instrument the
live demo path.

---

## 2. Goals and non-goals

### Goals

- One 128-bit trace ID across k8s OTel, AWS X-Ray, and future backends.
- Correct parent/child relationships across boundaries.
- Doctor search returns one logical trace when multiple backends saw it.
- Doctor trace detail shows spans from all participating backends.
- Partial traces are explicit and never leak unauthorized spans.
- A partition can have multiple placements, each with its own backend.
- Fleet view can identify which partitions/placements participated in a trace.
- 50k-target trace lookup does not require unbounded fan-out.
- Customer applications use standard OTel APIs and configuration.

### Non-goals

- Replacing X-Ray or requiring every customer to use X-Ray SDKs.
- Centralizing all spans immediately.
- A custom Strata tracing API in customer applications.
- Tracing application internals for uninstrumented black-box services.
- Guaranteeing a complete trace when a backend is down or sampling drops spans.

---

## 3. Current behavior and gaps

### Works today

- Doctor has one query surface for native OTel (`source=doctor`) and AWS
  X-Ray (`source=aws`).
- `MergeTraceSummaries` deduplicates search results by trace ID.
- `MergeTraceRecords` merges spans by `(trace_id, span_id)`.
- X-Ray IDs are normalized from `1-<8hex>-<24hex>` to 32-hex.
- AWS spans are canonicalized to a Guardian partition using account/region
  policy bindings.
- AWS X-Ray is live for the three demo accounts.

### Gaps

| Gap | Current behavior | Impact |
| --- | --- | --- |
| Mixed trace detail | `serveTempoTraceByID` tries AWS first and returns only AWS spans when found | A k8s + AWS trace shows only the AWS half |
| Span sets | `SearchSpanSets` overwrites `merged[traceID]` per backend | Grafana span list loses one side |
| Cross-backend duplicates | `MergeTraceRecords` keeps duplicate span IDs from different backends | Dual-exported spans appear twice |
| Mixed partition model | Federation feed emits one account/region/provider/target pusher per partition | One partition cannot reliably own k8s + AWS placements |
| AWS target selection | First non-empty `targetPusher` from any intent can win | An AWS placement may route through a k8s pusher |
| Propagation | AWS sidecar templates set an OTLP endpoint but not `OTEL_PROPAGATORS`; Lambda only has active tracing | Inbound X-Ray context may start a new trace |
| ID generation | No X-Ray-compatible ID generator is configured for k8s roots | Random OTel IDs may be rejected by X-Ray |
| Trace locator | No `trace_id -> partitions/backends` index | Fleet trace lookup would require fan-out |
| Async propagation | No SQS/SNS/EventBridge context contract | Trace breaks at queues |
| Demo bootstrap | `step2_cdk_deploy.sh` sets X-Ray destination to CloudWatch Logs | A redeploy can recreate the missing-spans-log-group blocker |

Relevant files:

- `doctor/internal/query/tempo_handlers.go`
- `doctor/internal/proxy/merge_traces.go`
- `doctor/internal/proxy/router_traces.go`
- `doctor/internal/proxy/aws.go`
- `doctor/internal/query/proxy_wiring.go`
- `guardian/internal/ui/federation_feed.go`
- `guardian/internal/domain/partition/partition.go`
- `guardian/internal/domain/intent/intent.go`
- `guardian/internal/cdkgen/embed/templates/aws/v1/observability.otel-sidecar/construct.ts`
- `guardian/internal/pusher/drivers/aws/observability.go`
- `demo-divisions/templates/cdk-app/lib/guardian-observe.ts`
- `demo-divisions/lib/step2_cdk_deploy.sh`

---

## 4. Design decisions

### D1. W3C 32-hex is the canonical internal trace ID

All backends normalize to lowercase 32-hex:

```text
4bf92f3577b34da6a3ce929d0e0e4736
```

X-Ray conversion is an adapter concern only:

```text
1-4bf92f35-77b34da6a3ce929d0e0e4736
```

### D2. X-Ray-compatible IDs at roots

X-Ray trace IDs embed epoch seconds in the first 8 hex characters. X-Ray
accepts trace IDs only within its retention window. Root services that may
send traces to X-Ray must use an X-Ray-compatible ID generator:

```text
bits 0..31   = current Unix epoch seconds
bits 32..127 = cryptographically random
```

Do not use `traceIdRatioBased` sampling together with this generator; the
non-random prefix skews ratio sampling. Use parent-based sampling.

### D3. Standard OTel in apps, composite propagation at boundaries

Customer applications use normal OTel instrumentation. The platform supplies:

```text
tracecontext + baggage + xray
```

For Lambda:

```text
tracecontext + baggage + xray-lambda
```

No customer app imports a Strata-specific context type.

### D4. Partitions own placements, not one provider

A partition can contain multiple placements:

```text
partition: checkout
  placements:
    checkout-k8s   provider=kubernetes backend=monofs
    checkout-aws   provider=aws        backend=xray
```

Each placement has its own routing and policy metadata.

### D5. Trace detail merges all participating backends

Trace detail is a merge, not a winner-take-all lookup:

- Query AWS.
- If AWS has the trace, query local only in a bounded time window around the
  AWS span timestamps.
- Merge and deduplicate.
- If only one side is visible, return a partial trace with a warning.

### D6. Trace locator before fleet-scale detail

A trace ID lookup must not fan out to 50k targets. Add a locator index:

```text
trace_id -> partitions, placements, backends, time range, root service
```

Initially this can live in MonoFS. It can move to ClickHouse with the planned
traces-only store.

### D7. Span-level RBAC and partial traces

Authorization is applied after merge, per span. A caller may see one part of a
cross-partition trace. That is a valid partial result, not an error.

---

## 5. Canonical trace model

### 5.1 Package

Add `doctor/internal/tracecontext` with no dependency on HTTP, storage, or
Doctor services.

```go
package tracecontext

type TraceID [16]byte
type SpanID  [8]byte

type Context struct {
    TraceID    TraceID
    SpanID     SpanID
    TraceState string
    TraceFlags uint8
}

func CanonicalTraceID(raw string) (string, error)
func XRayTraceID(canonical string) (string, error)
func FromXRayTraceID(raw string) (string, error)
func IsXRayCompatible(canonical string, now time.Time) bool
func XRayIDGenerator() sdktrace.IDGenerator
```

The package is a platform adapter. It is not a customer-facing API.

### 5.2 Identity rules

- Trace IDs are lowercase hex, 32 characters.
- Span IDs are lowercase hex, 16 characters.
- Parent span IDs are preserved verbatim.
- Empty parent means root.
- A zero trace ID or zero span ID is invalid.
- X-Ray `Lineage` is not mapped to W3C; it may be retained as an attribute.
- W3C `tracestate` is retained when available. X-Ray does not carry it.

### 5.3 Span representation

```go
type Span struct {
    TraceID       string
    SpanID        string
    ParentSpanID  string
    TraceState    string
    TraceFlags    uint8

    Name          string
    Service       string
    Partition     string
    Placement     string
    Backend       string
    Source        string

    StartTime     time.Time
    EndTime       time.Time
    Status        string

    Attributes         map[string]string
    ResourceAttributes map[string]string
}
```

Existing `segment.TraceRecord` can be extended or mapped to this shape. The
backend adapter owns conversion.

### 5.4 Deduplication and merge

Key:

```text
(trace_id, span_id)
```

Rules:

1. Same backend, same key: keep one.
2. Different backends, same key: keep one canonical span.
   - Merge attributes, preferring non-empty values.
   - Keep `source` as the preferred source for compatibility.
   - Add `sources=doctor,aws` and `backends=monofs,xray` attributes.
3. Different keys: keep both.
4. Never rewrite parent span IDs.
5. A missing parent is marked `missing_parent=true`; do not invent a parent.

---

## 6. Context propagation

### 6.1 Composite bridge propagator

Implement one platform bridge propagator:

```go
type BridgePropagator struct {
    W3C     propagation.TextMapPropagator // tracecontext + baggage
    XRay    propagation.TextMapPropagator
    Lambda  bool
}
```

Behavior:

- Extract W3C first.
- If no valid W3C context, extract X-Ray.
- In Lambda, when there is no active span, extract `_X_AMZN_TRACE_ID` using the
  `xray-lambda` rules before falling back.
- Inject both W3C and X-Ray on outbound calls.
- If both inbound headers are present with different trace IDs, prefer W3C,
  emit a warning attribute, and never silently split the trace.

Use the upstream OTel implementation for X-Ray:

```text
go.opentelemetry.io/contrib/propagators/aws/xray
```

Its `xray.Propagator` and `xray.NewIDGenerator()` are the Go reference.

### 6.2 Propagation matrix

| Boundary | Outbound | Inbound |
| --- | --- | --- |
| k8s → k8s | `traceparent`, `tracestate`, `baggage` | W3C |
| k8s → AWS | W3C + `X-Amzn-Trace-Id` | X-Ray/ADOT |
| AWS → AWS | `X-Amzn-Trace-Id` | X-Ray/ADOT |
| AWS → k8s | X-Ray + W3C | W3C, fallback X-Ray |
| Lambda | `traceparent` + X-Ray, `xray-lambda` extract | `_X_AMZN_TRACE_ID` |
| SQS/SNS/EventBridge | message trace attributes / `AWSTraceHeader` | consumer propagator |

### 6.3 Environment defaults

Non-Lambda:

```text
OTEL_PROPAGATORS=tracecontext,baggage,xray
OTEL_TRACES_SAMPLER=parentbased_always_on
OTEL_EXPORTER_OTLP_ENDPOINT=<platform endpoint>
```

Lambda:

```text
OTEL_PROPAGATORS=tracecontext,baggage,xray-lambda
OTEL_TRACES_SAMPLER=parentbased_always_on
```

Production sampling may use X-Ray remote sampling instead of always-on, but
the parent decision must be preserved.

### 6.4 Async boundaries

- SQS: use the `AWSTraceHeader` system attribute where supported, plus message
  attributes for W3C.
- SNS/EventBridge: message attributes.
- Batch consumers: use span links for each message rather than forcing one
  parent for the batch.
- If a message has no trace context, start a new trace and mark it
  `trace.origin=consumer`.

### 6.5 Security

- Never trust an inbound `guardian.partition` or `tenant` header for
  authorization.
- Partition identity is derived from workload identity and account/region
  bindings.
- Baggage may carry correlation hints but is not an authorization source.
- Apply RBAC per span after merge.

---

## 7. Placement-aware partitions

### 7.1 Schema

Add to the partition domain:

```go
type Placement struct {
    Name         string            `yaml:"name" json:"name"`
    Provider     string            `yaml:"provider" json:"provider"`
    Cluster      string            `yaml:"cluster,omitempty" json:"cluster,omitempty"`
    Account      string            `yaml:"account,omitempty" json:"account,omitempty"`
    Region       string            `yaml:"region,omitempty" json:"region,omitempty"`
    TargetPusher string            `yaml:"targetPusher,omitempty" json:"targetPusher,omitempty"`
    Backend      string            `yaml:"backend,omitempty" json:"backend,omitempty"`
    Labels       map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}
```

Partition:

```yaml
apiVersion: guardian/v1alpha1
kind: Partition
metadata:
  name: checkout
spec:
  placements:
    - name: checkout-k8s
      provider: kubernetes
      cluster: prod-east
      targetPusher: k8s-main
      backend: monofs
      labels:
        environment: prod

    - name: checkout-aws
      provider: aws
      account: "123456789012"
      region: us-east-1
      targetPusher: aws-123456789012-us-east-1
      backend: xray
      labels:
        environment: prod
```

Intent:

```yaml
spec:
  placement: checkout-aws
```

### 7.2 Federation feed

Replace the current one-placement `federationPartition` shape with placement
rows:

```json
{
  "partition": "checkout",
  "placement": "checkout-aws",
  "provider": "aws",
  "account": "123456789012",
  "region": "us-east-1",
  "targetPusher": "aws-123456789012-us-east-1",
  "backend": "xray",
  "labels": {"environment": "prod"}
}
```

The controller builds one catalog target per placement. The AWS target pusher
comes from the AWS placement, not from the first intent in the partition.

### 7.3 Backward compatibility

If `spec.placements` is absent:

- Derive one placement from `spec.defaults.target` and `targetPusher`.
- Derive AWS placement from an AWS intent's `spec.target`.
- Preserve current behavior for existing partitions.

If a partition has multiple AWS placements, each must have its own policy
binding and catalog target.

### 7.4 Placement attributes

Every span should carry:

```text
guardian.partition
guardian.placement
guardian.intent
guardian.asset
```

AWS spans get `guardian.partition` canonicalized from account/region and
`guardian.placement` from the catalog.

---

## 8. Query, merge, and partial traces

### 8.1 Search

Search remains fan-out by backend/placement with result caps:

- Native local backend.
- AWS federated backend.
- Other trace backends.

Summaries merge by canonical trace ID. Add:

```go
type TraceSummary struct {
    TraceID           string
    RootServiceName   string
    RootTraceName     string
    Service           string
    StartTime         time.Time
    Duration          time.Duration
    SpanCount         int
    Partitions        []string
    Placements        []string
    Backends          []string
    Sources           []string
    Region            string
    Account           string
    Status            string
}
```

Do not sum span counts blindly when the same backend may have returned
overlapping spans. For now, keep `SpanCount` approximate and add
`Sources`/`Backends` for the fleet view.

### 8.2 Trace detail algorithm

```text
loadFederatedTrace(traceID, from, to):
    awsRaw, awsFound = AWS.GetTraceRaw(traceID)
    visibleAWS = policyFilter(awsRaw)

    if awsFound:
        localFrom, localTo = bound(awsRaw timestamps, margin)
        local = MonoFS.QueryTraces(traceID, localFrom, localTo)
        merged = Merge(visibleAWS, local)
        if merged empty and awsVisible empty:
            return not found
        if some AWS spans filtered:
            partial = true
        return merged

    # No AWS trace: try local/other backends with the request window
    return router.GetTraceWindow(traceID, from, to)
```

Rules:

- The local query must use an explicit bounded window; never all-time.
- Default window margin: `DOCTOR_TRACE_LOCAL_MARGIN` (30m).
- Use the request `start`/`end` when provided.
- If AWS exists but the caller cannot see it:
  - Query local bounded window.
  - If local spans exist, return a partial trace.
  - If none exist, return `404`.
- If local errors and AWS has visible spans, return AWS with a warning.
- If AWS errors and local has spans, return local with a warning.

### 8.3 Span sets

`SearchSpanSets` must merge per trace ID:

```go
merged[traceID] = append(merged[traceID].Spans, set.Spans...)
```

Deduplicate by `span_id` after concatenation. Do not overwrite one backend's
span set with another's.

### 8.4 Partial responses

Use the existing warning mechanism and add response headers:

```text
X-Doctor-Partial: true
X-Doctor-Trace-Sources: doctor,aws
```

If only one source is visible, return the visible spans with a warning. Do not
return a trace that is intentionally empty because of RBAC.

---

## 9. Trace locator and fleet view

### 9.1 Locator record

```go
type TraceLocation struct {
    TraceID      string
    Partitions   []string
    Placements   []string
    Backends     []string
    RootService  string
    StartTime    time.Time
    EndTime      time.Time
    SpanCount    int
    RetentionEnd time.Time
}
```

Storage options, in order:

1. MonoFS JSON/object keyed by a hash shard of `trace_id`.
2. ClickHouse `trace_locations` table when the traces-only store lands.

Writes are asynchronous and idempotent: merge sets on each update.

### 9.2 Lookup flow

```text
GET /api/traces/{id}
    -> locator lookup
    -> query only listed placements/backends
    -> merge
    -> RBAC filter
    -> partial trace if needed
```

Fallback when no locator entry exists:

- Query local + AWS within the requested window.
- Populate the locator.
- Never fan out to all 50k targets.

### 9.3 Fleet view

Fleet graph nodes:

```text
(partition, placement, service.name)
```

Fleet graph edges:

```text
(source_partition, source_placement, source_service)
    ->
(target_partition, target_placement, target_service)
```

Derived from parent-child spans after merge. Mark edges with missing parents
as partial/inferred; do not fabricate them.

The X-Ray service graph is an AWS-only input. Merge it with native k8s edges
only through canonical service IDs.

---

## 10. Instrumentation defaults

### 10.1 k8s

- OTel SDK / auto-instrumentation.
- `OTEL_PROPAGATORS=tracecontext,baggage,xray`.
- X-Ray-compatible ID generator for roots.
- OTel Collector resource processor adds partition/placement/asset labels.
- Export to MonoFS/Doctor.

### 10.2 AWS ECS/Fargate

- ADOT sidecar already exists in the CDK template.
- Add propagator env to the application container.
- App exports OTLP to the sidecar.
- Sidecar exports to X-Ray and/or the Doctor OTLP relay.
- Add `guardian.partition` and `guardian.placement` resource attributes.

### 10.3 AWS Lambda

- Add ADOT Lambda layer or equivalent OTel Lambda instrumentation.
- `OTEL_PROPAGATORS=tracecontext,baggage,xray-lambda`.
- Keep active tracing enabled.
- The runtime context is in `_X_AMZN_TRACE_ID`.
- Emit application child spans; active tracing alone only records the Lambda
  service segment.

### 10.4 AWS API Gateway / ALB

- Enable X-Ray tracing.
- Verify both `X-Amzn-Trace-Id` and `traceparent` are forwarded.
- If only one is forwarded, use a gateway/edge injector or an app-level
  propagator.

### 10.5 Async

- SQS: `AWSTraceHeader` plus W3C message attributes where supported.
- SNS/EventBridge: message attributes.
- Consumer extracts and continues the trace; batch consumers use links.

---

## 11. Implementation plan

### Day 1 — canonical context and merge correctness

This is the vertical slice that makes a synthetic mixed trace work.

#### Step 1. Add `doctor/internal/tracecontext`

Files:

- `doctor/internal/tracecontext/context.go`
- `doctor/internal/tracecontext/xray.go`
- `doctor/internal/tracecontext/id.go`
- `doctor/internal/tracecontext/context_test.go`

Work:

- Canonicalize and validate W3C/X-Ray IDs.
- Implement `XRayIDGenerator()` using the upstream OTel contrib package.
- Implement the bridge propagator with precedence rules.
- Add round-trip and conflict tests.

Acceptance:

- `4bf92f...` ↔ `1-4bf92f...` round trip.
- Invalid IDs fail.
- Generated IDs have a current epoch prefix.
- W3C-only, X-Ray-only, both, and conflict cases are deterministic.

#### Step 2. Make AWS and query code use the canonical package

Files:

- `doctor/internal/proxy/aws.go`
- `doctor/internal/query/proxy_wiring.go`

Work:

- Replace local `normalizeXRayTraceID` / `xrayTraceID` logic with
  `tracecontext` helpers.
- Add a windowed trace lookup interface:

```go
type TraceWindowSource interface {
    GetTraceWindow(ctx context.Context, tenant, traceID string, from, to time.Time) ([]segment.TraceRecord, bool, error)
}
```

- Implement it for local MonoFS and AWS.
- Keep existing methods as compatibility wrappers.

Acceptance:

- No behavior change for AWS-only trace detail.
- Local trace lookup accepts an explicit window.

#### Step 3. Fix trace detail merging

Files:

- `doctor/internal/query/tempo_handlers.go`
- `doctor/internal/query/proxy_wiring.go`
- `doctor/internal/proxy/router_traces.go`

Work:

- Replace the AWS-first “return only AWS” shortcut with:
  - AWS raw lookup,
  - policy filtering,
  - bounded local lookup,
  - merge.
- If AWS is hidden but local is visible, return partial local.
- If both sides fail and nothing is visible, return the appropriate error/404.
- Preserve the existing fast AWS-only path for AWS-only traces.

Acceptance:

- Synthetic k8s parent + AWS child returns both spans.
- AWS-only trace remains fast (~0.4s).
- Hidden AWS-only trace returns 404 without scan.
- Mixed hidden/visible trace returns partial visible spans.

#### Step 4. Fix span-set and duplicate merging

Files:

- `doctor/internal/proxy/merge_traces.go`
- `doctor/internal/proxy/router_traces.go`
- `doctor/internal/proxy/merge_traces_test.go`

Work:

- Merge same-key spans across backends.
- Add `sources` / `backends` attributes.
- Concatenate span sets per trace ID and dedupe by span ID.
- Verify parent IDs survive.

Acceptance:

- Cross-backend duplicate is one span with two sources.
- Distinct spans are both present.
- Span sets from two backends both survive.

#### Step 5. Add the synthetic mixed-trace test

Files:

- `doctor/internal/query/mixed_trace_test.go`
- `doctor/internal/proxy/merge_traces_test.go`

Work:

- Fake local backend returns a root span with trace ID `T`, span ID `A`.
- Fake AWS backend returns a child span with trace ID `T`, parent `A`.
- Assert:
  - one trace summary,
  - two spans,
  - parent chain `A -> B`,
  - sources `doctor,aws`,
  - RBAC partial behavior.

Acceptance:

- `go test ./internal/query ./internal/proxy` passes.

#### Step 6. Add instrumentation defaults

Files:

- `guardian/internal/cdkgen/embed/templates/aws/v1/observability.otel-sidecar/construct.ts`
- `guardian/internal/pusher/drivers/aws/observability.go`
- `demo-divisions/templates/cdk-app/lib/guardian-observe.ts`

Work:

- Add `OTEL_PROPAGATORS=tracecontext,baggage,xray` to application containers.
- Keep the sidecar X-Ray exporter.
- Add `OTEL_TRACES_SAMPLER=parentbased_always_on` for the demo.
- Document `xray-lambda` for Lambda.
- Make demo bootstrap not reset X-Ray destination to CloudWatch Logs
  unintentionally.

Acceptance:

- Generated ECS task definitions contain the propagator env.
- Rendered CDK templates include the env.
- No customer application imports a Strata tracing library.

### Day 2 — placements and live propagation

1. Add `Placement` to the partition/intent domain.
2. Emit placement rows from the Guardian federation feed.
3. Add `placement` and `backend` to Doctor catalog targets.
4. Select the AWS target pusher from the AWS placement.
5. Add placement-aware policy bindings.
6. Update the controller and `/v1/federation/targets`.

### Day 3 — live cross-boundary validation

1. k8s checkout API → AWS API Gateway/Lambda.
2. Lambda → SQS → Lambda.
3. AWS → k8s callback.
4. Verify one trace ID in Doctor and X-Ray.
5. Verify parent chain, source labels, partition filters, RBAC.
6. Add a partial-trace chaos test with one backend stopped.

### Day 4+ — locator and fleet view

1. Add the MonoFS trace locator.
2. Use it in trace detail.
3. Add fleet trace graph aggregation.
4. Move the locator to ClickHouse when the traces-only store lands.

---

## 12. Test matrix

### Unit

- W3C ↔ X-Ray conversion.
- X-Ray-compatible ID generation.
- Composite propagator extraction/injection.
- Header conflict handling.
- Cross-backend span dedupe.
- Span-set merge.
- Partial-trace policy filtering.

### Integration

- Fake local + AWS same trace.
- Mixed partition placement catalog.
- Controller placement rows.
- Trace detail with one backend slow/down.
- RBAC: caller sees only one side.

### Live

- `k8s → API Gateway → Lambda`.
- `Lambda → SQS → Lambda`.
- `AWS → k8s`.
- X-Ray console and Doctor show the same trace ID.
- Trace detail shows k8s and AWS spans.
- Fleet view shows a cross-partition edge.

### Commands

```bash
cd strata/doctor
go test ./...

cd ../monofs
go test ./internal/storage/logengine ./internal/server ./internal/router

cd ../guardian
go test ./internal/ui ./internal/domain/... ./internal/awsread
```

---

## 13. Rollout and rollback

### Rollout

- Deploy the canonical package and merge fixes behind a config flag:
  - `DOCTOR_TRACE_MERGE_MODE=bounded|legacy`
  - default `bounded` in demo, can revert to `legacy`.
- Deploy placement support behind:
  - `DOCTOR_FEDERATION_PLACEMENTS=true`
- Keep the existing AWS-only fast path as a fallback.
- Canary one Doctor query replica first.
- Roll the controller after the query path is healthy.

### Rollback

- Set `DOCTOR_TRACE_MERGE_MODE=legacy`; trace detail returns to the old
  AWS-first path.
- Set `DOCTOR_FEDERATION_PLACEMENTS=false`; controller uses the old
  single-placement derivation.
- Roll back the query image if a merge regression is observed.
- Do not roll back the X-Ray account destination; it is independent of the
  code path.

---

## 14. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Standard OTel random IDs rejected by X-Ray | Use X-Ray-compatible ID generator for roots |
| One side sampled out | Parent-based sampling; partial-trace marking |
| Header conflict between W3C and X-Ray | Prefer W3C; warn; never silently split |
| AWS sidecar only exports X-Ray | Add OTLP relay later for fleet retention |
| Local trace lookup scans too much | Explicit time window + locator index |
| Mixed partition misroutes AWS | Placement model; test target selection |
| Duplicate dual-export spans | Dedupe by `(trace_id, span_id)` |
| X-Ray retention 30 days | Mirror through OTLP/ClickHouse if longer retention is required |
| RBAC leaks across a partial trace | Filter spans after merge; return partial, never leak |

---

## 15. Acceptance criteria

The design is implemented when:

- [ ] A synthetic k8s root span and AWS child span appear in one trace with
      the same trace ID and correct parent.
- [ ] A live k8s → AWS → k8s request produces one trace ID in both Doctor and
      X-Ray.
- [ ] Doctor trace detail shows spans from both sources.
- [ ] A cross-partition trace can be found without fan-out to every target.
- [ ] `usera`/`userb` only see their visible spans; partial traces are marked.
- [ ] AWS-only traces remain fast and backward compatible.
- [ ] Applications use standard OTel and no Strata-specific tracing API.
- [ ] The demo deploy no longer resets X-Ray destination to a broken
      Transaction Search configuration.

---

## 16. Resolved decisions (2026-10-23)

1. **AWS backend:** X-Ray remains the AWS source of truth. Add a traces-only
   OTLP relay for the trace locator/fleet metadata, not a second full copy of
   every AWS span in the first slice.
2. **Partitions:** implement `spec.placements` now. A partition may own
   multiple k8s/AWS placements and each placement selects its own backend and
   target pusher.
3. **Root IDs:** enforce X-Ray-compatible ID generation in every root service
   that may send a trace into AWS. Do not silently rewrite an existing trace.
4. **Retention:** 30-day X-Ray retention is sufficient for the first fleet
   view. Defer ClickHouse/central span storage; keep the locator in MonoFS for
   now.
5. **First live boundary:** use **API Gateway HTTP API → Lambda** for the first
   synchronous path. Add **SQS → Lambda** immediately afterward for the async
   path. Do not use the existing ALB/ECS as the first boundary; it is already
   paid for in the demo, but its nginx workload would require replacement or
   deeper instrumentation before it can prove app-level propagation.

### 16.1 Cost comparison for the first boundary

Approximate `us-east-1` list prices; demo scale is tiny, so the fixed charges
matter more than request charges.

| Path | New fixed cost | Per-request cost | Tradeoff |
| --- | ---: | ---: | --- |
| Existing ALB + ECS + nginx | ~$0 incremental (already running); ~$16.43/ALB-month + Fargate if new | ALB LCU + request data; ECS always-on | Cheapest incremental only because it exists; nginx cannot produce app spans without instrumentation |
| API Gateway HTTP API + Lambda | **$0 fixed** | ~$1.00/M API calls + ~$0.20/M Lambda requests + GB-s; X-Ray free tier | Cheapest greenfield synchronous path; standard X-Ray propagation |
| SQS + Lambda | **$0 fixed** | ~$0.40/M SQS requests + Lambda requests/GB-s | Cheapest absolute and already in the demo; async only |
| Lambda Function URL | $0 fixed | Lambda requests/GB-s | No API Gateway charge, but propagation/feature behavior is less standard; avoid for the first proof |

At 1M requests/month, HTTP API + Lambda is roughly **$1–$2/month** before
free-tier allowances. ALB has a **~$16.43/month baseline per ALB** before
LCU charges; ECS Fargate is additional if not already running. X-Ray is free
for the first 100k traces recorded and 1M retrieved/scanned per month, then
~$5/M recorded and ~$0.50/M retrieved/scanned.

**Decision:** API Gateway HTTP API + Lambda is the cheapest *synchronous*
boundary and is the first live validation target. SQS + Lambda is the second,
because it proves the async path. The existing ALB/ECS remains available for a
later production-shaped test once the app is instrumented.
