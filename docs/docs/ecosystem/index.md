---
id: index
slug: /ecosystem
title: Ecosystem
sidebar_label: Overview
sidebar_position: 1
description: Where this service sits among the warehouse-systems bounded contexts.
---

# Ecosystem

`workforce-management` is one of the Go bounded-context services in the
`warehouse-systems` platform. Each has its own model, its own database and its
own deployment.

| Page | Contents |
| --- | --- |
| [Context map](./context-map.md) | The Mermaid diagram — what is actually wired, and what is only strategically related |
| [Sibling services](./siblings.md) | What each sibling owns, in its own vocabulary |
| [Integration](./integration.md) | Topics, envelopes, env vars, and how to smoke-test the edge |

## The one-paragraph version

This service **publishes** `ShiftPlanCommitted` to
`warehouse.workforce.events`, one message per `PathPlan` line. Two siblings
consume it: `wes-work-planning`, which projects it into a read model called
`LaborPlanObserved`, keyed by `path_id`, and `warehouse-planning`, whose
labor-capacity consumer registers it as a labor constraint.

It also **reads from** three siblings, each edge selected by an env var: the
live installed capacity from `fulfillment-execution` on every
`CommitShiftPlan`, measured rates and idle share from `labor-performance`, and
optionally the process-path catalogue from `process-path-management`'s topic.
[Integration](./integration.md) lists every edge.

With `fulfillment-execution` it shares only that capacity read. No task,
claim, or associate identity crosses between them, which is a design decision
rather than a gap. See [the path boundary](../business-context/path-boundary.md).
