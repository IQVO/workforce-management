---
id: class-diagram
title: Class diagram
sidebar_label: Class diagram
sidebar_position: 13
description: UML class diagrams of internal/domain (aggregates, value objects, domain events) and the hexagonal ports-and-adapters wiring of workforce-management.
---

# Class diagram

UML class diagrams of `internal/domain/**`, split in three so each stays
readable: the aggregates with their value objects, the domain events, and
the hexagonal ports with their adapters. Field and method names are the real
Go identifiers; unexported fields keep their lower-case names.

## Aggregates and value objects

```mermaid
classDiagram
    direction LR
    class ShiftPlan {
        <<AggregateRoot>>
        -buildingId string
        -shiftId string
        -lines PathPlan[]
        -events DomainEvent[]
        +CommitShiftPlan(buildingId, shiftId, lines, installedStations, installedCapacity, maxHoursPerShift, at) ShiftPlan
        +PlannedHeadsFor(pathId) int
        +Lines() PathPlan[]
        +PullEvents() DomainEvent[]
    }
    class PathPlan {
        <<ValueObject>>
        +PathId PathId
        +PlannedHeads int
        +PlannedRate float64
        +PlannedHours float64
    }
    class AssociateShift {
        <<AggregateRoot>>
        -associateId AssociateId
        -certifications set of Certification
        -onBreak bool
        -hoursLogged float64
        -ended bool
        -version int
        +NewAssociateShift(associateId, certifications, at) AssociateShift
        +Certify(c, at) error
        +StartBreak(at) error
        +EndBreak(at) error
        +LogHours(hours, maxHoursPerShift) error
        +EndShift(at)
        +CanBeAssigned() error
        +HasCertification(c) bool
        +SetVersion(v)
        +PullEvents() DomainEvent[]
    }
    class LaborAssignment {
        <<AggregateRoot>>
        -associateId AssociateId
        -active Interval
        -history Interval[]
        -version int
        +NewLaborAssignment(associateId) LaborAssignment
        +Assign(pathId, hasCertification, at) error
        +EndActive(at) Interval
        +IsActive() bool
        +ActivePathId() PathId
        +PullEvents() DomainEvent[]
    }
    class Interval {
        <<ValueObject>>
        +PathId PathId
        +Start time
        +End time optional
        +Hours(at) float64
    }
    class AssociateId {
        <<ValueObject>>
        string
    }
    class PathId {
        <<ValueObject>>
        string
    }
    class Certification {
        <<ValueObject>>
        string
    }
    class Capability {
        <<ValueObject>>
        string
    }
    class Catalogue {
        <<Entity>>
        -defs PathDefinition[]
        +Lookup(id) PathDefinition
        +Ids() string[]
    }
    class PathDefinition {
        <<ValueObject>>
        +Id string
        +MatchPrefix string
        +RequiredCapabilities string[]
        +DestinationLocationRole string
    }
    ShiftPlan *-- "1..*" PathPlan : lines
    PathPlan --> PathId
    AssociateShift *-- "0..*" Certification
    AssociateShift --> AssociateId
    LaborAssignment *-- "0..1" Interval : active
    LaborAssignment *-- "0..*" Interval : history
    LaborAssignment --> AssociateId
    Interval --> PathId
    Catalogue *-- "1..*" PathDefinition
    PathDefinition ..> Capability : RequiredCapabilities resolve to
```

Source: `internal/domain/shiftplan/shift_plan.go`,
`internal/domain/associate/associate_shift.go`,
`internal/domain/assignment/labor_assignment.go`,
`internal/domain/shared/ids.go`, `internal/domain/pathcatalog/path_definition.go`.
Omits: getters that only expose a field (`BuildingId`, `ShiftId`,
`AssociateId`, `Version`, `IsOnBreak`, `HoursLogged`, `Ended`,
`Certifications`, `ActiveInterval`, `History`), every `Rehydrate`
constructor, the private `record`/`closeActive` helpers, the `New*`
validating constructors of the string value objects, and the free function
`shiftplan.ProposedHeads(charge, plannedRate)`. `Catalogue` is marked
`<<Entity>>` for want of a better stereotype: it is reference data with no
identity of its own. There are **no enumerations**: no aggregate has a status
enum. The three roots reference each other only by `AssociateId` / `PathId`
values, never by object reference.

