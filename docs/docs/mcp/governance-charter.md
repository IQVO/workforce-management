---
id: governance-charter
title: MCP Governance Charter
sidebar_label: MCP Governance Charter
description: "The estate-wide rules every warehouse-systems MCP server follows — tool curation, naming, annotations, auth scopes, audit, and the review gate. Federated: global standards, domain-owned servers."
---

# MCP Governance Charter

This charter is the **federated computational governance** for MCP across
warehouse-systems: one set of global standards, enforced the same way in every
repository, while each bounded context owns its own server. It is the MCP
counterpart to the platform's existing 5-stage quality gate and its ADR
discipline. `fulfillment-execution` is the reference implementation
(see [ADR-0008](../adr/0008-mcp-inbound-adapter.md)); every other context in
the fleet that ships an MCP server — including `workforce-management` —
copies it.

Keywords **MUST**, **SHOULD**, **MAY** are used per RFC 2119.

## 1. Architecture rules (non-negotiable)

1. An MCP server **MUST** be an inbound adapter at
   `internal/adapters/inbound/mcp/`, depending inward on `application` only.
2. Tool handlers **MUST** call existing application-layer **use cases**. They
   **MUST NOT** touch the domain layer directly, run SQL, or duplicate use-case
   logic. No MCP type may appear in `internal/domain/**` or
   `internal/application/**` — the arch-go fitness tests (ADR-0006) enforce the
   dependency rule and **MUST** be extended to cover the mcp adapter.
3. Each server **MUST** ship as a separate `cmd/mcp` binary reusing the service's
   existing composition wiring.
4. Transport **MUST** be Streamable HTTP. stdio builds **MUST NOT** be shipped.
5. The official Go SDK (`github.com/modelcontextprotocol/go-sdk`) **MUST** be
   used and **MUST** be version-pinned in `go.mod`. Deprecated MCP features
   (`roots`, `sampling`, `logging`; SEP-2577) **MUST NOT** be used — prefer tool
   parameters, resource URIs, and configuration.

## 2. Tool curation — the surface is a product

The single most important rule: **expose intent-level tools, not one tool per
REST endpoint.** Tools are designed around decisions an agent makes.

1. A tool **MUST** map to an outcome an agent wants (`diagnose_stuck_tasks`),
   not to a transport route (`post_tasks_id_complete`).
2. A server **SHOULD** expose **no more than 8 tools**. A PR that pushes a server
   over that count **MUST** carry an explicit justification in its description
   and be approved by a second reviewer. This is the "curated surface" review
   rule; the Phase-6 CI lint enforces the count mechanically.
3. Bulk read access **MUST NOT** be exposed as a tool. Large read models are
   **resources**, scoped to a decision (see §5).

## 3. Naming conventions

| Element | Convention | Example |
| --- | --- | --- |
| Tool name | `snake_case`, `verb_noun`, intent-level | `get_queue_status` |
| Resource URI | `<kind>://<context>/<scope>` | `queue://fulfillment/PICK/status` |
| Auth scope | `mcp:<context>:<read\|write>` | `mcp:fulfillment:write` |
| Prompt name | `snake_case`, names the SOP | `triage_backlog` |

`<context>` is the bounded-context short name (`fulfillment`, `inventory`,
`work-planning`, `workforce`, `facility`).

## 4. Tool annotations (mandatory)

Every tool **MUST** declare annotations so a host can reason about risk before
letting a model call it:

1. A **read** tool **MUST** be annotated read-only (no state change).
2. A **write** tool **MUST** be annotated destructive and **MUST** require the
   `:write` scope.
3. Annotations and descriptions are treated as **untrusted** across servers; a
   host **MUST NOT** rely on another server's annotations for its own safety
   decisions. (Within our own trusted servers they are authoritative.)
4. Descriptions **MUST** state what the tool does and its side effects plainly —
   the description is read by the model and is part of the safety surface.

## 5. Resources — scoped context contracts

1. A resource **MUST** be scoped to a decision, backed by an existing read model
   / projection. It **MUST NOT** dump an entire table, config, or log.
