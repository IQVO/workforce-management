---
id: 0041-agent-authority-levels-and-attributable-assignments
slug: /adr/0041-agent-authority-levels-and-attributable-assignments
title: 0041. Agent authority levels and attributable assignments
sidebar_label: 0041. Agent authority and attribution
sidebar_position: 42
description: "ADR 0041: every MCP tool and A2A skill declares one authority level (L1 to L3 here), the MCP server is stateless so it also answers the 2026-07-28 revision, and an agent-initiated assign_labor stamps an additive initiated_by on LaborAssigned and LaborReassigned."
---

# 0041. Agent authority levels and attributable assignments

## Status

Accepted (2026-11-02). Applies the fleet decision "Agentic Standard v2"
(`docs/architecture/agentic-standard.md` and the fleet rule
`agents/fleet/agent-authority.md` in IQVO/warehouse-docs, PR #96) to this
context. Extends [ADR 0008](./0008-mcp-inbound-adapter.md) (MCP) and
[ADR 0039](./0039-a2a-inbound-adapter.md) (A2A); changes nothing in
[ADR 0026](./0026-cloudevents-mandatory-event-envelope.md).

## Context

This context already lets an agent act: `assign_labor` is a write tool over MCP
and a write skill over A2A. Two things were missing for a delegating agent such
as warehouse-ops-agent to use it responsibly:

1. **Nothing declared how much an agent may do with a tool.** The only signal
   was the MCP `ReadOnlyHint` / `DestructiveHint` annotations and the A2A
   `write` tag, which say *whether* a call changes state, not *what level of
   authority* the call represents.
2. **A moved associate could not be traced to the agent that moved them.**
   `LaborAssigned` / `LaborReassigned` were identical whether a supervisor used
   the console or an agent acted on a proposal.

Separately, the fleet's MCP servers used the Streamable HTTP handler with its
default (stateful) options, which advertises only the 2025-11-25 revision and
older, so a client speaking the 2026-07-28 per-request handshake was refused.

## Decision

### Authority levels, declared where the tool is registered

Authority levels are the strings `L1`..`L4`. In this context:

| Surface | Name | Level | Why |
|---|---|---|---|
| MCP | `get_staffing_gap` | `L1` | reads facts |
| MCP | `get_workforce_labor_report` | `L1` | reads facts (registered only when a reports client is wired) |
| MCP | `propose_path_heads` | `L2` | a pure recommendation (`ceil(charge/rate)`), persists nothing |
| MCP | `assign_labor` | `L3` | changes state |
| A2A | `staffing_status` | `L1` | reads; the optional sizing persists nothing |
| A2A | `assign_labor` | `L3` | changes state; keeps its `write` tag |

- **MCP** declares it in the tool's `Meta` under `com.warehouse/authority`; it
  is returned verbatim in `tools/list` `_meta`.
- **A2A** declares it as the skill tag `authority:L<n>`.
- **L4 is not granted here.** No ADR in this context lets an agent act without
  a human-gated step, so `L4` is forbidden.

Two sensors enforce it. `TestEveryAgentToolDeclaresAuthority` exists in both
adapters (`internal/adapters/inbound/mcp` and `internal/adapters/inbound/a2a`):
every registered tool or skill has exactly one valid level, every tool with
`ReadOnlyHint=false` (or tagged `write`) is `L3`, and every read tool is `L1` or
`L2`. The MCP golden `testdata/tool_registry.golden` gained an authority column,
so a level can only change through a reviewed golden edit. The level is a label
for callers and reviewers; **it is not access control** and nothing in this
context decides anything from it (the surface stays unauthenticated, like REST,
per the fleet decision).

### Stateless MCP

`mcp.NewStreamableHTTPHandler` now receives
`&mcp.StreamableHTTPOptions{Stateless: true}`. No `Mcp-Session-Id` is issued and
the server answers `server/discover` for the 2026-07-28 revision, while a legacy
`initialize` (for example `2025-06-18`) still answers 200, so existing session
clients keep working. Nothing in this adapter relied on cross-call session
state; a tool that ever needs it takes an explicit, server-minted handle as an
argument. `stateless_test.go` pins all three wire facts with raw POSTs.

### Attributable assignments

`assign_labor` over MCP and over A2A accepts three **optional** arguments,
`proposalId`, `approvedBy` and `agentId`: strings, trimmed, at most 128
characters, otherwise rejected with the surface's usual invalid-argument answer.
Strict decoding still rejects every other unknown name, and `via` is **not** an
argument.

- A domain value object `shared.Initiator` carries `via`, `proposal_id`,
  `approved_by` and `agent_id`; its zero value means "not agent-initiated".
- The adapter, never the caller, sets `via` (`mcp` or `a2a`). A call that gives
  no attribution at all is still marked with its `via`.
- `AssignLabor.ExecuteInitiatedBy` threads the `Initiator` to the aggregate
  (`LaborAssignment.AssignBy`) and onto the event. `Execute` is unchanged and
  passes the zero `Initiator`, so the REST handler is untouched.
- The analytics event **data** gains `initiated_by`, snake_case like the rest of
  the payload:
  `{"via":"mcp"|"a2a","proposal_id"?,"approved_by"?,"agent_id"?}`. It is omitted
  when absent. It is additive on the `v1` dataschema: nothing renamed, nothing
  removed, **no CloudEvents extension attribute**, and consumers ignore it
  (the projector reads only `associate_id`, `path_id` and `to_path_id`).
- It is stamped on **both** `LaborAssigned` (first assignment) and
  `LaborReassigned` (a move). The reallocation this exists for is almost always
  a move of an associate who is already on a path, which raises
  `LaborReassigned`; attributing only `LaborAssigned` would leave the main case
  untraceable. `apis/asyncapi.yaml` documents both, with an example each.

The three values are attribution labels, not credentials. They grant nothing,
are not verified, and never change whether an assignment is accepted; the
domain invariants (certification, single active assignment, no break, shift not
ended) decide that exactly as before.

## Consequences

- A reviewer can read an agent's reach off the registry, and a new tool cannot
  ship without declaring a level.
- Because the labels are free-form and unauthenticated, `initiated_by` proves
  *which surface* made the call (the adapter sets it) but only *claims* the
  rest. Treat `approved_by` as a record of what the caller said, not evidence.
- Facts still travel as CloudEvents on Kafka; MCP still carries tools and A2A
  still carries delegation. Nothing here adds a producer or a transport.
- The in-process SDK client negotiates the older revision against a default
  server, so serving 2026-07-28 does not mean the fleet's clients speak it yet.
- `assign_labor` is still not idempotent (see ADR 0039).
