---
id: 0023-kafka-integration-producer-partition-key
slug: /adr/0023-kafka-integration-producer-partition-key
title: "0023. Key the integration Kafka publisher by ShiftPlan aggregate id"
sidebar_label: "23. Integration publisher partition key"
sidebar_position: 24
description: "ADR 0023 — why the integration publisher (warehouse.workforce.events) now keys every message with the ShiftPlan aggregate id (buildingId/shiftId), closing a per-aggregate ordering gap the 1->8 partition scaleup exposed."
---

# 0023. Key the integration Kafka publisher by ShiftPlan aggregate id

## Status

Accepted — implemented in the same change that introduced this record.

## Context

`warehouse-infra` PR #42 scaled every business topic on the fleet's shared
Kafka broker, including `warehouse.workforce.events`, from 1 partition to 8
— a routine capacity change, not something this repo's own code or CI
caught. `kafka.Publisher.Encode` (`internal/adapters/outbound/kafka/
publisher.go`) built every `ShiftPlanCommitted` message with no `Key`, and
said so explicitly in its own doc comment ("Messages carry no key"). With
exactly one partition that omission was harmless by accident: a
single-partition topic has only one ordered log, so every consumer saw
every message for every `ShiftPlan` in the order the broker received them,
key or no key. The 1->8 scaleup removed that accident: `segmentio.Writer`'s
configured `LeastBytes` balancer distributes unkeyed messages round the
partitions by current queue size, not by content, so the several messages
one `CommitShiftPlan` fans out (one per `PathPlan` line, see ADR 0004) — and
any later re-commit for the same `buildingId`/`shiftId` — could now land on
different partitions and be consumed out of order relative to each other.
`wes-work-planning`'s `LaborPlanObserved` projection reads this topic
keyed by `path_id` today (per `rules/integrations.md`); an out-of-order
`ShiftPlanCommitted` pair for the same plan risks that projection
reflecting a stale or partially-applied revision with no error raised
anywhere.

