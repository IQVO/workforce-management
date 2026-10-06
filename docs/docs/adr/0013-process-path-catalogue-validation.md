---
id: 0013-process-path-catalogue-validation
slug: /adr/0013-process-path-catalogue-validation
title: 0013. Process-path catalogue validation, mirroring fulfillment-execution's ADR-0017
sidebar_label: 0013. Path catalogue validation
sidebar_position: 14
description: ADR 0013 — validate every caller-supplied path_id (ProposePathPlan, CommitShiftPlan, AssignLabor, staffing-gap) against the fleet's declared process-path catalogue, loaded from warehouse-infra's published-language YAML file at boot.
---

# 0013. Process-path catalogue validation

## Status

Accepted.

## Context

This service's HTTP surface accepts a caller-supplied `path_id` at four
endpoints: `POST /paths/{pathId}/plan/propose`, `POST /shift-plans`
(one `path_id` per line), `POST /associates/{id}/assignments`, and
`GET /paths/{pathId}/staffing-gap`. Until now, every one of these simply
called `shared.NewPathId(value)` — which only rejects an *empty*
string — and trusted the result. Nothing anywhere in this service ever
validated that a `path_id` corresponds to a real, currently-declared
process path. A typo or a stale caller referencing a retired path would
silently seed a brand-new `ChargeForecast`-analog proposal, a
`ShiftPlan` line, or a `LaborAssignment` for a path nothing downstream
(`fulfillment-execution`'s own execution layer, in particular) will ever
recognize — the exact "undeclared paths silently accumulate" failure
mode `fulfillment-execution`'s ADR-0017 documents fixing on its own
side of this same integration.

`fulfillment-execution` closed this gap for its `WorkReleased` consumer,
and `wes-work-planning` closed the identical gap across its own four
`path_id` entry points (see its ADR-0012), both by loading a shared,
`warehouse-infra`-published catalogue
(`config/process-paths/sortable-fc.yaml`) at boot and validating every
incoming `path_id` against it. That catalogue is explicitly a
**published language** — the single source of truth for "what process
paths exist in this building's topology" — meant to be read
independently by every consuming service. This service is the third and
final named consumer in that catalogue's own schema comment
(`fulfillment-execution`, `wes-work-planning`, `workforce-management`),
so closing the same gap here completes the pattern across the fleet.

### Built from a corrected starting point

`fulfillment-execution`'s first version of this idea did an EXACT string
match against the catalogue's bare canonical id (`"PICK"`), which is
wrong — no real `path_id` in this fleet is ever the bare canonical id
(see that repo's ADR-0017 addendum for the full incident). This
service's implementation is built from day one with the ALREADY-FIXED
`MatchPrefix` family-matching design `wes-work-planning`'s own ADR-0012
also used, avoiding a repeat of that regression.

## Decision

**Load the same `warehouse-infra`-published process-path catalogue this
fleet already uses, and validate every caller-supplied `path_id` against
it (via `Catalogue.Lookup`, a case-insensitive prefix-family match) at
all four HTTP entry points.**

```go
// internal/domain/pathcatalog/path_definition.go — a byte-for-byte
// mirror of fulfillment-execution's and wes-work-planning's own
// packages, since all three read the SAME file and must agree on its
// matching semantics.
type PathDefinition struct {
	Id                   string
	MatchPrefix          string
	RequiredCapabilities []string
}

func (c *Catalogue) Lookup(id string) (PathDefinition, error) // ErrUnknownPath
```

```go
// internal/adapters/outbound/filecatalog/loader.go — identical schema
// and boot-time failure contract to the other two services' loaders.
func Load(path string) (*pathcatalog.Catalogue, error)
```

Wiring: `Handler` gains a `Catalogue ports.PathCatalogue` field and a
`validatePathId` helper, called from `proposePathPlan`,
`commitShiftPlan` (per line), `assignLabor`, and `staffingGap` — every
handler that accepts a caller-supplied `path_id`, including the
read-only staffing-gap lookup, since an unrecognized `path_id` there is
a genuinely different failure than "no plan exists yet for this valid
path" (`ports.ErrNotFound`).

_Amended 2026-10 (ADR-conformance): on the nil-catalogue question._ The
`Handler` type's zero value (nil `Catalogue`) makes `validatePathId` a
no-op, which looks like validation being optional. It is not: both
deployables that accept caller-supplied path ids construct the catalogue
through `composition.BuildCatalogue`
(`internal/composition/catalogue.go`), which **fails closed at boot** —
a missing/malformed file, unset `KAFKA_BROKERS`, or a kafka catalogue
that never replays within `WaitReadyTimeout` returns an error that stops
the process before it serves anything. A nil catalogue therefore only
ever exists as a unit-test seam (the Handler zero value used by tests
that predate the field), never as a runtime state; the handler-level nil
branch is defensive, not an operating mode. Making the HTTP constructor
panic on nil instead was considered and rejected: it would break every
existing test fixture for a state production cannot reach, and the
composition root is the layer that already owns the fail-closed
contract.

`cmd/workforce/main.go` loads the catalogue once, before any adapter
stands up, from `PATH_CATALOGUE_FILE` (default
`/etc/workforce-management/process-paths.yaml`); a load failure is
fatal — the process exits before serving any traffic, mirroring the
other two services' identical boot-time contract.

### What this decision does NOT do

- It does not change `shared.PathId`'s shape — it is still a plain
  string type. Only what values are ACCEPTED at each entry point
  changes.
- It does not give this service ownership of the catalogue file, and
  does not add a network dependency on another service — the catalogue
  is a local file read once at boot.
- It does not touch any existing `AssociateShift`, `LaborAssignment`,
  or `ShiftPlan` invariant (certification gating, break state, max
  hours, planned-heads-vs-installed) — only how a `path_id` is
  validated before those invariants ever run.

## Consequences

### Easier

- **A malformed or stale `path_id` supplied to any of this service's
  four endpoints is now loud and traceable** instead of silently
  seeding a plan/proposal/assignment nothing downstream will ever route
  real work through.
- **One schema, shared across all three consuming services, zero
  forking** — this completes the fleet-wide rollout `fulfillment-execution`
  and `wes-work-planning` already shipped.

### Harder

- **A new boot-time external dependency**: this service now refuses to
  start without a readable, valid catalogue file — the same trade the
  other two services already accepted.
- **A breaking behavior change** for any caller currently relying on
  this service accepting an undeclared `path_id`. Any such request now
  fails with `pathcatalog.ErrUnknownPath` (mapped to HTTP 400 via the
  new `unknown-path-id` RFC 7807 problem type) instead of silently
  succeeding. Called out explicitly here, and it required updating one
  existing test fixture (`TestAssignLabor_RejectsMissingCertification`
  previously used `"hazmat"` as a `path_id`, which was never a real
  declared process path — the test's actual intent, an associate
  lacking the required certification, is unaffected by switching the
  fixture to `"pick"`).

## Verification

Domain layer (`internal/domain/pathcatalog/path_definition_test.go`): 9
tests, including a dedicated real-fleet-variants regression test
(`TestCatalogue_Lookup_RealFleetPathIdVariants`) covering this service's
own actual `path_id` forms (`"pick"`, `"pick-zone-a"`,
`"pack-station-3"`) — the exact regression class
`fulfillment-execution`'s ADR-0017 addendum documents, avoided here from
the start.

Adapter layer (`internal/adapters/outbound/filecatalog/loader_test.go`):
9 tests covering every documented failure mode plus a real-integration
test, gated on `WAREHOUSE_INFRA_CATALOGUE_PATH`, that loads the ACTUAL
file `warehouse-infra` publishes.

HTTP layer (`internal/adapters/inbound/http/router_test.go`): new
`TestProposePathPlan_RejectsUnknownPathId`,
`TestProposePathPlan_ResolvesRealFleetPathIdVariant`,
`TestCommitShiftPlan_RejectsUnknownPathId`,
`TestAssignLabor_RejectsUnknownPathId`,
`TestStaffingGap_RejectsUnknownPathId` — one failing-path test per
entry point, plus the real-variant proof.

`go test ./... -race` (all packages, including `internal/architecture`'s
hexagonal fitness tests and the godog BDD suite), `golangci-lint run
./...` (0 issues), `make check-all` (fmt/vet/build/lint/test/coverage
97.9%/arch-test/bdd), and `gremlins unleash` on both new packages (100%
efficacy/100% mutator coverage on each) all pass.
