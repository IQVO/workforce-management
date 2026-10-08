---
paths:
  - "internal/**"
  - "cmd/**"
  - "features/**"
  - "apis/**"
  - "web/**"
---

# Domain model — ubiquitous language, aggregates, events, use cases, REST API

## Ubiquitous Language (use these exact names — do not invent synonyms)

- **ShiftPlan** — the committed split of headcount across paths for one shift.
  ONE per Site per shift (keyed by `siteCode` + `shiftId`; `buildingId` is the
  deprecated alias of `siteCode`, same value, same column, ADR-0035). Contains PathPlan lines: path, plannedHeads,
  plannedRate, plannedHours. Committed by a human; the software proposes
  (charge per path / planned rate = heads needed), a human commits it.
- **AssociateShift** — who is on, their certifications, their breaks, and
  optionally the canonical Site code they work at (`siteCode`, the
  facility-layout Site code; ADR-0034). Owned here, referenced everywhere else
  (e.g. Fulfillment Execution reads certifications to gate station claims, but
  never writes here).
- **LaborAssignment** — one associate on one path for an interval. INVARIANT:
  exactly one ACTIVE assignment per associate at a time. The assignment MUST
  satisfy the path's certification requirement (reject if uncertified).
- **Certification** — a named qualification (e.g. "pack", "hazmat", "pick").
  An associate untrained on a path cannot be assigned to it; training is
  itself a path that consumes hours (do not special-case that here — just
  enforce the gate on assignment). `"hazmat"` is a REAL, in-use value, not
  hypothetical: a path literally named `"hazmat"` requires the associate hold
  the `"hazmat"` certification, via the existing
  path-name-equals-certification-name convention — no new code was needed to
  support it (ADR-0009). This is the independent, path-level half of hazmat
  handling; `fulfillment-execution` separately gates hazmat at the
  station-capability level for individual task claims — different bounded
  context, different mechanism, same real-world concern.
- **PathUnderstaffed** — a flag, not a decision: plannedHeads(path) not
  currently met by active assignments (optionally scoped to one Site: the event
  carries an optional `site_code`, omitted when the query was unscoped; a
  consumer treats absence as "fleet-wide", ADR-0034). Surfacing the gap, not moving anyone,
  is this context's job — moving people is a human call recorded via
  AssignLabor.
- What this context explicitly does NOT do: it does not link an associate to
  a task, does not dispatch work, and does not decide rebalancing — it only
  makes the labor picture legible and enforces the invariants below.

## Aggregates & invariants (enforce in domain, unit-tested)

- **ShiftPlan**: `plannedHeads(path) <= installedStations(path)` — the same
  invariant Work Planning enforces on its own PathPlan; enforce it here too,
  independently, since this is the aggregate that actually commits headcount.
  Also `plannedHeads(path) <= liveInstalledCapacity(path)` fetched fresh from
  fulfillment-execution on every commit — a SECOND, independent ceiling
  (ADR-0014; see rules/integrations.md). Sum of plannedHours per associate
  must not exceed a shift's max hours.
- **LaborAssignment**: exactly ONE active assignment per associate at a time
  (no double-booking across paths) — enforced **by construction**: assigning
  a second path always ends the first active one and raises
  `LaborReassigned`, rather than rejecting on conflict. Assignment requires
  the associate holds the path's required certification, or it is rejected.
- **AssociateShift**: cannot be assigned while on a logged break; hours
  logged must not exceed a configured max-hours-per-shift limit
  (`MAX_HOURS_PER_SHIFT`, default 8).
- Read models (heads-planned-vs-active per path, per-associate utilization)
  are PROJECTIONS built from events — NOT state stored redundantly on
  aggregates.

Design decisions worth calling out:

- **Path → required certification is a naming convention**, not a separate
  concept: a path's required certification is the `Certification` with the
  same name as its `PathId` (path `"pack"` requires certification `"pack"`).
- **`GetStaffingGap` takes `siteCode`/`shiftId` as query parameters**
  (`GET /paths/{pathId}/staffing-gap?siteCode=&shiftId=`; `buildingId` is the
  deprecated alias, ADR-0035) because `ShiftPlan` is keyed by site + shift, and
  a path's planned heads only make sense within one committed plan.
- **`PathPlan.plannedHours <= plannedHeads * maxHoursPerShift`** is how "sum
  of hours valid" is enforced on `ShiftPlan`.

## Domain events (past tense — use these exact names)

