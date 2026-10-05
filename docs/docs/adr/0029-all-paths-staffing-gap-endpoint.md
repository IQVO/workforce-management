---
id: 0029-all-paths-staffing-gap-endpoint
slug: /adr/0029-all-paths-staffing-gap-endpoint
title: 0029. All-paths staffing-gap list endpoint for a building/shift
sidebar_label: 0029. All-paths staffing-gap endpoint
sidebar_position: 30
description: "ADR 0029 — the fast-follow ADR-0011 deferred: GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap lists every path planned in a committed shift plan in one call, reusing the exact per-path computation of GET /paths/{pathId}/staffing-gap so the two can never disagree."
---

# 0029. All-paths staffing-gap list endpoint for a building/shift

## Status

Accepted — recorded retroactively (2026-10 ADR-conformance pass) for an
endpoint that shipped earlier without its own record; ADR-0011's deferral
note is amended to point here.

## Context

ADR-0011 shipped the workforce MFE scoped to the only read model that then
existed: `GET /paths/{pathId}/staffing-gap?buildingId=&shiftId=`, one path
per request. A console screen that wants "how is this shift staffed, across
every path" had to know every `pathId` in advance and issue N requests —
exactly the list-endpoint gap ADR-0011 flagged as a deferred fast-follow
(the same category of gap `warehouse-ops-agent`'s ADR-0002 flagged for
order-management's `GET /orders?status=`).

## Decision

Add `GET /buildings/{buildingId}/shifts/{shiftId}/staffing-gap`
(operationId `listStaffingGapsForShift`): one call returning the staffing
gap for EVERY path planned in that building's committed shift plan, as an
array of the same `StaffingGapResponse` objects the per-path endpoint
returns.

- The per-entry computation is the EXISTING `GetStaffingGap` logic run per
  planned line — the two endpoints cannot disagree for a given path.
- `PathUnderstaffed` events are raised for every understaffed path in the
  plan, exactly as the single-path lookup does for its one path.
- 404 `resource-not-found` when no committed plan exists for the
  building/shift (the same failure the per-path endpoint reports).
- The endpoint only SURFACES gaps; it never decides or performs a
  rebalance. That stays a human call recorded via
  `POST /associates/{id}/assignments` (ADR-0002).
- The MFE screen (ADR-0011) uses this endpoint as its default view and
  keeps the single-path lookup as an explicit narrow mode.

## Consequences

- One request per building/shift instead of one per path; the console's
  all-paths view is now first-class.
- No new domain concept: the plan's lines already enumerate the paths;
  this is a read-model shape change only.
- Documented in `apis/openapi.yaml` (and the generated API reference);
  the workforce MFE's URL builders (`web/src/apiUrls.ts`) are unit-tested
  against both endpoint shapes.
