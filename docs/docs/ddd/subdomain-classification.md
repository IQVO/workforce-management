---
id: subdomain-classification
title: Subdomain classification
sidebar_label: Subdomain classification
sidebar_position: 2
description: Supporting subdomain — the justification, and what follows from it.
---

# Subdomain classification

**Workforce Management is a Supporting subdomain.**

## The justification

The e-commerce-fulfillment DDD reference classifies subdomains by competitive
differentiation, and puts labor squarely in the Supporting bucket:

| Subdomain | Type | Why |
| --- | --- | --- |
| **Labor & Workforce Management** | **Supporting** | "Allocates workforce to workload; important, industry-common." |

Two clauses, both load-bearing:

- **"Important"** — you cannot run a shift without it. A missing certification
  gate is a safety incident; a double-booked associate corrupts every headcount
  number downstream. This is not a nice-to-have.
- **"Industry-common"** — every warehouse in the world does this, in
  recognisably the same way. Nobody wins on their break-tracking code.

Supporting, not Generic: the model here is specific enough to this platform's
process-path vocabulary that an off-the-shelf labor-management product would
need a translation layer wider than the service itself. Supporting, not Core:
the platform's differentiators are `wes-work-planning`'s continuous release and
flow balancing, `inventory-storage`'s chaotic stow and revocable reservations,
and `fulfillment-execution`'s pull-based dispatch. Not this.

## What follows from being Supporting

Classification is not a label, it is a budget. Being Supporting is why this
service is built the way it is:

**Invest in correctness, not cleverness.** The four invariants are enforced in
pure domain code and tested on their failing paths. There is no optimiser, no
heuristic, no scoring function anywhere in this codebase — because the
interesting decisions (who to move, when) belong to humans and to other
contexts.

**Take dependencies out, not in.** No Core context depends on this service at
runtime. `installedStations` arrives in the request rather than being fetched
from `wes-work-planning`, precisely so that a Supporting context never becomes
a runtime risk to a Core one. The dependencies this service does take are
reads *into* it: `fulfillment-execution`'s live installed capacity on commit
([ADR 0014](../adr/0014-installed-capacity-ceiling.md)), and measured rates /
idle share from `labor-performance`. When the capacity read fails, only this
service's own commit fails.

**Keep the surface small.** Ten business endpoints (plus the `/healthz` and
`/readyz` probes), three aggregates, nine use-case structs. Every addition
would have to earn its place against the fact that this is not where the
business differentiates.

## Where it sits in the WMS / WES / WCS layering

The platform reference splits systems by time horizon:

| Layer | Horizon | Answers |
| --- | --- | --- |
| WMS | minutes → days | What needs to happen, and why |
| **WES** | **seconds → minutes** | **Who does it, right now, in what order** |
| WCS | milliseconds → seconds | How the machine performs the next physical step |

Workforce Management is **WES-adjacent**. It supplies the "who" half of the
WES question, on a slower clock than the "right now, in what order" half.

The reference model is explicit that worker identity must live outside the WMS
tier:

> WMS "has **zero** knowledge of individual workers, shifts, real-time
> location, or travel distance. If it acquires that knowledge, a
> supporting/generic concern (labor orchestration) has leaked into the core
> domain (order fulfillment truth), and every labor policy change now forces a
> WMS regression."

It is also explicit that the WES tier owns a `ResourcePool` of workers with
"skill/certification, current zone, shift" and an `Assignment` aggregate
binding resources to work. This service implements the **worker half** of that
(`AssociateShift` ≈ the `Worker` entity; `LaborAssignment` ≈ a path-level, not
task-level, `Assignment`), while the **task half** — the ephemeral
`Task → Resource → Time` binding, recomputed continuously — lives in
`fulfillment-execution`.

Splitting the reference model's single `Assignment` aggregate along the
path/task line is a deliberate departure from it, for cadence reasons argued in
[The path boundary](../business-context/path-boundary.md) and recorded in
[ADR 0002](../adr/0002-stop-at-the-path-boundary.md).

## The original five contexts, for comparison

The fleet has since grown to twelve backend contexts (see
[Sibling services](../ecosystem/siblings.md)); this table keeps the original
five, whose classifications this page's reasoning is argued against.

| Service | Tier | Classification |
| --- | --- | --- |
| `inventory-storage` | WMS | **Core** — chaotic stow, bin-accurate location, usable inventory |
| `wes-work-planning` | WES | **Core** — the conductor: charge → plan → continuous release → flow balancing |
| **`workforce-management`** | **WES-adjacent** | **Supporting** — headcount planning and labor assignment |
| `fulfillment-execution` | WES | **Core** — Pick/Pack/SLAM task lifecycle, pull-based `claimNext` |
| `facility-layout` | — | **Generic** — the physical warehouse map, an Open Host Service |
