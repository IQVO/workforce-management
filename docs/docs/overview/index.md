---
id: index
slug: /overview
title: Workforce Management
sidebar_label: Introduction
sidebar_position: 1
description: The Supporting bounded context that owns who is on shift, on which process path, at what rate.
---

# Workforce Management

:::warning[Study project]
This documentation site is an educational Domain-Driven Design exercise. It
follows real industry-standard patterns and terminology, but it is **not a
production system** and is **not affiliated with, endorsed by, or
representative of any real-world company**.
:::

**Workforce Management** is one of the twelve Go backend bounded contexts that
make up the `warehouse-systems` platform. It is a **Supporting** bounded
context, and it owns exactly one question:

> *Who is on shift, on which process path, at what rate — and how many of
> those hours are direct versus indirect?*

It covers two horizons that people often (wrongly) fuse into one:

1. **The shift-start planning horizon.** A human commits a split of headcount
   across process paths for a building's shift. The software *proposes*
   (`heads = ceil(charge ÷ plannedRate)`); a human *commits*. That commitment
   is the `ShiftPlan` aggregate, made of `PathPlan` lines.
2. **Intra-shift assignment tracking.** As backlogs deviate from plan,
   associates get moved between paths. The move itself is always a human call.
   This context records it (`LaborAssignment`) and makes the resulting gap
   legible (`GetStaffingGap`, the `PathUnderstaffed` flag). It does **not**
   decide the rebalance.

## Where the boundary stops

This context **never links an associate to a specific task**. It stops at
"this associate is on this path, for this interval," full stop. Dispatching an
individual unit of work to a claiming station is
[fulfillment-execution](https://github.com/IQVO/fulfillment-execution)'s
job, and it happens on a completely different cadence — seconds, versus the
minutes-to-hours on which headcount moves between paths.

This is not an omission. It is the design decision that lets task-dispatch
policy change without touching workforce planning, and vice versa. The
reasoning is spelled out in [Why it stops at the path
boundary](../business-context/path-boundary.md) and recorded as
[ADR&nbsp;0002](../adr/0002-stop-at-the-path-boundary.md).

## What is actually built

| Capability | Where |
| --- | --- |
| Three aggregates — `AssociateShift`, `ShiftPlan`, `LaborAssignment` | [Aggregates](../ddd/aggregates.md) |
| Four hard invariants, each with a dedicated red-path test | [Invariants](../ddd/invariants.md) |
| Ten domain events | [Domain events](../ddd/domain-events.md) |
| Ten business REST endpoints plus `/healthz` and `/readyz`, RFC 7807 errors | [API Reference](../api-reference/index.md) |
| One outbound integration topic, `warehouse.workforce.events` (plus the internal analytics topic) | [Integration](../ecosystem/integration.md) |
| Hexagonal layering, enforced by executable architecture tests | [Architecture](./architecture.md) |
| The full ddd-crew artifact pack and UML / ER / sequence diagrams | [DDD artifacts](../ddd/ddd-artifacts.md) |

## Where to go next

- New here? Read [Domain vision](../business-context/domain-vision.md), then
  [Ubiquitous language](../business-context/ubiquitous-language.md).
- Integrating with this service? Go straight to the
  [REST API](../api-reference/rest/workforce-management-api.info.mdx) and the
  [Events catalog](../api-reference/events.md).
- Wondering *why* something is shaped the way it is? Every consequential
  decision has an [ADR](../adr/index.md).
