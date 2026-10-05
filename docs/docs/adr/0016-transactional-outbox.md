---
id: 0016-transactional-outbox
slug: /adr/0016-transactional-outbox
title: 0016. Transactional outbox feeding both Kafka topics from one table
sidebar_label: 0016. Transactional outbox
sidebar_position: 17
description: ADR 0016 — why this service stopped publishing to Kafka from inside its use cases and now commits every event's wire form (for the integration AND the analytics topic) to one outbox table in the same transaction as the aggregate, with an in-process relay draining it to both topics.
---

# 0016. Transactional outbox feeding both Kafka topics from one table

## Status

Accepted — implemented in the same change that introduced this record.
Follows process-path-management's ADR 0003 (the fleet's single-topic
reference implementation) and adapts it to this service's fan-out.

## Context

Every publishing use case in this service had the same shape:

```go
if err := uc.Associates.Save(ctx, shift); err != nil { return err }
return uc.Events.Publish(ctx, shift.PullEvents()...)
```

Two independent writes to two independent systems, no compensation. A
crash, a broker timeout or a pod eviction between them leaves a
shift/assignment/plan in Postgres whose event never reached Kafka. For
this service that has two distinct victims:

- **The integration contract** (`warehouse.workforce.events`): a
  `ShiftPlanCommitted` that never reaches the topic means wes-work-planning
  never observes the labor plan, and its `LaborPlanObserved` read model
  silently disagrees with what a human actually committed here.
- **The analytics data product** (`warehouse.workforce.analytics`,
  ADR 0010): a missing `LaborAssigned`/`AssociateBreakStarted` makes the
  Labor Utilization & Staffing report wrong with no error anywhere.

The reverse failure — Publish succeeds, then the HTTP response is lost and
the operator retries — was already tolerable (the aggregates are idempotent
upserts), but the first one had no answer at all. Worse, with the direct
publisher a broker outage turned every mutating request into a 500 *after*
the row was already committed: the worst of both worlds.

Three things distinguish this service from the PPM reference and shape
the design below:

1. `ports.EventPublisher.Publish` is **variadic** — one call carries every
   event an aggregate pulled.
2. There are **two topics**, fanned out today by `events.MultiPublisher`
   over `kafka.Publisher` (integration) and `kafka.AnalyticsPublisher`
   (analytics). Both must be fed from the outbox, or the guarantee holds
   for only half the consumers.
3. The integration publisher **reads a repository at publish time**: a
   domain `ShiftPlanCommitted` carries only `buildingId`/`shiftId`, and the
   publisher loads the plan's `PathPlan` lines to fan one message out per
   line (INTEGRATION.md). That read must see the row the use case just
   saved — so encoding has to happen *inside* the use case's transaction,
   not later in a relay.

## Decision

Adopt the **transactional outbox**, in a fan-out variant:

1. **`outbox_events` table** (migration `000002_outbox`). One row is one
   already-encoded Kafka message: `topic`, `event_type`, `key`, `value`
   (the JSON envelope, byte-for-byte what the topic will carry), `headers`
   (JSON array of `{key,value}` — the W3C trace context captured when the
   event was raised), plus `published_at`, `attempts`, `last_error` for
   the relay. A single domain event therefore becomes N rows: for
   `ShiftPlanCommitted` with three lines, three integration rows and one
   analytics row. A partial index over `published_at IS NULL` keeps the
   relay's scan tiny.

2. **`kafka` package: Encode split from Send.** `kafka.Encoded` carries
   `Topic`, `EventType`, `Key`, `Value`, `Headers`; `kafka.Encoder` is
   `Encode(ctx, events...) ([]Encoded, error)`. Both `Publisher` and
   `AnalyticsPublisher` now implement it — envelope construction, the
   `shiftPlans` lookup, trace-header injection and the "skip events with
   no analytics payload" rule all moved into `Encode`, and `Publish` is
   simply `Encode` + `WriteMessages` inside the existing producer span. The
   outbox and the direct path can never disagree on wire format because
   they share the same `Encode`. A new `kafka.RelaySink` wraps a writer
   with **no** fixed topic and sets `Message.Topic` per message: kafka-go
   rejects a message that names a topic when the writer also has one, and
   vice versa, which is why `Encoded.Topic` is carried separately and only
   applied by the sink. A `kafka.Writer` interface and `…WithWriter`
   constructors make all three unit-testable without a broker.

3. **`ports.UnitOfWork`** — a new driven port,
   `Execute(ctx, fn func(ctx) error) error`. Every publishing use case
   (all nine: StartAssociateShift, CertifyAssociate, StartBreak,
   EndBreak, CommitShiftPlan, AssignLabor, EndAssociateShift,
   ProposePathPlan, GetStaffingGap) wraps **all** of its `Save`s and its
   `Publish` in one `atomically(ctx, uc.UnitOfWork, …)` scope. Reads that
   only decide whether to act stay outside; use cases that publish
   without saving (ProposePathPlan, GetStaffingGap) still wrap the
   Publish so every publishing path is uniform. The port is optional
   (`nil` = "run them back to back"), which is exactly the in-memory
   test configuration. The domain layer is untouched and the arch-go
   fitness tests pass unchanged.

