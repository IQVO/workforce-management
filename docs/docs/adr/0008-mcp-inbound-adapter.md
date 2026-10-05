---
id: 0008-mcp-inbound-adapter
title: 0008. Model Context Protocol as an inbound adapter, not a new service
sidebar_label: 8. MCP inbound adapter
sidebar_position: 9
description: "Expose this bounded context to the AI ecosystem via an MCP server built as a second driving adapter over the existing use cases — Streamable HTTP, official Go SDK, unauthenticated by decision (auth superseded by ADR-0018), curated intent-level tools."
---

# 0008. Model Context Protocol as an inbound adapter, not a new service

## Status

**Accepted — auth/static-bearer sections superseded by
[ADR-0018](./0018-remove-fleet-rest-identity.md).** REST and MCP surfaces in
this repo are unauthenticated by decision; the `auth.go` bearer-key
middleware, the two key classes and the chart's key Secret described below
were removed with ADR-0018 and no longer exist. The reference implementation
is [`fulfillment-execution`](../ecosystem/siblings.md); this record adopts
that same decision for `workforce-management`, Phase 5 of the estate-wide
MCP rollout. The estate-wide rules it follows live in the
[MCP Governance Charter](../mcp/governance-charter.md).

**Addendum (2026-09-07) — deployable to the cluster.** Until now `cmd/mcp`
existed only as code: the image did not build it and the chart had no
Deployment for it. This addendum makes it a real deployable without changing
the decision above: the Dockerfile now builds `/app/mcp`; the Helm chart
gains an `mcp.*` block (`enabled: false` by default) rendering a Deployment
and a ClusterIP Service (`<release>-mcp`, port 8090); and `cmd/mcp` serves
an **unauthenticated** `GET /healthz` for probes while mounting the
Streamable HTTP handler at both `/` and `/mcp`. `warehouse-infra` flips
`mcp.enabled` and wires the endpoint into `warehouse-ops-agent`'s
`WORKFORCE_MANAGEMENT_MCP_ENDPOINT`.
(_Amended 2026-10: the bearer-key Secret this addendum originally rendered
went away with ADR-0018; the 2026-10 ADR-conformance pass additionally
wired `EVENT_PUBLISHER`/`KAFKA_BROKERS`/`PATH_CATALOGUE_*` into the mcp
Deployment so MCP-triggered assignments reach the outbox (ADR-0016) and get
path-id validation (ADR-0013)._)

## Context

The platform is being connected to the AI ecosystem (Claude, Cursor, ChatGPT,
agent frameworks). The interoperability standard those clients speak is the
**Model Context Protocol (MCP)**: a client discovers a server's *tools*
(model-callable functions), *resources* (read-only context), and *prompts*
(reusable templates), then an LLM decides which to call.

The forces:

- **There is already a clean action surface.** Every capability of this service
  is an application-layer **use case** (`internal/application/usecases`), one
  struct per use case, reached through ports. The `chi` HTTP adapter is a thin
  driving adapter over exactly those use cases. An AI client needs the same
  actions the HTTP client already has.
- **The domain must not learn about MCP.** ADR-0001's dependency rule is
  load-bearing: domain depends on nothing, application depends on domain,
  adapters depend inward. A protocol whose shape is set by an external LLM
  ecosystem is precisely the kind of concern that must stay in an adapter.
- **MCP has an idiomatic Go path now.** The official **MCP Go SDK**
  (`github.com/modelcontextprotocol/go-sdk`) is a Tier-1 SDK. Building the
  server in Go keeps it in the same language, module, and quality gate as the
  rest of the service — no Python sidecar, no second toolchain.
- **The spec is versioned aggressively.** Revisions in 2025-06, 2025-11, and
  2026-07 have already deprecated features (`roots`, `sampling`, `logging` —
  SEP-2577). Whatever is built will need to track a moving contract.
- **Tools are model-controlled and can act.** Unlike an HTTP client driven by
  code we wrote, an LLM chooses *when* to call a tool and *with what arguments*.
  The spec's own guidance is emphatic: curate a small set of intent-level
  tools, treat tool invocation as requiring host consent, and guard
  state-changing tools most heavily. That matters more here than in most
  contexts: `assign_labor` moves a person between paths, and this context's
  whole design premise (see [ADR-0002](./0002-stop-at-the-path-boundary.md)) is
  that moving people is a human call.
- **This is an internal, non-user-facing deployment.** The servers run inside
  the `warehouse` kind cluster for agent and developer use, not on the public
  internet for end users. (_Superseded by ADR-0018: the fleet ultimately
  removed the static bearer tokens this bullet originally justified, keeping
  both surfaces unauthenticated for in-cluster use._)

## Decision

**We will expose this bounded context to the AI ecosystem through an MCP server
built as a second driving adapter over the existing use cases — leaving the
domain and application layers untouched.**

### The adapter, mirroring the HTTP one

A new `internal/adapters/inbound/mcp/` sits beside `internal/adapters/inbound/http/`:

```
internal/adapters/inbound/mcp/
  server.go      MCP Server wiring (Go SDK), capability registration
  tools.go       intent-level tool handlers -> call use cases
  resources.go   read-model resources (scoped, not bulk)
  prompts.go     workflow prompts (operational SOPs)
  mapping.go     tool I/O <-> DTOs; domain errors -> structured tool errors
```

(_The `auth.go` bearer-key middleware this list originally included was
removed by ADR-0018; REST and MCP are unauthenticated by decision._)

