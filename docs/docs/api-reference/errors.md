---
id: errors
title: Errors
sidebar_label: Errors
sidebar_position: 3
description: RFC 7807 Problem Details — the shape, the status mapping, and every problem type this service emits.
---

# Errors

Every error response from this service is
[RFC 7807](https://www.rfc-editor.org/rfc/rfc7807) **Problem Details**, served
as `application/problem+json`. There is no bespoke `{"error": "..."}` shape
anywhere. Recorded as
[ADR 0005](../adr/0005-rfc-7807-problem-details.md).

## The shape

```bash
curl -i -X POST localhost:8080/associates/ghost/certifications \
  -d '{"certification":"hazmat"}'
```

```http
HTTP/1.1 404 Not Found
Content-Type: application/problem+json

{
  "type": "https://errors.workforce-management.warehouse-systems.dev/resource-not-found",
  "title": "Resource not found",
  "status": 404,
  "detail": "not found",
  "instance": "/associates/ghost/certifications"
}
```

| Member | Semantics |
| --- | --- |
| `type` | Identifies the error **category**. Fixed per sentinel domain error, not per occurrence. It is a stable identifier and **does not need to resolve** — RFC 7807 explicitly permits this. |
| `title` | The fixed, category-level human summary. Same for every occurrence of a `type`. |
| `status` | The HTTP status, duplicated in the body per the RFC. |
| `detail` | The **specific** message for this occurrence — carries the underlying `err.Error()` text. This is the only member that varies between two responses of the same category. |
| `instance` | The request path that produced the error. |

The `type`/`title` versus `detail` split is the part clients should build on:
switch on `type`, show `detail`.

## Status mapping

Typed domain errors are mapped to HTTP status in the inbound adapter — the
domain layer knows nothing about HTTP.

| Status | When |
| --- | --- |
| `400 Bad Request` | Malformed JSON, or a value object that refuses to construct (empty id, empty certification), or a missing required query/body field |
| `404 Not Found` | `ports.ErrNotFound` — the associate or the committed plan does not exist |
| `409 Conflict` | A domain invariant refused the operation. The request was well-formed; the domain said no |
| `422 Unprocessable Entity` | `shared.ErrConflictingSiteAndBuilding` — the request named the ShiftPlan key twice (`siteCode` and the deprecated `buildingId`) with different values — and an Idempotency-Key reused with a different body |
| `500 Internal Server Error` | Anything unmapped — a repository or publisher failure |
| `503 Service Unavailable` | `CommitShiftPlan` could not verify installed capacity against `fulfillment-execution` ([ADR 0014](../adr/0014-installed-capacity-ceiling.md)) — retry later |

`409` is the interesting one. It is used for **every** invariant rejection —
certification missing, associate on break, planned heads over installed
stations — because these are all "the current state of the resource conflicts
with what you asked for," which is precisely what `409` means. They are not
`422`: the payload is valid and processable, the *domain state* disagrees.

## Every problem type

Base URI: `https://errors.workforce-management.warehouse-systems.dev`

### `400` — validation

| `type` suffix | `title` | Sentinel |
| --- | --- | --- |
| `/empty-associate-id` | Associate id must not be empty | `shared.ErrEmptyAssociateId` |
| `/empty-path-id` | Path id must not be empty | `shared.ErrEmptyPathId` |
| `/unknown-path-id` | Unrecognized process-path id | `pathcatalog.ErrUnknownPath` |
| `/empty-certification` | Certification must not be empty | `shared.ErrEmptyCertification` |
| `/missing-building-id` | siteCode is required (buildingId is a deprecated alias) | `shared.ErrMissingSiteKey` — neither `siteCode` nor `buildingId` was sent. The slug keeps its historic name so existing clients keep matching it ([ADR 0035](../adr/0035-sitecode-converges-building-id.md)) |
| `/missing-shift-id` | shiftId is required | HTTP-layer sentinel |
| `/shift-plan-no-path-plans` | Shift plan must have at least one path plan line | `shiftplan.ErrNoPathPlans` |
| `/shift-plan-missing-installed-stations` | Missing installed station count for path | `shiftplan.ErrMissingInstalledStations` |
| `/malformed-request-body` | Malformed request body | fallback for any unmatched `400` |

### `404` — not found

| `type` suffix | `title` | Sentinel |
| --- | --- | --- |
| `/resource-not-found` | Resource not found | `ports.ErrNotFound` |

### `409` — invariant conflict

| `type` suffix | `title` | Sentinel |
| --- | --- | --- |
| `/certification-required` | Associate lacks the certification required for this path | `assignment.ErrCertificationRequired` |
| `/associate-on-break` | Associate on break cannot be assigned | `associate.ErrOnBreak` |
| `/associate-already-on-break` | Associate is already on break | `associate.ErrAlreadyOnBreak` |
| `/associate-not-on-break` | Associate is not on break | `associate.ErrNotOnBreak` |
| `/associate-shift-ended` | Associate shift has already ended | `associate.ErrShiftEnded` |
| `/max-hours-exceeded` | Max hours per shift exceeded | `associate.ErrMaxHoursExceeded` |
| `/planned-heads-exceed-installed` | Planned heads exceed installed stations for path | `shiftplan.ErrPlannedHeadsExceedInstalled` |
| `/exceeds-installed-capacity` | Planned heads exceed the live installed capacity reported by fulfillment-execution | `shiftplan.ErrExceedsInstalledCapacity` |
| `/planned-hours-exceed-capacity` | Planned hours exceed capacity for planned heads within max hours per shift | `shiftplan.ErrPlannedHoursExceedCapacity` |

### `422` — unprocessable

| `type` suffix | `title` | Sentinel |
| --- | --- | --- |
| `/conflicting-site-and-building` | siteCode and its deprecated alias buildingId have different values | `shared.ErrConflictingSiteAndBuilding` — a request carried both names of the ShiftPlan key with different values ([ADR 0035](../adr/0035-sitecode-converges-building-id.md)) |

The `422 Unprocessable Entity` status is also used by the idempotency middleware
(`/idempotency-key-reused`, [ADR 0027](../adr/0027-idempotency-key-middleware.md)).

### `500` — unmapped

| `type` suffix | `title` |
| --- | --- |
| `/internal-error` | Internal server error |

### `503` — dependency unavailable

| `type` suffix | `title` | Sentinel |
| --- | --- | --- |
| `/installed-capacity-unavailable` | Could not verify installed capacity against fulfillment-execution | `ports.ErrInstalledCapacityUnavailable` |

## Worked example: the certification gate

```bash
curl -i -X POST localhost:8080/associates/assoc-1/assignments \
  -d '{"pathId":"hazmat"}'
```

```http
HTTP/1.1 409 Conflict
Content-Type: application/problem+json

{
  "type": "https://errors.workforce-management.warehouse-systems.dev/certification-required",
  "title": "Associate lacks the certification required for this path",
  "status": 409,
  "detail": "associate lacks the certification required for this path",
  "instance": "/associates/assoc-1/assignments"
}
```

The client switches on `type` — a stable, machine-readable identifier — and
displays `detail`. Adding a new failure mode to the domain adds a new `type`;
it never changes the shape or the meaning of an existing one.

## Guarantees

- **Every** error path produces a well-formed problem document. Errors that
  match no category fall back to a status-keyed generic one, so there is no
  route through the adapter that emits a bare string or an empty body.
- The `Content-Type` is `application/problem+json` on every error, and
  `application/json` on every success.
- `title` never varies for a given `type`; `detail` always may.
- Every mapping above is asserted by an `httptest` test against the real chi
  router, and the `409` paths are additionally covered by the
  [godog acceptance specs](../adr/0006-godog-bdd-acceptance-tests.md).