4. **`postgres.UnitOfWork`** opens a `pgx.Tx`, binds it to the context,
   and commits or rolls back around `fn`. Every repo resolves its querier
   from the context (`querierFrom`): the pool when standalone, the bound
   transaction inside a unit of work. `AssignmentRepo.Save` and
   `ShiftPlanRepo.Save`, which are multi-statement and used to open their
   own `pool.Begin`, now use `beginOrJoin`: they join the outer
   transaction when there is one (commit/rollback become no-ops owned by
   the UnitOfWork) and behave exactly as before when there is not.
   Nested `Execute` calls join the outer scope.

5. **`postgres.OutboxPublisher`** implements `ports.EventPublisher`:
   `NewOutboxPublisher(pool, encoders...)`. For each configured Encoder it
   calls `Encode(ctx, events...)` — inside the use case's transaction, so
   the integration Encoder's plan lookup sees the just-saved rows — and
   `INSERT`s one row per `Encoded`. It never touches the broker.

6. **`postgres.OutboxRelay`** runs as a goroutine inside `cmd/workforce`
   next to the HTTP server. Each pass claims up to 100 pending rows with
   `SELECT … FOR UPDATE SKIP LOCKED ORDER BY id`, sends them to the
   `RelaySink` **one at a time in id order**, and marks each
   `published_at`. On a send failure it stops the pass at that row (a
   later event for the same aggregate can never overtake a failed earlier
   one), records `attempts+1`/`last_error` on it, commits what was already
   sent, and retries on the next tick. Sleep between empty passes is
   `OUTBOX_RELAY_INTERVAL` (default `1s`); a full batch is followed
   immediately by another pass. One relay serves both topics — the topic
   is a column on the row, not a property of the relay.

7. **Composition root** (`cmd/workforce/main.go` only; the projector,
   reports and MCP binaries are untouched) picks the mode and logs it at
   startup as `event publisher configured publisher=kafka mode=outbox|direct`:

   | `DATABASE_URL` | `EVENT_PUBLISHER` | Publisher wired                                | Relay |
   |----------------|-------------------|------------------------------------------------|-------|
   | unset          | `log` (default)   | log                                            | none  |
   | unset          | `kafka`           | `MultiPublisher(integration, analytics)` direct | none  |
   | set            | `log`             | log                                            | none  |
   | set            | `kafka`           | **`OutboxPublisher(pool, integration, analytics)`** | **yes** |

   This binary requires `DATABASE_URL`, so in practice only the last two
   rows are reachable; the direct row is kept so the wiring documents the
   fleet-wide matrix and an in-memory composition could still use it.
   Every publishing use case gets the same `postgres.UnitOfWork`. Graceful
   shutdown stops the HTTP server first, then cancels the relay and waits
   for its in-flight pass (bounded by the 10s shutdown deadline), so an
   event committed by a request that completed a moment before SIGTERM is
   not stranded until the next pod boots.

### Delivery semantics (what consumers may now rely on)

- **Atomicity**: an aggregate change and its events — for BOTH topics —
  commit together or not at all. Verified by an integration test that
  makes the outbox insert fail after `ShiftPlanRepo.Save` has already run
  and asserts the `shift_plan` row is absent (and likewise for the
  single-statement `AssociateRepo` path with a `NOT NULL` violation).
- **At-least-once**: a crash between a successful `Send` and the row's
  `UPDATE` republishes that row on the next pass. Both consumers already
  tolerate this: the analytics projector is idempotent on `event_id`
  (ADR 0010, `consumed_events`), and the integration consumer keys its
  read model by `path_id`, so a duplicate is an idempotent overwrite. A
  republished message carries the **same `event_id`** because the
  envelope is encoded once, at insert time.
- **Per-key ordering**: preserved. Rows are drained in insertion order
  within one relay, analytics messages are keyed by aggregate id, and a
  failed row blocks everything behind it rather than being skipped.
- **Latency**: events reach the topic within one relay interval (≤1s by
  default) of the HTTP response instead of before it. For shift planning
  and labor assignment — decisions made at shift cadence — this is
  invisible.
- **Tracing**: the `traceparent` captured when the event was raised is
  persisted with the row and forwarded untouched by the relay, so a
  consumer's span still parents onto the originating request's trace.

## Consequences

**Positive**
- No code path persists a workforce change without also persisting its
  events for both the integration contract and the analytics report.
- No broker dependency on the request path: `POST /shift-plans` succeeds
  when Kafka is down; the events are published once it returns.
