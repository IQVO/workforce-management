---
id: 0022-resilience-circuit-breakers-retry-dlq-shutdown
slug: /adr/0022-resilience-circuit-breakers-retry-dlq-shutdown
title: 22. Per-dependency circuit breakers, read-only retry, Kafka DLQ, and graceful shutdown hardening
sidebar_label: 22. Circuit breakers, retry, DLQ, shutdown
description: "ADR 0022 — Phase 2 resilience for workforce-management, ported verbatim from order-management's ADR-0025 (PR #107): sony/gobreaker/v2 circuit breakers per outbound dependency (never one global breaker) that reuse each client's EXISTING permissive fail-open/fail-loud behaviour as the OPEN-state fallback rather than inventing a new one; cenkalti/backoff/v4 jittered retry on labor-performance's read-only GET only, never on fulfillment-execution's commit-gating GET; a dead-letter topic for the analytics consumer so one poison message cannot block its partition; and a readiness-flip-first graceful shutdown sequence."
---

# 22. Per-dependency circuit breakers, read-only retry, Kafka DLQ, and graceful shutdown hardening

## Status

Accepted — implemented in the same change that introduced this record.
This is Phase 2 (resilience) of the fleet production-readiness plan,
porting order-management's ADR-0025 (PR #107) verbatim to this
service's own two outbound HTTP clients and its own inbound Kafka
consumer, per that ADR's stated intent to be the reference pattern the
other fleet repos in this phase copy.

## Context

