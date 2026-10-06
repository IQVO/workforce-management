---
id: 0018-remove-fleet-rest-identity
slug: /adr/0018-remove-fleet-rest-identity
title: 0018. Remove the fleet REST identity layer (revert ADR-0017)
sidebar_label: 0018. Remove REST identity
sidebar_position: 19
description: ADR 0018 — the static-bearer-key auth layer adopted in ADR-0017 is removed from the REST and MCP surfaces across all three binaries (cmd/workforce, cmd/workforce-reports, cmd/mcp); the fleet rollout is reverted for this service.
---

# 0018. Remove the fleet REST identity layer (revert ADR-0017)

## Status

Accepted — implemented in the same change that introduced this record.
Supersedes [ADR-0017](./0017-adopt-fleet-rest-identity.md).

## Context

[ADR-0017](./0017-adopt-fleet-rest-identity.md) adopted the fleet-wide static
bearer-key REST identity (warehouse-ops-agent ADR 0005) across this service's
REST surface (`cmd/workforce`, `cmd/workforce-reports`) and its MCP surface
(`cmd/mcp`): a shared `internal/adapters/inbound/auth` package, chi/MCP
middleware enforcing read/read-write scopes, static keys sourced from
`API_READ_KEY`/`API_READWRITE_KEY` (with `MCP_READ_KEY`/`MCP_READWRITE_KEY`
fallbacks), and matching bearer credentials on the outbound
fulfillment-execution and labor-performance clients.

The fleet decision to run this auth layer has been reversed. This service
must stop enforcing, logging, or wiring any part of it — REST, MCP, and the
outbound peer clients alike — and the Helm chart, OpenAPI contract, and
README must reflect a service with no bearer-key gate.

## Decision

We will remove the static-bearer-key auth layer entirely:

- Delete `internal/adapters/inbound/auth` (the whole package) and its MCP
  alias layer (`internal/adapters/inbound/mcp/auth.go`).
- `cmd/workforce`, `cmd/workforce-reports`, and `cmd/mcp` no longer construct
  an `Authenticator`, parse `AUTH_MODE`, or read `API_READ_KEY`/
  `API_READWRITE_KEY`/`MCP_READ_KEY`/`MCP_READWRITE_KEY`. Every route on all
  three binaries — including `/healthz` and the MCP Streamable HTTP endpoint
  at `/` and `/mcp` — is unauthenticated at this layer.
- `internal/adapters/inbound/http`'s `NewRouter`/`NewReportsRouter` drop the
  `WithAuth` option and the auth middleware group; `internal/adapters/inbound/mcp`'s
  `Handler`/`NewServer` drop scope gating on every tool, resource, and prompt.
- The outbound `fulfillmentexecution` and `laborperformance` clients drop
  `WithBearerToken`/`FULFILLMENT_EXECUTION_API_KEY`/`LABOR_PERFORMANCE_API_KEY`;
  they send no `Authorization` header.
- The Helm chart drops `auth.*`, `mcp.readKey`/`mcp.readWriteKey`/
  `mcp.existingSecret`, `fulfillmentExecution.apiKey`, `laborPerformance.apiKey`,
  the `AUTH_MODE` ConfigMap entry, and the auth/MCP-key Secret templates.
- `apis/openapi.yaml` drops `components.securitySchemes.bearerAuth`, the
  top-level `security:` requirement, and every per-operation `401`/`403`
  response. `apis/asyncapi.yaml` carried no auth content and is unchanged.
- README drops the `AUTH_MODE`/`API_READ_KEY`/`API_READWRITE_KEY`/
  `FULFILLMENT_EXECUTION_API_KEY`/`LABOR_PERFORMANCE_API_KEY` env rows and the
  API "Authentication" section.

[ADR-0017](./0017-adopt-fleet-rest-identity.md) is left in place, unedited,
as the historical record of the rollout — it is superseded, not deleted.

## Consequences

### Easier

- One less moving part on every request path across all three binaries: no
  key material to provision, rotate, or accidentally omit before a route
  starts rejecting traffic.
- The Helm chart and README are smaller and describe exactly what the
  binaries do today.

### Harder

- The REST, reports, and MCP surfaces are open to anyone who can reach them
  on the network — this service now relies entirely on network-level
  controls (namespace/cluster boundary, ingress policy) for access control.
  Re-adopting a fleet identity decision later is a fresh ADR, not a revert of
  this one.
