---
id: 0020-idle-share-staffing-signal
slug: /adr/0020-idle-share-staffing-signal
title: 0020. Consume labor-performance's idle-share signal for staffing surfacing and proposal trim
sidebar_label: 0020. Idle-share staffing signal
sidebar_position: 21
description: laborperformancecache.Consumer now also tracks a running idle share per TaskType, surfaced on GetStaffingGap and used by ProposePathPlan to trim over-proposed heads when associates are already substantially idle.
---

# 0020. Consume labor-performance's idle-share signal for staffing surfacing and proposal trim

## Status

Accepted — implemented in the same change that introduced this record.

## Context

`labor-performance` (Phase 1 of the fleet's labor-utilization/idleness
plan, PR #44 on that repo) now measures idle gaps between an associate's
consecutive completed tasks and publishes the result additively on its
existing `TaskPerformanceRecorded` integration event
(`warehouse.labor-performance.events`, the SAME topic this repo's
`laborperformancecache.Consumer` already consumes for the measured-rate
feed, ADR-0019): a new nullable `idle_seconds_before` field. `nil` means
"not observed" — the associate's first-ever completion, a negative/zero
gap from Kafka's at-least-once/unordered delivery, or an empty
`AssociateId` (a robot-occupied station) — and must never be coerced into
a zero-idle observation.

This is exactly the kind of signal `workforce-management` (staffing
decisions) needs to consume, per the fleet's idleness plan: labor-
performance measures idle time, this context (a staffing consumer) turns
it into two things —

1. **Visibility**: a shift manager reading `GetStaffingGap` should be able
   to see the observed idle share for a path's task type alongside the
   planned-vs-active headcount, without a separate call.
2. **A proposal input**: `ProposePathPlan`'s headcount proposal is
   currently blind to whether the associates already on a path are mostly
   idle. A path with, say, a 50% observed idle share needs measurably
   fewer ADDITIONAL heads to clear the same charge than the naive
   `ceil(charge / rate)` computation assumes — associates already have
   slack.

This context never links an associate to a specific task (`AGENTS.md`'s
"stops at the path boundary" rule, ADR-0002) — idle share here is a
TaskType-level aggregate, exactly like the existing measured-rate signal,
never a per-associate quantity surfaced or acted on individually.

## Decision

**Extend `laborperformancecache.Consumer`, not a new adapter.** The
Consumer already replays `warehouse.labor-performance.events` and
maintains a running mean of `actual_seconds` per `TaskType` (ADR-0019).
Alongside that existing `runningMean` (`sum float64, count int64`), it now
also keeps an `idleShareTotals` per `TaskType`:

```go
type idleShareTotals struct {
	idleSeconds   float64
	actualSeconds float64
	observed      bool
}
```

`idleShare = idleSeconds / (idleSeconds + actualSeconds)` — the fraction
of a TaskType's clocked time (task-doing time plus between-task idle
waiting time) that was idle. This mirrors ADR-0019's own running-mean
strategy byte-for-byte, for the identical reasons documented there: small,
fixed TaskType cardinality (`PICK`/`PACK`/`SLAM`), no eviction complexity,
and it answers "what is the measured idle share" (a stable aggregate), not
"was the last task idle" (a noisy single sample). No EMA, no bounded
window — sum+count running totals, same as the mean.

Every `TaskPerformanceRecorded` message contributes its `actual_seconds`
to the denominator (via the SAME code path that already updates the
mean); a message with a non-nil `idle_seconds_before` additionally
contributes to the numerator and flips `observed = true`. A message with
`idle_seconds_before: null` contributes to the denominator only — it is
absent from the idle numerator by construction, exactly matching the
"not observed, never a fabricated 0" rule labor-performance's own event
documents. A TaskType that has an `actual_seconds` mean but has never
observed a non-nil `idle_seconds_before` reports `ErrIdleShareUnavailable`,
not a fabricated 0% idle share.

