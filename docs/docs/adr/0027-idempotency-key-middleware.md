---
id: 0027-idempotency-key-middleware
slug: /adr/0027-idempotency-key-middleware
title: 0027. Transactional Idempotency-Key middleware
sidebar_label: 27. Idempotency-Key middleware
sidebar_position: 28
description: "ADR 0027 — POST /shift-plans and POST /associates/{id}/assignments require an Idempotency-Key header, enforced by a route-scoped, transactional HTTP middleware that shares the request's Postgres transaction with the wrapped use case (and its outbox insert), so a client retry after a dropped response can never double-apply the write or double-publish its events. Ported from order-management ADR 0023 and inventory-storage ADR 0018."
---

# 27. Transactional Idempotency-Key middleware

## Status

Accepted. Ported from order-management's
[ADR 0023](https://github.com/IQVO/order-management) (PR #105) and
inventory-storage's ADR 0018; this was the only service in the fleet without
it. ADR [0021](./0021-optimistic-concurrency-version-column.md) already
referred to such a middleware as a future companion.

## Context

Two endpoints change real state **and** publish events on every call, so a
client that retries after a lost response (timeout, proxy reset, crash
mid-read) would apply the effect twice:

- `POST /shift-plans` (`CommitShiftPlan`). The repository upserts on
  `(buildingId, shiftId)`, so the *row* is naturally idempotent — but every
  call still re-publishes `ShiftPlanCommitted` (one Kafka message per
  `PathPlan` line) through the outbox, which downstream consumers
  (wes-work-planning) would see twice.
- `POST /associates/{id}/assignments` (`AssignLabor`). Each call closes the
  associate's active interval (logging its hours against the shift's
  max-hours budget) and opens a new one, raising `LaborAssigned` /
  `LaborReassigned`. A blind retry therefore double-logs hours and emits a
  spurious `LaborReassigned`.

## Decision

Add a route-scoped (chi's `r.With(...)`, never global) transactional
`Idempotency-Key` middleware, `RequireIdempotencyKey`
(`internal/adapters/inbound/http/idempotency.go`), applied to exactly those
two routes and **only when `Handler.IdempotencyPool != nil`** (i.e. Postgres
is wired; the in-memory dev/test configuration leaves them unprotected,
matching every other optional Postgres-backed capability).

Contract (identical to order-management and inventory-storage):

1. Missing `Idempotency-Key` → **400** `idempotency-key-required`.
2. Read the body, `requestHash = hex(sha256(body))`, restore `r.Body`.
3. Begin a pgx transaction; `INSERT INTO idempotency_keys ... ON CONFLICT
   (key) DO NOTHING`.
   - **1 row (new key):** call the wrapped handler with the transaction
     bound into the context via `internal/pgtx`, capture its response in a
     recorder, then — in the **same** transaction — `UPDATE` the row with the
     outcome and commit; only then copy the recorder to the real writer.
     Every normal response is cached, including a business-logic 4xx.
   - **0 rows (key seen):** Postgres' unique-index lock guarantees the
     original transaction has already resolved, so a plain read follows:
     hash mismatch → **409** `idempotency-key-reused` (was 422 before [ADR 0040](./0040-adopt-the-fleet-rest-api-guideline.md)); hash match → replay the
     stored status, headers (including `Location`) and body verbatim, without
     calling the handler.
   - A **panic** rolls the transaction back (no idempotency row, no domain
     write, no outbox row survives) and re-panics for chi's `Recoverer`; a
     panic is never cached, so a retry re-attempts the real work.

A committed `idempotency_keys` row therefore never has a NULL `status_code`:
no "in progress" state, polling, timeout or 409-retry-later is needed.

### Why the two transactions are one: `internal/pgtx`

The middleware lives in the inbound HTTP adapter and must bind the
transaction into the *same* context slot `UnitOfWork.Execute` reads, but the
fitness tests forbid inbound↔outbound adapter imports. The
`txKey`/`WithTx`/`TxFrom` trio moved from the postgres adapter into the tiny
dependency-free `internal/pgtx` package; `unit_of_work.go`'s `withTx`/`txFrom`
are thin aliases. `UnitOfWork.Execute`'s existing "already in a transaction?
join it" branch then makes the aggregate `Save`, the outbox inserts (ADR
[0016](./0016-transactional-outbox.md)) and the idempotency row commit or roll
back as one unit. **No use case changed**: both already wrap their writes in
`atomically(...)` (ADR 0016). This is proven by the integration tests, which
assert an unchanged `outbox_events` count after a replay.

### Which routes, and why not more

| Route | Protected? | Reason |
| --- | --- | --- |
| every `POST` route | **yes** (ADR 0040) | the v1 scope below (only the two creation routes) was superseded: the fleet guideline keys EVERY POST |

Revisit if a real double-apply incident surfaces on one of the unprotected
routes.

