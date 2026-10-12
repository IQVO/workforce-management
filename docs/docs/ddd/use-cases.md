---
id: use-cases
title: Application use cases
sidebar_label: Use cases
description: Every use case in internal/application/usecases. For each one, what triggers it, its inputs, the invariants it checks, and the events it raises.
---

# Application use cases

The application layer (`internal/application/usecases`) has **nine** use
cases. Each is one struct that depends only on domain packages and
`internal/application/ports`. Every use case that publishes wraps its saves
and its `Publish` in the shared `UnitOfWork` (`atomically(...)`), so with
Postgres and `EVENT_PUBLISHER=kafka` the state change and its `outbox_events`
rows commit together ([ADR 0016](../adr/0016-transactional-outbox.md)).

None is triggered by Kafka or a scheduler. The only inbound Kafka consumer
(the analytics projector) writes the read model and calls no use case. The
triggers are REST (`cmd/workforce`) and MCP (`cmd/mcp`).

All event types share the prefix `com.warehouse.wes.workforce-management.`
(`internal/adapters/kafka/cloudevents/types.go`). Each event goes to both
`warehouse.workforce.events` and `warehouse.workforce.analytics` (see
[Events](../api-reference/events.md)).

## Summary

| Use case | Trigger | Aggregate | Events raised |
|---|---|---|---|
| `StartAssociateShift` | `POST /associates/{id}/start-shift` | `AssociateShift` | `associate.AssociateShiftStarted` |
| `CertifyAssociate` | `POST /associates/{id}/certifications` | `AssociateShift` | `associate.AssociateCertified` |
| `StartBreak` | `POST /associates/{id}/break/start` | `AssociateShift` | `associate.AssociateBreakStarted` |
| `EndBreak` | `POST /associates/{id}/break/end` | `AssociateShift` | `associate.AssociateBreakEnded` |
| `AssignLabor` | `POST /associates/{id}/assignments` (Idempotency-Key); MCP `assign_labor` | `LaborAssignment` (+ `AssociateShift` hours) | `assignment.LaborAssigned`, plus `assignment.LaborReassigned` when moving paths |
| `EndAssociateShift` | `POST /associates/{id}/end-shift` | `AssociateShift` (+ `LaborAssignment`) | `associate.AssociateShiftEnded` |
| `ProposePathPlan` | `POST /paths/{pathId}/plan/propose`; MCP `propose_path_heads` | none (pure computation) | `shiftplan.ShiftPlanProposed` |
| `CommitShiftPlan` | `POST /shift-plans` (Idempotency-Key) | `ShiftPlan` | `shiftplan.ShiftPlanCommitted` |
| `GetStaffingGap` | `GET /paths/{pathId}/staffing-gap`, `GET /sites/{siteCode}/shifts/{shiftId}/staffing-gap`, deprecated `GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap`; MCP `get_staffing_gap` | `ShiftPlan` (read) | `shiftplan.PathUnderstaffed`, once per understaffed path |

