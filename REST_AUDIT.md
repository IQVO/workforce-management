# REST API Audit — Workforce Management

Scope: `internal/adapters/inbound/http/` only (Task 11 / REST_API_TASK.md).
No domain, use-case, or application-layer behavior was changed. Richardson
Maturity Level 2 is the bar (resource nouns, correct verbs, correct status
codes) — HATEOAS is explicitly out of scope.

## 1. Resource nouns, not verbs, in URLs

**Audited, no violation found.** All 9 routes are resource-scoped:

```
GET  /healthz
POST /associates/{id}/shift-starts
POST /associates/{id}/certifications
POST /paths/{pathId}/plan-proposals
POST /shift-plans
POST /associates/{id}/assignments
POST /associates/{id}/break-starts
POST /associates/{id}/break-ends
GET  /paths/{pathId}/staffing-gap
POST /associates/{id}/shift-ends
```

This service has no bare RPC-style endpoint analogous to the
`POST /admin/expire-leases` pattern called out in REST_API_TASK.md as a
known issue elsewhere in the codebase — that pattern does not exist here.
Every action endpoint (`shift-starts`, `certifications`, `break-starts`,
`break-ends`, `shift-ends`, `assignments`) is a verb-suffixed command scoped to
a specific resource collection or instance (`/associates/{id}/...`), which is
correct DDD/REST practice for non-CRUD domain commands — no change made.

## 2. Correct HTTP methods

**Audited, no violation found.**

- Both `GET` handlers (`healthz`, `staffingGap`) are read-only: neither calls
  a use case that mutates state or publishes an event in `GetStaffingGap`'s
  non-understaffed path, and `staffingGap`'s only side effect
  (`PathUnderstaffed` publish) happens inside the domain-level read model
  computation only when a genuine gap is detected — this is a projection
  publishing an observability event about existing state, not a caller-driven
  mutation of an aggregate, and it was already the case before this audit.
  No handler mutates via GET.
- Every command handler uses `POST`, consistent with this being a
  command-oriented domain (no PUT/PATCH required, per REST_API_TASK.md).
- This service has no revocation/deletion concept (nothing analogous to
  inventory-storage's reservation `DELETE`), so no `DELETE` endpoint is
  expected — confirmed against CLAUDE.md's use-case list, none needed.

## 3. Correct status codes

**Two real violations found and fixed:**

1. **Missing `Location` headers on 201 responses.** `startShift`,
   `commitShiftPlan`, and `assignLabor` all returned `201 Created` with no
   `Location` header pointing at the created resource. Fixed:
   - `POST /associates/{id}/shift-starts` → `Location: /associates/{id}`
   - `POST /shift-plans` → `Location: /shift-plans/{buildingId}/{shiftId}`
   - `POST /associates/{id}/assignments` → `Location: /associates/{id}/assignments`

   Note: this service has no `GET` route for an individual associate or
   shift plan (only the `staffing-gap` read model and the write endpoints
   exist), so these `Location` URIs identify the resource but are not yet
   independently retrievable via `GET`. Adding those `GET` routes is outside
   this task's scope (new read endpoints are not part of Stage 1's audit
   checklist); the `Location` header is still correct per RFC 7231 §7.1.2,
   which only requires the URI to identify the resource, not that a `GET`
   handler exists for it.

