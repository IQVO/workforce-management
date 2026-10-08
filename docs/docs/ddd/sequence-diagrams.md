---
id: sequence-diagrams
title: Sequence diagrams
sidebar_label: Sequence diagrams
sidebar_position: 15
description: UML sequence diagrams for every command use case of workforce-management, traced from the use-case function bodies — idempotency middleware, unit of work, version checks and error branches included.
---

# Sequence diagrams

One diagram per command use case (the three single-aggregate associate
commands share one), plus the staffing-gap query because it publishes an
event. Each is traced from the `Execute` body in
`internal/application/usecases/` and the handler in
`internal/adapters/inbound/http/router.go`.

Shared facts every diagram relies on:

- **Unit of work.** Every use case wraps its `Save` calls and `Publish` in
  `atomically(ctx, UnitOfWork, fn)`. With Postgres wired, that is one
  transaction (`postgres.UnitOfWork`); if a transaction is already bound to
  the context — by the idempotency middleware — it joins that one instead.
  The middleware binds a savepoint inside its transaction and rolls back to
  it on any non-2xx response, so a late failure (a `409` on the second
  `Save`) cannot leave an earlier `Save` of the same request committed.
- **Outbox.** With `EVENT_PUBLISHER=kafka` and Postgres, `Publish` is
  `postgres.OutboxPublisher`: it encodes every event into CloudEvents rows for
  `warehouse.workforce.events` and `warehouse.workforce.analytics` and
  `INSERT`s them into `outbox_events` in the same transaction. The relay in
  `cmd/workforce` drains the table to Kafka later
  ([ADR 0016](../adr/0016-transactional-outbox.md)). With the default
  `EVENT_PUBLISHER=log`, events are only logged.
- **Version guard.** `AssociateRepo.Save` and `AssignmentRepo.Save` are
  `INSERT … ON CONFLICT DO UPDATE … WHERE version = loaded`; zero rows means
  `ports.ErrConcurrentModification` → `409`
  ([ADR 0021](../adr/0021-optimistic-concurrency-version-column.md)).
- **Errors** are mapped to RFC 7807 responses by `statusFor` in
  `internal/adapters/inbound/http/errors.go`.

## CommitShiftPlan — `POST /shift-plans`

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant IK as RequireIdempotencyKey
    participant H as http Handler
    participant UC as CommitShiftPlan
    participant Cat as PathCatalogue
    participant FE as InstalledCapacityClient
    participant SP as ShiftPlan
    participant Repo as ShiftPlanRepo
    participant OB as OutboxPublisher
    Client->>IK: POST /shift-plans, Idempotency-Key
    alt key missing
        IK-->>Client: 400 idempotency-key-required
    end
    IK->>IK: BEGIN, INSERT idempotency_keys ON CONFLICT DO NOTHING
    alt key already stored
        IK-->>Client: replay stored response, or 422 if body hash differs
    end
    IK->>H: request with the savepoint's tx bound to ctx
    H->>Cat: validatePathId for every line
    alt unknown path
        H-->>IK: 400 unknown-path-id
    end
    H->>H: ResolveSiteKey siteCode or deprecated buildingId, same value
    alt neither, or both with different values
        H-->>IK: 400 missing-building-id or 422 conflicting-site-and-building
    end
    H->>UC: Execute siteCode (stored as building_id), shiftId, lines, installedStations
    loop each distinct path
        UC->>Cat: Lookup pathId
        loop each required capability not yet fetched
            UC->>FE: InstalledCapacity capability
            alt unavailable
                FE-->>UC: ErrInstalledCapacityUnavailable
                UC-->>H: error, mapped to 503
            end
        end
    end
    UC->>SP: CommitShiftPlan with both ceilings and MaxHoursPerShift
    alt invariant broken
        SP-->>UC: ErrPlannedHeadsExceedInstalled, ErrExceedsInstalledCapacity, ErrPlannedHoursExceedCapacity, ErrNoPathPlans or ErrMissingInstalledStations
        UC-->>H: error, mapped to 409 or 400
    end
    SP-->>UC: ShiftPlan with ShiftPlanCommitted
    UC->>Repo: Save, joins the middleware tx
    UC->>OB: Publish ShiftPlanCommitted
    OB->>Repo: FindByBuildingAndShift to fan out one row per line
    OB->>OB: INSERT outbox_events, N integration rows and 1 analytics row
    UC-->>H: ShiftPlan
    H-->>IK: 201 Created, Location /shift-plans/siteCode/shiftId, body carries siteCode and buildingId (same value)
    IK->>IK: RELEASE SAVEPOINT, UPDATE idempotency_keys with the response, COMMIT
    IK-->>Client: 201 Created