2. Resources are read-only. Anything that changes state is a write **tool**, not
   a resource.

## 6. Prompts — operational SOPs

Prompts **SHOULD** encode operational discipline the model should follow: how to
interpret a tool result, when to stop and escalate, what "done" means. They are
user-initiated and carry the least risk, but they standardize agent behaviour
across clients and **SHOULD** be used rather than leaving procedure implicit.

## 7. Security & authorization (current posture: no IdP)

:::warning Not implemented in this repository

The static-bearer-key posture below was implemented here
([ADR-0017](../adr/0017-adopt-fleet-rest-identity.md)) and then **removed**
([ADR-0018](../adr/0018-remove-fleet-rest-identity.md)). Today every route on
`cmd/mcp` is **unauthenticated at this layer**: the Streamable HTTP endpoint
at `/` and `/mcp`, and `/healthz`. No `MCP_READ_KEY`/`MCP_READWRITE_KEY` is
read, and no tool, resource or prompt is scope-gated. Access control relies
on network-level boundaries (in-cluster ClusterIP Service). Items 1, 2 and 4
below, and the scope/`client_id` fields of the audit record in §9, describe
the charter's target rather than current behaviour here. Re-adopting them
would need a new ADR.

:::

Per ADR-0008, the charter's posture for these internal, non-user-facing servers:

1. Every request **MUST** be authenticated with a static bearer API key held in
   a Kubernetes Secret. Missing/invalid key **MUST** return `401`.
2. Two key classes **MUST** exist: read-only and read-write. A `:write` tool
   **MUST** reject a read-only key (`403`), audited.
3. The API key **MUST NEVER** be logged. No secret, token, or key may appear in
   any log line.
4. The auth check **MUST** be a middleware behind a stable interface, so the
   OAuth 2.1 upgrade is a drop-in with no change to tool handlers.
5. Servers **MUST** remain reachable only in-cluster; ingress **MUST** enforce
   HTTPS. A server **MUST NOT** be exposed to public/end-user traffic until the
   OAuth 2.1 resource-server seam is taken (a future ADR-0009).
6. When a tool must call another service, the server **MUST** authenticate as
   its own client for that hop and **MUST NOT** pass a client token through
   (confused-deputy prevention) — applies the day any upstream hop exists.

## 8. Guardrails (regardless of auth)

1. Every tool handler **MUST** validate its inputs defensively — the caller is a
   model, arguments are untrusted.
2. Write tools **MUST** be rate-limited.
3. Domain errors **MUST** surface as clean structured tool errors, mapped from
   RFC 7807 (ADR-0005). The existing invariants (at-most-once, ownership, lease,
   SLAM tolerance) are the safety net for model-invoked writes and **MUST NOT**
   be bypassed.

## 9. Auditability

Every tool call **MUST** emit an audit record with, at minimum:

- `client_id` (which key/caller),
- `tool` name,
- `scope` presented,
- `outcome` (allowed / denied / error),
- timestamp and trace id.

Audit records **MUST** carry the OpenTelemetry trace id so a call links to its
span. The adapter **MUST** be instrumented with the platform's existing OTel
setup: a span per tool call plus invocation and denial counters, visible in
Jaeger and Grafana alongside HTTP.

## 10. Quality gate (same bar as the rest of the service)

1. Tool handlers **MUST** be unit-tested (table-driven, in-memory adapters) to
   the platform's ≥90% coverage bar, plus at least one transport-level test.
2. The MCP adapter **MUST** pass `make check` (fmt, vet, build, lint, test) and
   the arch-go fitness tests.
3. **Phase-6 governance gate:** implemented in this repository as
   `internal/adapters/inbound/mcp/governance_test.go` — a plain `go test`
   (so it runs in the CI `test` job) that boots the real server and asserts
   the tool-count budget, the naming convention, mandatory annotations and
   non-empty descriptions.
