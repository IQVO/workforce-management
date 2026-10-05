---
id: 0019-labor-performance-cache-consumer
slug: /adr/0019-labor-performance-cache-consumer
title: 0019. Replace the synchronous labor-performance MeasuredRateClient with an event-fed local cache
sidebar_label: 0019. Labor-performance cache consumer
sidebar_position: 20
description: ProposePathPlan's measured-rate enrichment can now be served from a local, in-memory read model fed by labor-performance's new integration topic, instead of a synchronous HTTP call, mirroring this repo's own process-path-management kafkacatalog consumer.
---

# 0019. Replace the synchronous labor-performance MeasuredRateClient with an event-fed local cache

## Status

Accepted — implemented in the same change that introduced this record.

## Context

Since [ADR 0012](./0012-measured-rate-feed-for-propose-path-plan.md),
`ProposePathPlan`'s optional measured-rate enrichment has called
`labor-performance` synchronously over HTTP
(`internal/adapters/outbound/laborperformance.Client`, satisfying
`ports.MeasuredRateClient`). That call is deliberately soft and fail-open
— `ports.ErrMeasuredRateUnavailable` covers every failure mode
uniformly and `ProposePathPlan` falls back to a caller-supplied
`plannedRate` — but it is still a live, synchronous, cross-repo runtime
dependency: every enrichment attempt is a network round trip to another
bounded context, on this service's own request path.

This service already has a proven alternative shape for exactly this
situation. [ADR 0013](./0013-process-path-catalogue-validation.md) (and
the earlier fulfillment-execution/wes-work-planning rollout it mirrors)
replaced a boot-time file read of the process-path catalogue with
`internal/adapters/outbound/kafkacatalog.Consumer`: a local, in-memory
read model kept current by replaying process-path-management's
`warehouse.process-path-management.events` topic from `FirstOffset` on
every process start, gated by a `Ready()`/`WaitReady()` readiness check
that blocks the composition root from serving traffic until the initial
replay completes, using a per-process-unique Kafka consumer group (never
a shared one — see that package's doc comment for the two concrete bugs,
a stuck-on-restart readiness deadlock and a shared-group data-loss case,
this exact design was built to close).

`labor-performance`'s own ADR 0013 (on that repo) now publishes exactly
the event this integration needs: `TaskPerformanceRecorded` on a new,
dedicated integration topic, `warehouse.labor-performance.events`,
specifically so `workforce-management` can build the same kind of local
cache instead of continuing the synchronous call. This mirrors the
precedent set by `facility-layout` → `inventory-storage`'s propagation
work: replacing a runtime dependency on another bounded context's
uptime with a locally-owned, eventually-consistent read model measurably
improves this service's own availability — `ProposePathPlan` no longer
degrades (nor even slows down) when `labor-performance` is briefly
unreachable, because there is no longer a request-time call to it at
all.

### The wire contract is per-task, not a pre-computed mean

The HTTP client this cache replaces read `meanActualSeconds` straight
off `labor-performance`'s own already-computed
`GetTaskTypePerformance`-equivalent endpoint — that service does the
averaging server-side. `TaskPerformanceRecorded`, by contrast, carries
one completed task's own `actual_seconds` per message; there is no
pre-computed mean on the wire. Whatever consumes this topic to answer
"the measured mean duration for a task type" must compute that mean
itself, from a raw stream of individual task facts.

### `efficiency_pct` nullability is unrelated to `actual_seconds` availability

The payload's `efficiency_pct` is nullable (a task that could not be
scored against a defined labor standard) and must never be coerced to
`0` — but that field is not used by this cache at all.
`actual_seconds` (what `MeanActualSeconds` needs) is documented as
always present, non-null, on every message. A null `efficiency_pct`
is therefore not a reason to skip a message's contribution to the
running mean; the two fields are independent facts about the same task.

## Decision

Add `internal/adapters/outbound/laborperformancecache`, a package that
mirrors `internal/adapters/outbound/kafkacatalog` byte-for-byte in its
concurrency and readiness design — the exact same `FirstOffset` replay,
per-partition target-offset capture, `Ready()`/`WaitReady(ctx)` gate, and
per-process-unique consumer group naming (`hostname-pid-nanotime`) — and
adapts it to labor-performance's topic, envelope, and payload shape
instead of process-path-management's.