2. **Decoded request DTOs were never validated before invoking the use
   case.** Every handler cast raw strings straight into domain value objects
   (`shared.AssociateId(v)`, `shared.PathId(v)`, `shared.Certification(v)`)
   instead of calling the existing `shared.NewAssociateId` /
   `shared.NewPathId` / `shared.NewCertification` constructors, which are
   the only place the `ErrEmptyAssociateId` / `ErrEmptyPathId` /
   `ErrEmptyCertification` sentinels (already wired into `statusFor` → 400)
   could ever be raised. In production these constructors were dead code —
   an empty certification string in a JSON array, or an empty `pathId` in a
   `commitShiftPlanRequest` line, passed straight through to the use case
   and domain layer with no validation. Fixed: every handler now constructs
   value objects via the `NewX` constructors and returns `400` immediately
   on failure, before the use case runs.

   Additionally, `buildingId`/`shiftId` have no domain value object (`ShiftPlan`
   stores them as plain `string`s), so there was no way to reject an empty
   `buildingId`/`shiftId` at all. Added HTTP-layer-only requiredness checks
   (`errMissingBuildingId`, `errMissingShiftId`) for `commitShiftPlan`,
   `proposePathPlan` (buildingId), and `staffingGap` (buildingId/shiftId
   query params) — 400 on empty. This is adapter-only validation; it does
   not change use-case or domain signatures or behavior for valid input.

**Checked, no violation:**