4. **Eval gate (E1–E3):** the tool surface **MUST** pass the eval suites in
   `internal/adapters/inbound/mcp/eval_*_test.go` and
   `evalsuite_test.go`, all plain `go test`s inside the CI `test` job:
   - **E1 — schema & metadata** (`eval_governance_test.go`): every
     advertised tool's input schema resolves as a JSON Schema, accepts a
     schema-shaped arguments object, and REJECTS wrong-typed values (it
     constrains model input, not just decorates it); every parameter
     carries a non-empty description; the advertised surface matches
     `testdata/tool_registry.golden` (which pins the DEFAULT surface — the
     curated report tool's conditional registration is pinned by its own
     eval); and this repo's tools are present, correctly credited, and
     globally unique in `testdata/fleet_tool_snapshot.golden` (the
     federated registry kept identical across all fleet repos — a model
     host mounts several of these servers together, so tool names MUST NOT
     collide).
   - **E2 — wire conformance** (`eval_conformance_test.go`): over the real
     Streamable HTTP handler — initialize handshake carries server info
     and non-empty instructions; unknown tools, wrong-typed arguments,
     unknown extra arguments, unknown resources and prompts are rejected;
     resource templates and prompts are discoverable; a closed session
     fails loudly.
   - **E3 — behavioral evals** (`evalsuite_test.go` +
     `testdata/features/mcp_tools.feature`): Gherkin scenarios driving
     `tools/call` with model-realistic arguments (stray keys, wrong types,
     unknown ids) against seeded state, pinning structured results and
     side effects (domain events, state visible through other tools).

### Pinned behavioral contracts the evals found

- Typed tool schemas are **strict** (`additionalProperties: false`, the
  SDK default): stray model-generated argument keys are rejected with a
  validation error, not silently ignored.
- `get_staffing_gap` cannot distinguish a path absent from the committed
  plan from one planned for zero heads — both report planned 0 / active 0
  / not understaffed. An unknown **building or shift** is a clean
  `not found` tool error; an unknown **path** inside a committed plan is
  zeros, not an error. Pinned as the visible contract.
- `get_staffing_gap` takes an **optional** `siteCode` ([ADR 0034](../adr/0034-site-scoped-staffing-gap.md)):
  absent = the fleet-wide count (unchanged); given = only associates with an
  active shift at that site. The result echoes `siteCode` only when scoped.
- Reading an understaffed path via `get_staffing_gap` **publishes
  `PathUnderstaffed`** — a read with a domain-event side effect (the
  analytics audit trail). Likewise `propose_path_heads` publishes
  `ShiftPlanProposed` on every call: the proposal commits nothing but
  still leaves an audit event.
- `assign_labor`'s single-active invariant is enforced **by construction**:
  assigning an associate who already holds an active assignment to a
  different path is a *reassignment* (prior assignment ended,
  `LaborReassigned` raised), never a double-booking rejection.
- `get_workforce_labor_report`'s optional filters (`pathId`,
  `granularity`) are schema-**required** (no Go field carries `omitempty`,
  so every parameter lands in `required`): a model must pass them,
  possibly as empty strings, which the handler treats as "unset". The
  tool itself is registered only when a reports client is wired into
  `Deps`; the golden registry pins the default three-tool surface.
- Tool handler errors surface as **tool-level error results** carrying the
  domain error text (the SDK's `ToolHandlerFor` mapping) — the clean
  structured tool errors §8.3 requires, never a silent failure.

### Status in this repository

`workforce-management-mcp` exposes 3 tools (`get_staffing_gap`,
`propose_path_heads`, `assign_labor`) plus `get_workforce_labor_report`
when a reports client is configured, one resource template
(`staffing://{buildingId}/{shiftId}/{pathId}/gap`) and one prompt
(`cover_staffing_gaps`). Each tool call gets an OTel span
(`mcp.tool <name>`). Not yet implemented here: write-tool rate limiting
(§8.2) and a dedicated audit record per call (§9).

## 11. Changing this charter

This charter is versioned with the docs. A change to a global standard **MUST**
be proposed as a PR and, because it binds every context's MCP server,
**SHOULD** be
recorded as an ADR when it changes an architecturally significant rule.
