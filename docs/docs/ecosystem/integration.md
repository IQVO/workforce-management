---
id: integration
title: Integration
sidebar_label: Integration
sidebar_position: 4
description: The topics this service publishes and consumes, its two synchronous sibling calls, the CloudEvents wire format, and how to smoke-test it.
---

# Integration

One integration topic out (plus a separate analytics topic). Up to two topics
in and up to two synchronous calls to siblings, each selected by an env var
and off by default. The table below lists every edge in
`cmd/workforce/main.go`.

| Direction | Counterpart | Mechanism | Selected by | Default |
| --- | --- | --- | --- | --- |
| Out | `wes-work-planning`, `warehouse-planning` | Kafka `warehouse.workforce.events` (`ShiftPlanCommitted`) | `EVENT_PUBLISHER=kafka` | `log` (no broker) |
| Out | own analytics projector | Kafka `warehouse.workforce.analytics` (every domain event) | `EVENT_PUBLISHER=kafka` | `log` |
| In (sync) | `fulfillment-execution` | `GET /capacity/{capability}` on every `CommitShiftPlan` ([ADR 0014](../adr/0014-installed-capacity-ceiling.md)) | `INSTALLED_CAPACITY_MODE=http` + `FULFILLMENT_EXECUTION_BASE_URL` | `permissive` = **every commit fails with 503** |
| In (sync) | `labor-performance` | `GET /task-types/{taskType}/performance` when `ProposePathPlan` has no caller rate ([ADR 0012](../adr/0012-measured-rate-feed-for-propose-path-plan.md)) | `LABOR_PERFORMANCE_MODE=http` + `LABOR_PERFORMANCE_BASE_URL` | `permissive` = no measured rate (fail-open) |
| In (async) | `labor-performance` | Kafka `warehouse.labor-performance.events` (`TaskPerformanceRecorded`) into an in-memory cache ([ADR 0019](../adr/0019-labor-performance-cache-consumer.md), [ADR 0020](../adr/0020-idle-share-staffing-signal.md)) | `LABOR_PERFORMANCE_MODE=kafka-cache` + `KAFKA_BROKERS` | off |
| In (async) | `process-path-management` | Kafka `warehouse.process-path-management.events` (`ProcessPathCreated`/`Updated`/`Deactivated`) into the in-memory process-path catalogue | `PATH_CATALOGUE_SOURCE=kafka` + `KAFKA_BROKERS` | `file` (`PATH_CATALOGUE_FILE`) |

The `warehouse-infra` kind cluster sets `INSTALLED_CAPACITY_MODE=http` and
`LABOR_PERFORMANCE_MODE=kafka-cache`, and sets `PATH_CATALOGUE_SOURCE=kafka`
when its `deploy_process_path_kafka_source` flag is on. All of these edges are
therefore live there.

## What is published

| | |
| --- | --- |
| **Topic** | `warehouse.workforce.events` |
| **Event** | `com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted` — the only type on this topic |
| **Trigger** | a successful `CommitShiftPlan` |
| **Fan-out** | **one message per `PathPlan` line** |
| **Client** | `github.com/segmentio/kafka-go` |
| **Adapter** | `internal/adapters/outbound/kafka/publisher.go` |
| **Consumers** | `wes-work-planning`, into its `LaborPlanObserved` read model, keyed by `path_id`; `warehouse-planning`, whose labor-capacity consumer (group `warehouse-planning-labor-capacity`) registers it as a LABOR capacity constraint |

## Selecting the publisher

The Kafka publisher is off by default. Both publishers implement the same
`ports.EventPublisher` interface, so nothing above the adapter layer knows the
difference.

| Variable | Default | Meaning |
| --- | --- | --- |
| `EVENT_PUBLISHER` | `log` | `log` (in-memory/log publisher) or `kafka` |
| `KAFKA_BROKERS` | `localhost:9092` | comma-separated broker list, used when `EVENT_PUBLISHER=kafka` |

The default keeps local runs and the whole test suite free of any broker
dependency.

## The fan-out

A `ShiftPlan` has multiple `PathPlan` lines. `CommitShiftPlan` with three path
lines publishes **three** Kafka messages, one per line, each carrying that
single line's `planned_heads`/`planned_rate`/`planned_hours` alongside the
plan's `building_id` and `shift_id`.

This matches how the consumer keys its read model: `LaborPlanObserved` is one
row per path. Consumers must expect N messages per commit and must not assume a
message carries the whole plan.

The domain event carries only the `ShiftPlan`'s identity (`buildingId`,
`shiftId`). The adapter loads the committed plan through the `ShiftPlanRepo` to
expand it. That keeps fan-out an integration concern: the domain has no opinion
about message granularity.

## The envelope on the wire

Every message on both topics is a **CloudEvents 1.0** event in structured
content mode — mandatory, no other envelope, no toggle
([ADR 0026](../adr/0026-cloudevents-mandatory-event-envelope.md)). The Kafka
message value is the JSON event format and every message carries the header
`content-type: application/cloudevents+json; charset=UTF-8` next to the W3C
trace headers. One line of a committed plan, byte for byte:

```json
{
  "specversion": "1.0",
  "id": "9f1c2b7e-4c3a-4a1d-9f0b-6c2b8a7d1e33",
  "source": "/warehouse/workforce-management",
  "type": "com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted",
  "subject": "bldg-1/shift-1",
  "datacontenttype": "application/json",
  "dataschema": "urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1",
  "time": "2026-08-21T22:00:00Z",
  "data": {
    "building_id": "bldg-1",
    "shift_id": "shift-1",
    "path_id": "pack",
    "planned_heads": 3,
    "planned_rate": 30,
    "planned_hours": 24
  }
}
```

