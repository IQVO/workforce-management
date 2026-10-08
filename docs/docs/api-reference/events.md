---
id: events
title: Events
sidebar_label: Events
sidebar_position: 2
description: The CloudEvents catalog for warehouse.workforce.events and .analytics — attributes, type convention, and every payload.
---

# Events

Hand-authored from `apis/asyncapi.yaml` (AsyncAPI 2.6.0), which is linted by
Spectral in CI on every push. Every Kafka message this service produces or
consumes is a **CloudEvents 1.0** event — mandatory, with no other envelope
([ADR 0026](../adr/0026-cloudevents-mandatory-event-envelope.md)).

| | |
| --- | --- |
| **Channel / topic** | `warehouse.workforce.events` |
| **Protocol** | Kafka |
| **Default content type** | `application/cloudevents+json` |
| **Events in the catalog** | 10 |
| **On the integration topic** | 1 — `ShiftPlanCommitted` |
| **On the analytics topic** (`warehouse.workforce.analytics`) | all 10 |

## The CloudEvents envelope

Every message is a **CloudEvents 1.0 structured-mode** event: the Kafka message
value is the JSON event format, and every message carries the Kafka header
`content-type: application/cloudevents+json; charset=UTF-8` alongside the W3C
`traceparent`/`tracestate` headers. Every attribute below is **required**.

| Attribute | Type | Meaning |
| --- | --- | --- |
| `specversion` | string, always `"1.0"` | CloudEvents specification version |
| `id` | string (uuid) | Unique identifier of this occurrence, a UUID v4 minted once at encode time and persisted with the outbox row, so a redelivery repeats it. The consumer dedupe key |
| `source` | string (uri-reference) | The producing context — always `/warehouse/workforce-management` |
| `type` | string | Reverse-DNS event type; see the convention below |
| `subject` | string | The aggregate instance the event is about |
| `time` | string (date-time) | RFC 3339 UTC timestamp of when the event occurred in the domain |
| `datacontenttype` | string, always `"application/json"` | Content type of `data` |
| `dataschema` | string (uri) | `urn:warehouse:workforce-management:<events\|analytics>:<EventName>:v1` — names the payload shape per topic |
| `data` | object | The event-specific payload |

## The `type` naming convention

```
com.warehouse.<subdomain>.<bounded-context>.<entity>.<EventName>
```

All lowercase except the final PascalCase event name. For this context:

- `<subdomain>` = `wes`
- `<bounded-context>` = `workforce-management`
- `<entity>` = the aggregate that raises it: `associate`, `shiftplan` or
  `assignment`

So a committed shift plan is published as:

```
com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted
```

`PathUnderstaffed` uses `shiftplan`, because the gap is measured against a
committed `ShiftPlan`'s planned heads.

Consumers must **route on the full `type`** and must **ignore unknown `type`
values**. The same occurrence keeps the same `type` on the integration and the
analytics topic; `dataschema` tells the two payload shapes apart. A breaking
payload change gets a new `.v2` type and a new `dataschema` version.

## Integration topic — `warehouse.workforce.events`

### `ShiftPlanCommitted`

**Type:** `com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted`
· **Subject:** `{site_code}/{shift_id}` (= the Kafka key; written `{building_id}/{shift_id}` before [ADR 0035](../adr/0035-sitecode-converges-building-id.md), same value, unchanged)
· **dataschema:** `urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1`

A human committed the split of headcount across process paths for one
site's shift. The commit is validated in the domain: `plannedHeads(path)`
must not exceed `installedStations(path)`, and planned hours must fit the
shift's max hours.

**This event is fanned out into one message per `PathPlan` line.** A plan
committed with three path lines produces three messages, each carrying that one
line's `path_id`, `planned_heads`, `planned_rate` and `planned_hours` alongside
the plan's `site_code`, `building_id` and `shift_id`. Consumers must expect N messages per
commit and must not assume a message carries the whole plan. Each line message
has its **own unique `id`**; all of them share the `subject`/key of the
ShiftPlan aggregate, so they land on one partition in order.

| `data` field | Type | Meaning |
| --- | --- | --- |
| `site_code` | string | The canonical Site code the plan was committed for. Additive on `v1` and not `required` (messages encoded before the change lack it): fall back to `building_id` when absent |
| `building_id` | string | **Deprecated** — same value as `site_code`; kept, removal needs a later breaking ADR |
| `shift_id` | string | The shift the plan was committed for |
| `path_id` | string | The process path this message's line covers |
| `planned_heads` | integer | Heads committed to this path |
| `planned_rate` | number | Expected units per head per hour |
| `planned_hours` | number | Total hours committed to this path |

```json
{
  "specversion": "1.0",
  "id": "9f1c2b7e-4c3a-4a1d-9f0b-6c2b8a7d1e33",
  "source": "/warehouse/workforce-management",
  "type": "com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted",
  "subject": "BLD1/SHIFT1",
  "time": "2026-08-21T22:00:00Z",
  "datacontenttype": "application/json",
  "dataschema": "urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1",
  "data": {
    "site_code": "BLD1",
    "building_id": "BLD1",
    "shift_id": "SHIFT1",
    "path_id": "pack",
    "planned_heads": 3,
    "planned_rate": 50,
    "planned_hours": 24
  }
}
```

**Downstream consumer:** `wes-work-planning`, which projects it into its
`LaborPlanObserved` read model keyed by `path_id`, and explicitly does *not*
feed it into its own `ShiftPlan` aggregate.

