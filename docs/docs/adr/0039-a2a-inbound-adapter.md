---
id: 0039-a2a-inbound-adapter
slug: /adr/0039-a2a-inbound-adapter
title: 0039. A2A inbound adapter (third driving adapter)
sidebar_label: 0039. A2A inbound adapter
sidebar_position: 40
description: "ADR 0039: workforce-management is an Agent2Agent (A2A) agent through a third driving adapter over the same use cases as REST and MCP. Two curated goal-shaped skills, staffing_status (read) and assign_labor (write), in a message-only agent served by cmd/a2a on port 8093. A2A carries delegation only; facts still leave as CloudEvents through the transactional outbox."
---

# 0039. A2A inbound adapter (third driving adapter)

## Status

Accepted (2026-10-29). Mirrors the reference rollout in
[wes-work-planning ADR-0046](https://iqvo.github.io/wes-work-planning/docs/adr/0046-a2a-inbound-adapter);
follows [ADR-0008](./0008-mcp-inbound-adapter.md) (the MCP adapter this one is
modelled on), [ADR-0026](./0026-cloudevents-mandatory-event-envelope.md) (CloudEvents on every
Kafka message) and [ADR-0038](./0038-postgres-and-kafka-are-the-only-runtime.md)
(Postgres and Kafka are the only runtime).

## Context

The fleet is adopting the Agent2Agent protocol so that a bounded context can
**delegate** a goal to another context's agent. This context already exposes
its use cases over REST and MCP. The two levers the flow-balance correlation in
`warehouse-ops-agent` names for this context are exactly the two things a
delegating agent would want to ask: *is this path staffed, and by how much
short?* and *put this associate on that path*.

Three transports now carry three different things and must not overlap:

| Transport | Carries |
|---|---|
| Kafka + CloudEvents | **facts** between contexts (ADR-0026) |
| MCP | an agent's **tools** (reads and single actions) |
| A2A | **delegation** of a goal to another context's agent |

## Decision

Add `internal/adapters/inbound/a2a`, a third driving adapter over the **same**
use cases (`GetStaffingGap`, `ProposePathPlan`, `AssignLabor`) as the HTTP and
MCP adapters, built on the official `a2aproject/a2a-go/v2` SDK and served by a
new `cmd/a2a` binary (JSON-RPC 2.0 over HTTP, port **8093**, the Agent Card at
`/.well-known/agent-card.json`, the endpoint mounted at `/` and `/a2a`).

### Two curated skills, not one per endpoint

| Skill | Kind | What it does |
|---|---|---|
| `staffing_status` | read | planned vs active heads and whether the path is understaffed for a shift at a site; when `charge` and `plannedRate` are both given, also the headcount needed to clear that charge (ceil(charge/rate)). Persists nothing. |
| `assign_labor` | write | puts an associate on a path, ending their prior active assignment; publishes `LaborAssigned` / `LaborReassigned` through the transactional outbox. |

The same curation discipline as ADR-0008: goal-shaped, never one skill per
endpoint. The sizing is composed into `staffing_status` rather than offered as
a third skill because it is only ever asked alongside the gap.

### Message-only, answered in-band

Both skills are transactional request/response work, so the agent is
message-only (no Task lifecycle). Domain rejections and malformed requests are
**answered** as RFC 7807-shaped `problem`s inside the message envelope, never
raised as JSON-RPC transport errors, with the **same classification as the REST
adapter** (`statusFor`): `404` not found, `400` validation (including an
unknown path id, checked against the process-path catalogue exactly as REST and
MCP do), `409` business rule (certification required, on break, shift ended,
…) or concurrent modification, `503` installed-capacity dependency unavailable,
`500` anything else.

### Strict input, canonical site key

- Unknown argument names are **rejected**, not ignored (a misspelled
  `plannedRte` must not silently drop the sizing). Go's `encoding/json` matches
  names case-insensitively, so `plannedrate` still reaches `plannedRate`.
- The surface takes only the canonical `siteCode`. The deprecated `buildingId`
  alias (ADR 0035) is **not offered** to new clients, and a test pins that the
  card never mentions it. `siteCode` also scopes the gap to associates at that
  site (ADR 0034), so an associate started with no site is not counted.

### Facts still travel on Kafka

A2A never becomes a data-sync channel. `assign_labor` writes the same outbox
rows `POST /associates/{id}/assignments` writes; `cmd/a2a` never runs the relay
(only `cmd/workforce` drains `outbox_events`). Proven on real Postgres by
`TestA2AAssignLabor_WritesOutboxRows` (build tag `integration`).

### Unauthenticated, like REST and MCP

The card declares no security schemes: this surface is unauthenticated by the
same fleet-wide decision as REST and MCP. Schemes are added to the card, not a
redesign, if that decision is ever reversed.

## Consequences

- A fourth deployable (`/app/a2a`) in the same image and a `a2a.*` chart block
  (`<release>-a2a`, default **off**), exposed through Kong at
  `/api/a2a/workforce-management` rewritten to `/`.
- `assign_labor` is **not idempotent** (each accepted call closes the prior
  interval and starts a new one, like the REST route without its
  `Idempotency-Key`), so a delegating agent must not blindly retry an ambiguous
  failure. The card says so. Using the A2A `messageId` as an idempotency key is
  a possible follow-up; the MCP tool has the same property today.
- Delegating *to* this agent does not change who decides: this context still
  "makes the gap legible, it does not decide" (ADR-0002); whether a caller may
  *ask* for an assignment is that caller's governance (for example
  warehouse-ops-agent keeps `assign_labor` recommendation-only).
- Port 8093 is the fleet's A2A port (8091 is this chart's projector admin
  port, 8092 the reports API).
