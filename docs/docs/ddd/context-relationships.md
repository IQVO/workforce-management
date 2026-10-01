---
id: context-relationships
title: Context relationships
sidebar_label: Context relationships
sidebar_position: 6
description: Upstream, downstream, and the non-relationships that are load-bearing.
---

# Context relationships

Strategic relationships in Evans/Vernon context-mapping vocabulary. For the
wiring-level detail (topics, envelopes, env vars) see
[Ecosystem → Integration](../ecosystem/integration.md); for the diagram see
[Ecosystem → Context map](../ecosystem/context-map.md).

## Summary

| Counterpart | Pattern | Direction | Wired today? |
| --- | --- | --- | --- |
| `wes-work-planning` | **Customer/Supplier** — this context supplies, Work Planning consumes | Publish only, one-way | **Yes** — `ShiftPlanCommitted` on `warehouse.workforce.events` |
| `fulfillment-execution` | **Deliberate non-relationship** at the task/write boundary; **Conformist** read of installed capacity | This context reads `GET /capacity/{capability}` | **Yes, capacity only** — [ADR 0014](../adr/0014-installed-capacity-ceiling.md) |
| `labor-performance` | **Conformist** on measured rates and idle share | Consume `warehouse.labor-performance.events` (`kafka-cache`) or `GET /task-types/{taskType}/performance` (`http`) | **Yes, opt-in** — [ADR 0012](../adr/0012-measured-rate-feed-for-propose-path-plan.md), [0019](../adr/0019-labor-performance-cache-consumer.md), [0020](../adr/0020-idle-share-staffing-signal.md) |
| `process-path-management` | **Conformist** on the process-path catalogue | Consume `warehouse.process-path-management.events` | **Yes, opt-in** (`PATH_CATALOGUE_SOURCE=kafka`); otherwise a file |
| `inventory-storage` | None | — | No |
| `facility-layout` | Would be **Conformist** if physical location ever mattered here; it does not | — | No |

## `wes-work-planning` — Customer/Supplier, one way

This context is the **supplier**; `wes-work-planning` is the **customer**.

Work Planning is the platform's Core WES context: it turns a shift's charge into
a plan and releases work continuously. To flow-balance it needs to know what
labor was actually committed — how many heads the building put on each path.
That is a fact this context owns, and it is published as
`ShiftPlanCommitted`.

Three properties make this a clean Customer/Supplier edge rather than a
coupling:

**It is asynchronous and one-way.** This service publishes to a topic (via
the transactional outbox, [ADR 0016](../adr/0016-transactional-outbox.md)) and
forgets. It has no client for Work Planning, no retry against it, no knowledge
of whether anyone is listening. A Supporting context must never become a
runtime availability risk to a Core one.

**The consumer translates rather than conforms.** Work Planning explicitly does
**not** feed `ShiftPlanCommitted` into its own `ShiftPlan` aggregate — the word
is shared, the model is not. It projects the event into a separate read model,
`LaborPlanObserved` (package `internal/domain/laborview/`), keyed by `path_id`,
holding the latest observed `planned_heads`/`rate`/`hours`, and exposed
read-only at `GET /paths/{pathId}/labor-plan-view`. That translation step is an
anti-corruption boundary in the classic sense: this context's model does not
leak into Work Planning's aggregate.

**The published language is stable and narrow.** One event type
(`com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted`, a
CloudEvents 1.0 event — [ADR 0026](../adr/0026-cloudevents-mandatory-event-envelope.md)),
a `data` payload of six scalars, one message per `PathPlan` line. Nothing about
`AssociateShift`, nothing about individual assignments, nothing about break
state. What the customer gets is the *plan*, not the roster.

## `fulfillment-execution` — the non-relationship, and why it matters

**At the level of tasks and people, there is no integration between this
context and `fulfillment-execution`.** No task, claim, associate identity or
assignment crosses between them, in either direction, and there is no shared
table.

The one live edge is narrow and physical. On every `CommitShiftPlan`, this
context reads `fulfillment-execution`'s installed station count for each
path's capability (`GET /capacity/{capability}`) and treats it as a ceiling on
planned heads ([ADR 0014](../adr/0014-installed-capacity-ceiling.md)). This
makes it a downstream Conformist on a *count*, not on the task model.

This is an ecosystem fact worth stating explicitly, because both services deal
in "people doing work" and a reader will reasonably expect a wider edge between
them. That the edge stays this narrow is the
[path boundary](../business-context/path-boundary.md) made visible in the
context map.

What the two contexts share is a *concept*, not a contract:

- `fulfillment-execution` gates a station claim on **capabilities**;
- this context is the writer of **certifications**.

If and when that gate is wired to real certification data, the correct shape is
`fulfillment-execution` conforming to this context's published read surface —
a **Conformist** relationship, downstream, read-only. What must never happen is
the reverse: `fulfillment-execution` writing into `AssociateShift`, or this
context learning what task anyone is performing.

The platform DDD reference states the general rule:

> **No shared aggregates across contexts.** All cross-context communication is
> via integration events/published APIs — enforce this with an explicit
> Anti-Corruption Layer at each boundary.

## `inventory-storage` and `facility-layout` — genuinely unrelated

`inventory-storage` owns stock truth: SKUs, bins, reservations, usable
inventory. Nothing in the labor model depends on it, and nothing in it depends
on labor. There is no edge and there is no reason to add one.

`facility-layout` is a Generic subdomain and an **Open Host Service** for
physical-location truth: `Site → Area → Zone → Aisle → Bay → Level →
Position`. Its own `CLAUDE.md` names all four sibling services — including this
one — as downstream **Conformists** to whatever it publishes, though it notes
that actually wiring that consumption is a separate, later task and that it has
no live integration with any of them today.

For this context specifically, that conformance is likely to stay theoretical.
A `PathId` here is `pack`, `pick`, `stow` — a *process path*, a queue with a
service rate — not a physical place. Nothing in the labor model needs to know
which aisle anything is in. If travel-time-aware labor planning were ever
added, that would be the moment to consume `facility-layout`'s zone/aisle
adjacency, and it would be a Conformist edge. Today: no edge.

## Where this differs from the reference model

The platform DDD reference puts a single WES-tier `Assignment` aggregate —
an ephemeral `Task → Resource → Time` binding, recomputed continuously by an
`AssignmentOptimizer` domain service — alongside a `ResourcePool` of workers.

This platform splits that aggregate in two along the path/task line:

| Reference model | This platform |
| --- | --- |
| `ResourcePool` (workers, skills, certifications, shift window) | `AssociateShift`, here |
| `Assignment` (task → resource → time) | `LaborAssignment` (associate → **path** → interval), here; task-level claiming in `fulfillment-execution` |
| `AssignmentOptimizer` domain service | **Not built.** Rebalancing is a human decision; this context surfaces `PathUnderstaffed` and stops |

The reference also frames Orchestration ↔ Task&nbsp;&&nbsp;Labor as a
**Partnership** — "they evolve together; sequencing and assignment are two
halves of one optimization loop." That framing assumes the optimization loop is
automated. Here it is not: a human commits the plan and a human moves people.
Without the shared loop there is nothing for a Partnership to synchronise, so
the relationship degrades — correctly — to plain Customer/Supplier over one
event.

If an `AssignmentOptimizer` is ever built, this is the first relationship that
would need revisiting.
