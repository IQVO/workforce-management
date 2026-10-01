---
id: labor-report
title: Labor Utilization & Staffing Report
sidebar_label: Labor report
description: The workforce-management analytical data product — a Labor Utilization & Staffing read model built from the service's own domain events, served read-only over REST and MCP. Contract, grain, inputs, freshness SLA, and versioning.
---

# Labor Utilization & Staffing Report

The analytical **data product** owned by Workforce Management. It is built
entirely from this service's own domain events (never another service's
database) and served read-only. See [ADR-0010](../adr/0010-analytical-data-product.md)
for the decision, and [ADR-0026](../adr/0026-cloudevents-mandatory-event-envelope.md)
for the CloudEvents 1.0 envelope every analytics event uses.

## Name & owner

- **Report:** Labor Utilization & Staffing.
- **Owner:** the Workforce Management service/team (the same team that owns the
  OLTP write model).

## Grain

One row per **(path × hour bucket)**, where `hourBucket` is the UTC hour the row
aggregates. Associate-scoped events do not carry a path — this context
[stops at the path boundary](../adr/0002-stop-at-the-path-boundary.md) — so their
metrics land under the **empty `pathId` `""`** (a building-wide bucket).
Path-scoped events land under their `pathId`.

Metrics per row:

| Metric | Meaning |
|---|---|
| `shiftsStarted` | Count of `AssociateShiftStarted` in the bucket (empty `pathId`). |
| `shiftsEnded` | Count of `AssociateShiftEnded` in the bucket (empty `pathId`). |
| `breaks` | Count of `AssociateBreakStarted` in the bucket (empty `pathId`). |
| `avgBreakSeconds` | Mean seconds from a break's start to its end, over breaks whose paired end was observed. Zero when no break in the bucket had a paired end. |
| `certifications` | Count of `AssociateCertified` in the bucket (empty `pathId`). |
| `laborAssigned` | Count of `LaborAssigned` to this path in the bucket. |
| `laborReassigned` | Count of `LaborReassigned` into this path in the bucket. |
| `understaffingEvents` | Count of `PathUnderstaffed` for this path in the bucket. |

## Inputs (analytics topic events)

Consumed from **`warehouse.workforce.analytics`** (the dedicated analytics topic,
separate from the integration topic). Every message is a CloudEvents 1.0 event
with `dataschema` `urn:warehouse:workforce-management:analytics:<EventName>:v1`;
the projector dispatches on the full `type`
(`com.warehouse.wes.workforce-management.<entity>.<EventName>`, entity
`associate`, `assignment` or `shiftplan`), uses `time` as the occurred-at
instant, dedupes on `id`, and dead-letters anything that fails CloudEvents
validation to `warehouse.workforce.analytics.dlq`.

| Event (`type` suffix) | Contributes |
|---|---|
| `AssociateShiftStarted` | `shiftsStarted` |
| `AssociateShiftEnded` | `shiftsEnded` |
| `AssociateBreakStarted` | `breaks`, break start timestamp (for duration) |
| `AssociateBreakEnded` | `avgBreakSeconds` (paired with the start) |
| `AssociateCertified` | `certifications` |
| `LaborAssigned` | `laborAssigned` |
| `LaborReassigned` | `laborReassigned` (into `to_path_id`) |
| `PathUnderstaffed` | `understaffingEvents` |

`ShiftPlanProposed` and `ShiftPlanCommitted` are published to the topic but do
not move this report; the projector acknowledges them without projecting.

## Interface

### REST (served by `cmd/workforce-reports`, read-only)

```
GET /reports/labor?from=<RFC3339>&to=<RFC3339>&pathId=&granularity=hour
GET /reports/labor/freshness
GET /healthz
```

- `from`, `to` — **required**, RFC3339, `[from, to)` compared against `hourBucket`.
- `pathId` — optional exact-match filter (`""` for the building-wide bucket).
- `granularity` — optional, defaults to `hour`.

Response (`200`):

```json
{
  "rows": [
    {
      "pathId": "pack",
      "hourBucket": "2026-08-26T14:00:00Z",
      "shiftsStarted": 0,
      "shiftsEnded": 0,
      "breaks": 0,
      "avgBreakSeconds": 0,
      "certifications": 0,
      "laborAssigned": 12,
      "laborReassigned": 3,
      "understaffingEvents": 1
    }
  ]
}
```

Freshness (`200`):

```json
{ "lagSeconds": 4.2 }
```

Errors use RFC 7807 `application/problem+json`, consistent with the OLTP API
([ADR-0005](../adr/0005-rfc-7807-problem-details.md)).

### MCP (curated, read-only)

Tool **`get_workforce_labor_report`** — same filters as the REST endpoint; it
calls the reports REST rather than opening the analytical database. Exposed by
the existing `cmd/mcp` server (Streamable HTTP) when `REPORTS_BASE_URL` is set,
consistent with [ADR-0008](../adr/0008-mcp-inbound-adapter.md).

## Freshness SLA

- **Definition:** `lagSeconds` = now − age of the most recently applied event.
- **Target:** p95 event-to-report lag **< 30s** under normal load.
- **Exposed:** `GET /reports/labor/freshness`.
- Breaching the SLA is an operational signal (projector lag / consumer down), not
  a correctness bug — the report catches up when the projector does.

## Versioning

- Additive fields (new optional row metric, new query filter) are non-breaking.
- A breaking change to a row's shape or meaning is a new endpoint/tool version.
- The analytics event contract versions independently via the CloudEvents
  `dataschema` (`...:analytics:<EventName>:v<N>`); a breaking payload change
  is a new `.v2` type with a new dataschema version.

## Runbook notes

- **Two processes, one writer.** `cmd/workforce-projector` is the only writer of
  the analytical DB; `cmd/workforce-reports` connects read-only. The OLTP
  `cmd/workforce` never opens the analytical DB.
- **Empty on first deploy.** The report is empty until events flow. To backfill
  history, replay `warehouse.workforce.analytics` from earliest into a fresh
  projector (consumer group `workforce-analytics` starts at the earliest offset);
  Kafka retention must cover the desired window.
- **Eventual consistency.** The report is a projection, not a real-time view; it
  meets the freshness SLA, not transactional consistency.
