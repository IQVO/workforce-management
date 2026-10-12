---
id: tools
title: MCP tools, resources and prompts
sidebar_label: MCP tools
description: Every tool, resource template and prompt the cmd/mcp server exposes, with input fields, output fields and read-only or write annotations.
---

# MCP tools, resources and prompts

`cmd/mcp` serves MCP over **Streamable HTTP** at `/mcp` and `/`, plus
`GET /healthz`, on `MCP_ADDR` (`:8090`) (`cmd/mcp/router.go`). It is
unauthenticated, like the REST API. It runs the same use cases as the REST
service through `internal/adapters/inbound/mcp`
([ADR 0008](../adr/0008-mcp-inbound-adapter.md)), and every path id is
validated against the same process-path catalogue
([ADR 0013](../adr/0013-process-path-catalogue-validation.md)). Each tool
call runs in an OTel span `mcp.tool <name>`. The rules that govern the
surface are in the [Governance charter](./governance-charter.md).

## Tools

| Tool | Annotations | Use case | Registered |
|---|---|---|---|
| `get_staffing_gap` | `readOnlyHint: true` | `GetStaffingGap.ExecuteForSite` | always |
| `propose_path_heads` | `readOnlyHint: true` | `ProposePathPlan.Execute` | always |
| `assign_labor` | `readOnlyHint: false`, `destructiveHint: true`, `idempotentHint: false` | `AssignLabor.Execute` | always |
| `get_workforce_labor_report` | `readOnlyHint: true` | none. Calls `GET /reports/labor` on `cmd/workforce-reports`. | only when `REPORTS_BASE_URL` is set |

The registry is pinned by `testdata/tool_registry.golden` (name, read-only,
destructive) and checked for fleet-wide name uniqueness against
`testdata/fleet_tool_snapshot.golden`. See [Testing](../development/testing.md#mcp-evals).

"Read-only" describes the client contract. `get_staffing_gap` and
`propose_path_heads` still **publish events**: `PathUnderstaffed` for each
understaffed path read, and `ShiftPlanProposed` for every proposal (see
[Use cases](../ddd/use-cases.md)).

### `get_staffing_gap`

Planned vs active heads for one path in a site's committed shift plan.

| Input | Type | Required | Meaning |
|---|---|---|---|
| `siteCode` | string | one of `siteCode`/`buildingId` | Canonical site code, e.g. `WH1`. It is the plan key **and** the associate scope: only associates with an active shift at that site are counted. |
| `buildingId` | string | | **Deprecated** alias of the plan key. A call with only `buildingId` is not scoped by site. Sending both with different values is rejected. |
| `shiftId` | string | yes | Shift whose committed plan to read. |
| `pathId` | string | yes | Process path, validated against the catalogue. |

Output: `buildingId`, `shiftId`, `pathId`, `plannedHeads`, `activeHeads`,
`understaffed`, and `siteCode` (only when the read was scoped by site). There
is no `observedIdlePct` on this surface.

### `propose_path_heads`

Headcount needed to clear a charge at a planned rate. It commits nothing.

| Input | Type | Required | Meaning |
|---|---|---|---|
| `siteCode` | string | one of `siteCode`/`buildingId` | Site the proposal is for. |
| `buildingId` | string | | **Deprecated** alias with the same value. |
| `pathId` | string | yes | Process path, validated against the catalogue. |
| `charge` | number | yes | Units the path must clear. Must be at least 0. |
| `plannedRate` | number | yes | Units per head per hour. **Must be greater than 0.** |

Output: `siteCode`, `buildingId`, `pathId`, `charge`, `plannedRate`,
`proposedHeads`. `plannedRate` is mandatory here, and `cmd/mcp` wires no
measured-rate or idle-share client. So unlike the REST endpoint, this tool
never uses labor-performance's measured rate and never trims for idle share.

### `assign_labor` (write)

Puts an associate on a path and ends their previous active assignment.

| Input | Type | Required | Meaning |
|---|---|---|---|
| `associateId` | string | yes | Associate to move. |
| `pathId` | string | yes | Target path, validated against the catalogue. The associate must hold the path family's certification. |

Output: `associateId`, `pathId`, `assignmentId` (`<associateId>@<pathId>`).
Domain rejections (not found, missing certification, on break, shift ended,
max hours) come back as the tool error unchanged. There is no
`Idempotency-Key` on MCP, so a repeated call simply assigns again; it is
annotated non-idempotent. Every attempt is counted on
`workforce.labor_assignments`. With a database and `EVENT_PUBLISHER=kafka`,
`LaborAssigned`/`LaborReassigned` go into `outbox_events`, and the
`cmd/workforce` relay ships them.

### `get_workforce_labor_report`

The Labor Utilization & Staffing data product, read over REST from
`cmd/workforce-reports`. The MCP server never opens the analytical database.

| Input | Type | Required | Meaning |
|---|---|---|---|
| `from` | string | yes | Window start, inclusive, RFC 3339. |
| `to` | string | yes | Window end, exclusive, RFC 3339. |
| `pathId` | string | schema: yes; handler: optional | Path filter. Empty means all rows, including the building-wide bucket. |
| `granularity` | string | schema: yes; handler: optional | Only `hour` is supported. |

The struct fields for `pathId` and `granularity` have no `omitempty`, so the
go-sdk publishes them as **required** in the tool's input schema, even though
the handler and its descriptions treat them as optional. Clients that follow
the schema must send them; an empty string is accepted. The output is the
reports API's `rows` array (see
[Runbook](../operations/runbook.md#analytics-reports-api-cmdworkforce-reports)).

## Resource templates

| URI template | Status | Behaviour |
|---|---|---|
| `staffing://sites/{siteCode}/{shiftId}/{pathId}/gap` | canonical | Same as `get_staffing_gap` scoped by `siteCode`. JSON. |
| `staffing://{buildingId}/{shiftId}/{pathId}/gap` | deprecated ([ADR 0035](../adr/0035-sitecode-converges-building-id.md)) | Plan key only, not scoped by site. |

## Prompts

| Prompt | Purpose |
|---|---|
| `cover_staffing_gaps` | SOP: call `get_staffing_gap` per path, rank by shortfall, size with `propose_path_heads`, make only clearly safe `assign_labor` calls, treat a rejection as a hard stop, and escalate to a human when certified associates run out. |

## Scaling note

Sessions (`Mcp-Session-Id`) live in process memory, and the chart's MCP
Service has no session affinity. Run **one** replica (see
[Runbook](../operations/runbook.md#scaling)).