## Domain events

```mermaid
classDiagram
    direction TB
    class DomainEvent {
        <<interface>>
        +EventName() string
        +OccurredAt() time
    }
    class ShiftPlanProposed {
        <<DomainEvent>>
        BuildingId string
        PathId PathId
        PlannedHeads int
        PlannedRate float64
    }
    class ShiftPlanCommitted {
        <<DomainEvent>>
        BuildingId string
        ShiftId string
    }
    class PathUnderstaffed {
        <<DomainEvent>>
        PathId PathId
        PlannedHeads int
        ActiveHeads int
    }
    class AssociateShiftStarted {
        <<DomainEvent>>
        AssociateId AssociateId
        Certifications Certification[]
    }
    class AssociateCertified {
        <<DomainEvent>>
        AssociateId AssociateId
        Certification Certification
    }
    class AssociateBreakStarted {
        <<DomainEvent>>
        AssociateId AssociateId
    }
    class AssociateBreakEnded {
        <<DomainEvent>>
        AssociateId AssociateId
    }
    class AssociateShiftEnded {
        <<DomainEvent>>
        AssociateId AssociateId
    }
    class LaborAssigned {
        <<DomainEvent>>
        AssociateId AssociateId
        PathId PathId
    }
    class LaborReassigned {
        <<DomainEvent>>
        AssociateId AssociateId
        FromPathId PathId
        ToPathId PathId
    }
    DomainEvent <|.. ShiftPlanProposed
    DomainEvent <|.. ShiftPlanCommitted
    DomainEvent <|.. PathUnderstaffed
    DomainEvent <|.. AssociateShiftStarted
    DomainEvent <|.. AssociateCertified
    DomainEvent <|.. AssociateBreakStarted
    DomainEvent <|.. AssociateBreakEnded
    DomainEvent <|.. AssociateShiftEnded
    DomainEvent <|.. LaborAssigned
    DomainEvent <|.. LaborReassigned
```

Source: `internal/domain/shared/events.go`. Omits: the embedded
`baseEvent` struct that carries `occurredAt`, and the `New*` constructors.
`ShiftPlanProposed` and `PathUnderstaffed` are constructed by use cases,
not aggregates.

## Ports and adapters