This service's own sibling publisher, `AnalyticsPublisher` (`warehouse.
workforce.analytics`, ADR 0010), already keys every message by aggregate id
(`AssociateId` for associate-scoped events, `PathId` for path-scoped ones,
`BuildingId` for `ShiftPlanCommitted` itself) specifically so per-aggregate
ordering holds regardless of partition count — the transactional-outbox
ADR (0016) calls this out directly under "Per-key ordering: preserved
... analytics messages are keyed by aggregate id." The integration
publisher was the one path in this codebase that had not adopted the same
discipline, and nothing enforced that it should — the two publishers'
`Encode` methods are independent implementations of the same `kafka.
Encoder` interface with no shared keying helper.

Alternatives considered:

- **Do nothing and rely on the single-partition-topic accident continuing.**
  Rejected outright — the fleet has already scaled the topic; the
  ordering gap is live in production today, not hypothetical.
- **Key by `PathId` (the individual line), matching how the downstream
  `LaborPlanObserved` projection reads the topic.** Rejected: a
  `ShiftPlanCommitted` fans out N messages, one per `PathPlan` line, and
  two different lines' messages for the SAME commit would then be free to
  land on different partitions and interleave with a later commit's lines
  for those same paths — ordering would hold per-path but not per-plan,
  and a plan revision that changes which paths are staffed could still
  race itself.
- **Key by `ShiftId` alone.** Rejected: `ShiftId` is not globally unique —
  `ShiftPlan` is keyed by `(buildingId, shiftId)` everywhere else in this
  codebase (the postgres repo's composite primary key, `ShiftPlan.
  BuildingId()`/`ShiftId()`) — a shift id reused across two buildings would
  collide onto one partition for no reason and, worse, gives no ordering
  guarantee for the actual aggregate identity.
- **Key by `buildingId + "/" + shiftId` (the ShiftPlan aggregate's real
  composite identity).** Chosen: mirrors the aggregate's own identity
  exactly, gives every message for one `ShiftPlan` — every line of one
  commit, and every subsequent re-commit for that same building/shift —
  the identical key, and requires no change to the envelope wire format
  consumers already depend on (the key is Kafka message metadata, not a
  JSON field).

## Decision

`kafka.Publisher.Encode` now computes `key := shiftPlanKey(committed.
BuildingId, committed.ShiftId)` once per `ShiftPlanCommitted` event, where
`shiftPlanKey(buildingId, shiftId) = buildingId + "/" + shiftId`, and sets
that same `[]byte` as `Encoded.Key` on every `PathPlan`-line message fanned
out from that event. The doc comment that incorrectly asserted "Messages
carry no key" is corrected to document the new keying behavior and the
ordering guarantee it now provides, referencing this ADR.

No other part of the publish path changes: the envelope's wire format
(`event_id`/`event_type`/`occurred_at`/`source`/`data`) is untouched, the
transactional outbox (ADR 0016) already persists `Encoded.Key` as a
first-class column and needed no schema change, and `RelaySink` already
forwards `Key` unmodified (it has done so correctly for the analytics
topic since ADR 0016).

## Consequences

### Easier

- Every message for one `ShiftPlan` aggregate — across every `PathPlan`
  line in one commit, and across successive commits for the same
  building/shift — is now guaranteed to land on the same partition and be
  consumed in the order it was produced, at any partition count. This
  matches the exact guarantee `AnalyticsPublisher` already provided, so
  the two publishers are now consistent in behavior as well as in wire
  intent.
- The fleet's Phase-3 partition scaleup (warehouse-infra PR #42) can
  proceed on `warehouse.workforce.events` without a hidden per-aggregate
  ordering regression; this closes the one gap that scaleup exposed in
  this service.
- Future partition-count changes to this topic no longer need any
  corresponding code change here.

### Harder

- The message key is now part of this publisher's effective contract:
  changing what identifies a `ShiftPlan` (unlikely, since `buildingId`/
  `shiftId` is the aggregate's identity everywhere else in this codebase)
  would need to change `shiftPlanKey` consistently with the postgres
  repo's composite key and the domain aggregate's own accessors.
- Consumers that were (incorrectly) relying on round-robin distribution
  across partitions for load-spreading of a single hot `buildingId`/
  `shiftId` would now see all of that aggregate's traffic on one
  partition — expected and desired per-aggregate ordering behavior, but
  worth calling out for anyone provisioning partition-level consumer
  parallelism against this topic.

## Verification

- Unit (`internal/adapters/outbound/kafka/encode_test.go`,
  `TestPublisherEncode_OneMessagePerLineOnIntegrationTopic`): every
  encoded message for one commit carries the exact key
  `"BLD1/SHIFT1"` instead of the old "must be nil" assertion.
- Unit (`internal/adapters/outbound/kafka/publisher_test.go`,
  `TestPublish_AllMessagesForSameShiftPlanCarryIdenticalKey`, new): a
  three-line commit followed by a second, later commit for the SAME
  building/shift — using a recording fake `Writer` — asserts every
  message across BOTH commits carries the identical, non-empty key.
- Integration (`-tags=integration`, testcontainers Kafka —
  `confluentinc/confluent-local:7.6.1`, mirroring `internal/adapters/
  outbound/kafkacatalog/consumer_integration_test.go`'s pattern —
  `internal/adapters/outbound/kafka/publisher_partition_integration_test.go`,
  new, `TestPublish_RealKafka_SameShiftPlanLandsOnSamePartition`): creates
  the topic with **8 partitions** (matching the fleet's post-scaleup
  count), publishes through a real `segmentio.Writer` with the production
  `Hash` balancer, and asserts every message for the same building/shift —
  across two separate commits — is read back from the SAME partition.
- Gates: `go build ./...`, `go vet ./...`, `gofmt -l .` clean, `make
  check` (fmt-check, vet, build, lint, test -race) green, `make arch-test`
  green, `go test -tags=integration ./... -race -count=1` green (includes
  the new real-Kafka test above and the pre-existing outbox integration
  suite, which continues to pass unchanged), `make coverage` at 99.3%
  (gate 90%).