It depends inward on `application` exactly as the HTTP adapter does. No MCP type
appears in `internal/domain/**` or `internal/application/**`. The tool handlers
call the **same** use case structs the HTTP handlers call — never a parallel
code path, never the domain directly.

### A separate `cmd/mcp` binary

The MCP server ships as its own composition root, `cmd/mcp/main.go`, reusing the
same repositories, ports, and `EVENT_PUBLISHER` wiring as `cmd/workforce`. Two
deployables from one module: the HTTP service and the MCP server. This isolates
blast radius, lets the two scale independently, and keeps least-privilege clean
(the MCP process can be given a narrower footprint).

### Streamable HTTP only

The single supported transport is **Streamable HTTP**, stateless where the SDK
allows. We do not ship stdio builds; local desktop-client use goes through the
same HTTP endpoint. One transport is one thing to secure, trace, and test.

### Curated, intent-level tools — not one tool per endpoint

Tools are designed around decisions an agent makes, not around REST endpoints.
Mechanically wrapping all ten HTTP routes would overwhelm the model — the
documented number-one MCP anti-pattern. The surface for this context:

- `get_staffing_gap` (read) — planned vs active heads for a path within a
  building's committed shift plan, and whether it is understaffed. Wraps the
  `GetStaffingGap` read model unchanged, including its `PathUnderstaffed`
  behaviour.
- `propose_path_heads` (read) — the pure `ProposePathPlan` computation
  (`ceil(charge / rate)`); it proposes headcount and commits nothing.
- `assign_labor` (write, annotated destructive) — wraps the `AssignLabor` use
  case; the existing single-active-assignment and certification-match
  invariants ([ADR-0003](./0003-certification-gated-single-active-assignment.md))
  make a model-invoked assignment safe by construction. A domain rejection
  (uncertified, on break, shift ended) surfaces as a clean structured tool
  error.

Resources expose existing read models as **scoped** context contracts
(`staffing://{buildingId}/{shiftId}/{pathId}/gap`), never a database dump.
Prompts encode operational SOPs (`cover_staffing_gaps`: how to read the gap,
when a safe assignment is warranted, when to escalate, what "done" means).

### Static bearer-key auth, behind an OAuth-ready seam — REMOVED

_This section is superseded by [ADR-0018](./0018-remove-fleet-rest-identity.md)._
The `auth.go` middleware, the per-client API keys from a Kubernetes Secret,
the 401 path and the two key classes described here were removed across all
fleet services in the ADR-0017/0018 adopt/revert cycle; both surfaces are
unauthenticated by decision, and no OAuth seam exists in the code today.
(Jump straight to ADR-0018 for the current state.)

### Reuse the existing observability

The adapter is instrumented with the same OpenTelemetry setup as the HTTP
boundary: the MCP router wraps the handler in `otelchi.Middleware` plus
`otelchimetric` request-duration metrics (ADR-0015 Tier 1), so MCP calls
appear in Jaeger and Grafana next to HTTP requests. (_Amended 2026-10: the
original "a span per tool call with tool name, scope and outcome
attributes" overstated what shipped — spans and duration metrics come from
the router-level otelchi middleware; there is no per-tool span-attribute
convention in the code, and no rate limiter._)

## Consequences

### Easier

- **The domain and application layers do not change at all.** MCP is purely
  additive; the dependency rule (ADR-0001) is preserved and checked by the
  existing arch-go fitness tests ([ADR-0007](./0007-arch-go-architecture-fitness-tests.md)),
  which now also cover the mcp adapter.
- **One action surface, two protocols.** HTTP and MCP call the same use cases,
  so behaviour — including every invariant — is identical regardless of caller.
- **Model-invoked writes are safe by construction.** The single-active-assignment
  and certification-match invariants (ADR-0003) already reject an unsafe
  `assign_labor`; the domain error surfaces as a clean structured tool error.
- **It stays in Go, in one quality gate.** The MCP adapter is unit-tested to the
  same ≥90% bar, linted, and CI-gated like every other package.
- **The auth upgrade is contained.** Moving to OAuth later is an adapter change
  behind a stable interface, not a rewrite.

### Harder

- **A second deployable to run and secure.** `cmd/mcp` is another binary, image,
  Helm release, and ingress. The isolation is deliberate but it is real
  operational surface that did not exist before.
- **Auth is deliberately minimal.** A static bearer key is appropriate for an
  internal, non-user-facing server, but it does **not** cover user-facing,
  multi-tenant use. The servers must stay in-cluster until the OAuth seam is
  taken. Recording that boundary is the point.
- **The MCP spec is a moving target.** Aggressive versioning and deprecations
  mean the SDK must be pinned and revisited; features like `roots`/`sampling`
  are already deprecated and must be avoided in favour of tool parameters.
- **Tool curation is an ongoing discipline, not a one-time choice.** Nothing in
  the compiler stops a future PR from adding a tool per endpoint. The
  [MCP governance charter](../mcp/governance-charter.md) and a CI lint on tool
  count/annotations exist to hold the line; without them the surface degrades.
- **LLM-chosen arguments are untrusted input.** Every tool handler must validate
  its inputs defensively — the caller is a model, not our own code — which is
  stricter than what the HTTP DTO layer assumes.
- **`assign_labor` is a state change an autonomous agent can trigger.** It is
  annotated destructive and the spec expects host-side consent, but the
  residual risk of an agent moving the wrong associate is higher than for a
  human-driven HTTP call. The domain invariants bound the damage; they do
  not eliminate the judgement risk — which is exactly why this context
  surfaces the gap and leaves the decision to a human.
