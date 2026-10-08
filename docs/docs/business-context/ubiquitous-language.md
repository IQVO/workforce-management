---
id: ubiquitous-language
title: Ubiquitous language
sidebar_label: Ubiquitous language
sidebar_position: 5
description: The exact vocabulary of this bounded context, with the definitions the code implements.
---

# Ubiquitous language

These are the exact names used in the domain model, the API, the events and
this documentation. Synonyms are not accepted: there is no "worker," no
"employee," no "job," no "workstation assignment."

## Core terms

| Term | Definition |
| --- | --- |
| **ShiftPlan** | The committed split of headcount across paths for one shift. **One per site per shift** (keyed by `siteCode` + `shiftId`; `buildingId` is the deprecated alias of `siteCode`, [ADR 0035](../adr/0035-sitecode-converges-building-id.md)). Contains `PathPlan` lines. Committed by a human — the software proposes, a human commits. |
| **PathPlan** | One line of a `ShiftPlan`: `pathId`, `plannedHeads`, `plannedRate`, `plannedHours`. |
| **AssociateShift** | Who is on, their certifications, their breaks, their logged hours. Owned here. No sibling reads or writes it today: certifications are not exposed by any REST route, MCP tool or event payload on the integration topic (`AssociateShiftStarted` / `AssociateCertified` go only to the internal analytics topic). |
| **LaborAssignment** | One associate on one path for an interval. Exactly one ACTIVE assignment per associate at a time. Must satisfy the path's certification requirement, or it is rejected. |
| **Certification** | A named qualification — `pack`, `hazmat`, `pick`. An associate untrained on a path cannot be assigned to it. Training is itself a path that consumes hours; it is not special-cased, because the gate lives on assignment. `hazmat` is a real certification value already gated by the ordinary path-name-equals-certification-name convention (a path named `hazmat` requires certification `hazmat`) — see [ADR 0009](../adr/0009-hazmat-certification-via-existing-path-gating.md). `fulfillment-execution` enforces the equivalent station-capability half of hazmat handling independently, via its own mechanism. |
| **PathUnderstaffed** | A **flag, not a decision**: `plannedHeads(path)` is not currently met by active assignments. Surfacing the gap is this context's job; moving people is a human call, recorded via `AssignLabor`. |
| **Process path** | A named station type that owns a queue — `pack`, `pick`, `stow`, `SLAM`. Not a workflow step. The finest granularity this context addresses. |
| **Charge** | The volume that must clear on a path. Input to `ProposePathPlan`; never stored here. Owned upstream by `wes-work-planning`. |
| **Planned rate** | Expected throughput per head per hour on a path. Input to the proposal arithmetic. |
| **Installed stations** | How many physical positions a path has. Supplied by the caller on `CommitShiftPlan`; one of two ceilings on `plannedHeads`. |
| **Installed capacity** | The **live** count of stations able to serve a path, read from `fulfillment-execution` per capability at commit time; the second, independent ceiling on `plannedHeads` ([ADR 0014](../adr/0014-installed-capacity-ceiling.md)). |
| **Capability** | A physical station capability as `fulfillment-execution` records it (`pick`, `pack`, `rebin`, `slam`). Deliberately a different vocabulary from a path id; a path is resolved to its capabilities through the process-path catalogue, never by casting. |
| **Process-path catalogue** | The fleet's declared process paths, each with a `MatchPrefix` family and `RequiredCapabilities`. A path id belongs to a family when it equals the prefix or starts with prefix + `-` (case-insensitive, longest prefix wins). Loaded from a file or from `process-path-management`'s topic ([ADR 0013](../adr/0013-process-path-catalogue-validation.md), [ADR 0030](../adr/0030-kafka-sourced-process-path-catalogue.md)). |
| **Interval** | One stretch of time an associate spent on one path; open while active, closed into the assignment's history on reassignment or shift end. Its hours are logged on the `AssociateShift`. |
| **Staffing gap** | Planned heads versus active heads for a path within a committed `ShiftPlan`, plus the optional observed idle share. A read model, not stored. Fleet-wide when only the legacy `buildingId` is given; with the canonical `siteCode` (plan key **and** scope) the active heads count only associates with an active shift at that site ([ADR 0034](../adr/0034-site-scoped-staffing-gap.md), [ADR 0035](../adr/0035-sitecode-converges-building-id.md)). |
| **Site code** (**Site**) | The canonical Site identifier of the fleet — the facility-layout Site code (e.g. `WH1`), the same value warehouse-planning carries as `site_id`. **Site is the term; "building" is deprecated.** It is the canonical name (`siteCode` / `site_code`) of the `ShiftPlan` key and, recorded on an `AssociateShift` at start, the scope of the staffing gap; accepted as given (no lookup), absent for legacy associate rows. `buildingId` / `building_id` remain only as a deprecated alias with the same value and the same stored column; removing it needs a later breaking ADR ([ADR 0034](../adr/0034-site-scoped-staffing-gap.md), [ADR 0035](../adr/0035-sitecode-converges-building-id.md)). |
| **Measured rate** | `labor-performance`'s measured mean task duration (`meanActualSeconds`), converted to a per-head hourly rate (`3600 / seconds`) and used by `ProposePathPlan` only when the caller gives no `plannedRate` ([ADR 0012](../adr/0012-measured-rate-feed-for-propose-path-plan.md), [ADR 0033](../adr/0033-docs-audit-corrections-2026-10.md)). **Verified 2026-10-06:** one work unit = one task = one unit of charge, so `3600 / MeanActualSeconds` is correct and needs no unit factor — in wes-work-planning a `WorkUnit` carries no quantity, one `WorkReleased` makes fulfillment-execution create exactly one `Task` (its Kafka consumer calls `CreateTask` once), and the charge forecast's quantity counts those units. ADR 0033 stays untouched. |
| **Idle share** | The fraction (0–1) of a task type's clocked time that was idle, from `labor-performance`'s events. Above `IDLE_SHARE_TRIM_THRESHOLD` (default 0.30) it trims a proposal ([ADR 0020](../adr/0020-idle-share-staffing-signal.md)). |
| **Max hours per shift** | `MAX_HOURS_PER_SHIFT` (default 8): caps logged hours per associate and `plannedHours` per head. |
| **Direct vs indirect hours** | Direct hours are spent on a production path; indirect hours are everything else (training, breaks, meetings). Both consume the shift's hour budget. |
| **Break** | A logged, explicitly-started and explicitly-ended interval during which an associate cannot be assigned. |

