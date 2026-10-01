---
id: 0026-cloudevents-mandatory-event-envelope
slug: /adr/0026-cloudevents-mandatory-event-envelope
title: 26. CloudEvents 1.0 as the mandatory event envelope
sidebar_label: 26. CloudEvents mandatory envelope
description: "ADR 0026 — every Kafka message workforce-management produces or consumes (integration warehouse.workforce.events AND analytics warehouse.workforce.analytics) is a CloudEvents 1.0 event in structured content mode, built and validated with github.com/cloudevents/sdk-go/v2/event. The flat envelope and the analytics Envelope v1 (schema_version) are removed with no coexistence. Supersedes the envelope half of ADR 0004 and ADR 0010."
---

# 26. CloudEvents 1.0 as the mandatory event envelope

## Status

Accepted — implemented in the same change that introduces this record. This
is workforce-management's adoption of the fleet-wide standard (accepted
2026-09-30 across every warehouse-systems service).

Supersedes the **envelope** decisions of
[ADR 0004](./0004-kafka-integration-events-and-cloudevents-catalog.md) ("keep
the flat envelope on the wire for now"; the AsyncAPI catalog documenting
CloudEvents "ahead of the wire format") and
[ADR 0010](./0010-analytical-data-product.md) (the analytics "Envelope v1"
with `schema_version`). The rest of both records — Kafka as the transport,
one-message-per-`PathPlan`-line fan-out, the separate analytics topic and
data product — still stands.

## Context

ADR 0004 adopted CloudEvents 1.0 as the *documented* contract in
`apis/asyncapi.yaml` while the adapter kept writing the flat cross-service
shape (`event_id`, `event_type`, `occurred_at`, `source`, `data`), and ADR
0004 itself called the gap "debt, not a feature": a reader who coded against
the spec would write a parser that did not match the bytes. ADR 0010 then
added a second flat shape for the analytics topic (Envelope v1, with
`schema_version`).

Every other service in the fleet carried the same split, and two of them
(fulfillment-execution, wes-work-planning) had started dual-envelope
migrations with toggles. The fleet decided to end it in one coordinated
cutover rather than let each consumer carry a dual-read path indefinitely.

## Decision

**Every Kafka message this service writes or reads is a CloudEvents 1.0
event. No exceptions, no coexistence.** No flat envelope, no dual-write, no
dual-read, no envelope toggle env var.

### Encoding

- CloudEvents Kafka protocol binding, **structured content mode**: the Kafka
  message value is the JSON event format.
- Every produced message carries the Kafka header
  `content-type: application/cloudevents+json; charset=UTF-8`, alongside the
  existing W3C `traceparent`/`tracestate` headers (trace context is NOT
  duplicated into CloudEvents extensions).
- Events are built, validated and (un)marshalled only with the official SDK
  event package `github.com/cloudevents/sdk-go/v2/event` (v2.16.2), via the
  single helper package `internal/adapters/kafka/cloudevents`
  (`New`, `Decode`, `ContentTypeHeader`, and the exact `type` constants).
  Transport stays `segmentio/kafka-go`; the SDK's protocol/client packages
  are not used.
- Kafka keys are unchanged (ShiftPlan aggregate id `<buildingId>/<shiftId>`
  on the integration topic per ADR 0023; the aggregate id on the analytics
  topic). Every writer uses the `kafkago.Hash{}` balancer so the key actually
  routes — the production writers previously used `LeastBytes`, which ignores
  the key, contrary to ADR 0023's intent.

### Context attributes (all required)

| attribute | value |
| --- | --- |
| `specversion` | `1.0` |
| `id` | UUID v4, minted **once** in `Encode` and persisted with the outbox row (ADR 0016), so a relay retry republishes the same id. `(source, id)` is the consumer idempotency key. |
| `source` | `/warehouse/workforce-management` (both topics) |
| `type` | `com.warehouse.wes.workforce-management.<entity>.<EventName>` |
| `subject` | the aggregate instance id — never empty |
| `time` | the domain event's occurred-at, UTC, RFC 3339 |
| `datacontenttype` | `application/json` |
| `dataschema` | `urn:warehouse:workforce-management:<events\|analytics>:<EventName>:v1` |

`data` is byte-for-byte the payload each topic carried before; this is an
envelope migration only. The analytics `schema_version` field is removed —
`dataschema` replaces it. No extension attributes.

### Types published

The same occurrence uses the same `type` on both topics; `dataschema` names
the shape.