```

Source: `internal/adapters/inbound/http/idempotency.go`, `router.go`
(`commitShiftPlan`), `internal/application/usecases/commit_shift_plan.go`,
`internal/domain/shiftplan/shift_plan.go`,
`internal/adapters/outbound/postgres/shift_plan_repo.go`, `outbox_publisher.go`,
`internal/adapters/outbound/kafka/publisher.go`. Omits: request-body decoding
errors, the `ErrCommitShiftPlanNoCatalogue` wiring error, and the circuit
breaker around the capacity client. The plan key is the canonical `siteCode`;
`buildingId` is accepted as a deprecated alias (same value, same
`building_id` column) and each published line message carries `site_code` next
to `building_id` ([ADR 0035](../adr/0035-sitecode-converges-building-id.md)). Every normal response, including a 4xx,
is stored against the key; only a panic rolls the key back. The wrapped
handler runs inside a savepoint: a 2xx releases it, any other status rolls
back to it first, so a failed request's domain writes and outbox rows are
discarded (no partial write) while the idempotency row still records the
failed response for replay.

## AssignLabor — `POST /associates/{id}/assignments` and MCP `assign_labor`

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant In as http Handler or MCP tool
    participant UC as AssignLabor
    participant AR as AssociateRepo
    participant AS as AssociateShift
    participant LR as AssignmentRepo
    participant LA as LaborAssignment
    participant OB as OutboxPublisher
    Client->>In: assign associateId to pathId
    Note over Client,In: HTTP only: Idempotency-Key middleware as in CommitShiftPlan
    In->>In: validatePathId against the catalogue
    In->>UC: Execute associateId, pathId
    UC->>AR: FindByID
    alt not found
        AR-->>UC: ErrNotFound, mapped to 404
    end
    UC->>AS: CanBeAssigned
    alt on break or shift ended
        AS-->>UC: ErrOnBreak or ErrShiftEnded, mapped to 409
    end
    UC->>UC: requiredCertification via catalogue MatchPrefix
    UC->>AS: HasCertification
    UC->>LR: FindByAssociateID
    alt none yet
        UC->>LA: NewLaborAssignment
    end
    UC->>LA: Assign pathId, hasCert, now
    alt not certified
        LA-->>UC: ErrCertificationRequired, mapped to 409
    else already active
        LA->>LA: close active interval, raise LaborReassigned
        UC->>AS: LogHours of the closed interval
        alt over MAX_HOURS_PER_SHIFT
            AS-->>UC: ErrMaxHoursExceeded, mapped to 409
        end
    else first assignment
        LA->>LA: raise LaborAssigned
    end
    UC->>AR: Save, only if hours were logged
    UC->>LR: Save, version-guarded
    alt stale version
        LR-->>UC: ErrConcurrentModification, mapped to 409
    end
    UC->>OB: Publish LaborAssigned or LaborReassigned
    OB->>OB: INSERT outbox_events, analytics row only
    UC-->>In: LaborAssignment
    In-->>Client: 201 Created, Location /associates/id/assignments
```

Source: `internal/application/usecases/assign_labor.go`, `metrics.go`,
`internal/adapters/inbound/http/router.go` (`assignLabor`),
`internal/adapters/inbound/mcp/tools.go` (`assignLabor`),
`internal/domain/assignment/labor_assignment.go`,
`internal/adapters/outbound/postgres/assignment_repo.go`. Omits: the
deferred `recordLaborAssignment` call that counts every attempt on
`workforce.labor_assignments`, and the MCP error-to-tool-result mapping.