Before this change, workforce-management had two sync cross-context
HTTP clients — `laborperformance.Client` (read-only:
`GET /task-types/{taskType}/performance`, feeding `ProposePathPlan`'s
optional rate enrichment) and `fulfillmentexecution.Client` (read-only:
`GET /capacity/{capability}`, feeding `CommitShiftPlan`'s installed-
capacity ceiling) — each with a `permissive`/`http` mode switch (plus a
third `kafka-cache` mode for labor-performance, out of scope here — see
below) but **no circuit breaker, no bounded retry, and no
context-deadline propagation**: a slow or failing downstream just hung
every caller until its own (fresh, hardcoded) per-call timeout, and a
failing dependency was retried on every single request forever, with
no mechanism to stop hammering it. Its inbound `AnalyticsConsumer` had
**no dead-letter handling at all** — a single message whose projection
application always errors would be logged and the loop would continue
(this consumer already did not wedge on a handling error, unlike
order-management's `RepromiseConsumer` pre-ADR-0025), but that message
was then simply abandoned on every subsequent poll with the offset
never committed for it specifically, and no replay-capable record of
the failure beyond a log line. Graceful shutdown already existed
(`signal.Notify` + `server.Shutdown`) but had no readiness-flip step
and no `GET /readyz` distinct from the liveness-only `/healthz`.

Both clients' existing fail-open-vs-fail-loud semantics are preserved
exactly, verified from the current code before writing this ADR:

- `laborperformance.PermissiveClient.MeanActualSeconds` always returns
  `ports.ErrMeasuredRateUnavailable` — the SAME sentinel the real
  `Client` returns for a malformed response, an unreachable service, or
  a genuine "no data yet" answer. `ProposePathPlan`'s single
  error-handling branch depends on this being the only error a
  `MeasuredRateClient` ever returns, and treats it as a soft,
  fail-OPEN signal (trim/skip the enrichment, do not fail the
  proposal).
- `fulfillmentexecution.PermissiveClient.InstalledCapacity` always
  returns `ports.ErrInstalledCapacityUnavailable` — and
  `CommitShiftPlan` treats this as FATAL to the commit (fail LOUD):
  every `ShiftPlan` commit fails while `INSTALLED_CAPACITY_MODE` is
  not `http`, per this fleet's rule that anything gating a mutation of
  real state fails loud rather than silently succeeding against a
  no-op. This is confirmed unchanged by this ADR — the breaker's
  OPEN-state fallback reuses this exact fail-loud contract, it does
  not soften it.

`LABOR_PERFORMANCE_MODE`'s third mode, `kafka-cache`
(`internal/adapters/outbound/laborperformancecache`), replaces the
synchronous HTTP call entirely with a local, in-memory read model fed
by labor-performance's own integration topic — it has no HTTP call for
a breaker to wrap and is explicitly out of scope for this ADR, exactly
as the task brief specifies.

## Decision

### 1. One circuit breaker PER downstream dependency, never one global breaker

`internal/resilience` (a new, tiny, dependency-free top-level package,
mirroring order-management's package of the same name and this
service's own existing `internal/adapters/outbound/bootretry`
"top-level internal helper" convention) holds the tuning EVERY breaker
in this service shares:

```go
const (
    DefaultMaxRequests = 1                // 1 probe per half-open cycle
    DefaultInterval    = 30 * time.Second // closed-state rolling-Counts reset window
    DefaultTimeout     = 30 * time.Second // open-state cooldown before a half-open probe
)

func ReadyToTrip(counts gobreaker.Counts) bool {
    if counts.ConsecutiveFailures >= 5 {
        return true
    }
    if counts.Requests < 10 { // minimum sample size before the error-rate leg engages
        return false
    }
    return float64(counts.TotalFailures)/float64(counts.Requests) > 0.5
}
```

`laborperformance.NewBreakerClient` and
`fulfillmentexecution.NewBreakerClient` each construct their OWN
`*gobreaker.CircuitBreaker[...]` instance using this shared
`ReadyToTrip`/tuning — a slow or failing labor-performance deployment
tripping its breaker can never affect fulfillment-execution's breaker
(or vice versa), and each keeps its own independent `*http.Client`
(bulkhead, §4).

### 2. The breaker's OPEN-state fallback REUSES each client's existing mode semantics — it does not invent a new one

- `laborperformance.BreakerClient`, while OPEN, calls
  `PermissiveClient.MeanActualSeconds` — the SAME fail-OPEN behaviour
  (`ports.ErrMeasuredRateUnavailable`) this client already had for a
  transport error, a malformed response, or genuinely no data yet.
- `fulfillmentexecution.BreakerClient`, while OPEN, calls
  `PermissiveClient.InstalledCapacity` — the SAME fail-LOUD behaviour
  (`ports.ErrInstalledCapacityUnavailable`, which
  `CommitShiftPlan` treats as fatal) this client already had, because
  an installed-capacity ceiling gates a COMMIT that mutates real state
  and must never appear to succeed against a tripped breaker any more
  than against a no-op.

`isBreakerRejection(err)` (duplicated, unexported, in each package —
adapters never depend on each other per this service's hexagonal
fitness tests) distinguishes gobreaker refusing to even ATTEMPT the
call (`gobreaker.ErrOpenState`/`ErrTooManyRequests`) from a real error
a call gobreaker DID let through; only the former routes to the
fallback.

### 3. Context deadline propagation: `resilience.CallTimeout`

Both `BreakerClient.MeanActualSeconds` and
`BreakerClient.InstalledCapacity` derive their per-call timeout from
the inbound request's own remaining `ctx.Deadline()` via
`resilience.CallTimeout`, capped at the CLIENT package's own
`DefaultTimeout` — **3s per attempt** in both `laborperformance` and
`fulfillmentexecution` (the same value as each client's `http.Client.Timeout`,
so the context cap and the transport cap agree) — never a fresh, hardcoded
timeout that could outlast the caller's own patience. The 3s per-attempt cap is
intentional: both calls sit on a synchronous request path (a staffing-gap or
proposal read; a shift-plan commit) where waiting 30s is worse than failing
over to the fallback. The unrelated 30s `resilience.DefaultTimeout` shown in §1
is the breaker's **open-state cooldown**, not a per-call timeout. (An earlier
revision of this record said the per-call cap was `DefaultTimeout` (30s); that
conflated the two constants and never matched the code.)

### 4. Bulkhead: confirmed, not newly built

Both `laborperformance.NewClient` and `fulfillmentexecution.NewClient`
already each construct/accept their own `HTTPDoer` — there was never a
shared client across the two dependencies. This ADR only confirms and
documents that invariant.

### 5. Retry (`cenkalti/backoff/v4`, jittered, max 3 attempts) ONLY on labor-performance's read

`laborperformance.BreakerClient.MeanActualSeconds` retries the
underlying HTTP call up to 3 total attempts
(`backoff.WithMaxRetries(policy, 2)`), jittered exponential backoff
(50ms–500ms), bounded by the SAME `callCtx` `CallTimeout` derived. A
path with no `TaskType` mapping fails immediately
(`ports.ErrMeasuredRateUnavailable`, no HTTP call at all) rather than
retrying — there is nothing a retry could change about that answer.
The retry loop runs INSIDE one `breaker.Execute` call, so a retry storm
against an already-degraded dependency still only ever counts as ONE
success/failure toward the breaker's trip condition, not three.