- Use cases are simpler: one atomic scope, one error.
- `Encode` is now unit-tested in isolation for both publishers, which the
  previous `Publish`-only tests could not do without a fake writer.

**Negative / accepted**
- One more table, one more goroutine, one more failure mode (the relay)
  to observe. The relay logs every failed pass at ERROR with the row id,
  topic and broker error; `outbox_events.attempts`/`last_error` are
  queryable. (_The "outbox-lag metric is a follow-up" note below shipped:
  `workforce.outbox.lag_seconds` (RegisterOutboxLagGauge,
  internal/adapters/outbound/postgres/outbox_metrics.go) reports the age of
  the oldest unpublished row, 0 when drained. Published rows are bounded by
  the ADR-0028 sweeper's 7-day retention._)
- Events are no longer synchronous with the HTTP response (documented
  above; acceptable for this domain).
- `ShiftPlanCommitted` now costs N+1 outbox rows and N+1 relay sends where
  it used to cost one batched integration write plus one analytics write.
  At shift-planning volumes this is noise.
- Encoding inside the transaction slightly lengthens it (one extra
  `SELECT` of the plan lines for `ShiftPlanCommitted`). It is the price of
  the integration publisher's fan-out-per-line contract; the alternative —
  moving that lookup into the relay — would read the plan *after* the
  transaction and could observe a later revision.
- With two pods overlapping during a rolling deploy, `SKIP LOCKED`
  prevents double-claiming within a pass but does not order rows across
  the two relays for *different* aggregates. Per-key order is what
  consumers depend on, and that is preserved.

## Alternatives considered

- **Keep Save-then-Publish and add retry around Publish.** Does not fix a
  crash between the two writes, and retries on the request path make the
  broker's latency the operator's latency. Rejected.
- **Publish-then-Save.** Inverts the failure: events on both topics for a
  change that never persisted, which is strictly worse for the analytics
  report and the integration consumer alike. Rejected.
- **One outbox row per domain event, encode in the relay.** Simpler table,
  but breaks difference 3 above: the integration Encoder's plan lookup
  would run outside the transaction, and the analytics envelope's
  `event_id` would be minted on every redelivery instead of once.
  Rejected.
- **Two outbox tables / two relays, one per topic.** Doubles the moving
  parts for no isolation benefit; a topic column and one relay give the
  same per-topic independence. Rejected.
- **Change-data-capture (Debezium).** Correct, but adds a Kafka Connect
  deployment to a kind cluster that already runs Istio, Kong, Kafka,
  Postgres and the observability stack, and moves envelope encoding out
  of the service's own code. Deferred fleet-wide, as in PPM ADR 0003.
- **A separate relay binary** (like `cmd/workforce-projector`). Cleaner
  isolation, but the relay's work is a single `SELECT`/`UPDATE` loop and
  this service already runs three binaries. In-process, with
  graceful-shutdown handling, is proportionate; lifting it into
  `cmd/workforce-relay` later would not touch the adapters.

## Verification

- Unit (`internal/application/usecases/unit_of_work_test.go`): for every
  publishing use case, every `Save` and the `Publish` run inside exactly
  one scope (checked via a ctx marker on the repos and publisher fakes),
  a publish failure rolls the scope back, a rejected command opens no
  scope, a begin failure propagates, and a nil UnitOfWork still saves and
  publishes.
- Unit (`internal/adapters/outbound/kafka/encode_test.go`): `Encode` for
  both publishers (topic, key, event_type, envelope, `traceparent` header
  present when a span is active, repo-miss error, unknown-event skip);
  `RelaySink` sets the topic per message, writes once, forwards persisted
  headers, rejects a topic-less message and wraps writer errors. All
  pre-existing `Publish` tests pass unchanged.
- Integration (`-tags=integration`, **testcontainers Postgres**
  `postgres:16-alpine` — the test owns its database, never an external
  `DATABASE_URL`, never `t.Skip`): `outbox_integration_test.go` —
  commit-together for `CommitShiftPlan` (2 integration rows + 1 analytics
  row + persisted trace header) and `AssignLabor` (associate + assignment
  + 3 analytics rows), rollback on encoder failure and on `NOT NULL`
  violation, relay publishes in id order across both topics and marks
  rows, relay stops at a failed row (`attempts=1`, `last_error` set,
  later row untouched) and recovers in order, and `Run` drains with
  batch size 1 until cancelled. Six tests, all passing locally against
  real containers.
- Gates: `gofmt -l .`, `go vet ./...`, `go build/vet -tags=integration
  ./...`, `make lint`, `make check`, `make arch-test`; domain+application
  coverage 97.7% → 97.8%.
- Cluster verification (post-merge, not part of this change): after
  deploy, `POST /shift-plans` then `SELECT topic, event_type,
  published_at FROM outbox_events ORDER BY id DESC LIMIT 4` should show
  all rows published within one interval, and wes-work-planning's
  `LaborPlanObserved` should reflect the plan without restart.
