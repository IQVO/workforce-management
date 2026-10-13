---
id: 0036-outbox-relay-attempt-cap
slug: /adr/0036-outbox-relay-attempt-cap
title: 0036. The outbox relay stops claiming a row after OUTBOX_MAX_ATTEMPTS failures
sidebar_label: 0036. Outbox relay attempt cap
sidebar_position: 37
description: "ADR 0036 — postgres.OutboxRelay gets WithMaxAttempts: a row that failed OUTBOX_MAX_ATTEMPTS times (default 20, 0 = unlimited) is excluded from future claims, kept in outbox_events with attempts and last_error, and logged once at ERROR. Later rows with the same key overtake it. Fleet gap-fix decision D12."
---

# 0036. The outbox relay stops claiming a row after `OUTBOX_MAX_ATTEMPTS` failures

## Status

Accepted. Implemented by the change that introduces this record (fleet
gap-fix wave 2026-10-12, decision D12).

Amends the relay behaviour of
[ADR 0016](./0016-transactional-outbox.md), which retried a failing row
without bound. The semantics are the fleet convention that
inventory-storage's relay introduced first (`WithMaxAttempts`).

## Context

`postgres.OutboxRelay` (`internal/adapters/outbound/postgres/outbox_relay.go`)
claims unpublished `outbox_events` rows oldest first and stops a pass at the
first send failure, so a later row never overtakes an earlier failed one. A
row the broker will never accept (a message over the broker's size limit, a
topic that cannot be created) therefore failed on every pass, forever, and
every event written after it waited behind it, on BOTH topics
(`warehouse.workforce.events` and `warehouse.workforce.analytics` share the
table and the relay). `workforce_outbox_lag_seconds` grew without bound and
nothing published until an operator deleted or fixed the row by hand.

## Decision

1. **Attempt cap.** `postgres.WithMaxAttempts(n)` makes the claim query skip
   a row whose `attempts` already reached `n`
   (`AND ($2::int <= 0 OR attempts < $2::int)`). `n = 0` keeps the old
   unlimited behaviour.
2. **Configuration.** `cmd/workforce` reads `OUTBOX_MAX_ATTEMPTS` (default
   `20`, `0` = unlimited; a negative or non-numeric value logs a warning and
   uses `20`) through `envNonNegativeIntOrDefault`, carries it in
   `composition.PublisherConfig.MaxAttempts`, and
   `composition.BuildEventPublisher` passes it to the relay. The chart
   exposes it as `config.outboxMaxAttempts` (ConfigMap, mounted with
   `envFrom`). `cmd/mcp` builds no relay, so the setting does not apply there.
3. **Nothing is deleted.** A capped row stays in `outbox_events` with
   `published_at IS NULL`, its `attempts` and its `last_error`, for
   inspection. The housekeeping sweeper
   ([ADR 0028](./0028-housekeeping-sweeper-idempotency-keys-and-outbox.md))
   only deletes published rows, so it never removes it.
4. **One ERROR line.** The failure that brings a row to the cap is logged at
   ERROR after the pass commits: `outbox relay: row reached the attempt cap
   and is no longer claimed`, with `outbox_id`, `topic`, `event_type`, `key`,
   `attempts`, `max_attempts`, the send `error` and the re-arm statement.
5. **Re-arm.** An operator re-arms a row after fixing the cause with
   `UPDATE outbox_events SET attempts = 0 WHERE id = ...`; the next pass
   claims it again.
6. **Sensor.** `TestOutboxRelaysPassAttemptCap`
   (`internal/architecture/outbox_relay_cap_fitness_test.go`) fails when a
   non-test file under `cmd/` or `internal/composition/` (where this repo
   builds the relay for its composition roots) calls `NewOutboxRelay` without
   `WithMaxAttempts`; `TestOutboxRelayCapSensorFailsOnBadFixtures` proves it
   can fail.

## Consequences

- **Per-key ordering has an exception.** Once a row is capped, later rows
  with the same Kafka key (the same associate, path or shift plan) publish
  before it, and it may never publish at all. A consumer can therefore see,
  for example, `AssociateCertified` without the `AssociateShiftStarted` that
  preceded it, and the analytics projection under-counts until the row is
  re-armed. That is the price of the cap, and it is accepted: without it one
  poison row stops every event of this context, not just one aggregate's.
  The capped row is visible (the ERROR log, the row itself, the lag gauge)
  and can be re-armed.
- A transient broker outage longer than 20 relay passes also caps rows. The
  relay sleeps `OUTBOX_RELAY_INTERVAL` (1 s) between failing passes, so 20
  attempts is about 20 s of continuous failure for the head row. Re-arm the
  rows after the outage (`UPDATE outbox_events SET attempts = 0 WHERE
  published_at IS NULL AND attempts >= 20`), or raise `OUTBOX_MAX_ATTEMPTS`
  where outages are expected to last longer.
- `workforce_outbox_lag_seconds` still measures the oldest unpublished row,
  so a capped row keeps the gauge growing. That is intended: the gauge is the
  alert that a row needs an operator.
- Tests: `TestOutboxRelay_MaxAttempts_PoisonRowIsExcludedAndTheNextRowPublishes`
  and `TestOutboxRelay_WithoutMaxAttempts_RetriesWithoutBound`
  (testcontainers Postgres, `outbox_integration_test.go`),
  `TestOutboxMaxAttemptsFromEnv` and
  `TestPublisherConfigFromEnv_CarriesTheOutboxCap` (`cmd/workforce`), the
  fitness test above and the chart render test
  (`charts/workforce-management/tests/render_test.sh`).

## Alternatives considered

- **Dead-letter the row to a Kafka DLQ topic.** Rejected: the row failed
  because Kafka would not take it, so a DLQ on the same broker usually fails
  too, and the fleet convention keeps the row in Postgres.
- **Skip the failed row within the same pass.** Rejected: it would break
  per-key ordering on every transient failure, not only for a poison row.
- **Keep unlimited retries and alert.** Rejected: one row still stops all
  publishing until a human acts.
