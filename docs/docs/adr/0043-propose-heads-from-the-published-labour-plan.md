---
id: 0043-propose-heads-from-the-published-labour-plan
slug: /adr/0043-propose-heads-from-the-published-labour-plan
title: "0043. Propose heads from warehouse-planning's published labour plan"
sidebar_label: "0043. Heads from the published labour plan"
sidebar_position: 44
description: "ADR 0043: a local copy of warehouse-planning's LabourPlanCommitted gives ProposePathPlan the required heads when no charge is sent (rateSource labour-plan), and the staffing gap and committed lines can show the requirement next to the commitment; the charge-based proposal stays, deprecated. This context keeps labour supply; the requirement is read, never computed twice."
---

# 0043. Propose heads from warehouse-planning's published labour plan

## Status

Accepted (2026-11-18). Extends [ADR 0012](./0012-measured-rate-feed-for-propose-path-plan.md)
(where a proposal's rate comes from) and [ADR 0029](./0029-all-paths-staffing-gap-endpoint.md)
(the staffing gap). Keeps [ADR 0002](./0002-stop-at-the-path-boundary.md). Producer:
warehouse-planning ADR 0023 (`LabourPlanCommitted`). Additive: no existing field, route or
published type changes.

## Context

`ProposePathPlan` computes `ceil(charge / resolvedRate)` from a `charge` the caller sends,
with the rate from the caller or from labor-performance's measured actuals (ADR 0012) and an
idle-share trim (ADR 0020). The charge it divides is the same forecast the caller also sends to
wes-work-planning, and the shift's heads are then committed here and, separately, in
wes-work-planning. Three contexts hold parts of one planning chain.

warehouse-planning now owns the forecast and the shift labour plan (its ADR 0023) and
publishes `LabourPlanCommitted` per site, shift and path: the heads the demand requires. This
context owns the other half: who is available, certified and assigned (labour supply).

## Decision

1. **Local copy.** A consumer of `warehouse.warehouse-planning.events` acts only on
   `com.warehouse.wes.warehouse-planning.labourplan.LabourPlanCommitted` and keeps
   `labour_requirements`, one row per `(site_code, shift_id, path_id)`: `planned_heads`,
   `rate_units_per_hour`, `hours`, `heads_source`, `version`. A message is applied only when its
   `version` is greater than the stored one. Claim on the CloudEvents `id` and apply in one unit
   of work; group id from the new required env var `LABOUR_PLAN_CONSUMER_GROUP` (the consumer
   is always on: Kafka is part of the runtime, ADR 0038). The wire `site_id` is a
   facility-layout site code and is stored as `site_code` (ADR 0035).
2. **Proposal from the requirement.** When a proposal request omits `charge` and names a
   `shiftId` (new optional field, with the existing `siteCode`), `proposedHeads` is the copy's
   `planned_heads` for `(siteCode, shiftId, pathId)` and `rateSource` is `labour-plan`. The
   idle-share trim of ADR 0020 does not apply to it: warehouse-planning's heads are the
   requirement, and trimming them here would make two contexts compute it. No row is `404
   labour-plan-not-found`, never zero heads.
3. **The charge-based proposal stays, deprecated.** A request with `charge` behaves exactly as
   today (ADR 0012, ADR 0020). Its removal is a later decision, taken once no caller sends a
   charge.
4. **The requirement next to the commitment.** The staffing-gap lines and the committed shift
   plan lines gain an optional `requiredHeads` from the copy (omitted when no row exists).
   `CommitShiftPlan` does not refuse a line that differs from the requirement: committing fewer
   heads than required is a legitimate supply decision, and warehouse-planning surfaces the
   difference (`LabourPlanDriftDetected`).
5. **Unchanged:** the installed-capacity ceiling of ADR 0014 (a REST sync edge to
   fulfillment-execution) and `ShiftPlanCommitted`'s payload. Note for a later decision: the
   ceiling reads fulfillment-execution's station count while warehouse-planning reads
   facility-layout's tally, so the two contexts can disagree on installed stations.

## Consequences

- A supervisor gets the required heads without re-sending a forecast, and sees requirement and
  commitment on the same line.
- One more consumer group and a required env var: chart and warehouse-infra values land
  together, infra first.
- The measured-rate and idle-share signals still shape charge-based proposals only. Whether
  they belong in warehouse-planning's requirement is open and not decided here.

## Alternatives rejected

- **Keep computing heads from a caller's charge only.** The requirement would be computed in
  two contexts from two copies of the forecast.
- **Call warehouse-planning's `GET /labour-plans` at proposal time.** Live cross-context reads
  are not how this fleet integrates; the event-fed copy is.
- **Refuse a commitment below the requirement.** Supply is this context's decision; the gap is
  information, not an error.