## Terms this context deliberately does not have

| Absent term | Owned by | Why not here |
| --- | --- | --- |
| **Task** | `fulfillment-execution` | This context stops at the path boundary. See [The path boundary](./path-boundary.md). |
| **Station** (as an occupiable position) | `fulfillment-execution` | Here, `installedStations` is only a **count** used as a capacity ceiling — never an entity with an occupant. |
| **Work unit / release** | `wes-work-planning` | What work exists and when it is released is a different context entirely. |
| **Bin, SKU, reservation** | `inventory-storage` | Stock truth. |
| **Handling classification, product dimensions** | `product-master` | SKU product master data (Hazmat, TemperatureSensitive, declared vs measured size and weight). |
| **Zone, aisle, location code** | `facility-layout` | Physical geography. |

## Same word, different model

`ShiftPlan` exists in **both** this context and `wes-work-planning`, and they
are **different models**. This is the classic DDD "same term, different bounded
context" situation, and the platform handles it explicitly rather than by
sharing a type:

- **Here**, `ShiftPlan` is the labor commitment — it is the authoritative record
  that a human committed *these heads to these paths*.
- **In `wes-work-planning`**, `ShiftPlan` is that service's own planning
  artefact, derived from charge and CPT.

When `wes-work-planning` consumes this context's `ShiftPlanCommitted` event, it
explicitly does **not** feed it into its own `ShiftPlan` aggregate. It projects
it into a separate read model called `LaborPlanObserved`, keyed by `path_id`.
Conflating the two would be exactly the trap the platform DDD reference warns
about:

> Same English word, two different models. Do not share the class across
> contexts.

## Ubiquitous language in the code

Every term above appears verbatim as a Go identifier. The mapping is
one-to-one:

| Term | Go |
| --- | --- |
| ShiftPlan / PathPlan | `internal/domain/shiftplan.ShiftPlan`, `.PathPlan` |
| AssociateShift | `internal/domain/associate.AssociateShift` |
| LaborAssignment | `internal/domain/assignment.LaborAssignment` |
| Certification | `internal/domain/shared.Certification` |
| PathId / AssociateId | `internal/domain/shared.PathId`, `.AssociateId` |
| PathUnderstaffed | `internal/domain/shared.PathUnderstaffed` |
| Interval | `internal/domain/assignment.Interval` |
| Capability | `internal/domain/shared.Capability` |
| Process-path catalogue | `internal/domain/pathcatalog.Catalogue`, `.PathDefinition`; port `ports.PathCatalogue` |
| Installed stations | request field `installedStations` → `map[shared.PathId]int` argument of `shiftplan.CommitShiftPlan` |
| Installed capacity | `ports.InstalledCapacityClient.InstalledCapacity` |
| Staffing gap | `usecases.StaffingGap`, `usecases.GetStaffingGap` |
| Charge | `charge` argument of `shiftplan.ProposedHeads` / `ProposePathPlan.Execute` |
| Planned rate | `PathPlan.PlannedRate`, `plannedRate` argument of `ProposedHeads` |
| Measured rate | `ports.MeasuredRateClient.MeanActualSeconds` |
| Idle share | `ports.IdleShareClient.IdleSharePct` |
| Max hours per shift | `MaxHoursPerShift` field on the use cases, env `MAX_HOURS_PER_SHIFT` |
| Break | `AssociateShift.StartBreak` / `EndBreak`, field `onBreak` |

### Where the code name differs

| Term | Code | Note |
| --- | --- | --- |
| Measured rate | `MeanActualSeconds` | The code name says what it is: a **duration per task in seconds**, not a throughput per head per hour. `ProposePathPlan` converts it to a per-head hourly rate (`3600 / seconds`, one task = one unit of charge) before dividing the charge by it, so the `resolvedRate` it reports is in the same unit as a caller's `plannedRate` ([ADR 0033](../adr/0033-docs-audit-corrections-2026-10.md)). |
| Idle share | `IdleSharePct` | Despite the `Pct` suffix the value is a fraction in [0, 1], not 0–100. The REST field is `observedIdleShare` (same fraction); `observedIdlePct` is its deprecated alias with the same value ([ADR 0033](../adr/0033-docs-audit-corrections-2026-10.md)). |
| Installed stations / installed capacity | `ErrPlannedHeadsExceedInstalled` / `ErrExceedsInstalledCapacity` | Two similar error names for two different ceilings — the first is the caller's number, the second the live one. |
| Direct vs indirect hours | none | There is no code identifier: `AssociateShift.hoursLogged` accumulates only closed assignment intervals (direct hours). Breaks are tracked as a state, not as hours. |
| Associate on break | `onBreak` / `ErrOnBreak` | No `Break` entity exists; a break is a boolean on `AssociateShift`. |
