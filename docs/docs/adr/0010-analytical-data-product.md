---
id: 0010-analytical-data-product
title: 10. Per-service analytical data product (report) via a separate analytics topic
sidebar_label: 10. Analytical data product
sidebar_position: 10
description: An analytical read model (the "report") built from this service's own domain events on a dedicated warehouse.workforce.analytics topic, projected into a separate analytical database and served by a read-only reports binary over REST and MCP — a lightweight data mesh with no central data platform.
---

# 10. Per-service analytical data product (the "report")

## Status

**Accepted**; the analytics **Envelope v1** wrapper (`schema_version`) in §1 is
**superseded by [ADR-0026](./0026-cloudevents-mandatory-event-envelope.md)** —
the analytics topic now carries CloudEvents 1.0 events with
`dataschema` `urn:warehouse:workforce-management:analytics:<EventName>:v1`. The
rest of this record stands.

## Context

The warehouse-systems estate needs a per-service **report** that supports
analytics while each service stays the **OLTP** system of record for its own
bounded context. The requirement, stated deliberately simply: *follow data-mesh
principles, but without standing up a whole data platform.* No central
warehouse, no lake, no shared ETL team.

Workforce Management already has everything the analytical side needs as a
substrate:

- Past-tense **domain events** (`AssociateShiftStarted`, `AssociateShiftEnded`,
  `AssociateBreakStarted`, `AssociateBreakEnded`, `AssociateCertified`,
  `LaborAssigned`, `LaborReassigned`, `PathUnderstaffed`, `ShiftPlanProposed`,
  `ShiftPlanCommitted`) raised by the aggregates.
- A Kafka **integration** path (`warehouse.workforce.events`) already carrying
  `ShiftPlanCommitted` (one message per PathPlan line) with the CloudEvents-like
  envelope and OTel trace propagation established in
  [ADR-0004](./0004-kafka-integration-events-and-cloudevents-catalog.md).
- The dual inbound-adapter pattern (HTTP + MCP) from
  [ADR-0008](./0008-mcp-inbound-adapter.md).

So the event backbone exists; what is missing is the **analytical read side**.
The forces shaping the decision:

- **The integration contract must not become coupled to reporting.** The report
  needs many more event types than the integration topic exposes, and they
  change on a different cadence. Widening `warehouse.workforce.events` with
  analytics-only event types would risk surprising the existing
  `ShiftPlanCommitted` consumer and entangle two contracts that should evolve
  separately.
- **Analytics must never contend with OLTP.** A report query load, a long
  aggregation, or a projection rebuild must not touch the transactional database
  that serves `AssignLabor` and `CommitShiftPlan`.
- **The service still owns its data as a product.** Data-mesh domain ownership
  means the read side lives in this repo, owned by the same team, with a
  contract, an owner, and a freshness SLA — not shipped off to a central team.
- **No new central platform.** Reuse what the estate already runs: Kafka,
  Postgres, chi, the MCP SDK, the Helm chart.

## Decision

**Workforce Management owns an analytical data product built solely from its own
domain events, delivered on a dedicated analytics topic, projected into a
separate analytical database, and served read-only over REST and MCP. Three
processes; one writer.**

### 1. Separate analytics topic

A new outbound adapter publishes the domain event set to
**`warehouse.workforce.analytics`**, using the shared **Envelope v1** wrapper
(`event_id`, `event_type`, `occurred_at`, `source`, `schema_version`, `data`)
with a per-`event_type` snake_case `data` payload. The message key is the
aggregate id (`AssociateId` for associate-scoped events, `PathId` for
path-scoped events). The existing integration publisher and
`warehouse.workforce.events` are **left untouched**, so no existing consumer is
affected. The two publishers run as a fan-out selected by `EVENT_PUBLISHER=kafka`
in the OLTP composition root. Analytics consumers switch on `event_type` and
ignore unknown types.

### 2. Separate analytical database

Projections land in a **separate analytical database** with its own credentials
(`ANALYTICS_DATABASE_URL`), its own golang-migrate migration set
(`migrations/analytics`), and a **read-only role** for the reader. Baseline is a
dedicated `*_analytics` database in the existing Postgres release; the
`ANALYTICS_DATABASE_URL` seam allows promotion to a physically separate instance
later without code changes. The OLTP `DATABASE_URL` database is never opened by
the analytical side.