`laborperformancecache.Consumer` satisfies `ports.MeasuredRateClient`
directly (the same interface the HTTP client and the permissive no-op
already satisfy — this is a third, drop-in implementation, not a new
port and not a replacement of the existing two).

### Running-mean strategy: incremental sum+count per TaskType

The cache keeps one `(sum float64, count int64)` pair per TaskType
(`PICK`, `PACK`, `SLAM` — the same closed set
`internal/adapters/outbound/laborperformance.taskTypeForPathId` already
maps `PathId` onto; that mapping is duplicated verbatim here since it is
unexported in its own package, and must stay byte-for-byte identical to
it). Every `TaskPerformanceRecorded` message adds its `actual_seconds`
to the matching TaskType's sum and increments its count; `mean = sum /
count` is O(1) to compute on every `MeanActualSeconds` call, with no
history retained beyond the two running numbers.

This was chosen over the two other candidates considered:

- **Track only the single most-recently-observed `actual_seconds` per
  TaskType.** Rejected: this answers "how long did the last task take",
  a materially noisier and different question than "what is the
  measured mean duration" `ProposePathPlan` actually wants — and the
  question the HTTP client's replaced endpoint already answered. A
  single slow or fast outlier task would swing the proposed headcount
  on every subsequent call until the next task of that type completes.
- **A bounded "most recent N tasks" window (ring buffer) per TaskType.**
  Rejected as unnecessary complexity for this fleet's actual data shape:
  TaskType cardinality is small and fixed (three values today, and this
  service's own `taskTypeForPathId` mapping is a closed switch, not an
  open set that could grow unboundedly), so an unbounded incremental
  sum+count carries a memory cost of a handful of `float64`/`int64`
  pairs regardless of how many tasks are ever recorded over the
  service's lifetime — there is no unbounded-growth risk an eviction
  window would be protecting against. A ring buffer would only add
  implementation and failure-mode surface (eviction ordering, size
  tuning) for a memory-safety property the closed TaskType set already
  gives for free.

A null `efficiency_pct` never excludes a message from the mean — see
Context above; only `actual_seconds`, which the wire contract guarantees
is always present, feeds the running mean.

### Wiring: a third `LABOR_PERFORMANCE_MODE` value, `kafka-cache`

`cmd/workforce/main.go`'s `buildMeasuredRateClient` continues to select
between `http` and `permissive` (its long-standing two modes,
unmodified). A new mode, `LABOR_PERFORMANCE_MODE=kafka-cache`, is
handled as a separate branch in `run()` — mirroring exactly how
`PATH_CATALOGUE_SOURCE=kafka` is already wired for
`kafkacatalog.Consumer` immediately above it in the same file: start the
consumer's `Run` goroutine first, then block on `WaitReady` with a
bounded timeout (`laborperformancecache.WaitReadyTimeout`, 60s, the same
value `kafkacatalog.WaitReadyTimeout` uses) before the service is
considered ready to serve traffic — never the other order, which would
guarantee a deadlock on a topic no consumer is yet reading. On shutdown,
the consumer's context is cancelled and its reader closed alongside the
existing `kafkaCatalogue` cleanup.

`http` and `permissive` are completely untouched — no existing code path
changes behavior. `kafka-cache` is strictly additive: an operator must
explicitly opt in by setting `LABOR_PERFORMANCE_MODE=kafka-cache` (and
`KAFKA_BROKERS`) for this service to stop calling `labor-performance`
over HTTP.

## Consequences

**Positive**

- `ProposePathPlan`'s measured-rate enrichment no longer depends on
  `labor-performance` being reachable at request time when
  `LABOR_PERFORMANCE_MODE=kafka-cache` is set — a Kafka outage or a
  `labor-performance` deploy no longer has any chance of slowing down or
  degrading a plan proposal, matching the same availability
  improvement `facility-layout` → `inventory-storage`'s propagation
  work already demonstrated for this fleet's pattern of replacing a
  synchronous cross-context call with a local, event-fed read model.
- Zero new infrastructure beyond a Kafka consumer this repo already
  knows how to build and operate — the concurrency/readiness design is
  copied, not reinvented, from `kafkacatalog`.
- `http` and `permissive` remain exactly as they were; this is a
  non-breaking, additive third option, not a replacement or a removal.

**Negative / accepted**

- **Eventual consistency, not synchronous freshness.** A `Mean
  ActualSeconds` answer reflects every `TaskPerformanceRecorded` message
  this process has replayed and consumed so far, not necessarily the
  very latest completed task — there is an unavoidable propagation
  delay from labor-performance's outbox relay through Kafka to this
  consumer. This is the same trade-off `kafkacatalog` already accepts
  for the process-path catalogue, and it is acceptable here for the same
  reason: `MeasuredRateClient` is a soft, optional enrichment
  (`ports.ErrMeasuredRateUnavailable`'s whole contract exists to make a
  stale-or-missing rate a non-failure), never a hard invariant a
  request's correctness depends on.
- **Memory bound is real but small and fixed.** The per-TaskType running
  totals map grows with the number of distinct TaskTypes ever observed,
  not the number of tasks — bounded by `taskTypeForPathId`'s closed
  `{PICK, PACK, SLAM}` set today. A future TaskType added to that closed
  set (a coordinated, additive change on both sides) adds one more
  `(sum, count)` pair; there is no path to unbounded growth from replay
  volume alone.
- **A brand-new process replays the entire integration topic's history
  from `FirstOffset` on every start**, same as `kafkacatalog` already
  does — an accepted, deliberate cost (bounded catch-up time, not
  unbounded resource growth) in exchange for the correctness guarantee
  that a fresh process's cache reflects the topic's full history rather
  than an arbitrary partial view.

## Alternatives considered

- **Keep the synchronous HTTP call, add caching/retries around it.**
  Rejected: still a live runtime dependency during any partial outage of
  `labor-performance`'s HTTP surface, and this fleet has an existing,
  proven event-fed-cache pattern purpose-built for exactly this
  situation.
- **A bounded most-recent-N-tasks window per TaskType instead of a
  running mean.** Rejected — see the Running-mean strategy section
  above.
- **Reuse `kafkacatalog`'s package for this too, parameterized by
  topic/payload.** Rejected: the payload shape, event type, and the
  answer being computed (a numeric running mean vs. a catalogue lookup)
  are different enough that forcing one package to serve both would
  couple two independent contracts (process-path-management's and
  labor-performance's) behind a single abstraction for no real code
  reuse — the two packages already share only structural shape
  (replay/readiness plumbing), which is exactly what is copied, not
  imported.

## Verification

- Unit (`internal/adapters/outbound/laborperformancecache/consumer_test.go`,
  mirroring `kafkacatalog/consumer_test.go`'s shapes with a fake
  `Reader`, no real broker): readiness with no target offsets, readiness
  after catching up a single partition, readiness gated on multiple
  partitions, a null `efficiency_pct` still updating the mean, an
  unrecognized event type being ignored, `MeanActualSeconds` returning
  `ErrMeasuredRateUnavailable` for a `PathId` with no TaskType
  counterpart and for a TaskType with no data observed yet, multiple
  TaskTypes tracked independently, and an unattributed (empty
  `AssociateId`) task still updating its TaskType's mean. All pass:
  `go test ./internal/adapters/outbound/laborperformancecache/... -race`.
- Integration (`-tags=integration`, testcontainers Kafka
  `confluentinc/confluent-local:7.6.1`, no external broker, mirroring
  `kafkacatalog/consumer_integration_test.go`'s setup exactly):
  publishes real `TaskPerformanceRecorded` envelopes (including one with
  a null `efficiency_pct`) onto a throwaway topic, replays them, and
  asserts `MeanActualSeconds` returns the correct running mean; a second
  test runs two consumer instances back to back against the same topic
  and confirms the second independently replays the full history rather
  than resuming the first instance's committed offset — the same
  regression shape `kafkacatalog`'s own integration test proves. Run and
  passed locally: `--- PASS:
  TestNewConsumerForTopic_ReplaysTaskPerformanceRecordedAndComputesMean
  (28.78s)`, `--- PASS:
  TestNewConsumerForTopic_TwoInstancesInARow_BothReplayFully (37.42s)`.
- `make check` (fmt-check, vet, build, lint, test -race) and `make
  check-all` (+ coverage gate, arch-test, bdd) both run locally — see
  the PR description for the verbatim output.
