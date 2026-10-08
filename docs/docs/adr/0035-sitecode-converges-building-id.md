---
id: 0035-sitecode-converges-building-id
slug: /adr/0035-sitecode-converges-building-id
title: 0035. siteCode is the canonical name of the ShiftPlan key; buildingId is a deprecated alias
sidebar_label: 0035. siteCode converges buildingId
sidebar_position: 36
description: "ADR 0035 — Site is the ubiquitous-language term. siteCode becomes the canonical name of the ShiftPlan key on every REST, MCP and event surface; buildingId stays as a deprecated alias with the same value and the same stored column. Additive: no migration, no Kafka key or CloudEvents subject change, nothing removed. Removing buildingId is a later breaking ADR."
---

# 0035. `siteCode` is the canonical name of the ShiftPlan key; `buildingId` is a deprecated alias

## Status

Accepted. Settles the question [ADR 0034](./0034-site-scoped-staffing-gap.md)
left open ("`ShiftPlan` stays keyed by `buildingId + shiftId` (mapping a
building to a site is not decided here)") without editing it; where this record
and ADR 0034 disagree about the name of the plan key, this record wins.
Refines [ADR 0029](./0029-all-paths-staffing-gap-endpoint.md),
[ADR 0026](./0026-cloudevents-mandatory-event-envelope.md) and
[ADR 0023](./0023-kafka-integration-producer-partition-key.md) without editing
them.

## Context

Two identifiers named one concept. The `ShiftPlan` was keyed by `buildingId`;
ADR 0034 added `siteCode` (the facility-layout **Site** code, the same value
warehouse-planning carries as `site_id`) to scope the staffing gap, and
deliberately did not map one to the other. Callers had to pick a consistent
`buildingId`/`siteCode` pair themselves. That is a ubiquitous-language defect:
the fleet's term is **Site**, and "building" names nothing the other contexts
know.

Decision 19 (architect, 2026-10-06, final): converge on `siteCode`, deprecate
`buildingId`, and remove `buildingId` only in a **later breaking ADR**. This
record is the additive, backward-compatible step.

## Decision

1. **One concept, one name.** The term is **Site**; "building" is deprecated.
   `siteCode` is the canonical name of the ShiftPlan key. `buildingId` is a
   deprecated **alias** with the **same value** and the **same stored column**
   (`shift_plan.building_id`; the `path_plan`/`shift_plan` tables are untouched).
   **No migration is needed and none is added.**
2. **Write requests** (`POST /paths/{pathId}/plan/propose`,
   `POST /shift-plans`; MCP `propose_path_heads`) accept `siteCode` **or**
   `buildingId`. At least one is required (neither → `400 missing-building-id`;
   the slug keeps its historic name so clients that match it keep working).
   Both present and **equal** is fine; both present and **different** is
   `422 conflicting-site-and-building` (new RFC 7807 slug, documented in the
   errors page and covered by the problem-catalogue tests). Surrounding
   whitespace is trimmed and a blank value counts as absent.
3. **Responses** that return a ShiftPlan (`POST /shift-plans`, MCP
   `propose_path_heads`, MCP `get_staffing_gap`) carry `siteCode` **and** keep
   `buildingId`, one value. The by-path / list staffing-gap REST responses keep
   their ADR 0034 shape: `siteCode` is echoed **only when the gap was scoped by
   site** (absence still means "unscoped", never a particular site), and they
   never carried `buildingId`.
4. **Staffing gap by path** (`GET /paths/{pathId}/staffing-gap`): `siteCode` is
   the canonical plan-lookup parameter **and** the associate scope (one value);
   `buildingId` stays as the deprecated alias for the plan lookup **only**.
   A call that gives only the legacy `buildingId` is **not** auto-scoped by site
   (callers use ids like `bldg-1`): behaviour identical to before. A call with
   `siteCode` is scoped as ADR 0034 defines. One of the two is required.
5. **New canonical route** `GET /sites/{siteCode}/shifts/{shiftId}/staffing-gap`
   (operation `listStaffingGapsForSiteShift`, same handler core and response
   shape; the path `siteCode` is plan key **and** scope). The old
   `GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap` keeps working,
   unchanged and unscoped, is `deprecated: true` in OpenAPI and answers
   `Deprecation: true` on every response, errors included.
