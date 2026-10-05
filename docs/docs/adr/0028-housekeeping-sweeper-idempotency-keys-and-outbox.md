---
id: 0028-housekeeping-sweeper-idempotency-keys-and-outbox
slug: /adr/0028-housekeeping-sweeper-idempotency-keys-and-outbox
title: 28. Housekeeping sweeper for idempotency keys and published outbox rows
sidebar_label: 28. Housekeeping sweeper
sidebar_position: 28
description: "ADR 0028 — a small background sweeper in cmd/workforce that deletes idempotency_keys older than a TTL (default 24h) and PUBLISHED outbox_events older than a retention (default 7d), closing the unbounded-growth gap left open by ADR 0016 and ADR 0027. Mirrors inventory-storage ADR 0026."
---

# 28. Housekeeping sweeper for idempotency keys and published outbox rows

## Status

Accepted. Closes the "grows unboundedly" follow-up recorded for
`outbox_events` ([ADR 0016](./0016-transactional-outbox.md)) and
`idempotency_keys` ([ADR 0027](./0027-idempotency-key-middleware.md)).
Mirrors inventory-storage's ADR 0026 (the merged reference).

## Context

Two tables only ever grow:

- `idempotency_keys` gets one row per `Idempotency-Key` seen on
  `POST /shift-plans` / `POST /associates/{id}/assignments`, including the
  stored response body.
- `outbox_events` keeps every row after the relay marks it `published_at`.
  Published rows are only useful for short-term forensics.

## Decision

`internal/adapters/outbound/postgres.Sweeper` — **one** small type with one
loop (both jobs are "delete old rows in batches" and share an interval) —
started by `cmd/workforce` for every process (idempotency keys are written
regardless of `EVENT_PUBLISHER`):

| Env var | Default | Meaning |
|---|---|---|
| `HOUSEKEEPING_INTERVAL` | `1h` | Sweep period. `0` disables the sweeper. |
| `IDEMPOTENCY_KEY_TTL` | `24h` | Delete `idempotency_keys` rows with `created_at` older than this. `0` keeps them forever. |
| `OUTBOX_RETENTION` | `168h` (7d) | Delete **published** `outbox_events` rows with `published_at` older than this. `0` keeps them forever. |

(Chart values: `config.housekeepingInterval`, `config.idempotencyKeyTtl`,
`config.outboxRetention`.) An unparsable or negative value logs a warning and
falls back to the default; `0` is honoured as "disabled".

Guarantees:

1. **Unpublished outbox rows are never deleted**, however old — an event
   still waiting for the relay (broker outage) is not garbage.
2. Deletes are **batched** (1000 rows per statement, repeated until a batch
   comes back short), each targeting an explicit key/id set chosen by a
   subquery, so it is safe to run in several replicas at once.
3. One pass runs **immediately on start**, then every interval; it never
   returns an error (a failed pass is logged and retried), and it is stopped
   in the shutdown sequence (ADR [0022](./0022-resilience-circuit-breakers-retry-dlq-shutdown.md)
   §8) **before** the pool closes.
4. The relay stays in `cmd/workforce`; `cmd/mcp` does not sweep.

Migration `000004` adds a partial index on `outbox_events (published_at)
WHERE published_at IS NOT NULL` (the existing partial index only covers the
unpublished tail) next to the `idempotency_keys (created_at)` index.

## Consequences

- The 24h TTL is now the **replay window** of the idempotency contract: a
  retry whose key is older than the TTL is treated as a brand-new request.
- Published outbox rows older than 7 days are gone; forensics beyond that
  come from Kafka topic retention.
- Proven against a real Postgres (testcontainers):
  `TestSweeper_DeletesOnlyExpiredIdempotencyKeysAndPublishedOutboxRows`,
  `TestSweeper_ZeroTTLAndRetentionKeepEverything`,
  `TestSweeper_RunSweepsOnStartAndStopsOnCancel`
  (`internal/adapters/outbound/postgres/sweeper_integration_test.go`) and the
  env wiring end to end by `TestStartSweeper_SweepsExpiredIdempotencyKeysAndStopsCleanly`
  (`cmd/workforce/housekeeping_integration_test.go`).

## Alternatives considered

- **A Kubernetes CronJob / pg_cron** — another deployable or extension to
  operate, with retention knobs living outside the service's own config.
- **Partitioned tables with drop-partition** — over-engineered for the
  current row volume.
- **Two independent jobs** — one loop with two statements is simpler and
  shares the interval, shutdown and logging.
