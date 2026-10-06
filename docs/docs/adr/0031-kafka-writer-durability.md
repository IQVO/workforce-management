---
id: 0031-kafka-writer-durability
slug: /adr/0031-kafka-writer-durability
title: "0031. Kafka writer durability: RequireAll acks and a 10ms batch timeout"
sidebar_label: 0031. Kafka writer durability
sidebar_position: 32
description: "ADR 0031 — every synchronous kafka-go writer (integration publisher, analytics publisher, outbox relay sink) runs with RequiredAcks=RequireAll and BatchTimeout=10ms: the defaults (RequireNone, 1s) made the transactional outbox silently at-most-once and capped throughput near one event per second. Recorded retroactively by the 2026-10 ADR-conformance pass."
---

# 0031. Kafka writer durability: `RequireAll` acks and a 10ms batch timeout

## Status

Accepted — recorded retroactively (2026-10 ADR-conformance pass); the
constants live in `internal/adapters/outbound/kafka/writer_config.go`.

## Context

kafka-go's `Writer` defaults are `RequiredAcks: RequireNone` and
`BatchTimeout: 1s`. For the transactional outbox's relay (ADR-0016) both
defaults are wrong:

- `RequireNone` makes `WriteMessages` return before the broker stores
  anything. The relay then marked outbox rows `published_at` for messages
  the broker never persisted — silently at-most-once, the exact opposite
  of the outbox's guarantee.
- The 1s `BatchTimeout` masked it: a synchronous one-row write waited up
  to a second for a batch, and mostly got its (unacknowledged) send
  through anyway. It also capped each service near one event/second —
  observed live when a warehouse-day simulation's ~2,600 events sat in
  `outbox_events` for an hour.

With prompt flushing, probes against a fresh 8-partition topic lost whole
batches in 3 of 6 runs under `RequireNone`, and 0 of 6 under `RequireAll`.

## Decision

Every synchronous writer in `internal/adapters/outbound/kafka` (the
integration publisher, the analytics publisher and the relay's sink)
sets:

- `RequiredAcks: segmentio.RequireAll` — a write returns only after the
  broker acknowledges it (on the single-broker kind cluster this equals
  the leader's ack).
- `BatchTimeout: 10ms` — writes still batch under load, but a lone event
  flushes almost immediately.

## Consequences

- The outbox's at-least-once guarantee holds end to end: a row is marked
  published only after the broker stored the message.
- Relay throughput is no longer batch-timeout-bound.
- Worst-case write latency gains a broker round-trip — irrelevant at this
  service's volumes.
