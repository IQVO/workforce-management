---
name: how-to-add-a-rest-endpoint
description: Add or change a REST endpoint in this service in the fleet's hexagonal order (domain invariant, use case, port, HTTP adapter, apis/openapi.yaml, generated docs, godog scenario). Use when touching internal/adapters/inbound/http, apis/openapi.yaml, or exposing a use case over HTTP.
---

# How to add a REST endpoint

Use when asked to add a new REST use case/endpoint to this service. Follow
this order — domain first, adapter last — never the reverse; writing the
HTTP handler before the domain invariant it enforces produces handlers
that validate nothing and use cases that get bypassed.

This walks the exact path `GET /paths/{pathId}/staffing-gap` took
(`internal/application/usecases/get_staffing_gap.go` +
`internal/adapters/inbound/http/router.go`'s `staffingGap` handler) as the
concrete worked example — read those two files alongside this guide.

## 1. Domain first: does an invariant already exist, or do you need one?

Check `internal/domain/<aggregate>/` for the rule this endpoint enforces.
A REST endpoint should almost never contain business logic itself — it
decodes a request, calls a use case, encodes the result. If the operation
needs a new domain rule (e.g. `shiftplan`'s
`plannedHeads(path) <= installedStations(path)` invariant), add it to the
aggregate/value-object in `internal/domain/`, with its own table-driven
unit test, BEFORE touching the application or adapter layers.

## 2. Application: define the use case

Add a new file in `internal/application/usecases/` (one file per use
case, this repo's convention — not one giant `usecases.go`). Shape, from
the real `GetStaffingGap`:

```go
package usecases

// StaffingGap is the read model: plannedHeads vs active assignments for a
// path. It surfaces the gap; it never decides or moves anyone.
type StaffingGap struct {
    PathId          shared.PathId
    PlannedHeads    int
    ActiveHeads     int
    Understaffed    bool
    ObservedIdlePct *float64
}

// GetStaffingGap computes the staffing gap for a path within a building's
// committed shift plan, raising PathUnderstaffed when active assignments
// fall short of plannedHeads.
type GetStaffingGap struct {
    ShiftPlans  ports.ShiftPlanRepo   // driven ports only — never a concrete adapter
    Assignments ports.AssignmentRepo
    Events      ports.EventPublisher  // this raises PathUnderstaffed
    Clock       ports.Clock           // never call time.Now() directly
    UnitOfWork  ports.UnitOfWork      // brackets the Publish — see how-to-test's ADR-0016 note
}

func (uc *GetStaffingGap) Execute(ctx context.Context, buildingId, shiftId string, pathId shared.PathId) (StaffingGap, error) {
    // 1. load aggregate(s) via the port (ShiftPlans.FindByBuildingAndShift)
    // 2. call domain methods to apply the rule (sp.PlannedHeadsFor(pathId) —
    //    never inline the invariant here — that belongs in internal/domain/)
    // 3. persist via the port, if this use case mutates state
    // 4. publish the domain event via Events, wrapped in UnitOfWork, if any
    // 5. return the result
}
```

Add the port to `internal/application/ports/` if it doesn't exist yet —
this repo's `internal/architecture/` fitness tests enforce that ports
packages contain interfaces ONLY; a struct or function there fails CI
(ADR-0007, `make arch-test`).

Write the use case's unit test against the in-memory adapter
(`internal/adapters/outbound/memory/`) — never a real Postgres/HTTP call
in a unit test. Cover the success path AND the domain-rule failure path.

## 3. Adapter: wire the HTTP handler

In `internal/adapters/inbound/http/`:

1. `dto.go` — add the request/response DTO structs (JSON tags). DTOs live
   ONLY in the adapter layer — domain types never carry JSON tags. See
   `proposePathPlanRequest`/`proposePathPlanResponse` for the naming
   convention this repo uses.
2. `router.go` — add the route in `NewRouter`
   (`r.Get("/paths/{pathId}/staffing-gap", h.staffingGap)`) and the
   handler function:
   - decode + validate the request, converting to domain value objects
     immediately (`shared.NewPathId`, `shared.NewAssociateId`, etc.) — a
     bad value fails here as an RFC 7807 validation error, never reaches
     the use case
   - **if the endpoint accepts a caller-supplied `path_id`, call
     `h.validatePathId(pathId)` before invoking the use case** — this
     repo validates every `path_id` entry point (propose, commit,
     assign, staffing-gap) against the fleet's process-path catalogue
     (ADR-0013); a new path-accepting endpoint that skips this silently
     reintroduces the exact "undeclared paths silently accumulate" bug
     ADR-0013 closed
   - call the use case's `Execute`
   - map use-case errors to HTTP status via `writeError`/`statusFor`
     (check `errors.go` for the existing error→status mapping before
     adding a new error type — a new domain error needs an entry in both
     `statusFor` and `categoryFor`)
   - encode the domain result back to the response DTO and `writeJSON`
3. Add the new use case field to the `Handler` struct and wire it in the
   composition root (`cmd/workforce/main.go`).

Write at least one httptest per endpoint in `router_test.go`: one success
path, one error path (validation failure AND/OR the domain-rule failure,
whichever this endpoint can produce). `TestProposePathPlan_RejectsUnknownPathId`
and its four siblings in ADR-0013 are the pattern to copy for any new
path-accepting endpoint's failing-path coverage.

## 4. Contract: update OpenAPI, then regenerate docs

Add the path to `apis/openapi.yaml` (request/response schemas, the RFC
7807 problem-detail response for each error case — see the existing
`/paths/{pathId}/staffing-gap` entry for the shape, including its `404`
`resource-not-found` example).

Regenerate the Docusaurus REST reference — this repo's `docs-api-drift`
CI job fails the PR if you skip this:

```bash
cd docs
npm ci
npm run clean-api-docs   # docusaurus clean-api-docs all
npm run gen-api-docs     # docusaurus gen-api-docs all
```

`docs-api-drift` (`.github/workflows/ci.yml`) runs the same two commands
and `git diff --exit-code -- docs/api-reference/rest` — any drift fails
the PR outright, not just a warning.

## 5. Behaviour: add a godog scenario

If this endpoint is user-facing behaviour (not purely internal
plumbing), add a step/scenario under `features/` exercising it
end-to-end against the real HTTP server — see `features/staffing_gap.feature`
for the exact shape this repo's `bdd` CI job expects (Given/When/Then over
real HTTP wired to in-memory adapters, ADR-0006).

## 6. Verify before opening the PR

```bash
make check       # fmt-check vet build lint test
make check-all    # + coverage (90% gate) + arch-test + bdd
```

`make coverage` gates `./internal/domain/...,./internal/application/...`
at 90% — a new use case with no test on its failure path is the most
common way to miss this gate.
