---
id: 0033-docs-audit-corrections-2026-10
slug: /adr/0033-docs-audit-corrections-2026-10
title: 0033. Corrections from the 2026-10-05 docs audit (idempotent-request atomicity, measured-rate unit, idle-share naming, /readyz)
sidebar_label: 0033. Docs-audit corrections
sidebar_position: 34
description: "ADR 0033 — the idempotency middleware rolls back a failed request's domain writes (savepoint) while still storing its response; the measured-rate fallback converts seconds-per-task into a per-head hourly rate; the REST staffing-gap response gains the correctly named observedIdleShare (observedIdlePct deprecated, same value); GET /readyz is declared in the OpenAPI spec."
---

# 0033. Corrections from the 2026-10-05 docs audit

## Status

Accepted. Amends the behaviour described in
[ADR 0012](./0012-measured-rate-feed-for-propose-path-plan.md),
[ADR 0020](./0020-idle-share-staffing-signal.md) and
[ADR 0027](./0027-idempotency-key-middleware.md) without editing them; where
this record and one of those disagree, this record wins.

## Context

The 2026-10-05 documentation refresh traced each use case and spec against
the code and found four defects that touch contracts or atomicity:

1. **Partial write behind a 409.** `RequireIdempotencyKey` binds its
   transaction into the request context and, after the wrapped handler
   returns, commits that transaction for every non-panic response so the
   response can be stored against the key. The use cases join that
   transaction (`UnitOfWork.Execute` is a no-op join when one is already
   bound), so nothing else rolls their writes back. `AssignLabor` on a
   reassignment saves the associate (closed-interval hours) and *then* the
   assignment; if the second `Save` loses an optimistic-concurrency race
   (ADR 0021, `409 concurrent-modification`), the associate's hours write
   from the same request was committed anyway.
2. **Unit mix-up in the measured-rate fallback.** `ProposePathPlan` fed
   labor-performance's `meanActualSeconds` (a *duration per task*) straight
   into `ceil(charge / rate)`, where `rate` is *units per head per hour*
   (the caller's `plannedRate`). A 36 s task proposed 100 heads for 3600
   units instead of 36.
3. **Misleading name on a published field.** `observedIdlePct` on the
   staffing-gap responses carries a fraction in `[0, 1]`, not a 0–100
   percentage.
4. **`GET /readyz` served but undeclared** in `apis/openapi.yaml`.

## Decision

1. **Run the wrapped handler inside a savepoint.** The middleware keeps its
   outer transaction and the idempotency row inserted at the start, then
   begins a savepoint and binds *that* into the request context. A `2xx`
   response releases the savepoint (domain writes and outbox rows commit
   together with the idempotency row, as before). Any other status rolls back
   to the savepoint — the request's domain writes and outbox rows vanish —
   and the idempotency row is then updated with the failed response and
   committed, so replay of the same key and body returns the same error
   (ADR 0027's stored-response semantics are unchanged). A panic still rolls
   back the whole outer transaction.
2. **Convert the measured duration to a per-head hourly rate.**
   `resolvedRate = 3600 / meanActualSeconds`, assuming one task is one unit
   of `charge`. A non-positive or non-finite `meanActualSeconds` cannot be
   converted and is treated exactly like `ErrMeasuredRateUnavailable`
   (fail-open: rate 0, 0 heads, `rateSource` `caller`). `resolvedRate`
   keeps its field name and type; its value for `rateSource=measured` is now
   in the same unit as a caller-supplied `plannedRate`. The
   `ShiftPlanProposed` event's `plannedRate` carries that same resolved
   value, so its documented unit ("units per head per hour") is now true for
   measured proposals too.
3. **Add `observedIdleShare`, deprecate `observedIdlePct` (additive).** Both
   fields carry the same fraction, are present together and omitted together
   when no signal exists. `observedIdlePct` stays on the wire with an
   unchanged value so no consumer breaks and is marked `deprecated: true` in
   the OpenAPI schema; removal needs a future ADR. The Go port method
   `ports.IdleShareClient.IdleSharePct` and the use-case field
   `StaffingGap.ObservedIdlePct` keep their names (internal, documented as a
   fraction) to keep this change surgical.
4. **Declare `GET /readyz`** (`200 {"status":"ready"}` /
   `503 {"status":"not_ready"}`) in the OpenAPI spec, and add a router↔spec
   drift test so a served route can no longer be missing from the spec.

## Consequences

- A failed idempotent request leaves no partial state. The stored `409` is
  still replayed for the same key, so a client that wants a fresh attempt
  must still use a new `Idempotency-Key` (unchanged).
- Savepoints add one round trip each way per protected request; the two
  creation POSTs are low-volume.
- Measured-rate proposals change relative to the pre-fix output (which
  divided the charge by raw seconds per task): a 36 s task now proposes
  `ceil(charge / 100)` heads instead of `ceil(charge / 36)`. Callers who
  relied on the old (wrong) numbers see a one-time change. Caller-supplied
  `plannedRate` is unaffected. The
  "one task = one unit of charge" assumption is recorded here; if charge is
  ever measured in a different unit than labor-performance's tasks, the
  conversion needs a unit factor (a product decision, not made here).
- `observedIdlePct` is a deprecated alias until a future ADR removes it.
- Not changed, deliberately: the staffing gap still counts active
  assignments across every building (the assignment model carries no
  building or shift), and the unused `domain_event` table remains (additive
  migrations only). Both are reported for a product decision.