**A new outbound port, `ports.IdleShareClient`,** mirrors
`ports.MeasuredRateClient`'s shape exactly:

```go
type IdleShareClient interface {
	IdleSharePct(ctx context.Context, pathId shared.PathId) (float64, error)
}
```

`Consumer` satisfies this directly (a second interface on the same
concrete type — not a new adapter, not a new port family), the same way
it already satisfies `MeasuredRateClient`. Only `LABOR_PERFORMANCE_MODE=
kafka-cache` wires it: `http` and `permissive` have no per-message
`idle_seconds_before` stream to observe, so both `GetStaffingGap` and
`ProposePathPlan` are constructed with `IdleShare: nil` under those modes,
and both use cases already treat a nil `IdleShareClient` — or
`ErrIdleShareUnavailable` from a wired one — identically: no signal, no
behavior change. This is the same fail-open discipline as every other
`*_MODE` in this fleet (see `AGENTS.md`'s `MeasuredRateClient`/
`InstalledCapacityClient` precedent).

**`GetStaffingGap` gains `ObservedIdlePct *float64`** — pure surfacing, no
change to the existing planned-vs-active gap computation or the
`PathUnderstaffed` event. `nil` whenever `IdleShare` is unwired or
reports `ErrIdleShareUnavailable`; never a fabricated `0`.

**`ProposePathPlan` trims its proposed heads** when the observed idle
share for the path's task type EXCEEDS (strictly greater than, not
greater-or-equal) `IdleShareTrimThreshold`
(`IDLE_SHARE_TRIM_THRESHOLD` env, default `DefaultIdleShareTrimThreshold
= 0.30`):

```
trimmed = ceil(heads * (1 - idleShare)), floored at 1 head minimum
```

The trim is applied AFTER the existing rate resolution (caller-supplied
or measured), so it composes with ADR-0012's existing fallback rather
than replacing it. `heads <= 0` (nothing to propose) never consults
`IdleShare` at all — there is nothing to trim. Every non-trim outcome
(no `IdleShare` wired, `ErrIdleShareUnavailable`, share at-or-below
threshold) returns an empty `trimReason` and unchanged `heads` — the
existing, pre-idle-share behavior is always the safe default. When a trim
IS applied, `Execute` returns a new `trimReason string` naming the
observed share, the threshold, and the before/after head counts, so the
decision is auditable without a separate log line. This mirrors the
existing `rateSource` transparency pattern (ADR-0012): a human reading
the response is never left guessing why a number changed.

Every new conditional in this change is `if`/`else if`/early-`return` —
never an expressionless `switch { case boolExpr: }` — per this fleet's
`gremlins` mutation-testing pitfall (`.gremlins.yaml`, the fast subset
blocking CI misses exactly that shape).

## Consequences

**Positive**

- A shift manager reading `GetStaffingGap` sees the idle-share signal
  alongside the existing planned-vs-active gap, with zero new API calls.
- `ProposePathPlan` proposals stop over-recommending heads for a path
  whose associates are already substantially idle — closing the exact gap
  the fleet's idleness plan identified (a staffing signal blind to
  observed idle time).
- Zero new infrastructure: the SAME `laborperformancecache.Consumer`
  instance, the SAME topic, the SAME readiness gate. No new Kafka
  consumer group, no new env wiring in `warehouse-infra` beyond the
  already-live `kafka-cache` mode (`IDLE_SHARE_TRIM_THRESHOLD` has a
  sensible Go code default; an operator overrides it only to change the
  trim sensitivity, e.g. `1.0` to effectively disable trimming).
- `http` and `permissive` `LABOR_PERFORMANCE_MODE`s, and every existing
  `ProposePathPlan`/`GetStaffingGap` caller that doesn't wire `IdleShare`,
  are completely unaffected — this is a strictly additive, opt-in-by-mode
  change.

**Negative / accepted**

- **Eventual consistency, same as the existing measured-rate signal**
  (ADR-0019): the idle share reflects every `TaskPerformanceRecorded`
  message this process has replayed so far, not necessarily the very
  latest completed task.
