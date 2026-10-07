---
id: domain-message-flow
title: Domain message flow
sidebar_label: Domain message flow
sidebar_position: 11
description: ddd-crew Domain Message Flow Modelling — four key workforce-management scenarios with every command, event and query numbered.
---

# Domain message flow

Follows ddd-crew
[Domain Message Flow Modelling](https://github.com/ddd-crew/domain-message-flow-modelling).
Every arrow is prefixed `cmd:` (command), `evt:` (event) or `qry:` (query)
and maps to a real REST route, MCP tool or CloudEvents `type`. Event names
are shortened to the last `type` segment inside the diagrams; the full
`type` strings are in [Domain events](./domain-events.md).

## 1. Shift-start planning: propose, then commit

A shift lead asks for a proposal, then commits a headcount split. The
proposal may read a measured rate; the commit must read live installed
capacity.

```mermaid
sequenceDiagram
    autonumber
    actor Lead as Shift lead
    participant WFM as workforce-management
    participant LP as labor-performance
    participant FE as fulfillment-execution
    participant WP as wes-work-planning
    participant PL as warehouse-planning
    Lead->>WFM: cmd: POST /paths/pathId/plan/propose
    WFM->>LP: qry: GET /task-types/taskType/performance, http mode, only if no plannedRate
    LP-->>WFM: meanActualSeconds or unavailable, fail-open
    WFM-->>WFM: evt: ShiftPlanProposed to warehouse.workforce.analytics
    WFM-->>Lead: proposedHeads, resolvedRate, rateSource, trimReason
    Lead->>WFM: cmd: POST /shift-plans with Idempotency-Key
    WFM->>FE: qry: GET /capacity/capability, once per required capability
    FE-->>WFM: installed station count, failure means 503
    WFM->>WP: evt: ShiftPlanCommitted on warehouse.workforce.events, one per PathPlan line
    WFM->>PL: evt: ShiftPlanCommitted on warehouse.workforce.events, same messages
    WFM-->>Lead: 201 Created with Location
```

Source: `internal/application/usecases/propose_path_plan.go`,
`commit_shift_plan.go`, `internal/adapters/outbound/laborperformance/client.go`,
`internal/adapters/outbound/fulfillmentexecution/client.go`,
`internal/adapters/outbound/kafka/publisher.go`; consumers in
`wes-work-planning/internal/adapters/inbound/kafka/consumer.go` and
`warehouse-planning/internal/adapters/inbound/kafka/labor_capacity_consumer.go`.
Omits: the `kafka-cache` alternative to step 2 (scenario 4), the idle-share
trim lookup, the outbox relay hop, and every rejection branch.

## 2. Intra-shift rebalancing: see the gap, move someone

`warehouse-ops-agent` (or a lead on the console) reads the gap; the move
itself is a human call recorded through `AssignLabor`.

```mermaid
sequenceDiagram
    autonumber
    participant Agent as warehouse-ops-agent
    actor Lead as Shift lead
    participant WFM as workforce-management
    participant AN as workforce analytics projector
    Agent->>WFM: qry: MCP get_staffing_gap, optional siteCode
    WFM-->>AN: evt: PathUnderstaffed, only when activeHeads below plannedHeads, site_code only when scoped
    WFM-->>Agent: plannedHeads, activeHeads, understaffed, siteCode when scoped
    Agent->>WFM: qry: MCP propose_path_heads
    WFM-->>AN: evt: ShiftPlanProposed
    WFM-->>Agent: proposedHeads
    Lead->>WFM: cmd: POST /associates/id/assignments with Idempotency-Key
    WFM-->>AN: evt: LaborReassigned, or LaborAssigned for a first assignment
    WFM-->>Lead: 201 Created
```

Source: `internal/adapters/inbound/mcp/tools.go`,
`internal/application/usecases/get_staffing_gap.go`, `assign_labor.go`,
`internal/adapters/outbound/kafka/analytics_publisher.go`;
`warehouse-ops-agent/internal/adapters/outbound/mcpclient/workforce_management.go`.
Omits: the MCP `assign_labor` tool (registered, but no sibling calls it in
code), the all-paths REST variant
`GET /buildings/buildingId/shifts/shiftId/staffing-gap`, and the rejection
branches (uncertified, on break, shift ended, max hours).

## 3. Associate day: roster, breaks, end of shift, reporting

```mermaid
sequenceDiagram
    autonumber
    actor Lead as Shift lead
    participant WFM as workforce-management
    participant AN as workforce-projector
    participant RP as workforce-reports
    participant Con as warehouse-console
    Lead->>WFM: cmd: POST /associates/id/start-shift, optional siteCode
    WFM-->>AN: evt: AssociateShiftStarted
    Lead->>WFM: cmd: POST /associates/id/certifications
    WFM-->>AN: evt: AssociateCertified
    Lead->>WFM: cmd: POST /associates/id/break/start
    WFM-->>AN: evt: AssociateBreakStarted
    Lead->>WFM: cmd: POST /associates/id/break/end
    WFM-->>AN: evt: AssociateBreakEnded
    Lead->>WFM: cmd: POST /associates/id/end-shift
    WFM-->>AN: evt: AssociateShiftEnded
    Con->>RP: qry: GET /reports/labor
    RP-->>Con: hourly labor rollup per path
```

Source: `internal/adapters/inbound/http/router.go`,
`internal/application/usecases/start_associate_shift.go`, `certify_associate.go`,
`breaks.go`, `end_associate_shift.go`,
`internal/adapters/inbound/kafka/analytics_consumer.go`,
`internal/adapters/inbound/http/reports_handler.go`;
`warehouse-console/src/features/context-reports/workforceManagement.config.tsx`.
Omits: `GET /reports/labor/freshness`, the `warehouse-ops-agent` reports
client that reads the same endpoint, and the DLQ (`warehouse.workforce.analytics.dlq`).

## 4. Reference data feeds into this context

Selected by `PATH_CATALOGUE_SOURCE=kafka` and
`LABOR_PERFORMANCE_MODE=kafka-cache`; the defaults are `file` and
`permissive`.

```mermaid
sequenceDiagram
    autonumber
    participant PPM as process-path-management
    participant LP as labor-performance
    participant WFM as workforce-management
    PPM->>WFM: evt: ProcessPathCreated on warehouse.process-path-management.events
    PPM->>WFM: evt: ProcessPathUpdated
    PPM->>WFM: evt: ProcessPathDeactivated
    LP->>WFM: evt: TaskPerformanceRecorded on warehouse.labor-performance.events
    WFM->>WFM: qry: catalogue Lookup and cached mean or idle share, used by later commands
```

Source: `internal/adapters/outbound/kafkacatalog/consumer.go`,
`internal/adapters/outbound/laborperformancecache/consumer.go`,
`internal/adapters/kafka/cloudevents/types.go`, `cmd/workforce/main.go`.
Omits: the boot-time readiness wait (`WaitReadyTimeout`) and the
per-process consumer-group naming.