| `type` | integration | analytics | `subject` |
| --- | --- | --- | --- |
| `com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted` | yes (one per `PathPlan` line) | yes (one per commit) | `<buildingId>/<shiftId>` |
| `com.warehouse.wes.workforce-management.shiftplan.ShiftPlanProposed` | — | yes | path id |
| `com.warehouse.wes.workforce-management.shiftplan.PathUnderstaffed` | — | yes | path id |
| `com.warehouse.wes.workforce-management.associate.AssociateShiftStarted` | — | yes | associate id |
| `com.warehouse.wes.workforce-management.associate.AssociateShiftEnded` | — | yes | associate id |
| `com.warehouse.wes.workforce-management.associate.AssociateBreakStarted` | — | yes | associate id |
| `com.warehouse.wes.workforce-management.associate.AssociateBreakEnded` | — | yes | associate id |
| `com.warehouse.wes.workforce-management.associate.AssociateCertified` | — | yes | associate id |
| `com.warehouse.wes.workforce-management.assignment.LaborAssigned` | — | yes | associate id |
| `com.warehouse.wes.workforce-management.assignment.LaborReassigned` | — | yes | associate id |

**ShiftPlanCommitted fan-out.** The fan-out (ADR 0004) is kept: N path lines
produce N integration messages. Each line message gets its **own** unique
`id` (they are distinct messages, and a consumer deduping on `id` must not
collapse lines into one). `subject` is the ShiftPlan aggregate id
`<buildingId>/<shiftId>` — the same value as the Kafka key — rather than the
path id, because the aggregate that raised the event is the ShiftPlan; the
line's `path_id` stays in `data`, where wes-work-planning already reads it.
wes-work-planning dispatches on exactly
`com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted`.

### Types consumed

| `type` | topic | consumer |
| --- | --- | --- |
| `com.warehouse.wes.process-path-management.processpath.ProcessPathCreated` | `warehouse.process-path-management.events` | `outbound/kafkacatalog` |
| `com.warehouse.wes.process-path-management.processpath.ProcessPathUpdated` | same | same |
| `com.warehouse.wes.process-path-management.processpath.ProcessPathDeactivated` | same | same |
| `com.warehouse.wes.labor-performance.performance.TaskPerformanceRecorded` | `warehouse.labor-performance.events` | `outbound/laborperformancecache` |
| the ten workforce types above (projecting subset) | `warehouse.workforce.analytics` | `inbound/kafka` analytics projector |

### Consumer rules

1. Decode with `cloudevents.Decode` (SDK unmarshal + `specversion` check +
   `Validate()`). A message that is not a valid CloudEvents 1.0 event —
   including the retired flat envelope — is a deterministic poison message:
   the analytics projector dead-letters it to `<topic>.dlq` immediately (its
   existing ADR 0022 path, no retries); the two cache consumers, which have no
   DLQ, log at WARN with topic/partition/offset and move on. Nobody ever
   falls back to parsing another shape.
2. Dispatch on the FULL `type` string; unknown types are ignored.
3. Dedupe on `id`: the analytics projector's `analytics_processed_events`
   table is now keyed by the CloudEvents `id`; the labor-performance cache
   keeps an in-memory `(source, id)` set because its running mean is a sum
   (the catalogue cache is a latest-value-per-path map, naturally
   idempotent).
4. Read `time`/`subject` from the attributes and the payload via `DataAs`.

### Versioning

Additive payload changes keep the `type` and `dataschema`. A breaking payload
change publishes a new event with a `.v2` type suffix and a new `dataschema`
version; an existing type is never mutated.

## Consequences

**Easier**

- The AsyncAPI catalog and the wire are the same thing. ADR 0004's "documented
  envelope and the wire envelope differ today" debt is closed.
- Consumers route on a namespaced `type` and can tell an integration payload
  from an analytics payload of the same occurrence by `dataschema`, without
  opening the payload.
- One helper package owns the envelope; golden exact-JSON tests pin every
  published type plus the header, and each consumer has a
  legacy-flat-message-rejected test.

**Harder**

- **No coexistence means a coordinated cutover.** This service's PR must
  deploy together with every other fleet service's; old consumers cannot
  read the new bytes and new consumers reject the old ones. Before deploying,
  drain `outbox_events` (rows were pre-encoded in the flat shape), then
  delete/recreate `warehouse.workforce.events` and
  `warehouse.workforce.analytics` (and re-seed the process-path catalogue
  topic), so no flat message remains for a FirstOffset replay — see
  warehouse-infra `docs/cloudevents-cutover.md`.
- Messages are larger (a handful of attributes per message).
- A new dependency, `github.com/cloudevents/sdk-go/v2` (event package only).

**Now true**

- `outbox_events.event_type` now stores the full CloudEvents `type` string.
- There is no `AnalyticsEnvelope`, no flat `envelope` struct and no short-name
  dispatch constant anywhere in the code.
