---
id: 0017-adopt-fleet-rest-identity
slug: /adr/0017-adopt-fleet-rest-identity
title: 0017. Adopt the fleet REST identity — static bearer keys with read/read-write scopes
sidebar_label: 0017. Fleet REST identity
sidebar_position: 18
description: "ADR 0017 — workforce-management adopts the fleet-wide REST identity decision (warehouse-ops-agent ADR 0005): static bearer keys mapped to read / read-write scopes, AUTH_MODE=enforce|log|off, one auth package per repository shared by the REST and MCP surfaces."
---

# 0017. Adopt the fleet REST identity — static bearer keys with read/read-write scopes

## Status

Superseded by [ADR-0018](./0018-remove-fleet-rest-identity.md). Originally
accepted, implemented in the same change that introduced this record.
Adopts warehouse-ops-agent
[ADR 0005](https://github.com/claudioed/warehouse-ops-agent/blob/develop/docs/docs/adr/0005-rest-identity-static-bearer-scopes.md)
(fleet-wide REST identity), which holds the full context and rationale.

## Decision

This service adopts the fleet decision verbatim. `internal/adapters/inbound/auth`
(the fleet template, unmodified) carries the `Authenticator`, `StaticKeyAuth`,
`Scope` and `Middleware` types; the MCP inbound adapter ([ADR-0008](./0008-mcp-inbound-adapter.md))
now aliases those types instead of keeping its own copy, so this repository has
exactly one identity implementation serving both surfaces. The chi routers mount
the middleware on every route except `/healthz`: `GET`/`HEAD`/`OPTIONS` require
the `read` scope and every other method `read-write` on `cmd/workforce`; every
`GET /reports/*` requires `read` on `cmd/workforce-reports`. Keys come from
`API_READ_KEY` / `API_READWRITE_KEY` (falling back to `MCP_READ_KEY` /
`MCP_READWRITE_KEY`), `AUTH_MODE=enforce|log|off` defaults to `enforce` when a
key is configured and to `off` (with a WARN) when none is, and failures are
RFC 7807 problems under this service's existing type base ([ADR-0005](./0005-rfc-7807-problem-details.md)):
401 `unauthenticated` with a `WWW-Authenticate: Bearer` challenge, 403
`insufficient-scope`. Both outbound REST clients (fulfillment-execution,
labor-performance) gain an optional bearer (`FULFILLMENT_EXECUTION_API_KEY`,
`LABOR_PERFORMANCE_API_KEY`) so this service can call peers once they enforce.
The Helm chart exposes `auth.mode` / `auth.readKey` / `auth.readWriteKey` /
`auth.existingSecret` and the per-peer `<peer>.apiKey`; warehouse-infra rolls
the fleet out in `log` mode first and flips to `enforce` after a clean soak.
