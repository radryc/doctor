# Cross-Boundary Tracing — Implementation Plan

- **Start:** 2026-10-24
- **Design:** [`cross-boundary-tracing-design.md`](./cross-boundary-tracing-design.md)
- **Status:** Ready to implement

## Locked decisions

1. AWS X-Ray remains the AWS source of truth.
2. Add a traces-only OTLP relay for trace locator/fleet metadata, not a full
   duplicate span store.
3. Implement `spec.placements` now.
4. Enforce X-Ray-compatible trace ID generation for roots that may enter AWS.
5. Accept 30-day X-Ray retention for the first fleet view; defer ClickHouse.
6. First live boundary: **API Gateway HTTP API → Lambda**.
7. Second live boundary: **SQS → Lambda**.

## Day 1 — canonical context and merge correctness

### 1. Add `doctor/internal/tracecontext`

Files:

- `doctor/internal/tracecontext/context.go`
- `doctor/internal/tracecontext/xray.go`
- `doctor/internal/tracecontext/id.go`
- `doctor/internal/tracecontext/context_test.go`

Implement:

- Canonical W3C 32-hex IDs.
- X-Ray `1-<8hex>-<24hex>` conversion.
- Validation and parent ID checks.
- X-Ray-compatible ID generator using
  `go.opentelemetry.io/contrib/propagators/aws/xray`.
- Bridge propagator:
  - extract W3C first,
  - fall back to X-Ray,
  - inject both,
  - warn on conflicting headers.

Acceptance:

- W3C/X-Ray round trip.
- Generated roots have current epoch prefix.
- W3C-only, X-Ray-only, both, and conflict tests pass.

### 2. Wire canonical context into Doctor

Files:

- `doctor/internal/proxy/aws.go`
- `doctor/internal/query/proxy_wiring.go`

Implement:

- Replace local X-Ray ID conversion with `tracecontext`.
- Add windowed trace lookup interface for local/AWS.
- Keep compatibility wrappers.

Acceptance:

- AWS-only trace behavior unchanged.
- Local trace lookup accepts explicit time window.

### 3. Fix trace-detail merge

Files:

- `doctor/internal/query/tempo_handlers.go`
- `doctor/internal/query/proxy_wiring.go`
- `doctor/internal/proxy/router_traces.go`

Implement:

- AWS raw lookup.
- Policy filtering.
- Bounded local lookup around AWS span timestamps.
- Merge both sources.
- Return partial trace when only one side is visible.
- Return 404 only when nothing is visible.

Acceptance:

- Synthetic k8s parent + AWS child returns both spans.
- AWS-only trace remains fast (~0.4s).
- Hidden AWS-only trace returns 404 without a full local scan.
- Mixed visible/hidden trace returns partial visible spans.

### 4. Fix cross-backend duplicates and span sets

Files:

- `doctor/internal/proxy/merge_traces.go`
- `doctor/internal/proxy/router_traces.go`
- `doctor/internal/proxy/merge_traces_test.go`

Implement:

- Deduplicate `(trace_id, span_id)` across backends.
- Merge attributes.
- Preserve parent IDs.
- Add `sources`/`backends` attributes.
- Merge span sets by trace ID and deduplicate by span ID.

Acceptance:

- Cross-backend duplicate is one span with two sources.
- Distinct spans remain distinct.
- Span sets from both backends survive.

### 5. Add synthetic mixed trace test

Files:

- `doctor/internal/query/mixed_trace_test.go`
- `doctor/internal/proxy/merge_traces_test.go`

Test:

- Local root span `T/A`.
- AWS child span `T/B`, parent `A`.
- Assert one trace, two spans, correct parent chain.
- Assert sources `doctor,aws`.
- Assert partial behavior for a hidden side.

### 6. Instrumentation defaults

Files:

