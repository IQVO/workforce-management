---
id: 0009-hazmat-certification-via-existing-path-gating
title: 0009. Hazmat handling via the existing path-name-equals-certification-name gate
sidebar_label: 0009. Hazmat via existing cert gating
sidebar_position: 10
description: A cross-repo effort added a Hazmat product classification upstream; this context needs no new code to gate it — the existing certification convention already covers it.
---

# 0009. Hazmat handling via the existing path-name-equals-certification-name gate

## Status

Accepted — superseded in part by [ADR 0013](./0013-process-path-catalogue-validation):
two clarifications from the 2026-10 ADR-conformance pass. First, `hazmat`
is a station **capability** a declared path can require (e.g. the
`HAZMAT-PICK` family, `RequiredCapabilities: [pick, hazmat]`), not itself a
catalogue-declared path id — a bare `pathId: "hazmat"` is rejected `400
unknown-path-id` by the REST route's catalogue validation. Second, the
required-certification name is now resolved through the catalogue's
canonical path family (see the Decision amendment below), fixing a real
mismatch between the catalogue's case-insensitive prefix ids and the exact
raw-id certification match.

## Context

A cross-repo effort across the `warehouse-systems` estate introduced a Hazmat
product classification tag upstream (inventory/product classification). As
part of that effort, an earlier draft proposed adding, to this context, a
direct link between a specific `AssociateId` and a specific station's
capability requirement — the idea being that workforce-management would
record which associate is qualified to claim which hazmat-capable station.

That draft was rejected on review against this context's own `CLAUDE.md` and
[ADR 0002](./0002-stop-at-the-path-boundary.md): this context explicitly
"never links an associate to a specific task," and "stops at the path
boundary" — dispatch of individual tasks to a claiming station belongs to
`fulfillment-execution`, not here. There is, correctly, no `AssociateId` ↔
`StationId` (or `AssociateId` ↔ task) relationship anywhere in this codebase,
and adding one for hazmat specifically would have been exactly the kind of
special case [ADR 0002](./0002-stop-at-the-path-boundary.md) and
[ADR 0003](./0003-certification-gated-single-active-assignment.md) already
argue against.

Once the proposed associate-to-station link was set aside, the real question
became: does *this* context need any new code at all to make hazmat-aware
assignment work? [ADR 0003](./0003-certification-gated-single-active-assignment.md)
already established the mechanism this context uses to gate any path behind
any certification: **a path's required certification is, by convention, the
`Certification` with the same name as the `PathId`.** That convention is
generic — it was never pack-specific or pick-specific, and it does not stop
being true for a path named `hazmat`.

## Decision

**No new aggregate, port, or use-case code.** A hazmat-designated process
path is gated exactly like any other path: a path named `hazmat` requires the
associate to hold the `Certification` value `"hazmat"`, enforced by the
existing check in `assignment.LaborAssignment.Assign` (via
`internal/application/usecases/assign_labor.go`'s
`requiredCert := shared.Certification(pathId)` /
`shift.HasCertification(requiredCert)`), which returns
`assignment.ErrCertificationRequired` when the associate lacks it.

`hazmat` is now documented as a real, in-use `Certification` value — not a
hypothetical placeholder — in `CLAUDE.md`'s Ubiquitous Language section and in
`docs/docs/business-context/ubiquitous-language.md`. `shared.Certification`
remains the same open string type it always was (see
`internal/domain/shared/ids.go`); it is not converted into a closed enum,
consistent with this context's existing convention of documenting known
values rather than closing the type.

_Amended 2026-10 (ADR-0013 interaction):_ when a catalogue is wired (every
real deployment; it has been mandatory for REST/MCP writes since
ADR-0013), the required certification is the RESOLVED path family's
canonical prefix rather than the raw caller-supplied id:
`AssignLabor.requiredCertification` resolves `PICK`, `pick` and
`pick-zone-a` all to certification `pick`, and a `hazmat-pick`-family id to
`hazmat-pick`. The earlier exact raw-id match was a genuine bug against
ADR-0013's matching semantics: an associate holding the documented
lower-case certification was rejected over a case variant or zone suffix.
Bare `hazmat` as a path id is unreachable via REST/MCP (400
unknown-path-id, above); a path FAMILY whose work requires hazmat
capability — `hazmat-pick` — is the real shape, gated by the `hazmat-pick`
certification. `TestAssignLabor_CatalogueNormalisesRequiredCertification`
(use cases) and `TestAssignLabor_HazmatPath_ThroughHTTP` (router) cover
both.

`internal/application/usecases/usecases_test.go` gains one illustrative test,
`TestAssignLabor_HazmatPath`, applying the same certified/uncertified
assertions already made generically by `TestAssignLabor_Succeeds` and
`TestAssignLabor_RejectsMissingCertification` to `pathId = "hazmat"`
specifically. It demonstrates existing behaviour; it does not exercise any
new code path.

No OpenAPI (`apis/openapi.yaml`) or AsyncAPI changes accompany this decision:
no new endpoint, no changed request/response shape. The `certification` field
already documents `"hazmat"` as an example value in both specs.

## Consequences

**Easier**

- Hazmat-designated paths are gated with zero new code, zero new tests beyond
  the one illustrative addition, and zero migration risk — the mechanism
  already existed and was already correct for this scenario.
- The context boundary from [ADR 0002](./0002-stop-at-the-path-boundary.md)
  stays intact: workforce-management continues to know nothing about stations
  or task dispatch, hazmat included.
- Anyone reading `CLAUDE.md` or the ubiquitous language doc now sees `hazmat`
  called out as a concrete, real example rather than inferring it from a code
  comment's parenthetical.

**Harder**

- Nothing new. This decision changes no runtime behavior; the "harder" side
  of the underlying mechanism is already recorded in
  [ADR 0003](./0003-certification-gated-single-active-assignment.md) (the
  path-to-certification convention is invisible in the type system).

**Now true**

- `hazmat` is documented, in two places, as a real certification value this
  context already gates correctly.
- `fulfillment-execution` remains solely responsible for the
  station-capability half of hazmat handling — the question of which
  physical stations are hazmat-capable and how a task gets claimed there.
  That is a different bounded context, a different mechanism (station
  capability, not associate certification), and a different, independent
  decision; this ADR does not touch it and does not create any link to it.
- A future reader tempted to add an `AssociateId` ↔ `StationId` relationship
  for hazmat specifically has a written record of why that was tried, and
  rejected, once already.