- **Per-task-type granularity only**, matching the existing measured-rate
  signal and this context's own path-boundary rule (ADR-0002): no
  per-associate idle data is ever surfaced or consulted here, by design.
- **The trim is a heuristic, not a guarantee.** `ceil(heads * (1 -
  idleShare))` is a simple linear discount; it does not model queueing,
  variance, or shift-boundary effects. It is explicitly a PROPOSAL input
  (a human still commits via `CommitShiftPlan`), not a hard cap — the
  existing "software proposes, a human commits" boundary this context has
  always enforced is unchanged.

## Alternatives considered

- **A new dedicated port/adapter for idle share, separate from
  `laborperformancecache`.** Rejected: the idle-share signal travels on
  the EXACT SAME topic and message this Consumer already replays for the
  measured-rate feed — a second consumer group replaying the same topic a
  second time would double the Kafka read cost and the readiness-gate
  complexity for zero isolation benefit. Extending the existing Consumer
  with a second running-total map is the same pattern ADR-0019 itself
  used when it chose sum+count over alternatives: minimal new moving
  parts for a signal that arrives on an already-consumed stream.
- **Trimming based on `share >= threshold` (inclusive) instead of
  `share > threshold` (exclusive).** Rejected: an idle share exactly
  equal to the configured threshold is, by construction, the boundary an
  operator chose as "acceptable, don't trim yet" — trimming AT the
  threshold would make the threshold itself the trigger, one head short
  of what the operator configured. Verified with an explicit boundary
  unit test (`TestProposePathPlan_IdleShareTrim`, the "exactly at
  threshold" case).
- **Coercing `nil idle_seconds_before` to `0`.** Rejected outright, for
  the identical reason labor-performance's own event documents: "not
  observed" and "observed zero idle" are different facts, and conflating
  them would make a TaskType look artificially staffed-tight the moment
  the first message with a nil idle observation arrives.

## Verification

- Unit (`internal/adapters/outbound/laborperformancecache/consumer_test.go`):
  nil `idle_seconds_before` contributes to the denominator only and
  leaves the TaskType `ErrIdleShareUnavailable`; a running idle share
  computed correctly across multiple messages including a mixed nil/
  non-nil sequence; the existing "no TaskType counterpart" / "no data
  observed yet" cases mirrored for `IdleSharePct`.
- Integration (`-tags=integration`, testcontainers Kafka, mirroring
  ADR-0019's own integration test): a real envelope with
  `idle_seconds_before` present alongside one that omits the field
  entirely (decodes to `nil`, same as an explicit JSON `null`), asserting
  the computed share reflects only the observed contribution.
- Application-layer unit tests
  (`internal/application/usecases/usecases_test.go`): the named trim
  boundary cases (exactly at threshold, just above, just below, nil/
  no-data fail-open, floor-at-1-head clamping), a custom
  `IdleShareTrimThreshold` override, a non-sentinel `IdleShareClient`
  error propagating as a hard failure (mirroring `MeasuredRateClient`'s
  own contract), and `GetStaffingGap`'s `ObservedIdlePct` surfacing
  (present when wired, nil when unwired or unavailable).
- HTTP-layer wire tests (`internal/adapters/inbound/http/router_test.go`):
  a high idle share visibly trims `proposedHeads` and populates
  `trimReason` on the JSON response; `observedIdlePct` is entirely
  omitted from the JSON body (not a JSON `null`) when unwired, and present
  with the wired value otherwise.
- BDD (`features/staffing_gap.feature`, "High idle share trims the
  proposal"): drives the real HTTP surface with a fixed
  `IdleShareClient` test double, asserting the trimmed head count and a
  non-empty `trimReason`.
- `make check-all` run locally: fmt-check, vet, build, lint, `test -race`,
  the 90% domain+application coverage gate, `arch-test`, and `bdd` all
  green — see the PR description for the verbatim output.
