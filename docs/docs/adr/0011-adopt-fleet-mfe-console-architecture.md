---
id: 0011-adopt-fleet-mfe-console-architecture
title: 0011. Adopt the fleet-wide micro-frontend console architecture (workforce-mfe)
sidebar_label: 0011. Adopt fleet MFE console
sidebar_position: 12
description: An adoption record — this context conforms to warehouse-ops-agent's fleet-wide ADR-0002 by shipping its own Module Federation remote, scoped to what its REST API can actually support today.
---

# 0011. Adopt the fleet-wide micro-frontend console architecture (workforce-mfe)

## Status

Accepted — the deferred all-paths read endpoint and the screen's adoption
of it are recorded in [ADR-0029](./0029-all-paths-staffing-gap-endpoint.md)
and the amended Decision bullet below.

## Context

`warehouse-ops-agent`'s [ADR-0002](https://github.com/claudioed/warehouse-ops-agent/blob/develop/docs/docs/adr/0002-micro-frontend-console-architecture.md)
made a fleet-wide decision, binding on all six bounded-context repos: there
will be one operator console (`warehouse-console`), composed at runtime via
Module Federation from one remote per bounded context, each remote owned,
built, and released inside that context's own repo, talking only to that
context's own REST API. This context is one of the six named there and has no
standing to re-litigate the fleet-wide shape — the only decision left at this
context's level is *how* it fulfills that decision inside its own repo, given
what its own REST API today actually exposes.

That question was not trivial. `warehouse-ops-agent`'s ADR-0002 records the
canonical remote name (`workforce-mfe`, port `5185`) and the shared-package
convention (`@warehouse/ui-kit`, `file:../warehouse-ui-kit`), but this
context's own REST API surface constrains what that remote can show. This
service's only read model over labor state is
`GET /paths/{pathId}/staffing-gap?buildingId=&shiftId=` — `ShiftPlan` is keyed
by building and shift, per [ADR-0003](./0003-certification-gated-single-active-assignment.md),
so there is no endpoint that lists staffing gaps across every path for a
building/shift without the caller already knowing each `pathId` to ask about.
A fleet-wide "all paths at once" view (the kind of list-first screen
`order-mgmt-mfe` and other remotes default to) is therefore not buildable from
today's API without first adding a new list-style GET endpoint — the same
category of gap `warehouse-ops-agent`'s own ADR-0002 flagged for
`order-management`'s `GET /orders?status=`. Blocking this adoption on that
endpoint, versus shipping the narrower screen the existing API already
supports and following up separately, was the concrete choice at this
context's level.

CORS was the other prerequisite: this service, like the other three
consumed directly by the console (`order-management`, `inventory-storage`,
`fulfillment-execution` — the Order Lifecycle BFF's four hops), never had a
browser client before and had no CORS middleware. That was added ahead of
this record, as its own PR (`feature/console-cors`), following the same
`go-chi/cors` / `CORS_ALLOWED_ORIGINS` shape ADR-0002 specifies for every
service the console touches directly.

## Decision

**We adopt `warehouse-ops-agent`'s ADR-0002 in full and record here only the
choices specific to this context's implementation of it.**

- A new `web/` directory (`workforce-mfe`, Vite + React, Module Federation
  remote, dev port `5185`) is added to this repo, built and released inside
  this repo's own CI — no coordination with any sibling repo required to ship
  or roll back this remote, exactly as ADR-0002 requires.
- It consumes `@warehouse/ui-kit` via `file:../../warehouse-ui-kit` (same
  workspace-relative convention every remote uses) so this context's status
  rendering (`StatusPill` for `Understaffed`/`Staffed`) is visually identical
  to how the other five remotes render their own domain statuses.
- It talks only to `WORKFORCE_API_BASE` — this service's own REST API — never
  another sibling's API and never a shared database. No new backend surface
  was added beyond the CORS middleware already shipped in
  `feature/console-cors`.
- **Scope is deliberately narrower than a fleet-standard list-first
  dashboard.** The shipped screen was originally a staffing-gap-**by-path**
  lookup: an operator supplies `pathId` + `buildingId` + `shiftId` and sees
  planned vs. active headcount for that one path, because that was the one
  read model `GET /paths/{pathId}/staffing-gap` supported when this record
  was written. _Amended 2026-10: the deferred all-paths endpoint shipped
  (`GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap`,
  operationId `listStaffingGapsForShift`), and the screen now defaults to
  that all-paths list view for a building/shift while keeping the
  single-path lookup as an explicit "By path" mode. The two endpoints
  compute a path's gap identically, so the views cannot disagree._
- **A fleet-wide "all paths, one building/shift" list endpoint was an
  explicit, deferred fast-follow**, not an oversight — the same category of
  gap ADR-0002 itself flagged for `order-management`. _Amended 2026-10: it
  has since shipped (see above); the deferral note is retained for the
  record of what was true at decision time._

## Consequences

### Easier

- This context's operator-facing surface now follows the same ownership
  rule as its REST API: the team that owns `workforce-management`'s domain
  also owns the screen that renders its own data, and a future API change
  lands in the same PR as the screen change that depends on it.
- Visual consistency with the other five remotes is a compile-time
  consumption of `@warehouse/ui-kit`, not a convention this context has to
  remember or re-derive.
- The remote can be built, tested, deployed, or rolled back entirely inside
  this repo's own CI/CD, with zero coordination cost against
  `warehouse-console` or any sibling remote.

### Harder

- The shipped screen exposes a real gap in this context's own read-model
  surface (no list-by-building/shift endpoint) that was previously invisible
  because nothing outside this service had needed it. That gap is now
  user-visible instead of merely theoretical.
- CORS is now permanent surface on this service that did not exist before —
  `CORS_ALLOWED_ORIGINS` must be kept current as `warehouse-console`'s real
  deployed origin is added, or the failure mode is a silent browser-side
  "Failed to fetch," not a loud backend error.
- This context now has a frontend build/release pipeline (`web/`) in
  addition to its Go service — more CI surface to keep green, and a second
  place (alongside the Go module) where a dependency bump can break a build.

### Now true

- `workforce-management` fulfills its named obligation under
  `warehouse-ops-agent`'s fleet-wide ADR-0002 with a remote scoped honestly
  to its current API, not a screen that outruns what the backend can answer.
- The domain/integration relationships recorded in this repo's own
  [context map](../ecosystem/context-map.md) are unchanged by this decision
  — see that document's new "Presentation-layer composition" section for why
  this is deliberately not a new domain-coupling edge.
- A fast-follow endpoint (all-paths staffing gap for a building/shift) has a
  written, cross-referenced reason to exist, rather than surfacing later as
  an unexplained ad hoc addition.