## Analytics topic — `warehouse.workforce.analytics`

Every event below (and `ShiftPlanCommitted`, as one message per commit carrying
only `site_code`/`building_id`/`shift_id`) is published to the internal analytics topic
with `dataschema` `urn:warehouse:workforce-management:analytics:<EventName>:v1`,
for this service's own analytics projector. None of them is on the integration
topic.

### `ShiftPlanProposed`

**Type:** `...shiftplan.ShiftPlanProposed` · **Subject:** `{path_id}` (= the Kafka key)

Heads for a path were computed as a pure computation —
`heads = ceil(charge ÷ plannedRate)` — before a human committed anything. A
proposal is advisory: the software proposes, a human commits.

| `data` field | Type |
| --- | --- |
| `site_code` | string (canonical, additive, not `required`; same value as `building_id`) |
| `building_id` | string (**deprecated**, same value as `site_code`) |
| `path_id` | string |
| `planned_heads` | integer |
| `planned_rate` | number |

### `PathUnderstaffed`

**Type:** `...shiftplan.PathUnderstaffed` · **Subject:** `{path_id}`

A path's active `LaborAssignment` count is below the `plannedHeads` committed
for it on the current `ShiftPlan`. A **flag, not a decision**: this context
surfaces the gap and never moves anyone in response. Rebalancing is a human
call, recorded back here as a new assignment.

| `data` field | Type |
| --- | --- |
| `path_id` | string |
| `planned_heads` | integer |
| `active_heads` | integer |

### `AssociateShiftStarted`

**Type:** `...associate.AssociateShiftStarted` · **Subject:** `{associate_id}`

An associate's roster entry was created for the shift.

| `data` field | Type | Meaning |
| --- | --- | --- |
| `associate_id` | string | The associate starting the shift |

### `AssociateCertified`

**Type:** `...associate.AssociateCertified` · **Subject:** `{associate_id}`

A certification was added to an associate's roster entry.

| `data` field | Type |
| --- | --- |
| `associate_id` | string |
| `certification` | string |

### `AssociateBreakStarted`

**Type:** `...associate.AssociateBreakStarted` · **Subject:** `{associate_id}`

An associate began a logged break and is no longer assignable.

| `data` field | Type |
| --- | --- |
| `associate_id` | string |

### `AssociateBreakEnded`

**Type:** `...associate.AssociateBreakEnded` · **Subject:** `{associate_id}`

An associate ended a logged break and is assignable again.

| `data` field | Type |
| --- | --- |
| `associate_id` | string |

### `AssociateShiftEnded`

**Type:** `...associate.AssociateShiftEnded` · **Subject:** `{associate_id}`

An associate's shift closed, ending all of their active assignments.

| `data` field | Type |
| --- | --- |
| `associate_id` | string |

### `LaborAssigned`

**Type:** `...assignment.LaborAssigned` · **Subject:** `{associate_id}`

An associate was placed on a process path for an interval.

| `data` field | Type | Meaning |
| --- | --- | --- |
| `associate_id` | string | The assigned associate |
| `path_id` | string | Process path the associate now works, e.g. `pack`. **Never a task id.** |

### `LaborReassigned`

**Type:** `...assignment.LaborReassigned` · **Subject:** `{associate_id}`

An associate's active assignment was ended in favour of another path.

| `data` field | Type | Meaning |
| --- | --- | --- |
| `associate_id` | string | The reassigned associate |
| `from_path_id` | string | Process path whose active assignment was ended |
| `to_path_id` | string | Process path the associate was moved onto |

## Consuming this channel

- **Route on the full `type`.** Never a short name or suffix match; do not
  switch on `subject` or on payload shape.
- **Ignore unknown types.** The catalog will grow.
- **Expect N messages per commit** for `ShiftPlanCommitted`, one per path line.
- **Assume at-least-once delivery.** Kafka redelivers; consumers must be
  idempotent on the CloudEvents `id` — `wes-work-planning` does exactly this,
  backed by a `processed_events` table with the event id as primary key.
- **Reject anything that is not a valid CloudEvents 1.0 event** (DLQ or
  WARN-and-skip). Never fall back to parsing another shape.

## What this service consumes

From siblings, two opt-in topics. Each is replayed from the earliest offset
into an in-memory cache under a per-process consumer group. Neither is
written to Postgres.

| Topic | Producer | Event types used | Enabled by | Adapter |
| --- | --- | --- | --- | --- |
| `warehouse.process-path-management.events` | `process-path-management` | `com.warehouse.wes.process-path-management.processpath.ProcessPathCreated`, `…ProcessPathUpdated`, `…ProcessPathDeactivated` | `PATH_CATALOGUE_SOURCE=kafka` | `internal/adapters/outbound/kafkacatalog` |
| `warehouse.labor-performance.events` | `labor-performance` | `com.warehouse.wes.labor-performance.performance.TaskPerformanceRecorded` | `LABOR_PERFORMANCE_MODE=kafka-cache` | `internal/adapters/outbound/laborperformancecache` |

Both cache consumers decode with the SDK, dispatch on the full `type`, and log
at WARN and skip any message that is not a valid CloudEvents 1.0 event. The
labor-performance cache also dedupes on `id`, because its running mean is a sum.

Internally, the analytics projector (`cmd/workforce-projector`) consumes this
service's own `warehouse.workforce.analytics` topic under the group
`workforce-analytics`. It dedupes on the CloudEvents `id` via the
`analytics_processed_events` table in the analytical database. See [Integration](../ecosystem/integration.md).