- `id` is a UUID v4 minted once per line message when the event is encoded and
  persisted with the outbox row, so a relay retry republishes the same id.
  Every line of a fan-out has its own id.
- `subject` is the ShiftPlan aggregate id `<buildingId>/<shiftId>` — the same
  value as the Kafka key, so every line of a plan lands on one partition.
- `time` is the domain occurred-at, RFC 3339 UTC.
- `dataschema` is `...:events:...` on this topic and `...:analytics:...` on
  `warehouse.workforce.analytics`; the `type` is the same on both.

Helpers live in one package, `internal/adapters/kafka/cloudevents`
(`New`/`Decode`/`ContentTypeHeader`, built on the official
`github.com/cloudevents/sdk-go/v2/event`). The full catalog is on the
[Events page](../api-reference/events.md).

## Smoke-testing the edge

The fleet runs one shared Kafka broker: the in-cluster release in the
`warehouse-infra` kind cluster, reachable from the host at `localhost:9092`.
This repo's own `docker-compose.yml` only runs Postgres — do not add a broker
to it.

```bash
export EVENT_PUBLISHER=kafka
export KAFKA_BROKERS=localhost:9092
go run ./cmd/workforce

# two path lines -> expect two messages
curl -X POST localhost:8080/shift-plans \
  -d '{"buildingId":"bldg-1","shiftId":"shift-1","lines":[
        {"pathId":"pack","plannedHeads":3,"plannedRate":30,"plannedHours":24,"installedStations":10},
        {"pathId":"pick","plannedHeads":2,"plannedRate":25,"plannedHours":16,"installedStations":10}
      ]}'

# in another terminal (any Kafka CLI pointed at the shared broker, e.g.
# kubectl exec into kafka-controller-0 in the warehouse-systems namespace)
kafka-console-consumer.sh \
  --bootstrap-server localhost:9092 \
  --topic warehouse.workforce.events \
  --from-beginning --max-messages 2
```

## What is deliberately not published

`LaborAssigned`, `LaborReassigned` and `PathUnderstaffed` never go on the
integration topic; they reach only this service's own analytics topic. Since
[ADR 0034](../adr/0034-site-scoped-staffing-gap.md) the analytics
`PathUnderstaffed` carries an optional `site_code` (additive, still `v1`):
present when the gap was computed for one canonical site, absent when it was
computed fleet-wide — consumers must treat absence as "unscoped".

Publishing individual assignment moves would let a downstream context
reconstruct a per-associate location feed — exactly the picture the
[path boundary](../business-context/path-boundary.md) exists to withhold. If a
real downstream need appears, the right answer is a read-model endpoint with a
defined shape, not a firehose of moves.

The remaining `AssociateShift` events stay off the integration topic for the
simpler reason that nobody has asked: no sibling consumes roster or break
events.

## What is consumed

Two sibling topics, both **opt-in** and both used only to build an in-memory
cache that the `cmd/workforce` process rebuilds from the earliest offset on
every start:

- **`warehouse.process-path-management.events`**
  (`internal/adapters/outbound/kafkacatalog`, `PATH_CATALOGUE_SOURCE=kafka`).
  It replaces the boot-time `PATH_CATALOGUE_FILE` read. Path ids on propose,
  commit, assign and staffing-gap requests are validated against this cache
  ([ADR 0013](../adr/0013-process-path-catalogue-validation.md)).
- **`warehouse.labor-performance.events`**
  (`internal/adapters/outbound/laborperformancecache`,
  `LABOR_PERFORMANCE_MODE=kafka-cache`). It supplies measured rates to
  `ProposePathPlan` and the observed idle share to `GetStaffingGap` and
  `ProposePathPlan`.

Both consumers decode every message with the CloudEvents SDK, dispatch on the
full `type` (`com.warehouse.wes.process-path-management.processpath.ProcessPathCreated`
/ `Updated` / `Deactivated` and
`com.warehouse.wes.labor-performance.performance.TaskPerformanceRecorded`), and
log at WARN and skip anything that is not a valid CloudEvents 1.0 event. The
labor-performance cache also dedupes on the CloudEvents `id`, because its
running mean is a sum.

Both consumers use a **per-process-unique consumer group** (prefix + host +
PID + timestamp), so every process replays the full history. Before serving
traffic, each one waits up to 60s (`WaitReadyTimeout`) for the replay to catch
up. Neither writes to Postgres, so there is no processed-events table on the
OLTP side. The only dedupe table (`analytics_processed_events`, keyed by the
CloudEvents `id`) belongs to
the analytics projector
(`cmd/workforce-projector`), which consumes this service's own
`warehouse.workforce.analytics` topic under the fixed group
`workforce-analytics`.

## Synchronous calls to siblings

- **`fulfillment-execution` — `GET /capacity/{capability}`**
  (`internal/adapters/outbound/fulfillmentexecution`). It is called for every
  distinct capability the commit's paths require — each line's path is first
  resolved through the process-path catalogue to its `requiredCapabilities`
  (path `PICK` → capability `pick`; the path id itself is never sent), and a
  path's ceiling is the smallest count across its capabilities. It is
  **fail-loud**: any failure rejects the
  whole commit with `503 installed-capacity-unavailable`. The default
  `permissive` client always fails, so a commit only succeeds when
  `INSTALLED_CAPACITY_MODE=http`.
- **`labor-performance` — `GET /task-types/{taskType}/performance`**
  (`internal/adapters/outbound/laborperformance`). It is called only when
  `LABOR_PERFORMANCE_MODE=http` and `ProposePathPlan` gets no positive
  `plannedRate`. It is **fail-open**: on any failure the proposal falls back to
  the caller rate.

Both clients send no `Authorization` header
([ADR 0018](../adr/0018-remove-fleet-rest-identity.md)).