**`fulfillmentexecution`'s `InstalledCapacity` deliberately does NOT
get blind retry**, even though it is itself a GET (a pure read),
because — unlike labor-performance's soft rate enrichment — it gates a
COMMIT that mutates real state (`CommitShiftPlan`'s installed-capacity
ceiling). This fleet's convention (mirroring order-management's
`inventorystorage.BreakerClient`, which withholds retry from its
mutating calls for the same reason) is that a call gating a mutating
commit gets a breaker but not blind retry: a retry storm against an
already-degraded dependency should trip the breaker promptly instead of
masking the degradation behind extra attempts on the exact call that
decides whether real capacity is being exceeded.

### 6. Dead-letter queue for `AnalyticsConsumer`

`AnalyticsConsumer.Run`'s per-message handler now retries
`HandleMessage` in-process, with jittered backoff
(`cenkalti/backoff/v4`, 100ms–2s), up to `maxAnalyticsHandlerAttempts`
(3) total attempts. Every attempt exhausted is published — raw payload
byte-for-byte, plus `x-dlq-source-topic`/`x-dlq-error`/`x-dlq-failed-at`
headers carrying replay/debugging context — to `<source-topic>.dlq`
via a `*segmentio.Writer` the consumer now owns (`AnalyticsConsumer`'s
new `dlqWriter` field, closed alongside the reader in `Close`), and
**the offset is committed anyway**: one poison message must never
permanently block every other analytics event behind it on the same
partition. This is logged at ERROR level with enough context (topic,
dlq_topic, attempts, error) to be an alert-worthy signal, not a silent
drop.

The dead-letter topic is always derived as
`<the consumer's own source topic>+".dlq"` (never a fixed constant),
mirroring order-management's `RepromiseConsumer` convention exactly.

Proven end to end with a real testcontainers Kafka
(`analytics_dlq_integration_test.go`,
`TestAnalyticsConsumer_PoisonMessage_GoesToDeadLetterTopicWithoutBlockingPartition`):
a `LaborAssigned` message whose projection application is made to
always fail for one specific `path_id` lands on the `.dlq` topic after
exactly 3 attempts, with the raw original JSON payload and the
error-context headers intact, and — published right after the poison
message on the SAME topic — a well-formed message for a different path
is applied without delay, proving the partition was never blocked.

`workforce-projector`'s composition
(`cmd/workforce-projector/main.go`) needed NO changes: it already
constructs its `AnalyticsConsumer` via the unchanged
`NewAnalyticsConsumer` constructor signature, which now wires the DLQ
writer internally.

### 7. Circuit breaker state as a Prometheus gauge

`telemetry.CircuitBreakerMetrics` (new,
`internal/adapters/outbound/telemetry/circuit_breaker_metrics.go`)
registers ONE OTel `Int64Gauge`, `circuit_breaker.state` — re-exported
by this fleet's OTel Collector prometheus exporter as
`circuit_breaker_state{dependency="labor-performance"|"fulfillment-execution"}`
(0=closed, 1=half-open, 2=open, gobreaker's own numbering verbatim, no
translation table) — on the SAME global `otel.Meter` this service's
other metrics already use, not a second, parallel registry.
`resilience.RecordStateChange(dependency, recorder)` adapts a
`resilience.StateRecorder` into `gobreaker.Settings.OnStateChange`'s
signature; a `nil` recorder is a documented no-op, so a test that does
not care about the metric never needs to construct one.

### 8. Graceful shutdown hardening

`cmd/workforce`'s `signal.Notify` + `server.Shutdown(shutdownCtx)` sequence
(`cmd/workforce/serve.go`, `shutdownSequence`) runs in this order:

1. **Flip readiness to not-ready FIRST**
   (`inbound.Readiness.SetNotReady`, backing a new `GET /readyz`,
   distinct from the pre-existing `GET /healthz` which stays a pure
   liveness signal and is never flipped by shutdown) — before anything
   else stops.
