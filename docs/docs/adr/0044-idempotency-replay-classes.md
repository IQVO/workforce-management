---
id: 0044-idempotency-replay-classes
slug: /adr/0044-idempotency-replay-classes
title: 0044. Every problem type has an idempotency replay class; a RETRYABLE refusal is not stored
sidebar_label: 44. Idempotency replay classes
sidebar_position: 45
description: "ADR 0044: adopting the fleet replay-class rule (owner decision 2026-11-18) — each problem type on an idempotent route declares FINAL, OUTCOME or RETRYABLE on its catalogue entry; the middleware reads the class, a RETRYABLE refusal is released exactly like a 5xx, and in this savepoint store OUTCOME cannot exist because every non-2xx answer's writes are rolled back."
---

# 44. Every problem type has an idempotency replay class; a RETRYABLE refusal is not stored

## Status

Accepted (2026-11-21). Amends [ADR 0027](./0027-idempotency-key-middleware.md)
(the stored-answer contract; its 2026-11-18 addendum already made a 5xx
never-stored) and extends it with the fleet replay-class rule
(`.claude/rules/fleet/idempotency-and-outbox.md`, owner decision 2026-11-18;
the standard is `docs/idempotency-replay-classes.md` in
warehouse-harness-template).

## Context

ADR 0027 stored every non-5xx answer under the key. That is wrong for
refusals whose cause can change within the key's TTL (24 h by default): a
`409 concurrent-modification` or a `409 certification-required` replayed to a
caller that did the right thing and retried pins a refusal the retry could
have passed. Classifying by status cannot fix it (`409` covers both
`concurrent-modification` and `planned-heads-exceed-installed`), and "store
only 2xx" would lose OUTCOME answers where they exist. The fleet rule gives
each problem type ONE class, declared on its catalogue entry; the middleware
reads the class and holds no slug list and no status ranges beyond 2xx/5xx.

## The store this repo has (verified from code)