## StartAssociateShift — `POST /associates/{id}/start-shift`

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant H as http Handler
    participant UC as StartAssociateShift
    participant AS as AssociateShift
    participant AR as AssociateRepo
    participant UoW as UnitOfWork
    participant OB as OutboxPublisher
    Client->>H: POST /associates/id/start-shift, certifications, optional siteCode
    H->>UC: ExecuteAtSite associateId, certifications, siteCode
    UC->>AS: NewAssociateShiftAtSite, raises AssociateShiftStarted
    UC->>AR: FindByID
    alt existing roster entry
        UC->>AS: SetVersion existing version
    else other error
        AR-->>UC: error, mapped to 500
    end
    UC->>UoW: Execute
    UoW->>AR: Save, version-guarded upsert
    UoW->>OB: Publish AssociateShiftStarted
    UoW-->>UC: COMMIT
    UC-->>H: AssociateShift
    H-->>Client: 201 Created, Location /associates/id
```

Source: `internal/application/usecases/start_associate_shift.go`,
`internal/domain/associate/associate_shift.go`,
`internal/adapters/outbound/postgres/associate_repo.go`, `unit_of_work.go`.
Omits: request validation (`ErrEmptyAssociateId`, `ErrEmptyCertification` →
400). Not behind the idempotency middleware: restarting is an upsert by
associate id. The optional `siteCode` ([ADR 0034](../adr/0034-site-scoped-staffing-gap.md))
is trimmed, accepted as given (no lookup in facility-layout) and persisted as
`associate_shift.site_code` (`NULL` when absent); a restart replaces it.
`Execute` (no site) is the same call with an empty site.

## CertifyAssociate, StartBreak, EndBreak

The three share one shape; only the aggregate method and event differ.

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant H as http Handler
    participant UC as CertifyAssociate or StartBreak or EndBreak
    participant AR as AssociateRepo
    participant AS as AssociateShift
    participant UoW as UnitOfWork
    participant OB as OutboxPublisher
    Client->>H: POST /associates/id/certifications or /break/start or /break/end
    H->>UC: Execute associateId
    UC->>AR: FindByID
    alt not found
        AR-->>UC: ErrNotFound, mapped to 404
    end
    UC->>AS: Certify or StartBreak or EndBreak
    alt rejected
        AS-->>UC: ErrShiftEnded, ErrAlreadyOnBreak or ErrNotOnBreak, mapped to 409
    end
    UC->>UoW: Execute
    UoW->>AR: Save, version-guarded
    UoW->>OB: Publish AssociateCertified, AssociateBreakStarted or AssociateBreakEnded
    UoW-->>UC: COMMIT
    H-->>Client: 204 No Content
```

Source: `internal/application/usecases/certify_associate.go`, `breaks.go`,
`internal/domain/associate/associate_shift.go`. Omits: request decoding of
the certification body.

## EndAssociateShift — `POST /associates/{id}/end-shift`

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant H as http Handler
    participant UC as EndAssociateShift
    participant AR as AssociateRepo
    participant AS as AssociateShift
    participant LR as AssignmentRepo
    participant LA as LaborAssignment
    participant UoW as UnitOfWork
    participant OB as OutboxPublisher
    Client->>H: POST /associates/id/end-shift
    H->>UC: Execute associateId
    UC->>AR: FindByID
    alt not found
        AR-->>UC: ErrNotFound, mapped to 404
    end
    UC->>LR: FindByAssociateID, not found is tolerated
    opt an active assignment exists
        UC->>LA: EndActive now
        UC->>AS: LogHours of the closed interval
        alt over MAX_HOURS_PER_SHIFT
            AS-->>UC: ErrMaxHoursExceeded, mapped to 409
        end
    end
    UC->>AS: EndShift, raises AssociateShiftEnded unless already ended
    UC->>UoW: Execute
    UoW->>LR: Save, only if an interval was closed
    UoW->>AR: Save
    UoW->>OB: Publish AssociateShiftEnded
    UoW-->>UC: COMMIT
    H-->>Client: 204 No Content