`ShiftPlanProposed`, `ShiftPlanCommitted`, `AssociateShiftStarted`,
`AssociateCertified`, `AssociateBreakStarted`, `AssociateBreakEnded`,
`LaborAssigned`, `LaborReassigned`, `PathUnderstaffed`, `AssociateShiftEnded`.

Full catalog is documented in `apis/asyncapi.yaml`; only `ShiftPlanCommitted`
is published externally today (see rules/integrations.md) — the rest are
raised and consumed in-process.

## Use cases (application layer, `internal/application/usecases/`)

1. `StartAssociateShift(associateId, certifications, siteCode?) -> AssociateShift`
2. `CertifyAssociate(associateId, certification)` — adds a certification
3. `ProposePathPlan(siteCode, charge-per-path, plannedRate) -> proposed heads`
   — pure computation: `heads = ceil(charge / resolvedRate)`; does not commit.
   `plannedRate` is optional — omit it (or send `<= 0`) to fall back to a
   real measured rate from labor-performance (ADR-0012); response includes
   `resolvedRate` + `rateSource` (`"caller"` or `"measured"`).
4. `CommitShiftPlan(siteCode, pathPlans) -> ShiftPlan` — validates
   `plannedHeads <= installedStations` AND `plannedHeads <= live installed
   capacity`; a human-initiated commit, not automatic.
5. `AssignLabor(associateId, pathId) -> LaborAssignment` — validates
   certification, ends any prior active assignment for this associate.
6. `StartBreak(associateId)` / `EndBreak(associateId)`
7. `GetStaffingGap(pathId, siteCode?) -> plannedHeads vs activeAssignments` read
   model; may raise `PathUnderstaffed`. With `siteCode` only assignments of
   associates whose active shift is at that Site count; without it, all do
   (legacy rows with no site count only unscoped, ADR-0034). `siteCode` is both
   the plan key and the scope (ADR-0035). A call giving only the legacy
   `buildingId` is NOT auto-scoped. A differing `siteCode`+`buildingId` pair is
   422 `conflicting-site-and-building` on every surface EXCEPT the three
   staffing-gap reads, which keep ADR-0034's pair (`buildingId` = plan key,
   `siteCode` = scope) for the whole deprecation window, because it is the only
   way to site-scope plans still stored under a legacy id such as `bldg-1`.
   Removing `buildingId` is a later, breaking ADR.
8. `EndAssociateShift(associateId)` — closes all active assignments, raises
   `AssociateShiftEnded`.

## REST API (inbound adapter, source of truth: `apis/openapi.yaml`)

| Method | Path | Use case |
|---|---|---|
| POST | `/associates/{id}/start-shift` | StartAssociateShift (optional `siteCode`, ADR-0034) |
| POST | `/associates/{id}/certifications` | CertifyAssociate |
| POST | `/paths/{pathId}/plan/propose` | ProposePathPlan |
| POST | `/shift-plans` | CommitShiftPlan (requires `Idempotency-Key`, ADR-0027) |
| POST | `/associates/{id}/assignments` | AssignLabor (requires `Idempotency-Key`, ADR-0027) |
| POST | `/associates/{id}/break/start` | StartBreak |
| POST | `/associates/{id}/break/end` | EndBreak |
| GET | `/paths/{pathId}/staffing-gap` | GetStaffingGap (optional `siteCode`) |
| GET | `/sites/{siteCode}/shifts/{shiftId}/staffing-gap` | GetStaffingGap.ExecuteAll (ADR-0029; canonical, ADR-0035) |
| GET | `/buildings/{buildingId}/shifts/{shiftId}/staffing-gap` | DEPRECATED alias of the route above (answers `Deprecation: true`; removed only by a later ADR) |
| POST | `/associates/{id}/end-shift` | EndAssociateShift |
| GET | `/healthz` | liveness |
| GET | `/readyz` | readiness — 503 once graceful shutdown starts (ADR-0022); declared in `apis/openapi.yaml` |

All bodies are JSON. Every error response is RFC 7807
`application/problem+json` (ADR-0005): `type` identifies the error CATEGORY
(fixed, does not need to resolve), `title` is the fixed category summary,
`detail` carries the specific occurrence message, `instance` is the request
path.

Whenever the REST surface (paths, request/response shapes, status codes)
changes, update `apis/openapi.yaml` AND regenerate
`docs/docs/api-reference/rest/*` via `cd docs && npm run gen-api-docs -- all`
— see the top-level CLAUDE.md's Definition of Done.
