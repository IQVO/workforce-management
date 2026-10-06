---
id: 0004-kafka-integration-events-and-cloudevents-catalog
title: 0004. Kafka for integration events, with a CloudEvents catalog ahead of the wire format
sidebar_label: 0004. Kafka + CloudEvents catalog
sidebar_position: 5
description: "One topic, one event, fanned out per path line — and why the AsyncAPI catalog originally documented an envelope the adapter did not yet write (envelope superseded by ADR-0026: CloudEvents 1.0 is now the only wire envelope)."
---

# 0004. Kafka for integration events, with a CloudEvents catalog ahead of the wire format

## Status

Accepted; **envelope decision superseded by
[ADR-0026](./0026-cloudevents-mandatory-event-envelope.md)**. Kafka publishing
added in commit `15a0530`; the AsyncAPI CloudEvents catalog added later in
commit `af2181c`. The flat envelope described below ("keep the flat envelope on
the wire for now") is retired: CloudEvents 1.0 is now the only envelope on the
wire, and the AsyncAPI catalog is no longer "ahead of" it. The topic, the
one-way publish and the per-line fan-out still stand.

## Context

`wes-work-planning` needs to know what labor was actually committed per path in
order to flow-balance. That fact is owned here. The question was how to get it
there.

**Why not a synchronous call.** Either direction is wrong. If Work Planning
calls this service, a Core context now has a runtime dependency on a Supporting
one — a labor-service outage becomes a planning outage. If this service calls
Work Planning, a Supporting context needs a client, retries and knowledge of the
consumer's availability, and gains a reason to fail a `CommitShiftPlan` that
already succeeded in its own domain.

**Why Kafka specifically.** Four other services in the platform already
integrate over a shared broker with a shared envelope. Adding a different
mechanism for one edge would mean a second set of operational concerns for the
sake of one topic. `github.com/segmentio/kafka-go` is pure Go with no cgo,
which keeps the build and the container image simple.

**The granularity question.** A `ShiftPlan` is committed as a whole, so the
domain event is naturally one-per-commit. But the consumer keys its read model
by `path_id`, one row per path. Publishing one message carrying the whole plan
would push a fan-out loop into every consumer.

**The envelope question.** The platform's original cross-service envelope is a
flat shape — `event_id`, `event_type`, `occurred_at`, `source`, `data` —
specified in each repo's `INTEGRATION.md` and already implemented across four
services. CloudEvents 1.0 is the industry standard for exactly this, with real
benefits: `specversion` for envelope evolution, a reverse-DNS `type` that
namespaces events across contexts, `subject` for routing without opening the
payload, and off-the-shelf tooling. But four services already speak the flat
shape, and `wes-work-planning`'s consumer already parses it.

## Decision

**Publish `ShiftPlanCommitted` to Kafka topic `warehouse.workforce.events`,
asynchronously and one-way.** This service publishes and forgets: it makes
no synchronous call to any sibling on the publish path, and its own writes
never depend on a consumer being up. (_Amended 2026-10: the "zero in / no
consumer group" claim below described the original state; this service now
runs three inbound Kafka consumers — the process-path catalogue
(`PATH_CATALOGUE_SOURCE=kafka`, ADR-0013), the labor-performance measured
rate cache (ADR-0019) and its idle-share feed (ADR-0020) — plus two
synchronous outbound clients (labor-performance, fulfillment-execution).
None of those consume THIS service's own integration topic, which is still
one topic out; the consumers and sync clients are each recorded by their
own ADRs._)

**Fan the event out into one message per `PathPlan` line.** A plan committed
with three path lines produces three messages, each carrying that line's
`path_id`, `planned_heads`, `planned_rate` and `planned_hours` alongside the
plan's `building_id` and `shift_id`. The fan-out lives in the *adapter*, which
loads the committed plan through `ShiftPlanRepo`, so the domain has no opinion
about message granularity.

**Select the publisher by environment.** `EVENT_PUBLISHER=kafka|log`, defaulting
to `log`. Both implement the same `ports.EventPublisher` interface, so the
choice is invisible above the adapter layer and the entire test suite runs with
no broker.

**Adopt CloudEvents 1.0 structured mode as the documented target contract**, in
`apis/asyncapi.yaml`, with the reverse-DNS type convention:

```
com.warehouse.<subdomain>.<bounded-context>.<entity>.<EventName>
```

**Keep the flat envelope on the wire for now.** The adapter continues to write
the flat cross-service shape, because that is what the live consumer parses. The
`data` payloads are identical between the two forms; only the surrounding
context attributes differ.

**Document the divergence explicitly rather than papering over it** — on the
[Events](../api-reference/events.md) page, on the
[Integration](../ecosystem/integration.md) page, and here.

## Consequences

**Easier**

- Committing a shift plan cannot fail because a downstream consumer is down.
- The Kafka publisher was added as a **new adapter only**, with no change to any
  aggregate, invariant or use case — the payoff of
  [ADR 0001](./0001-hexagonal-ports-and-adapters.md).
- Consumers get exactly the granularity they key on. No fan-out logic
  downstream.
- The default `log` publisher keeps unit tests, acceptance specs and local runs
  broker-free.
- The AsyncAPI catalog documents all ten domain events, not just the one that is
  published, so a downstream team can see what is *available* to wire next.
- Both specs are Spectral-linted in CI, so the catalog cannot silently rot.

**Harder**

- **The documented envelope and the wire envelope differ today.** That is a real
  cost — a reader of the AsyncAPI spec who codes against it without reading the
  caveat will write a parser that does not match the bytes. Mitigated by
  flagging it prominently in three places, but the honest position is that this
  is debt, not a feature.
- Migrating to CloudEvents on the wire is a coordinated change across the
  producer and every consumer, or a dual-write period. It has not been done.
- At-least-once delivery makes consumer idempotency mandatory.
  `wes-work-planning` carries that burden with a `processed_events` table keyed
  by event id; this service does not, because it consumes nothing.
- Consumers must expect N messages per commit. A consumer assuming one message
  per plan will silently see only the last path line.

**Now true**

- One topic out, zero in. That asymmetry is the shape of a Supporting
  context. (_Superseded in part by ADR-0019/0020 and the
  `PATH_CATALOGUE_SOURCE=kafka` mode of ADR-0013: this service now consumes
  three feed topics, though still none of its own integration topic._)
- The migration path is: emit CloudEvents alongside the flat envelope, move
  `wes-work-planning`'s consumer onto `type`-based routing, then drop the flat
  shape. Whoever does it should supersede this ADR.
