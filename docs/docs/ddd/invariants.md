---
id: invariants
title: Invariants
sidebar_label: Invariants
sidebar_position: 4
description: Every rule enforced in the domain layer, and the failing-path test that pins it down.
---

# Invariants

Every rule below is enforced in `internal/domain`, in pure Go, with no
framework or SQL type in sight. Adapters cannot bypass them, because adapters
have no other way to construct an aggregate.

Four of them are named in this context's Definition of Done and each has a
dedicated **failing-path** test — a test that asserts the rule *rejects*, not
merely that the happy path works.

## ShiftPlan

### `plannedHeads(path) ≤ installedStations(path)`

You cannot commit more heads to a path than it has physical positions.

```go
if line.PlannedHeads > installed {
    return nil, ErrPlannedHeadsExceedInstalled
}
```

This is the same invariant `wes-work-planning` enforces on *its* `PathPlan`,
and it is enforced here **independently**, because this is the aggregate that
actually commits headcount. Trusting an upstream check this service does not
control would make correctness a distributed property.

| Layer | Test |
| --- | --- |
| Domain | `shiftplan.TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations` |
| Application | `usecases.TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations` |
| HTTP | `http.TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations` |
| Boundary | `shiftplan.TestCommitShiftPlan_AllowsPlannedHeadsExactlyEqualToInstalledStations` |

The boundary test is not decoration. Mutation testing with `gremlins` produced
a surviving mutant that flipped `>` to `>=`, which no test at the time caught;
the equality case was added to kill it. Details in `MUTATION.md`.

### `plannedHours ≤ plannedHeads × maxHoursPerShift`

A path line cannot commit more total hours than its planned heads could
physically work within one shift's cap. This is how "the sum of planned hours
must be valid" is operationalised.

Error: `ErrPlannedHoursExceedCapacity` → `409`.
Test: `TestCommitShiftPlan_RejectsPlannedHoursExceedingCapacity`.

### A plan must have at least one line, and every line needs an installed count

Errors: `ErrNoPathPlans`, `ErrMissingInstalledStations` → `400`.
Tests: `TestCommitShiftPlan_RejectsEmptyLines`,
`TestCommitShiftPlan_RejectsMissingInstalledStations`.

### `plannedHeads(path) ≤ liveInstalledCapacity(path)`

A second, **independent** ceiling ([ADR 0014](../adr/0014-installed-capacity-ceiling.md)).
`CommitShiftPlan` resolves each line's path through the process-path catalogue
to its `requiredCapabilities`, asks `fulfillment-execution` for the live
station count per capability (`GET /capacity/{capability}`), and takes the
smallest count as the path's ceiling. The domain then rejects any line above
it:

```go
if line.PlannedHeads > installedCapacity[line.PathId] {
    return nil, ErrExceedsInstalledCapacity
}
```

A path with no entry in the map gets a ceiling of `0` — a missing live fetch
can never silently bypass the check. Errors: `ErrExceedsInstalledCapacity` →
`409 exceeds-installed-capacity`; any failure to fetch the count →
`ErrInstalledCapacityUnavailable` → `503 installed-capacity-unavailable`
(fail-loud, the whole commit is rejected); an undeclared path →
`pathcatalog.ErrUnknownPath` → `400 unknown-path-id`.

| Layer | Test |
| --- | --- |
| Domain | `shiftplan.TestCommitShiftPlan_RejectsPlanExceedingInstalledCapacity` |
| Domain (boundary) | `shiftplan.TestCommitShiftPlan_AllowsPlannedHeadsExactlyEqualToInstalledCapacity` |
| Domain (missing entry) | `shiftplan.TestCommitShiftPlan_MissingInstalledCapacityEntryDefaultsToZeroCeiling` |
| Application | `usecases.TestCommitShiftPlan_RejectsPlanExceedingLiveInstalledCapacity`, `usecases.TestCommitShiftPlan_FailsLoudWhenInstalledCapacityUnavailable`, `usecases.TestCommitShiftPlan_QueriesInstalledCapacityByRequiredCapability` |
| HTTP | `http.TestCommitShiftPlan_RejectsPlanExceedingLiveInstalledCapacity`, `http.TestCommitShiftPlan_ServiceUnavailableWhenInstalledCapacityUnreachable` |

### Validation is all-or-nothing

`CommitShiftPlan` validates every line *before* constructing the aggregate. A
plan with one bad line commits nothing at all — there is no partially committed
`ShiftPlan`.

## LaborAssignment

### Exactly one ACTIVE assignment per associate

No double-booking across paths, ever. This one is **structural**: the aggregate
is keyed by `AssociateId` and holds a single `active *Interval` field, so a
second active assignment has nowhere to live.

Because it holds by construction, the "failing path" is expressed as the
supersede behaviour rather than an error: assigning a second path closes the
first and raises `LaborReassigned`.

Test: `assignment.TestAssign_SecondAssignmentEndsPriorAndRaisesReassigned`,
plus `usecases.TestAssignLabor_SecondAssignmentEndsPriorAndLogsHours` which
also asserts the closed interval's hours are logged against the associate's
shift.