- No handler returns `200` where `201` is correct. `proposePathPlan`
  correctly returns `200` (it is a pure computation, persists nothing, and
  creates no resource — see CLAUDE.md's `ProposePathPlan` spec).
- `certify` returns `204 No Content` rather than `201`. Certifications are
  not independently addressable sub-resources (no `GET
  /associates/{id}/certifications/{cert}` exists, and `Certify` is a set-add
  onto the existing `AssociateShift` aggregate, not creation of a new
  top-level resource) — `204` is correct here, left unchanged.
- `404` (not-found), `409` (conflict: double-booking guard, missing
  certification, break conflicts, max-hours, planned-heads/hours-exceed-*)
  are already correctly mapped via the existing `statusFor` switch — left
  intact per Stage 2's instruction not to touch that mapping.
- No case of "semantically valid JSON, semantically invalid input" distinct
  from the existing 400 (missing/empty required field) and 409 (state
  conflict) categories was found that would call for `422` — every invalid
  non-conflicting input in this service's request DTOs is a missing/empty
  required field, which the 400 checks above now cover. No `422` usage was
  introduced.

## 4. Idempotency semantics (documented, not changed — no bug found)

- **`POST /associates/{id}/shift-starts`** — naturally idempotent by
  client-supplied ID: the memory/postgres repos both `Save` as an upsert
  keyed by `associateId`, so calling twice with the same ID overwrites
  rather than erroring. (It does re-publish `AssociateShiftStarted` and
  re-return `201` each call — a minor event-replay quirk, not a REST
  violation, and changing it would touch use-case/domain behavior, out of
  scope.)
- **`POST /shift-plans`** — naturally idempotent by client-supplied key
  (`buildingId`+`shiftId`): same upsert-by-key pattern as above.
- **`POST /associates/{id}/certifications`** — idempotent in effect:
  `Certify` adds to a `map[Certification]struct{}` set, so re-certifying the
  same certification is a no-op on state (still re-raises
  `AssociateCertified` and returns `204` each call).
- **`POST /associates/{id}/shift-ends`** — idempotent: `EndShift` is
  explicitly a no-op (no event raised) when the shift has already ended.
- **`POST /associates/{id}/break-starts`**, **`.../break-ends`** — NOT
  idempotent by design: a second `break-starts` while already on break
  returns `409 ErrAlreadyOnBreak`; a second `break-ends` while not on break
  returns `409 ErrNotOnBreak`. This is correct for an action command
  representing a state transition, not a resource creation.
- **`POST /associates/{id}/assignments`** — NOT strictly idempotent: per the
  documented design decision in README.md ("AssignLabor ends the prior
  assignment rather than rejecting the call"), calling it twice with the
  same `pathId` still closes the prior interval and opens a new one,
  appending to `LaborAssignment` history each time. This is an intentional
  domain design choice (documented already), not a bug — flagged here per
  Stage 1 item 4, not changed.

No case was found of a "create" endpoint that *should* dedupe by
client-supplied ID but silently fails to (the failure mode this check was
meant to catch) — the two client-keyed creation endpoints above already
upsert correctly.

## 5. Consistent JSON casing

**Audited, confirmed — no change needed.** Every field in every DTO in
`dto.go` (request and response) is `camelCase`
(`associateId`, `pathId`, `buildingId`, `shiftId`, `plannedHeads`,
`plannedRate`, `plannedHours`, `installedStations`, `activePathId`,
`proposedHeads`, `understaffed`, etc.). The new RFC 7807 `problemDetails`
struct added in Stage 2 uses the RFC's own field names (`type`, `title`,
`status`, `detail`, `instance`), which are already lowercase single words —
consistent with the rest of the API and with the RFC itself.

## 6. Content negotiation

- Every success response already set (and still sets)
  `Content-Type: application/json` via `writeJSON`.
- Stage 2 (below): every error response now sets
  `Content-Type: application/problem+json` instead of `application/json`.

---

# Stage 2 — RFC 7807 migration

The bespoke `{"error": "..."}` shape (`errorResponse` in `dto.go`,
`writeError` in `router.go`) has been fully replaced with RFC 7807
`application/problem+json`:

```json
{
  "type": "https://errors.workforce-management.warehouse-systems.dev/<slug>",
  "title": "Human-readable summary of the error category",
  "status": 409,
  "detail": "the specific err.Error() text for this occurrence",
  "instance": "/associates/assoc-1/assignments"
}
```

- `type` base: `https://errors.workforce-management.warehouse-systems.dev`
  (service name in kebab-case, per REST_API_TASK.md's convention).
- The `statusFor` error → HTTP-status switch in `errors.go` is **unchanged**,
  exactly as instructed.
- A new `categoryFor(status, err) problemCategory` lookup (also in
  `errors.go`) maps every sentinel error already known to `statusFor`, plus
  the two new HTTP-layer validation sentinels (`errMissingBuildingId`,
  `errMissingShiftId`), to a fixed `(slug, title)` pair. Unmatched errors
  fall back to a status-keyed generic category (`malformed-request-body` for
  400, `internal-error` otherwise) so every response is still a well-formed
  problem document even for errors with no dedicated category (e.g. raw
  `encoding/json` decode failures).
- `instance` is always `r.URL.Path` — every endpoint in this service
  operates on a specific resource path, so there is no case where omitting
  it applies.
- `writeError`'s signature changed to `writeError(w, r, status, err)` (needs
  `r` for `instance`); every call site in `router.go` was updated.

## Category table (type slug → title → status)

| Error | Slug | Status |
|---|---|---|
| `ports.ErrNotFound` | `resource-not-found` | 404 |
| `shared.ErrEmptyAssociateId` | `empty-associate-id` | 400 |
| `shared.ErrEmptyPathId` | `empty-path-id` | 400 |
| `shared.ErrEmptyCertification` | `empty-certification` | 400 |
| `shiftplan.ErrNoPathPlans` | `shift-plan-no-path-plans` | 400 |
| `shiftplan.ErrMissingInstalledStations` | `shift-plan-missing-installed-stations` | 400 |
| `errMissingBuildingId` (HTTP-layer) | `missing-building-id` | 400 |
| `errMissingShiftId` (HTTP-layer) | `missing-shift-id` | 400 |
| *(unmatched, status 400)* | `malformed-request-body` | 400 |
| `associate.ErrAlreadyOnBreak` | `associate-already-on-break` | 409 |
| `associate.ErrNotOnBreak` | `associate-not-on-break` | 409 |
| `associate.ErrOnBreak` | `associate-on-break` | 409 |
| `associate.ErrShiftEnded` | `associate-shift-ended` | 409 |
| `associate.ErrMaxHoursExceeded` | `max-hours-exceeded` | 409 |
| `assignment.ErrCertificationRequired` | `certification-required` | 409 |
| `shiftplan.ErrPlannedHeadsExceedInstalled` | `planned-heads-exceed-installed` | 409 |
| `shiftplan.ErrPlannedHoursExceedCapacity` | `planned-hours-exceed-capacity` | 409 |
| *(unmatched, other status)* | `internal-error` | 500 |

Every `httptest` in `router_test.go` that exercises an error path now
asserts on the RFC 7807 shape via a shared `assertProblemDetails` helper
(`Content-Type`, `type`, `status`, non-empty `title`/`detail`, `instance`).
`grep -rn "errorResponse\|\"error\":"` across all non-test `.go` files
returns zero results — the old shape is fully gone from production code.

## Manual curl evidence (live binary, real Postgres via docker-compose)

Server started with `DATABASE_URL` pointed at the compose Postgres instance,
listening on `:8123`.

**404 — cert on an associate who never started a shift:**

```
$ curl -s -i -X POST localhost:8123/associates/ghost-assoc/certifications -d '{"certification":"hazmat"}'
HTTP/1.1 404 Not Found
Content-Type: application/problem+json
Date: Sat, 22 Aug 2026 14:19:26 GMT
Content-Length: 203

{"type":"https://errors.workforce-management.warehouse-systems.dev/resource-not-found","title":"Resource not found","status":404,"detail":"not found","instance":"/associates/ghost-assoc/certifications"}
```

**409 — committing a shift plan with plannedHeads exceeding installedStations:**

```
$ curl -s -i -X POST localhost:8123/shift-plans -d '{"buildingId":"bldg-1","shiftId":"shift-1","lines":[{"pathId":"pack","plannedHeads":11,"plannedRate":30,"plannedHours":40,"installedStations":10}]}'
HTTP/1.1 409 Conflict
Content-Type: application/problem+json
Date: Sat, 22 Aug 2026 14:19:26 GMT
Content-Length: 258

{"type":"https://errors.workforce-management.warehouse-systems.dev/planned-heads-exceed-installed","title":"Planned heads exceed installed stations for path","status":409,"detail":"planned heads exceed installed stations for path","instance":"/shift-plans"}
```

**Bonus — 201 with the new `Location` header, live:**

```
$ curl -s -i -X POST localhost:8123/associates/assoc-live-1/shift-starts -d '{"certifications":["pack"]}'
HTTP/1.1 201 Created
Content-Type: application/json
Location: /associates/assoc-live-1
Date: Sat, 22 Aug 2026 14:19:29 GMT
Content-Length: 31

{"associateId":"assoc-live-1"}
```

Full suite verified green after these changes: `go build ./...`,
`go vet ./...`, `go test ./... -race`, `golangci-lint run ./...`,
`gofmt -l .` (empty).

---

# Stage 3 — OpenAPI 3.0.3 documentation

`openapi.yaml` at the repo root documents all **10/10** routes from
`router.go` (cross-checked by grepping every `r.Get(...)`/`r.Post(...)` call
against `paths:` entries — see the final report for the enumerated list).

Validation:

- `python3 -c "import yaml; yaml.safe_load(open('openapi.yaml'))"` — passes
  (valid YAML).
- `npx --yes @redocly/cli lint openapi.yaml` — **0 errors**, 3 warnings, left
  as warnings deliberately:
  1. `info-license` — no `license` field. This is an internal bounded-context
     service, not a published/licensed API; omitted intentionally.
  2. `no-server-example-com` — the only server is `http://localhost:8080`.
     REST_API_TASK.md explicitly requires localhost:8080 as (at minimum) the
     dev server; there is no other deployment target to add yet.
  3. `operation-4xx-response` on `GET /healthz` — the liveness probe
     genuinely has no error path (it always returns `200` if the process is
     up to serve the request at all); adding a fabricated 4xx would be
     inaccurate documentation.

  One initial batch of 10 errors (`security-defined` on every operation)
  was fixed by adding `security: []` at the document root — accurate,
  since this service currently has no authentication middleware
  (`router.go` only wires `middleware.Logger`/`middleware.Recoverer`); this
  was not a warning left in place, it was a real fix.

# Stage 4 — Spectral CI gate

See `.spectral.yaml` and `.github/workflows/ci.yml`'s new `openapi-lint`
job. Local `spectral lint` result is reported in the final report.