2. **Wait the drain delay** (`SHUTDOWN_DRAIN_DELAY`, default `10s` = two
   `readinessProbe` periods of 5s; `0` disables it, which is what tests use).
   While the pod keeps serving, a Kubernetes `readinessProbe` polling
   `/readyz` observes the 503 and endpoint removal begins, so NEW traffic
   stops being routed here *before* the listener closes. (An earlier
   revision flipped readiness and called `server.Shutdown` in the same
   instant, so with a 5s probe period the flip was never observable and the
   step was cosmetic.)
3. Stop accepting new HTTP connections and drain in-flight requests —
   `server.Shutdown(shutdownCtx)`, with a 10s budget shared by steps 3–6.
4. Stop the process-path catalogue/labor-performance-cache Kafka consumers.
5. Stop the housekeeping sweeper
   ([ADR 0028](./0028-housekeeping-sweeper-idempotency-keys-and-outbox.md)).
6. Stop the outbox relay and wait for its in-flight pass.
7. The deferred closes then release the publisher's adapters and the
   Postgres pool **last**, after nothing can touch it any more.

Worst case this is 10s drain + 10s budget + 5s telemetry flush = 25s inside
the chart's `terminationGracePeriodSeconds: 30`. The order is unit-tested
(`cmd/workforce/shutdown_test.go`), including that `/readyz` answers 503 while
the listener is still serving during the drain window.

`Readiness`'s zero value (and a `nil *Readiness`) is always ready —
every existing test and any caller that predates this type behaves
exactly as before. `GET /readyz` is deliberately NOT added to
`apis/openapi.yaml` — mirroring order-management's own choice not to
document this operational endpoint in its public API contract (only
`/healthz` is documented there), so this change makes no
`apis/openapi.yaml` edit and needs no `docs && npm run gen-api-docs`
regeneration.

## Consequences

- Every outbound call from `laborperformance`/`fulfillmentexecution`
  now derives its timeout from the caller's remaining budget rather
  than a fresh hardcoded one.
- A failing labor-performance or fulfillment-execution deployment now
  trips ONE breaker after 5 consecutive failures (or a sustained >50%
  error rate with enough volume) and stops sending real traffic to it
  for `DefaultTimeout` (30s) before probing again.
- `AnalyticsConsumer` can no longer be permanently wedged by one
  poison message; every other event on the partition keeps flowing.
  The `.dlq` topic is a new operational surface: it needs monitoring/
  alerting (out of scope for this change — the ERROR-level log line is
  the interim signal) and a manual replay tool (also out of scope).
- `GET /readyz` is a new, distinct endpoint fleet operators/SRE tooling
  should point `readinessProbe`s at going forward — `/healthz` alone is
  no longer sufficient for a pod that participates in a graceful drain.
- `sony/gobreaker/v2` and `cenkalti/backoff/v4` (already present as an
  indirect dependency; promoted to direct) are new/promoted direct
  dependencies.
- `LABOR_PERFORMANCE_MODE=kafka-cache` is unaffected: it has no
  synchronous HTTP call for a breaker to wrap.

## Alternatives considered

- **One global circuit breaker for both outbound dependencies:**
  rejected — a fulfillment-execution outage tripping the SAME breaker
  that guards labor-performance's soft rate enrichment would
  incorrectly degrade a wholly unrelated, non-mutating call.
- **A brand-new fallback behaviour when a breaker opens:** rejected —
  the breaker only decides WHEN to fall back, not WHAT the fallback
  is; inventing new fallback semantics here would diverge from the
  pre-existing, already-understood `permissive` mode contracts this
  service (and its operators) already reason about.
- **Retrying `fulfillmentexecution.InstalledCapacity` since it is a
  GET:** considered and rejected — it gates a commit that mutates real
  state; see §5.
- **Dropping a DLQ message instead of publishing it:** rejected — an
  alert-worthy signal with full replay context is strictly more
  operationally useful than a silent drop.

## References

- Reference implementation ported verbatim: order-management PR #107,
  ADR-0025 (`docs/docs/adr/0025-resilience-circuit-breakers-retry-dlq-shutdown.md`).
- ADR-0012/ADR-0019 — labor-performance's `MeasuredRateClient`
  fail-open contract this ADR's breaker-fallback design inherits
  unchanged.
- ADR-0014 — fulfillment-execution's `InstalledCapacityClient`
  fail-loud contract this ADR's breaker-fallback design inherits
  unchanged.