- `guardian/internal/cdkgen/embed/templates/aws/v1/observability.otel-sidecar/construct.ts`
- `guardian/internal/pusher/drivers/aws/observability.go`
- `demo-divisions/templates/cdk-app/lib/guardian-observe.ts`
- `demo-divisions/lib/step2_cdk_deploy.sh`

Implement:

- `OTEL_PROPAGATORS=tracecontext,baggage,xray` for ECS apps.
- `OTEL_TRACES_SAMPLER=parentbased_always_on` for demo.
- Document `xray-lambda` for Lambda.
- Stop the demo bootstrap from resetting X-Ray destination to CloudWatch Logs.

## Day 2 — placements

1. Add `Placement` to partition/intent domain.
2. Add `spec.placements` YAML support.
3. Emit placement rows from Guardian federation feed.
4. Add placement/backend to Doctor catalog targets.
5. Select AWS target pusher from the AWS placement.
6. Add placement-scoped policy bindings.
7. Update `/v1/federation/targets` and controller.
8. Add backward-compatible derivation when placements are absent.

## Day 3 — live cross-boundary validation

1. Deploy API Gateway HTTP API → Lambda.
2. k8s checkout service → HTTP API → Lambda.
3. Lambda → SQS → Lambda.
4. AWS → k8s callback.
5. Verify one trace ID in Doctor and X-Ray.
6. Verify parent chain, source labels, partition/placement filters.
7. Verify `usera`/`userb` partial-trace isolation.
8. Test one backend down: partial trace with warning.

## Day 4 — locator and fleet view

1. Add MonoFS trace locator:
   `trace_id -> partitions, placements, backends, time range`.
2. Populate from native ingest and AWS OTLP relay.
3. Use locator in trace detail.
4. Add bounded fallback when no locator entry exists.
5. Add fleet trace graph edges:
   `source service/placement -> target service/placement`.
6. Defer ClickHouse/central span storage until after this slice.

## Commands

```bash
cd /home/rydzu/strata/doctor
go test ./...

cd /home/rydzu/strata/monofs
go test ./internal/storage/logengine ./internal/server ./internal/router

cd /home/rydzu/strata/guardian
go test ./internal/ui ./internal/domain/... ./internal/awsread
```

## Definition of done for Day 1

- [ ] `doctor/internal/tracecontext` has conversion, ID generator, and
      propagator tests.
- [ ] Synthetic mixed trace test passes.
- [ ] Trace detail merges both sources.
- [ ] AWS-only trace detail remains fast.
- [ ] Cross-backend duplicate handling and span-set merge are tested.
- [ ] Instrumentation templates include standard OTel propagator env.
- [ ] No customer app requires a Strata-specific tracing library.

## Remaining risks

- X-Ray trace IDs must have a current epoch prefix; enforce root ID policy.
- X-Ray and OTel sampling must preserve the parent sampled flag.
- Header conflicts must not silently split traces.
- Mixed partition routing must come from placement, not intent order.
- Partial traces must be explicit, especially with RBAC.

## Progress — 2026-10-24

- [x] Added `doctor/internal/tracecontext` with canonical W3C/X-Ray IDs,
      X-Ray-compatible ID generation, and the bridge propagator.
- [x] Wired canonical conversion into the AWS upstream.
- [x] Added bounded trace-window lookup and cross-source trace-detail merge.
- [x] Merged cross-source duplicate spans and combined span sets.
- [x] Added a synthetic k8s-parent + AWS-child trace test.
- [x] Added `OTEL_PROPAGATORS` and parent-based sampler defaults to the
      Guardian ECS observability templates and demo observer.
- [x] Changed the demo AWS bootstrap to keep the X-Ray destination on `XRay`
      instead of resetting it to CloudWatch Logs.
- [ ] Add the ADOT Lambda layer and `xray-lambda` configuration.
- [ ] Wire the X-Ray-compatible ID generator into every app SDK/agent root.
- [ ] Implement `spec.placements` and placement-aware federation.
- [ ] Live API Gateway HTTP API -> Lambda -> k8s validation.