### An assignment requires the path's certification

```go
if !hasCertification {
    return ErrCertificationRequired
}
```

Rejected with `409`. A path's required certification is, by convention, the
`Certification` with the same name as the path. When the process-path
catalogue is wired (every deployed composition root), `AssignLabor` resolves
that name through the catalogue first and uses the matched family's
`MatchPrefix` (`requiredCertification` in
`internal/application/usecases/assign_labor.go`), so `PICK`, `pick` and
`pick-zone-a` all require certification `pick`; without a catalogue the raw
`PathId` is used. This convention is generic: a hazmat-designated path
named `hazmat` is gated the same way, requiring certification `hazmat`,
with no hazmat-specific code — see
[ADR 0009](../adr/0009-hazmat-certification-via-existing-path-gating.md) and
its amendment in [ADR 0013](../adr/0013-process-path-catalogue-validation.md).

| Layer | Test |
| --- | --- |
| Domain | `assignment.TestAssign_RejectsMissingCertification` |
| Application | `usecases.TestAssignLabor_RejectsMissingCertification` |
| Application (hazmat, illustrative) | `usecases.TestAssignLabor_HazmatPath` |
| HTTP | `http.TestAssignLabor_RejectsMissingCertification` |

Training is itself a path that consumes hours. It is deliberately **not**
special-cased: gate on assignment, and the training path behaves like every
other path.

## AssociateShift

### An associate on a logged break cannot be assigned

```go
func (a *AssociateShift) CanBeAssigned() error {
    if a.ended  { return ErrShiftEnded }
    if a.onBreak { return ErrOnBreak }
    return nil
}
```

Rejected with `409`.

| Layer | Test |
| --- | --- |
| Domain | `associate.TestCanBeAssigned_RejectsWhileOnBreak` |
| Application | `usecases.TestAssignLabor_RejectsWhileOnBreak` |
| HTTP | `http.TestAssignLabor_RejectsWhileOnBreak` |

### Logged hours must not exceed the max-hours-per-shift limit

`LogHours` rejects with `ErrMaxHoursExceeded` (`409`) when the additional hours
would breach `MAX_HOURS_PER_SHIFT` (default 8). This fires from `AssignLabor`
and `EndAssociateShift`, both of which log a closed interval's hours.

Tests: `usecases.TestAssignLabor_LogHoursExceedsMax`,
`usecases.TestEndAssociateShift_LogHoursExceedsMax`.

### Break state transitions are strict

Starting a break while already on one → `ErrAlreadyOnBreak`. Ending a break
while not on one → `ErrNotOnBreak`. Either after the shift ended →
`ErrShiftEnded`. All `409`.

### An ended shift is terminal

`EndShift` raises `AssociateShiftEnded` exactly once, and every subsequent
mutating operation on that roster entry is rejected with `ErrShiftEnded`.

Test: `associate.TestEndShift_RaisesEventOnceAndBlocksFurtherOps`.

## Value objects

`AssociateId`, `PathId` and `Certification` are constructed through validating
constructors and reject empty values (`400`). An invalid identifier cannot
exist as a value in this system.

## Concurrency guard (infrastructure, not a business rule)

`AssociateShift` and `LaborAssignment` carry an inert `version` field
([ADR 0021](../adr/0021-optimistic-concurrency-version-column.md)). The domain
never branches on it; the Postgres repos' `Save` only applies when the row's
`version` still equals the loaded one, and otherwise returns
`ports.ErrConcurrentModification` → `409 concurrent-modification`. `ShiftPlan`
has no version column: `CommitShiftPlan` always builds a fresh plan from the
request instead of loading and mutating the old one.

Tests: `postgres.TestAssociateRepo_Save_StaleVersionFails`,
`postgres.TestAssignmentRepo_Save_StaleVersionFails`,
`postgres.TestAssociateRepo_ConcurrentSaves_ExactlyOneWinner`,
`usecases.TestStartAssociateShift_RestartCarriesOverExistingVersion`.

## Read models are projections

`GetStaffingGap` computes `plannedHeads` from the committed `ShiftPlan` and
`activeHeads` by counting active assignments, at read time. Nothing caches it,
nothing stores it on an aggregate. This is a platform-wide rule: read models
are PROJECTIONS built from events and aggregates, never state stored
redundantly on the write model.

## How the invariants are held down over time

| Mechanism | What it protects against |
| --- | --- |
| Failing-path unit tests | The rule silently stops rejecting |
| ≥ 90% coverage gate on domain + application | New rules land untested |
| `gremlins` mutation testing on `internal/domain/...` | Tests that pass for the wrong reason — this is what caught the `>` vs `>=` boundary |
| `godog` acceptance specs over the real HTTP surface | The rule holds at the domain layer but is bypassed by an adapter |
| `arch-go` fitness tests | A framework or SQL type creeps into the domain layer, making the rules un-unit-testable |