```

Source: `internal/application/usecases/end_associate_shift.go`. Omits:
nothing material; `EndActive` raises no event of its own.

## ProposePathPlan — `POST /paths/{pathId}/plan/propose` and MCP `propose_path_heads`

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant In as http Handler or MCP tool
    participant UC as ProposePathPlan
    participant MR as MeasuredRateClient
    participant IS as IdleShareClient
    participant SP as shiftplan.ProposedHeads
    participant OB as EventPublisher
    Client->>In: charge, optional plannedRate
    In->>In: validatePathId
    In->>In: ResolveSiteKey siteCode or deprecated buildingId, same value
    alt neither, or both with different values
        In-->>Client: 400 missing-building-id or 422 conflicting-site-and-building
    end
    In->>UC: Execute siteCode (stored as building_id), pathId, charge, plannedRate
    opt plannedRate not positive and MeasuredRate wired
        UC->>MR: MeanActualSeconds pathId
        alt available and seconds positive
            MR-->>UC: seconds per task, rate = 3600 / seconds, rateSource measured
        else ErrMeasuredRateUnavailable or seconds not positive
            MR-->>UC: keep caller rate, rateSource caller
        end
    end
    UC->>SP: ceil of charge over rate, 0 when rate not positive
    opt IdleShare wired and heads above 0
        UC->>IS: IdleSharePct pathId
        alt share above IDLE_SHARE_TRIM_THRESHOLD
            UC->>UC: trim heads, set trimReason
        end
    end
    UC->>OB: Publish ShiftPlanProposed
    UC-->>In: heads, resolvedRate, rateSource, trimReason
    In-->>Client: 200 OK
```

Source: `internal/application/usecases/propose_path_plan.go`,
`internal/domain/shiftplan/shift_plan.go` (`ProposedHeads`). Omits: the
circuit breaker and retry around the HTTP measured-rate client. Nothing is
persisted except the outbox row for the event.

## GetStaffingGap — `GET /paths/{pathId}/staffing-gap` and the all-paths variant

```mermaid
sequenceDiagram
    autonumber
    actor Client
    participant In as http Handler or MCP
    participant UC as GetStaffingGap
    participant SR as ShiftPlanRepo
    participant LR as AssignmentRepo
    participant IS as IdleShareClient
    participant OB as EventPublisher
    Client->>In: siteCode (or deprecated buildingId), shiftId, pathId or all paths
    In->>In: ResolveGapLookup, siteCode is plan key and scope, buildingId only is plan key and unscoped
    In->>UC: Execute or ExecuteAll, or ExecuteForSite or ExecuteAllForSite with the scope
    UC->>SR: FindByBuildingAndShift, the stored building_id column holds the site code
    alt no committed plan
        SR-->>UC: ErrNotFound, mapped to 404
    end
    loop each requested path
        UC->>SR: PlannedHeadsFor pathId
        alt no siteCode, unscoped
            UC->>LR: CountActiveByPath pathId
        else siteCode given
            UC->>LR: CountActiveByPathAtSite pathId, siteCode
        end
        UC->>IS: IdleSharePct, optional, nil on failure
        opt activeHeads below plannedHeads
            UC->>UC: build PathUnderstaffed, with siteCode when scoped
        end
    end
    opt any path understaffed
        UC->>OB: Publish PathUnderstaffed events in one unit of work
    end
    UC-->>In: StaffingGap or list
    In-->>Client: 200 OK
```

Source: `internal/application/usecases/get_staffing_gap.go`,
`internal/adapters/inbound/http/router.go` (`staffingGap`,
`staffingGapForSiteShift`, `staffingGapForShift`),
`internal/adapters/inbound/mcp/tools.go`, `resources.go`. Omits:
query-parameter validation (neither `siteCode` nor `buildingId`, or no `shiftId`
→ 400). **Resolved (Decided 2026-10-06, [ADR 0034](../adr/0034-site-scoped-staffing-gap.md),
[ADR 0035](../adr/0035-sitecode-converges-building-id.md)):**
`CountActiveByPath` still counts every active assignment on the path across
sites — that is the **unscoped** answer, which is what a call that sends only
the legacy `buildingId` gets (callers use ids like `bldg-1`, not Site codes).
With a `siteCode`, which is both the plan key and the scope,
`CountActiveByPathAtSite` counts
only assignments whose associate has a not-ended shift at that site
(`labor_assignment` ⨝ `associate_shift`); legacy `NULL`-site rows never match a
site. Routes: `GET /sites/{siteCode}/shifts/{shiftId}/staffing-gap` is
canonical; `GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap` is the
deprecated alias (`Deprecation: true`, unscoped unless `?siteCode=`). MCP
resource `staffing://sites/{siteCode}/{shiftId}/{pathId}/gap` is scoped;
`staffing://{buildingId}/…/gap` stays unscoped and deprecated.
