---
id: entity-relationship
title: Entity-relationship diagram
sidebar_label: Entity-relationship
sidebar_position: 14
description: ER diagrams of the OLTP schema (migrations/*.up.sql) and the analytics schema (migrations/analytics), with the table-to-aggregate mapping.
---

# Entity-relationship diagram

Two separate databases, two diagrams. Both show the **final** schema after
every migration is applied in order. A relationship line is drawn only where
a real `FOREIGN KEY` exists.

## OLTP database (`DATABASE_URL`)

Migrations `000001_init` → `000002_outbox` → `000003_version` →
`000004_idempotency_keys` → `000005_associate_shift_site_code`, applied by `cmd/workforce` (and `cmd/mcp`) on boot
through `MIGRATIONS_DATABASE_URL` ([ADR 0025](../adr/0025-migrations-direct-postgres-connection.md)).

```mermaid
erDiagram
    shift_plan {
        TEXT building_id PK "holds the Site code (siteCode); legacy column name kept, ADR 0035"
        TEXT shift_id PK
    }
    path_plan {
        TEXT building_id PK,FK "the Site code (siteCode)"
        TEXT shift_id PK,FK
        TEXT path_id PK
        INTEGER planned_heads
        DOUBLE_PRECISION planned_rate
        DOUBLE_PRECISION planned_hours
    }
    associate_shift {
        TEXT associate_id PK
        TEXT_ARRAY certifications
        BOOLEAN on_break
        DOUBLE_PRECISION hours_logged
        BOOLEAN ended
        INTEGER version
        TEXT site_code "nullable, ADR 0034"
    }
    labor_assignment {
        TEXT associate_id PK
        TEXT active_path_id
        TIMESTAMPTZ active_start
        INTEGER version
    }
    labor_assignment_history {
        BIGSERIAL id PK
        TEXT associate_id FK
        TEXT path_id
        TIMESTAMPTZ interval_start
        TIMESTAMPTZ interval_end
    }
    domain_event {
        BIGSERIAL id PK
        TEXT event_name
        TIMESTAMPTZ occurred_at
        JSONB payload
    }
    outbox_events {
        BIGSERIAL id PK
        TEXT topic
        TEXT event_type
        BYTEA key
        BYTEA value
        JSONB headers
        TIMESTAMPTZ created_at
        TIMESTAMPTZ published_at
        INTEGER attempts
        TEXT last_error
    }
    idempotency_keys {
        TEXT key PK
        TEXT method
        TEXT path
        TEXT request_hash
        INTEGER status_code
        BYTEA response_body
        JSONB response_headers
        TIMESTAMPTZ created_at
        TIMESTAMPTZ completed_at
    }
    shift_plan ||--|{ path_plan : "lines, ON DELETE CASCADE"
    labor_assignment ||--o{ labor_assignment_history : "closed intervals, ON DELETE CASCADE"
```

Source: `migrations/000001_init.up.sql`, `000002_outbox.up.sql`,
`000003_version.up.sql`, `000004_idempotency_keys.up.sql`,
`000005_associate_shift_site_code.up.sql`. Omits: the
indexes (`idx_labor_assignment_active_path`,
`idx_associate_shift_site_code` (partial, non-NULL `site_code` only),
`idx_outbox_events_unpublished`, `idx_outbox_events_published_at`,
`idx_idempotency_keys_created_at`), column defaults, and golang-migrate's own
`schema_migrations` table. Types with spaces or brackets are written with
`_` (`DOUBLE_PRECISION`, `TEXT_ARRAY` for `TEXT[]`).

### Logical links without a foreign key

- `labor_assignment.associate_id` and `associate_shift.associate_id` hold the
  same `AssociateId` but are **not** linked: they are two aggregates, and
  `AssignLabor` / `EndAssociateShift` coordinate them in the application
  layer, not through the schema. The **site-scoped staffing gap** (ADR 0034)
  joins them by value at query time — `labor_assignment` ⨝ `associate_shift`
  on `associate_id`, `site_code = :siteCode AND ended = FALSE` — a read-side
  join, still not a constraint. `site_code` is `NULL` for every legacy row and
  for shifts started without a site; `NULL` matches no site, so those rows
  count only in unscoped queries. The code is accepted as given (no lookup in
  facility-layout).
- `path_plan.path_id`, `labor_assignment.active_path_id` and
  `labor_assignment_history.path_id` are process-path ids validated against
  the process-path catalogue at the adapter edge; the catalogue is not a
  table here.
- `outbox_events` and `idempotency_keys` reference nothing: they are written
  in the same transaction as the aggregate rows, which is the only coupling.

## Analytics database (`ANALYTICS_DATABASE_URL`)

Migration `migrations/analytics/0001_report.up.sql`, applied by
`cmd/workforce-projector`; read-only for `cmd/workforce-reports`.

```mermaid
erDiagram
    analytics_processed_events {
        TEXT event_id PK
        TIMESTAMPTZ occurred_at
        TIMESTAMPTZ applied_at
    }
    analytics_consumed_events {
        TEXT event_id PK
        TIMESTAMPTZ processed_at
    }
    analytics_pending_breaks {
        TEXT associate_id PK
        TIMESTAMPTZ started_at
    }
    labor_rollup {
        TEXT path_id PK
        TIMESTAMPTZ hour_bucket PK
        BIGINT shifts_started
        BIGINT shifts_ended
        BIGINT breaks
        BIGINT certifications
        BIGINT labor_assigned
        BIGINT labor_reassigned
        BIGINT understaffing_events
        DOUBLE_PRECISION break_seconds
        BIGINT breaks_with_duration
    }
```

Source: `migrations/analytics/0001_report.up.sql`. Omits: the two indexes.
There are no foreign keys in this schema at all.

## Table ≠ aggregate

| Table | Kind | Aggregate / role |
| --- | --- | --- |
| `shift_plan` + `path_plan` | aggregate state | `ShiftPlan` with its `PathPlan` lines; `Save` deletes and re-inserts the lines. The key column `building_id` stores the **Site code** — `siteCode` is the canonical name and `buildingId` its deprecated alias, one value, one column, **no migration** ([ADR 0035](../adr/0035-sitecode-converges-building-id.md)) |
| `associate_shift` | aggregate state | `AssociateShift` (certifications as a `TEXT[]`, not a child table; optional canonical `site_code`, [ADR 0034](../adr/0034-site-scoped-staffing-gap.md)) |
| `labor_assignment` + `labor_assignment_history` | aggregate state | `LaborAssignment`: active interval in the parent row, closed `Interval`s in the child |
| `domain_event` | legacy, unused; retained | Decided 2026-10-06: **keep** — created by `000001_init`, no production code reads or writes it; retained because migrations are additive only (dropping a table is destructive and needs explicit approval) |
| `outbox_events` | infrastructure | transactional outbox, one row per encoded Kafka message on either topic ([ADR 0016](../adr/0016-transactional-outbox.md)) |
| `idempotency_keys` | infrastructure | `Idempotency-Key` middleware store ([ADR 0027](../adr/0027-idempotency-key-middleware.md)), swept by housekeeping ([ADR 0028](../adr/0028-housekeeping-sweeper-idempotency-keys-and-outbox.md)) |
| `schema_migrations` | infrastructure | golang-migrate bookkeeping |
| `analytics_processed_events` | projection idempotency + freshness | analytics read side ([ADR 0010](../adr/0010-analytical-data-product.md)) |
| `analytics_consumed_events` | consumer dedupe (`ports.ProcessedEvents`) | analytics read side |
| `analytics_pending_breaks` | projection working state | analytics read side |
| `labor_rollup` | projection (fact table) | analytics read side, served by `GET /reports/labor` |

The `version` columns on `associate_shift` and `labor_assignment` are
optimistic-concurrency metadata ([ADR 0021](../adr/0021-optimistic-concurrency-version-column.md));
`shift_plan` deliberately has none.
