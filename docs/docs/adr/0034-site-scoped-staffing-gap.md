---
id: 0034-site-scoped-staffing-gap
slug: /adr/0034-site-scoped-staffing-gap
title: 0034. Site-scoped staffing gap (optional canonical siteCode)
sidebar_label: 0034. Site-scoped staffing gap
sidebar_position: 35
description: "ADR 0034 — StartAssociateShift accepts an optional canonical siteCode (the facility-layout Site code) persisted on associate_shift.site_code; the single and list staffing-gap queries (REST and MCP) accept an optional siteCode that counts only associates with an active shift at that site; PathUnderstaffed gains an optional site_code. Unscoped callers and legacy rows are unchanged."
---

# 0034. Site-scoped staffing gap (optional canonical `siteCode`)

## Status

Accepted. Resolves the open question left by
[ADR 0033](./0033-docs-audit-corrections-2026-10.md) ("the staffing gap still
counts active assignments across every building") and refines
[ADR 0029](./0029-all-paths-staffing-gap-endpoint.md) and
[ADR 0003](./0003-certification-gated-single-active-assignment.md) without
editing them; where this record and one of those disagree, this record wins.

## Context

`CountActiveByPath` counted every ACTIVE `LaborAssignment` on a path across the
whole database, because neither `labor_assignment` nor `associate_shift`
carried a building, shift or site. The `ShiftPlan` is keyed by
`buildingId + shiftId`, so the gap compared one building's planned heads with
**every** building's active heads. That is wrong for a multi-site network, and
the rest of the fleet is converging on a canonical **Site**:
facility-layout owns the Site aggregate (its `SiteCode`), warehouse-planning
requires `site_id` on capacity plans (the same facility-layout Site code), and
network flows are site-scoped.

Decision taken on 2026-10-06 (architect, final): site-scope the staffing gap,
additively and backward compatibly.

## Decision

1. **Identifier.** The canonical **Site code** is the facility-layout Site code
   (`WH1`, `SIM1`, …; facility-layout publishes it as `site_code` on
   `SiteCapabilityChanged`, warehouse-planning carries it as `site_id`). It is
   an optional `siteCode` on the REST/MCP surface (camelCase, like every other
   field there) and `site_code` in the snake_case analytics event payload.
   This context treats it as an opaque string, **accepted as given**: only
   surrounding whitespace is trimmed, a blank value means "no site", and there
   is **no live lookup or validation** against facility-layout (this context
   keeps its data local and declarative).
2. **Write side.** `POST /associates/{id}/start-shift` accepts an optional
   `siteCode`, persisted on the `AssociateShift` aggregate as
   `associate_shift.site_code` — a **nullable** column added by the additive
   migration `000005` (plus a partial index on the non-NULL values). A restart
   (the documented upsert) replaces the whole roster entry, site included, just
   like certifications; a restart without `siteCode` therefore makes the
   associate site-unknown again.
3. **Read side.** `GET /paths/{pathId}/staffing-gap`,
   `GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap` and the MCP tool
   `get_staffing_gap` accept an optional `siteCode`. When it is non-empty,
   `activeHeads` counts only assignments whose **associate has an active
   (not-ended) shift at that site** (`labor_assignment` joined to
   `associate_shift` on `associate_id`, `site_code = :siteCode AND ended =
   FALSE`). When it is absent or blank the query behaves **exactly as before**
   (fleet-wide, via the unchanged `CountActiveByPath`). The response echoes
   `siteCode` only when the request was scoped.
4. **Legacy rows.** `NULL` site never matches any `siteCode`: associates
   started before the migration (or without a site) count **only in unscoped
   queries**. An unknown code simply counts 0.
5. **Event.** `PathUnderstaffed` (analytics stream, `v1`) gains an optional
   `site_code`, **omitted when the gap was computed unscoped** (additive, no
   `.v2`, `dataschema` unchanged). Consumers MUST treat absence as
   "unscoped" (every site together), never as a particular site. The Kafka key
   and CloudEvents `subject` stay the path id. `PathUnderstaffed` is not on the
   integration topic.
6. **Unchanged.** Idempotency-Key behaviour (ADR 0027) — the two guarded POSTs
   do not carry a site; `ShiftPlan` stays keyed by `buildingId + shiftId`
   (mapping a building to a site is not decided here); `LaborAssignment`
   gains no site (the associate's shift is the single place the site lives);
   `AssociateShiftStarted` is not extended.

## Consequences

- A multi-site deployment gets a correct per-site gap by starting shifts with
  `siteCode` and querying with it. Unscoped callers (warehouse-ops-agent via
  MCP, e2e-tests via REST, the console) are untouched: every new request and
  response field is optional.
- The site is only as trustworthy as what the caller sends: a typo'd code
  makes an associate invisible to the intended site's gap. Accepted trade-off
  for not coupling this context to facility-layout (ADR 0002 spirit).
- A site-scoped gap is compared with the building's plan for the shift, so
  callers must pick a consistent `buildingId`/`siteCode` pair themselves.
- The additive column can be dropped by the `down` migration only; no forward
  migration drops anything. The unused `domain_event` table is **kept** and
  documented as legacy (additive migrations only).
- Ecosystem check (sibling `origin/develop` greps): nothing consumes
  `PathUnderstaffed` outside this repo's own analytics projector (which ignores
  unknown fields; covered by a test); warehouse-ops-agent calls MCP
  `get_staffing_gap` with `buildingId/shiftId/pathId` and e2e-tests call
  `start-shift`/`staffing-gap` without a site — all remain valid.