The fourth MCP tool, `get_workforce_labor_report`, calls no use case. It is a
REST client of `cmd/workforce-reports` (see
[Runbook](../operations/runbook.md#analytics-reports-api-cmdworkforce-reports)).
HTTP status codes for the errors below are listed in [Errors](../api-reference/errors.md)
and [Troubleshooting](../operations/troubleshooting.md#requests).

## Associate shift lifecycle

### `StartAssociateShift`

- **Inputs:** associate id, the list of certifications, an optional `siteCode` (`ExecuteAtSite`, [ADR 0034](../adr/0034-site-scoped-staffing-gap.md)). The site code is accepted as given. Nothing checks it against facility-layout.
- **Behaviour:** idempotent by associate id. A second call **replaces** the roster entry (certifications and site), carrying over the stored row's `version` so the version-guarded save ([ADR 0021](../adr/0021-optimistic-concurrency-version-column.md)) accepts it.
- **Invariants:** non-empty associate id. Any repository error other than "not found" fails the call.
- **Raises:** `AssociateShiftStarted`.

### `CertifyAssociate`

- **Inputs:** associate id, certification (non-empty).
- **Invariants:** the associate exists (`resource-not-found`) and the shift has not ended (`ErrShiftEnded`).
- **Raises:** `AssociateCertified`.

### `StartBreak` / `EndBreak`

- **Inputs:** associate id.
- **Invariants:** the shift has not ended. `StartBreak` rejects an associate already on break (`ErrAlreadyOnBreak`). `EndBreak` rejects one not on break (`ErrNotOnBreak`).
- **Raises:** `AssociateBreakStarted` / `AssociateBreakEnded`.

### `EndAssociateShift`

- **Inputs:** associate id. Uses `MAX_HOURS_PER_SHIFT`.
- **Behaviour:** if the associate has an active assignment, it is ended first and its hours are logged against the shift. Then the shift is closed. Ending an already-ended shift is a no-op that raises nothing.
- **Invariants:** the associate exists. Logging the closed assignment's hours must not exceed `MAX_HOURS_PER_SHIFT` (`ErrMaxHoursExceeded`).
- **Raises:** `AssociateShiftEnded`. Closing the assignment raises no assignment event.

## Labor assignment

### `AssignLabor`

- **Inputs:** associate id, path id. Uses `MAX_HOURS_PER_SHIFT` and the process-path catalogue.
- **Invariants, in order:**
  1. The associate exists.
  2. `CanBeAssigned`: the shift has not ended (`ErrShiftEnded`) and the associate is not on break (`ErrOnBreak`).
  3. **Certification gate** ([ADR 0003](../adr/0003-certification-gated-single-active-assignment.md), [ADR 0009](../adr/0009-hazmat-certification-via-existing-path-gating.md)): the associate must hold the certification named after the path's catalogue family (`MatchPrefix`, so `PICK`, `pick` and `pick-zone-a` all require `pick`). Without a catalogue, the raw path id is the certification name (`ErrCertificationRequired`).
  4. **Single active assignment:** an existing active assignment is closed and moved into history. The associate is never double-booked, and a second call is never rejected for it.
  5. The closed interval's hours must fit under `MAX_HOURS_PER_SHIFT` (`ErrMaxHoursExceeded`).
- **Persists:** the `LaborAssignment`, plus the `AssociateShift` when hours were logged. Both are saved in one unit of work.
- **Raises:** `LaborAssigned`, or `LaborReassigned` (from path → to path) when an active assignment on another path was closed.
- **Metrics:** every attempt adds one to `workforce.labor_assignments` with `outcome` and, on rejection, `reason` ([Observability](../operations/observability.md)).
- **REST only:** `RequireIdempotencyKey` guards the route ([ADR 0027](../adr/0027-idempotency-key-middleware.md)).

## Shift planning

### `ProposePathPlan`

- **Inputs:** building/site id, path id, `charge` (units to clear), optional `plannedRate` (units per head per hour).
- **Behaviour** ([ADR 0012](../adr/0012-measured-rate-feed-for-propose-path-plan.md), [ADR 0020](../adr/0020-idle-share-staffing-signal.md)):
  - When `plannedRate` is at most 0 and a `MeasuredRateClient` is wired, the rate becomes `3600 / meanActualSeconds` from labor-performance and `rateSource` is `measured`. Otherwise `rateSource` is `caller`. When the measured rate is unavailable, the rate stays at 0 and 0 heads are proposed (fail-open).
  - `heads = shiftplan.ProposedHeads(charge, rate)`.
  - When an `IdleShareClient` is wired (only in `LABOR_PERFORMANCE_MODE=kafka-cache`) and the idle share is above `IDLE_SHARE_TRIM_THRESHOLD` (0.30), heads are trimmed to `ceil(heads × (1 − share))`, never below 1. The response then carries a `trimReason`.
- **Persists:** nothing. A human still has to commit the plan.
- **Raises:** `ShiftPlanProposed` (building, path, heads, resolved rate).
- **MCP difference:** `cmd/mcp` wires this use case with **no** measured-rate or idle-share client, so `propose_path_heads` always uses the caller's rate and never trims.

### `CommitShiftPlan`

- **Inputs:** building/site id, shift id, plan lines (path, planned heads, planned rate, planned hours), `installedStations` per path (supplied by the caller). Uses `MAX_HOURS_PER_SHIFT`.
- **Invariants:**
  1. A catalogue is wired (`ErrCommitShiftPlanNoCatalogue` otherwise).
  2. Every line's path is in the catalogue (`unknown-path-id`).
  3. **Live installed-capacity ceiling** ([ADR 0014](../adr/0014-installed-capacity-ceiling.md)): for each path, the use case looks up the catalogue's `requiredCapabilities`, asks fulfillment-execution how many stations have each capability (once per capability per commit), and takes the minimum. A path with no capabilities gets ceiling 0. Any `ErrInstalledCapacityUnavailable` fails the **whole** commit with 503. In `permissive` mode, every commit fails.
  4. Domain checks in `shiftplan.CommitShiftPlan`: at least one line (`ErrNoPathPlans`), installed stations given for every path (`ErrMissingInstalledStations`), planned heads within installed stations (`ErrPlannedHeadsExceedInstalled`), within the live ceiling (`ErrExceedsInstalledCapacity`), and planned hours within heads × `MAX_HOURS_PER_SHIFT` (`ErrPlannedHoursExceedCapacity`).
- **Persists:** the `ShiftPlan`. The integration publisher re-reads the saved plan while encoding, so `Publish` has to run inside the same unit of work.
- **Raises:** `ShiftPlanCommitted`.
- **REST only:** `RequireIdempotencyKey`. There is no MCP tool for commits.

### `GetStaffingGap`

- **Inputs:** building/site id, shift id, and either one path (`Execute` / `ExecuteForSite`) or every line of the plan (`ExecuteAll` / `ExecuteAllForSite`, [ADR 0029](../adr/0029-all-paths-staffing-gap-endpoint.md)). An optional `siteCode` scopes the active-head count to associates whose active shift is at that site ([ADR 0034](../adr/0034-site-scoped-staffing-gap.md), [ADR 0035](../adr/0035-sitecode-converges-building-id.md)). Without it, the count covers every site.
- **Computation per line:** `plannedHeads` comes from the committed `ShiftPlan`, `activeHeads` from `CountActiveByPath` or `CountActiveByPathAtSite`, and `understaffed = activeHeads < plannedHeads`. `observedIdlePct` is filled only when an idle-share client is wired. Otherwise it is null, never 0.
- **Invariants:** a committed plan exists for building + shift (`resource-not-found`). `siteCode` and the deprecated `buildingId` must agree (`conflicting-site-and-building`).
- **Raises:** `PathUnderstaffed` (path, planned, active, site) for each understaffed line. The list variants publish every event in **one** unit of work. Note that this read raises events: every query of an understaffed path publishes again.

## Ports used

| Port | Implemented by |
|---|---|
| `AssociateRepo`, `ShiftPlanRepo`, `AssignmentRepo` | `internal/adapters/outbound/postgres`; `internal/adapters/outbound/memory` (in-memory MCP and tests) |
| `EventPublisher` | `events.LogPublisher`; `postgres.OutboxPublisher` (kafka + Postgres); `events.MultiPublisher` over the Kafka publishers (kafka, no Postgres) |
| `UnitOfWork` | `postgres.UnitOfWork`. When nil, saves and publish run back to back. |
| `PathCatalogue` | `filecatalog`, `kafkacatalog` |
| `MeasuredRateClient`, `IdleShareClient` | `laborperformance` (permissive or HTTP + breaker); `laborperformancecache` (kafka-cache, both ports) |
| `InstalledCapacityClient` | `fulfillmentexecution` (permissive fail-loud, or HTTP + breaker) |
| `LaborMetrics` | `telemetry.LaborMetrics` |
| `Clock` | `clock.System` |