`RequireIdempotencyKey` (`internal/adapters/inbound/http/idempotency.go`)
begins the request transaction, inserts the key row with
`ON CONFLICT (key) DO NOTHING`, and runs the handler behind a pgx
**savepoint** bound into the request context (`pgtx.WithTx`, idempotency.go
savepoint begin): a **2xx releases the savepoint** (the command's writes and
the outbox rows commit together with the key row and the stored answer),
**any other answer rolls back to it** (the command's writes vanish) while the
key row — inserted before the savepoint — survives and stores the answer,
and a **5xx or panic rolls the whole transaction back** (key row and writes;
ADR 0027's addendum). Because EVERY refusal's writes are discarded by that
rollback, **no refusal of this service can commit state, so an OUTCOME entry
cannot exist here**; the middleware implements the class anyway (a marked
OUTCOME would release the savepoint and store the answer in the same commit),
so the first OUTCOME catalogue entry is a catalogue change, not a middleware
change.

## Decision

1. Every problem category carries `class` on its catalogue entry
   (`problemCategory{slug, title, class}` in
   `internal/adapters/inbound/http/errors.go`). Every `problemCategory`
   literal of the package lives in that file; an AST test
   (`TestProblemCategoryLiteralsLiveOnlyInErrorsGo`) enforces it, and
   `TestEveryProblemCategoryDeclaresAReplayClass` fails on any unclassified
   entry (no runtime default).
2. The shared error writers (`writeError`, `writeProblem`) mark the
   middleware's capture recorder with the resolved category's class. The
   middleware reads only the mark:
   - **2xx** — stored, writes committed (unchanged).
   - **FINAL** — stored and replayed; the refusal's writes still vanish
     (unchanged bytes on the wire).
   - **OUTCOME** — would commit the writes with the stored answer; no entry
     uses it (see above), the mechanism is unit-proven.
   - **RETRYABLE** — released by exactly the branch that releases a 5xx:
     the whole transaction rolls back (key row and writes), the client still
     receives the answer verbatim, and a retry under the same key executes
     again. A **different body under a released key is a NEW request**, not
     `idempotency-key-reused`.
   - **unmarked refusal** (a handler bypassing the shared writers) —
     released like RETRYABLE: the safe fallback, never a silently-stored
     refusal.
3. A concurrent duplicate whose INSERT blocked on the key row proceeds as a
   fresh request when the first ends RETRYABLE (the key row no longer
   exists) — the same serialization argument as the 5xx addendum.
4. MCP `assign_labor` and the A2A `assign_labor` skill call the use case
   directly with no key store (ADR 0039: "NOT idempotent … a caller must not
   blindly retry"), so no second store needs the class logic.

## Class table (held equal to the catalogue by TestADRClassTableMatchesTheCatalogue)

| Problem type | Status | Class | Why |
| --- | --- | --- | --- |
| `resource-not-found` | 404 | RETRYABLE | `ports.ErrNotFound` is FindByID of a caller-chosen id (associate_repo.go:79); the shift-start POST another request sends later creates the row |
| `labour-plan-not-found` | 404 | RETRYABLE | the labour plan is a Kafka-fed fact (labour_plan_consumer.go; ADR 0043); the event can deliver it later |
| `empty-associate-id` | 400 | FINAL | request-only field validation |
| `empty-path-id` | 400 | FINAL | request-only field validation |
| `unknown-path-id` | 400 | RETRYABLE | resolved against the Kafka-sourced process-path catalogue (ADR 0030); a path declared later becomes known |
| `empty-certification` | 400 | FINAL | request-only field validation |
| `shift-plan-no-path-plans` | 400 | FINAL | request-only (no lines; shift_plan.go ErrNoPathPlans) |
| `shift-plan-missing-installed-stations` | 400 | FINAL | installedStations is caller-supplied in the request body (commit_shift_plan.go doc); a missing entry is a property of the request |
| `missing-building-id` | 400 | FINAL | request-only field validation (ADR 0035) |
| `conflicting-site-and-building` | 422 | FINAL | request-only: two aliases of one value disagree (ADR 0035) |
| `missing-shift-id` | 400 | FINAL | request-only field validation |
| `missing-charge` | 400 | FINAL | request-only field validation |
| `associate-already-on-break` | 409 | RETRYABLE | break state moves (POST break-ends) |
| `associate-not-on-break` | 409 | RETRYABLE | break state moves (POST break-starts) |
| `associate-on-break` | 409 | RETRYABLE | the break this refusal reads ends later |
| `associate-shift-ended` | 409 | RETRYABLE | a new shift-start (POST shift-starts) makes the same request pass |
| `max-hours-exceeded` | 409 | RETRYABLE | hours accrue with the clock and reset on the next shift-start |
| `certification-required` | 409 | RETRYABLE | `POST /associates/{id}/certifications` adds the missing certification |
| `planned-heads-exceed-installed` | 409 | FINAL | both operands are in the request (plannedHeads vs the request's own installedStations; shift_plan.go CommitShiftPlan) — the same bytes always get the same answer |
| `exceeds-installed-capacity` | 409 | RETRYABLE | the live ceiling is fetched from fulfillment-execution at commit time (ADR 0014); stations are installed and removed |
| `planned-hours-exceed-capacity` | 409 | FINAL | plannedHours vs plannedHeads × maxHoursPerShift — every operand is request data times boot-time config |
| `installed-capacity-unavailable` | 503 | RETRYABLE | dependency not ready (ADR 0014); also a 5xx, never stored (ADR 0027 addendum) |
| `concurrent-modification` | 409 | RETRYABLE | optimistic-lock loss (ADR 0021): the competing writer's commit is what changes the answer |
| `idempotency-key-required` | 400 | FINAL | middleware pre-check; answered before any transaction exists, never stored |
| `idempotency-key-invalid` | 400 | FINAL | middleware pre-check (ADR 0042); never stored |
| `idempotency-key-reused` | 409 | RETRYABLE | the replay-path answer depends on whether a stored row exists — a released key makes the same request new; never stored itself |
| `malformed-request-body` | 400 | FINAL | a function of the request bytes alone |
| `internal-error` | 500 | RETRYABLE | a 5xx is never stored (ADR 0027 addendum) |

Off-keyed-route problems are classified in errors.go for the same
no-unclassified-entry rule but are not part of the idempotent-route walker:
`unknown-query-parameter`, `invalid-limit`, `invalid-cursor` (collection GET
paging, FINAL) and `dependency-unavailable` (GET /readyz, RETRYABLE).

## Judgement calls

1. **`planned-heads-exceed-installed` is FINAL, not RETRYABLE** — the
   counter-intuitive call. The phase-0 scan guessed RETRYABLE ("depends on
   another context's fact"), but the installedStations number is carried BY
   THE REQUEST (`CommitShiftPlan`'s doc: "installedStations is supplied by
   the caller (Work Planning owns that number; … the request carries it)");
   the live cross-context check is the separate
   `exceeds-installed-capacity`. A bigger number needs a different request
   body, and different bodies are different requests anyway. The one-word
   flip is a follow-up if installedStations ever stops being request-borne.
2. **`planned-hours-exceed-capacity` is FINAL** — maxHoursPerShift is
   boot-time configuration (`MAX_HOURS_PER_SHIFT`), not per-request state;
   if it ever becomes editable per site, reclassify to RETRYABLE.
3. **`unknown-path-id` is RETRYABLE although it answers 400** — it looks
   like request validation but reads the Kafka-sourced catalogue projection
   (ADR 0030); warehouse-planning declaring the path later changes the
   answer for identical bytes.
4. **`idempotency-key-reused` is RETRYABLE** (inbound-receiving #137 made
   the same call): whether the answer fires at all depends on the stored
   row existing — exactly the state a released RETRYABLE key erases.
5. **`associate-shift-ended` is RETRYABLE, not FINAL** — the shift's end is
   one-way for THAT shift, but the aggregate state the refusal reads ("no
   active shift") clears on the next shift-start, so identical bytes can
   pass later.
6. **No OUTCOME entry is declared** — in this savepoint store every
   non-2xx answer's writes are rolled back (idempotency.go), so no refusal
   provably commits state; the rule says OUTCOME then cannot exist. The
   middleware's OUTCOME branch is implemented and unit-proven so a future
   catalogue entry needs no middleware change.

## Consequences

- Callers that retried a stored refusal now execute again on RETRYABLE
  answers (`concurrent-modification`, the associate-state 409s, the 404s,
  `exceeds-installed-capacity`): only automation should retry
  (outbox-driven callers); a caller driven by a person must surface the
  refusal — the retry benefit arrives when the human re-submits.
- FINAL answers (`planned-heads-exceed-installed`,
  `planned-hours-exceed-capacity`, every request-shape 400) replay
  byte-for-byte exactly as before; 2xx and 5xx behaviour is unchanged.
- No deterministic-key outbound caller exists in this service (verified:
  no caller derives an Idempotency-Key from an id), so no consumer-side
  change follows from this PR.
- Open point (not fixed here): the replay SELECT has no TTL age check —
  between a row's 24 h expiry and the sweeper's hourly pass a replay can
  still be answered from an expired row (ADR 0028 sweeps; the read-side
  guard is a follow-up in the fix-batch shape).

## Verification

- Unit (docker-free): `errors_replay_class_test.go` (every entry classified,
  5xx ⇒ RETRYABLE, literals only in errors.go, this table equals the
  catalogue), `idempotency_5xx_test.go` (FINAL stored, RETRYABLE and
  unmarked released like a 5xx, OUTCOME mechanism commits writes with the
  answer, 2xx stored).
- Integration (CI, real Postgres + the real router): the rendered fleet
  template `idempotency_replay_classes_integration_test.go` over every
  catalogue entry, plus `TestIdempotency_Assign_ConcurrentEditConflict_…`
  (the loser's key row is absent, its retry executes and is then stored and
  replayed) and
  `TestIdempotency_Assign_CertificationRequiredIsReleasedThenRetried`
  (certification POST unblocks the same key+body).