### Schema (migration `000004`)

```sql
CREATE TABLE idempotency_keys (
    key              TEXT PRIMARY KEY,
    method           TEXT NOT NULL,
    path             TEXT NOT NULL,
    request_hash     TEXT NOT NULL,
    status_code      INTEGER,
    response_body    BYTEA,
    response_headers JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ
);
CREATE INDEX idx_idempotency_keys_created_at ON idempotency_keys (created_at);
```

Growth is bounded by the housekeeping sweeper
([ADR 0028](./0028-housekeeping-sweeper-idempotency-keys-and-outbox.md)): a
key older than `IDEMPOTENCY_KEY_TTL` (default 24h) is forgotten and a retry
after that is a brand-new request.

## Consequences

- Callers of the two routes must send a non-empty `Idempotency-Key` (a
  breaking tightening: they get a 400 without it). Every caller is inside the
  fleet.
- **OpenAPI.** The header is the reusable `IdempotencyKey` parameter in
  `apis/openapi.yaml` with `400 idempotency-key-required` and
  `409 idempotency-key-reused` documented. It is declared
  `required: false` with **no** `minLength` only because the in-memory mode
  the Schemathesis contract job boots does not enforce it; with Postgres —
  every deployment — a non-empty key is mandatory, and the description says
  so.
- **Browsers.** `Idempotency-Key` is in the router's CORS `AllowedHeaders`,
  otherwise the preflight for these POSTs is rejected. The operator console
  (`web/`, ADR [0011](./0011-adopt-fleet-mfe-console-architecture.md)) is
  read-only today; a future write must send `crypto.randomUUID()` per logical
  submit (reused for retries of that submit).
- The middleware needs a real `*pgxpool.Pool`; the MCP server calls the use
  cases directly and is not an HTTP retry surface, so it is unaffected.
- Replay is by key only (the PK), as in the reference services: reusing a key
  across *different routes* with an identical body replays the first route's
  response. Clients generate one fresh key per logical request, which makes
  this moot.

## Alternatives considered

- **An "in progress" state with a client-facing retry-after** — rejected:
  Postgres' unique-index lock already serialises concurrent identical
  requests.
- **Relying on the natural idempotency of the shift-plan upsert** — rejected:
  it covers the row, not the re-published events.
- **A per-route table** — rejected: one table keyed by `key` with
  `method`/`path` columns is simpler and matches the reference design.

## Verification

Real-Postgres integration tests (testcontainers) through the real chi router:
fresh key, replay without a second publish or outbox row, same key with a
different request → 409, missing header → 400, five goroutines racing one key →
exactly one effect, a cached business error, the transaction being bound into
the request context, and a panic rolling back with nothing cached — for both
routes (`internal/adapters/inbound/http/idempotency_integration_test.go`).

## Addendum (2026-11-18) — a 5xx is never stored

The Decision above says "every normal response is cached". The fleet rule is
narrower: a 5xx answer is **never** stored under the key. The middleware used
to store it (a `503 installed-capacity-unavailable` was replayed for the whole
TTL, 24 h, even after fulfillment-execution recovered). It now rolls the whole
transaction back on any status `>= 500` — the key row and every write the
handler made go with it — and still writes the 5xx itself to the client. A
retry under the same key runs the handler again; a different body under that
key is a new request, not a 409. A concurrent request with the same key is
still serialised by the primary-key unique-index lock described above: it
waits for the first transaction, and when that one fails with a 5xx it
proceeds as a fresh request (it never replays the failure and never runs
beside the first). 2xx and 4xx behaviour is unchanged, and a panic was and
remains rolled back and never cached. Tests:
`internal/adapters/inbound/http/idempotency_5xx_test.go` (unit, fake
transaction) and `idempotency_5xx_integration_test.go` (real Postgres).

## Addendum (2026-11-21) — replay classes: a RETRYABLE refusal is not stored

The Decision above (and the 2026-11-18 addendum's "2xx and 4xx behaviour is
unchanged") stored every 4xx. The fleet replay-class rule
([ADR 0044](./0044-idempotency-replay-classes.md)) narrows that: every
problem type now declares FINAL, OUTCOME or RETRYABLE on its catalogue
entry; FINAL answers are stored and replayed as before, a RETRYABLE refusal
(concurrent-modification, the associate-state 409s, the not-yet-known 404s,
the live-capacity 409) is released by the same whole-transaction rollback
the 5xx branch uses — the key row is gone, a retry under the same key
executes again, and a different body under the released key is a new
request. No OUTCOME entry exists in this store: every non-2xx answer's
writes are rolled back to the request's savepoint, so a refusal can never
commit state. The class lives on the catalogue entry
(`internal/adapters/inbound/http/errors.go`); the middleware reads a mark
the shared error writers set and holds no slug list.
