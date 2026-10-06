---
id: 0012-measured-rate-feed-for-propose-path-plan
title: 0012. Close-the-loop measured rate feed from labor-performance into ProposePathPlan
sidebar_label: 0012. Measured rate feed for ProposePathPlan
sidebar_position: 13
description: ProposePathPlan can now propose headcount against a real measured task duration from labor-performance instead of always requiring a caller-supplied plannedRate guess.
---

# 0012. Close-the-loop measured rate feed from labor-performance into ProposePathPlan

## Status

Accepted.

## Context

`ProposePathPlan(buildingId, pathId, charge, plannedRate)` computes proposed
headcount as `ceil(charge / plannedRate)`. Until now `plannedRate` was always
a caller-supplied number — in practice a human's hand-guess of how many units
per hour an associate can clear on a path, with no feedback from what
associates actually achieved.

`labor-performance` (the eighth bounded-context service, PR #7 on that repo,
ADR-0006 there) now exposes `GET /task-types/{taskType}/performance`,
returning `meanActualSeconds`: the real measured mean duration for a task
type, computed from every completed task's actual duration, with **zero
dependency on any engineered labor standard existing first** — so it is
available even to an operator who has never defined a standard, the exact
caller this feed is most useful to. This closes the loop competitor research
(ADR context for labor-performance's own creation) identified as missing
across this fleet: a real measured rate feeding back into planning, not just
a scored efficiency percentage.

`workforce-management` is a **Customer** of `labor-performance`'s **Open Host
Service** here — the same directional Customer/Supplier relationship this
fleet already uses for `order-management` → `inventory-storage`. This is a
new synchronous HTTP dependency crossing a bounded-context boundary, which
this fleet's convention (see `order-management`'s `inventorystorage.Client`)
requires be optional at the composition root, fail predictably, and never be
imported as a Go package across repos.

Two problems specific to this integration needed a real decision, not just a
copy of the existing pattern:

1. **Vocabulary mismatch.** `workforce-management`'s `PathId` is a lowercase,
   open string (`"pack"`, `"pick"`, `"stow"`, `"hazmat"`, ...) describing a
   *process path* — a queue this context routes labor to. `labor-performance`'s
   `TaskType` is a closed, uppercase enum (`PICK`, `PACK`, `SLAM`) mirrored
   from `fulfillment-execution`'s task type. They are NOT the same concept
   with different casing: `"stow"` and `"hazmat"` are real
   `workforce-management` paths with no `TaskType` counterpart at all — there
   is no task type to measure a rate against for them.
2. **Optionality and failure mode.** Reserving stock (order-management →
   inventory-storage) is a mutation this fleet fails LOUD for. A measured
   rate is the opposite: a soft enrichment input to a pure computation that
   already has a caller-supplied override. Failing a plan proposal because
   labor-performance is briefly unreachable would be a strictly worse outcome
   than falling back to the existing (pre-this-feature) zero-rate behavior.

## Decision

We will add a `MeasuredRateClient` outbound port in `application/ports`, with
a real HTTP adapter (`outbound/laborperformance`, mirroring
`order-management`'s `inventorystorage` client shape exactly — same
`HTTPDoer` seam, same `DefaultTimeout`, same env-selected construction) and a
`PermissiveClient` no-op default, selected by `LABOR_PERFORMANCE_MODE`
(`http`|`permissive`, default `permissive`) + `LABOR_PERFORMANCE_BASE_URL`.

`ProposePathPlan.Execute`'s `plannedRate` parameter becomes **optional**: a
caller passing `<= 0` (including simply omitting the field over JSON) means
"no rate supplied." In that case, and only then, the use case calls
`MeasuredRate.MeanActualSeconds(ctx, pathId)`. A caller-supplied `plannedRate`
always wins outright and `MeasuredRateClient` is never consulted when one is
given — this is a fallback, not an override.

The vocabulary mismatch is resolved entirely inside the HTTP adapter, not the
use case: `taskTypeForPathId` maps `PathId` to `TaskType` by uppercasing and
checking membership in the closed `{PICK, PACK, SLAM}` set. A `PathId` with
no counterpart (`"stow"`, `"hazmat"`, any future path) fails fast with
`ErrMeasuredRateUnavailable` **without making an HTTP call at all** — the use
case's application layer never needs to know this mapping exists; it only
sees "a measured rate was, or was not, available for this path."

`ErrMeasuredRateUnavailable` is a single sentinel error covering every
failure mode the use case cannot usefully act on differently: unreachable
service, non-2xx, malformed body, no `TaskType` mapping for this path, or a
genuine `200` with `meanActualSeconds: null` (no data recorded yet). All of
them mean exactly one thing to `ProposePathPlan`: fall back to the existing
zero-rate behavior (0 proposed heads), never fail the request. Any other
error from a `MeasuredRateClient` implementation is treated as a programming
error and propagates as a hard failure — the interface's contract is that
`ErrMeasuredRateUnavailable` is the *only* error a well-behaved
implementation ever returns.

The response gains `resolvedRate` (the rate actually used) and `rateSource`
(`"caller"` or `"measured"`), so a human reading the response is never left
guessing why `proposedHeads` came back `0`, or silently trusting a number
whose provenance they can't see.

## Consequences

- A shift manager can call `POST /paths/{pathId}/plan/propose` with just
  `buildingId` and `charge` for `pack`/`pick`/`slam` paths and get a
  headcount proposal grounded in what associates actually achieved, closing
  the exact competitor-research gap that motivated `labor-performance`'s
  creation.
- `workforce-management` now has a live, synchronous, cross-repo dependency
  on `labor-performance` being reachable in any environment that sets
  `LABOR_PERFORMANCE_MODE=http` — the same operational cost every other
  Customer/Supplier HTTP integration in this fleet already carries, and
  already mitigated the same way (a `PermissiveClient` default so this
  never blocks local dev, unit tests, or CI).
- Paths with no `TaskType` counterpart (`"stow"`, `"hazmat"`, and any future
  workforce-management-only path) can never benefit from this feed — a
  caller-supplied `plannedRate` remains mandatory in practice for those,
  even though the API no longer enforces it at the schema level. This is a
  known, accepted gap, not a bug: those paths have no analogous concept in
  `labor-performance` to measure.
- The MCP inbound tool (`propose_path_heads`) deliberately keeps its
  existing `plannedRate > 0` required-input validation, so an LLM caller
  through that surface never silently triggers the measured-rate fallback
  and gets a less predictable answer; only the REST API exposes the
  optional-rate behavior today.