```mermaid
classDiagram
    direction LR
    class AssociateRepo {
        <<Repository>>
        +Save(ctx, a) error
        +FindByID(ctx, id) AssociateShift
    }
    class ShiftPlanRepo {
        <<Repository>>
        +Save(ctx, sp) error
        +FindByBuildingAndShift(ctx, buildingId, shiftId) ShiftPlan
    }
    class AssignmentRepo {
        <<Repository>>
        +Save(ctx, la) error
        +FindByAssociateID(ctx, id) LaborAssignment
        +CountActiveByPath(ctx, pathId) int
    }
    class EventPublisher {
        <<interface>>
        +Publish(ctx, events) error
    }
    class UnitOfWork {
        <<interface>>
        +Execute(ctx, fn) error
    }
    class InstalledCapacityClient {
        <<interface>>
        +InstalledCapacity(ctx, capability) int
    }
    class MeasuredRateClient {
        <<interface>>
        +MeanActualSeconds(ctx, pathId) float64
    }
    class IdleShareClient {
        <<interface>>
        +IdleSharePct(ctx, pathId) float64
    }
    class PathCatalogue {
        <<interface>>
        +Lookup(id) PathDefinition
    }
    class Clock {
        <<interface>>
        +Now() time
    }
    class LaborMetrics {
        <<interface>>
        +AssignmentAccepted(ctx, pathId)
        +AssignmentRejected(ctx, pathId, reason)
    }
    class ProcessedEvents {
        <<interface>>
        +MarkProcessed(ctx, eventId) bool
    }
    class PostgresRepos["postgres AssociateRepo / ShiftPlanRepo / AssignmentRepo"]
    class MemoryRepos["memory repos"]
    class OutboxPublisher["postgres OutboxPublisher"]
    class KafkaPublishers["kafka Publisher + AnalyticsPublisher"]
    class LogPublisher["events LogPublisher"]
    class PgUnitOfWork["postgres UnitOfWork"]
    class FEClient["fulfillmentexecution Client / PermissiveClient"]
    class LPClient["laborperformance Client / PermissiveClient"]
    class LPCache["laborperformancecache Consumer"]
    class Catalogues["pathcatalog Catalogue via filecatalog / kafkacatalog Consumer"]
    class SystemClock["clock System"]
    class Telemetry["telemetry LaborMetrics"]
    class ConsumedEvents["analyticsstore ConsumedEventsRepo"]
    AssociateRepo <|.. PostgresRepos
    ShiftPlanRepo <|.. PostgresRepos
    AssignmentRepo <|.. PostgresRepos
    AssociateRepo <|.. MemoryRepos
    EventPublisher <|.. OutboxPublisher
    EventPublisher <|.. KafkaPublishers
    EventPublisher <|.. LogPublisher
    UnitOfWork <|.. PgUnitOfWork
    InstalledCapacityClient <|.. FEClient
    MeasuredRateClient <|.. LPClient
    MeasuredRateClient <|.. LPCache
    IdleShareClient <|.. LPCache
    PathCatalogue <|.. Catalogues
    Clock <|.. SystemClock
    LaborMetrics <|.. Telemetry
    ProcessedEvents <|.. ConsumedEvents
```

Source: `internal/application/ports/ports.go`, `internal/adapters/outbound/**`,
`internal/composition/*.go`, `cmd/workforce/main.go`. Omits: the inbound
side (shown below), the circuit-breaker decorators that wrap the two HTTP
clients, and the in-memory implementations of the other two repos and of
`ProcessedEvents`.

```mermaid
flowchart LR
    subgraph inbound["Inbound adapters"]
        HTTP["http Handler<br/>cmd/workforce :8080"]
        MCP["mcp Deps<br/>cmd/mcp :8090"]
        REP["http ReportsHandlers<br/>cmd/workforce-reports :8092"]
        KC["kafka AnalyticsConsumer<br/>cmd/workforce-projector"]
    end
    subgraph app["Application"]
        UC["9 use cases<br/>internal/application/usecases"]
        PORTS["ports<br/>internal/application/ports"]
    end
    subgraph domain["Domain"]
        D["shiftplan, associate,<br/>assignment, shared, pathcatalog"]
    end
    subgraph outbound["Outbound adapters"]
        PG["postgres"]
        KP["kafka publishers + outbox relay"]
        FE["fulfillmentexecution"]
        LP["laborperformance, laborperformancecache"]
        CAT["filecatalog, kafkacatalog"]
        AS["analyticsstore"]
    end
    HTTP --> UC
    MCP --> UC
    UC --> D
    UC --> PORTS
    PG -. implements .-> PORTS
    KP -. implements .-> PORTS
    FE -. implements .-> PORTS
    LP -. implements .-> PORTS
    CAT -. implements .-> PORTS
    KC --> AS
    REP --> AS
```

Source: `cmd/*/main.go`, `internal/adapters/**`. Omits: telemetry, clock,
bootretry and resilience packages. The analytics side (`KC`, `REP`,
`analyticsstore`) bypasses the use cases: it is a read-side projection, not
part of the domain model ([ADR 0010](../adr/0010-analytical-data-product.md)).
The dependency rule is enforced by `make arch-test`
([ADR 0007](../adr/0007-arch-go-architecture-fitness-tests.md)).