### 3. Three processes, one writer

- **`cmd/workforce`** — the OLTP binary. Unchanged, except its composition root
  additionally fans domain events out to the analytics topic when
  `EVENT_PUBLISHER=kafka`.
- **`cmd/workforce-projector`** — the analytics **writer**. Consumes
  `warehouse.workforce.analytics` (consumer group `workforce-analytics`, reading
  from the earliest offset), applies idempotent projections, and is the **only**
  writer of the analytical database. Runs the analytical migrations on start.
- **`cmd/workforce-reports`** — the **read-only reader**. Opens the analytical
  database with the read-only role and serves `GET /reports/labor` and
  `GET /reports/labor/freshness`. Never writes, never migrates.

### 4. Served over REST and MCP

The reports binary serves the REST report resource. A curated, read-only MCP
tool (`get_workforce_labor_report`) — following the intent-level tool discipline
of [ADR-0008](./0008-mcp-inbound-adapter.md) — calls the reports REST rather than
opening the analytical database itself, so no process touches a datastore it does
not own. It is registered only when `REPORTS_BASE_URL` is configured on the MCP
server.

### 5. The report

A **Labor Utilization & Staffing** read model, keyed per process path × UTC hour
bucket: shifts started/ended, break count and average break duration,
certifications, labor assigned/reassigned counts, and understaffing events.
Associate-scoped events do not carry a path — this context deliberately
[stops at the path boundary](./0002-stop-at-the-path-boundary.md) — so their
metrics aggregate under the empty path id `""` (a building-wide bucket). Break
duration is derived from paired `AssociateBreakStarted`/`AssociateBreakEnded`
events; where a pair is unavailable, only the count contributes. It is a
**projection** from events (consistent with the existing "read models are
projections, not aggregate state" rule), eventually consistent to a freshness
SLA (p95 event-to-report lag < 30s), not real-time.

The analytical read model lives in a new `internal/analytics/` region that
depends on nothing; the consumer and store adapters depend on it. The OLTP
**domain and application layers are not modified**, and `arch-test` enforces that
they do not import the analytics region or its store.

## Consequences

### Easier

- **The integration contract is untouched**, so widening what analytics consumes
  never risks the `ShiftPlanCommitted` integration consumer. Analytics retention
  is tuned independently of the integration topic.
- **Analytics cannot contend with OLTP** — separate database, separate
  connection, read-only reader role. A runaway report query cannot touch
  transactional throughput.
- **The report is rebuilt purely from events** — no dual-write from OLTP, so the
  transactional write path gains no new failure mode. The read model can be
  rebuilt from scratch by replaying the topic from the earliest offset.
- **No central platform.** Everything reuses the estate's existing Kafka,
  Postgres, chi, MCP SDK and Helm.
- **Least privilege by construction.** The read-only DB role plus a read-only
  pool (`default_transaction_read_only=on`) make "a report can never corrupt the
  analytical store" a hard guarantee, not a convention.

### Harder

- **One more topic, two more binaries, and a second database** to operate.
  Mitigated by reusing the OLTP Postgres pattern and the existing
  consumer/publisher scaffolding.
- **Eventual consistency.** The report lags the OLTP truth by the freshness SLA;
  it is not a real-time view. This is the correct data-mesh tradeoff but must be
  communicated to report consumers.
- **The analytics publisher is a second producer path** for the same domain
  events. It re-serializes them under Envelope v1 for the analytics topic; the
  event set it publishes must be kept in step with the report's inputs.
- **First deploy has an empty report** until events flow; historical backfill
  requires replaying `warehouse.workforce.analytics` from earliest into a fresh
  projector, so Kafka retention must cover the desired backfill window.

## References

- Envelope v1 contract and analytics governance charter: cross-service infra repo
  (`warehouse-infra/docs/analytics/`).
- [ADR-0002 — Stop at the path boundary](./0002-stop-at-the-path-boundary.md)
- [ADR-0004 — Kafka integration events and CloudEvents catalog](./0004-kafka-integration-events-and-cloudevents-catalog.md)
- [ADR-0008 — MCP inbound adapter](./0008-mcp-inbound-adapter.md)
- Report contract: [Labor Utilization & Staffing Report](../analytics/labor-report.md)