6. **MCP.** `get_staffing_gap` and `propose_path_heads` take `siteCode` with
   `buildingId` as a deprecated alias; their descriptions and argument schemas
   say so. The staffing-gap resource gains the canonical template
   `staffing://sites/{siteCode}/{shiftId}/{pathId}/gap` (plan key and scope);
   `staffing://{buildingId}/{shiftId}/{pathId}/gap` stays, deprecated and
   unscoped. The `cover_staffing_gaps` prompt names `siteCode`. The tool
   registry and fleet snapshot goldens list tool names and hints only, so they
   do not change; nothing is removed.
7. **Events.** `ShiftPlanCommitted` (integration `warehouse.workforce.events`
   and analytics `warehouse.workforce.analytics`) and `ShiftPlanProposed`
   (analytics only: it has never been on the integration topic) gain
   `site_code`, carrying the **same value** as `building_id`. `building_id`
   stays and is marked deprecated in the AsyncAPI. Additive on `v1`: no `.v2`,
   `dataschema` unchanged. `site_code` is deliberately **not** `required`:
   outbox rows encoded before this change lack it, so consumers fall back to
   `building_id` when it is absent. **The Kafka key and the CloudEvents
   `subject` (`<buildingId>/<shiftId>`) are unchanged** — changing either would
   re-partition and break consumers; their first segment **is** the site code.
8. **OpenAPI.** `buildingId` is `deprecated: true` on every request body,
   response schema and parameter; "one of the two is required" is `anyOf` with
   `required` alternatives on the two request bodies (Spectral clean).

## Not done here (and why)

- **`buildingId` is not removed**, not from REST, MCP, events, the database or
  the Kafka key. Removing it is a **future breaking ADR** with its own
  announcement window (the `Deprecation` header and the deprecated flags are
  that announcement's first step).
- **No Kafka key / CloudEvents `subject` change** (breaking for partitioning
  and ordering).
- **No column rename and no migration** (`building_id` stays the stored name).
- **Legacy `buildingId`-only staffing-gap calls are not scoped by site.**
  Inventing a building→site mapping would be a new business rule.
- **No live validation** of `siteCode` against facility-layout (ADR 0034
  point 1 stands: accepted as given).
- **Decided (2026-10-08) — the `422` rule does not apply to staffing-gap reads during the deprecation window.** Decision 19 also
  says a differing `siteCode`/`buildingId` pair is rejected. ADR 0034 defined
  the opposite for the three gap-read surfaces (`GET /paths/{pathId}/staffing-gap`,
  `GET /buildings/…/staffing-gap?siteCode=`, MCP `get_staffing_gap`): the
  plan is keyed by a building id and the site is the scope, and ADR 0034's
  own tests and docs use a differing pair (`bldg-1` + `WH1`). Rejecting it
  would break that documented usage, so those reads **keep ADR 0034 behaviour
  for a differing pair** (`buildingId` is the plan key, `siteCode` the scope)
  for the whole deprecation window. The pair is the only way to scope a gap
  over plans still stored under a legacy building id such as `bldg-1` until they
  are re-committed under a site code, so rejecting it would remove that path
  with no replacement. The differing pair stops being valid when `buildingId`
  is removed (the later, breaking ADR). Every other surface already enforces
  the 422 rule. No caller outside this repo's tests passes a differing
  pair (see the ecosystem check).

## Consequences

- One name in the ubiquitous language, the ER/class/sequence diagrams, the
  canvases and the API reference; `buildingId` is visibly legacy everywhere.
- Every existing caller keeps working unchanged: all new request and response
  fields are optional/additive, and no value, key, subject or column moved.
- A site-scoped gap now needs a plan stored under the site code, so callers
  that want per-site gaps commit their plans with `siteCode` (ADR 0034's
  "pick a consistent pair" caveat disappears for them).
- Until `buildingId` is removed, every surface carries two names for one value;
  the deprecation markers are the only guard against new uses.
- **Ecosystem check** (sibling `origin/develop` greps):
  warehouse-ops-agent's `mcpclient` calls `get_staffing_gap` and
  `propose_path_heads` with `buildingId` only; e2e-tests call
  `/paths/{pathId}/staffing-gap?buildingId=…&shiftId=…` only;
  wes-work-planning and warehouse-planning consume `ShiftPlanCommitted` through
  `DataAs` (plain `json.Unmarshal`, unknown fields ignored) and read
  `building_id`; nothing consumes `ShiftPlanProposed` or the analytics topic
  outside this repo's projector (which ignores unknown fields). No consumer
  rejects an extra field and no caller passes both names with different values.
