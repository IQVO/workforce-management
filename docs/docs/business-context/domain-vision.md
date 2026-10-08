---
id: domain-vision
title: Domain vision
sidebar_label: Domain vision
sidebar_position: 2
description: What Workforce Management is for, and why labor deserves its own bounded context.
---

# Domain vision

> Make the labor picture of a shift **legible and enforceable**: record who is
> on shift and what they are qualified for, let a human commit a split of
> headcount across process paths, track where each person actually is as that
> split drifts, and surface the gap — without ever deciding what any individual
> person should do next.

Everything about the shape of this service follows from that sentence, and in
particular from its last clause.

## Why labor is its own bounded context

The e-commerce-fulfillment DDD reference model classifies **Labor & Workforce Management** as a **Supporting** subdomain:
"allocates workforce to workload; important, industry-common." It is
genuinely necessary — you cannot plan a shift without knowing your headcount —
but nobody wins the market on their break-tracking code.

The platform-level DDD reference is blunter about *where* labor knowledge is
allowed to live:

> WMS "has **zero** knowledge of individual workers, shifts, real-time
> location, or travel distance. If it acquires that knowledge, a
> supporting/generic concern (labor orchestration) has leaked into the core
> domain (order fulfillment truth), and every labor policy change now forces a
> WMS regression."

So worker identity, certifications and shift windows have to sit somewhere
outside the WMS tier. In this platform they sit here, in a WES-adjacent
Supporting context that the WMS tier never reads and never writes.

## The three things it owns

**1. Who is on.** `AssociateShift` is the roster entry: an associate, the
certifications they hold, whether they are currently on a logged break, and how
many hours they have logged against the shift's cap. Every other context that
needs to know whether someone is qualified reads this — none of them write it.

**2. What the plan is.** `ShiftPlan` is one site's committed split of
headcount across paths for one shift, expressed as `PathPlan` lines: path,
planned heads, planned rate, planned hours. There is exactly one per site
per shift (the site is named by `siteCode`; "building" is a deprecated name for
the same thing, [ADR 0035](../adr/0035-sitecode-converges-building-id.md)).

**3. Where people actually are.** `LaborAssignment` records one associate on
one path for an interval. Comparing (2) against (3) yields the staffing gap,
which is the operational output the floor actually consumes.

## Software proposes, humans commit

`ProposePathPlan` computes `heads = ceil(charge ÷ plannedRate)` and returns a
number. It writes nothing, raises no persisted state, and has no aggregate
identity. `CommitShiftPlan` is a separate call that a human makes.

This split is deliberate. A shift plan is a *commitment* — it implies a staffing
roster, break scheduling and, in many buildings, an actual conversation with a
shift manager. Making the arithmetic available without making it automatic
keeps the accountability where it belongs while still removing the arithmetic
from a clipboard.

The same discipline applies intra-shift. When a path falls behind its plan,
this context raises `PathUnderstaffed`. It is a **flag, not a decision**: the
service will never move an associate off `pick` and onto `pack` because a
backlog grew. A supervisor makes that call and records it via `AssignLabor`.

## Why the invariants are worth enforcing here

Two rules are enforced in the domain layer and cannot be bypassed by any
adapter:

- **Exactly one ACTIVE assignment per associate.** A person cannot be counted
  as staffing two paths at once. If that invariant leaks, every downstream
  headcount number silently inflates.
- **An assignment requires the path's certification.** Putting an untrained
  associate on a path is a safety and quality problem, not a data-quality one.
  Training is itself a path that consumes hours; it needs no special case,
  because the gate is enforced on assignment rather than on the roster.

A third rule — `plannedHeads ≤ installedStations` — is enforced here
*independently* of `wes-work-planning`, which enforces the same rule on its own
`PathPlan`. That is not duplication by accident: this is the aggregate that
actually commits headcount, so it validates its own commitment rather than
trusting an upstream check it does not control.

See [Invariants](../ddd/invariants.md) for the enforcement points and the tests
that pin them down.
