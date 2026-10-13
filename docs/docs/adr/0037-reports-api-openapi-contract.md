---
id: 0037-reports-api-openapi-contract
slug: /adr/0037-reports-api-openapi-contract
title: 0037. The reports binary's routes are specified in apis/openapi-reports.yaml
sidebar_label: 0037. Reports API OpenAPI contract
sidebar_position: 38
description: "ADR 0037 — cmd/workforce-reports (GET /healthz, /reports/labor, /reports/labor/freshness) gets its own OpenAPI 3.0.3 spec, apis/openapi-reports.yaml, linted by Spectral, rendered as the Reports API reference and pinned by Go contract tests against the real router. Fleet gap-fix decision D18."
---

# 0037. The reports binary's routes are specified in `apis/openapi-reports.yaml`

## Status

Accepted. Implemented by the change that introduces this record (fleet
gap-fix wave 2026-10-12, decision D18). Complements
[ADR 0010](./0010-analytical-data-product.md), which introduced the reports
reader, without editing it.

## Context

`cmd/workforce-reports` serves the read side of the Labor Utilization &
Staffing data product: `GET /healthz`, `GET /reports/labor` and
`GET /reports/labor/freshness` (`NewReportsRouter` in
`internal/adapters/inbound/http/reports_handler.go`). Those routes were
described only in prose (the labor-report page). They appeared in no OpenAPI
file, so Spectral never linted them, the generated API reference never showed
them, and nothing failed when the router and the documentation diverged.
`apis/openapi.yaml` is the contract of a different binary (`cmd/workforce`,
port 8080, the OLTP database). Mixing the reports routes into it would make
the Schemathesis `contract` job fuzz routes that binary does not serve.

## Decision

1. **A separate spec.** `apis/openapi-reports.yaml` (OpenAPI 3.0.3) specifies
   exactly the routes `NewReportsRouter` serves, with the RFC 7807 `400`
   (`invalid-report-query`) and `500` (`report-store-error`) responses, the
   `LaborReport`/`LaborRow`/`Freshness`/`Health` schemas and the server
   `http://localhost:8092`. This follows the fleet convention of one spec per
   binary for `*-reports` binaries (labor-performance first).
2. **Linted.** The `api-lint` CI job runs Spectral on it with the same
   ruleset as `apis/openapi.yaml`.
3. **Rendered.** `docusaurus-plugin-openapi-docs` generates it into
   `docs/docs/api-reference/rest-reports/` (config id `workforceReports`);
   `npm run clean-api-docs && npm run gen-api-docs` regenerates both specs
   and the `docs-api-drift` job fails on any diff, untracked pages included.
4. **Contract-tested.** `TestReportsContract_RoutesMatchSpec` walks the chi
   router and fails when a served route is not in the spec or a specified
   route is not served. `TestReportsContract_ResponsesMatchSpec` drives every
   documented status of every operation through the real router and checks
   the status, the media type, every required property and that no response
   property is undocumented. Both live in
   `internal/adapters/inbound/http/reports_contract_test.go` and run in the
   `test` job.

## Consequences

- A new reports route, field or status must be added to the spec in the same
  change, or the contract tests fail.
- The contract is additive only: a breaking change to a row's shape is a new
  endpoint version, as the labor-report page's versioning section says.
- The Schemathesis `contract` job keeps fuzzing `apis/openapi.yaml` only; the
  reports spec is covered by the Go contract tests, which need no database.
