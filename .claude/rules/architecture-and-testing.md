---
paths:
  - "cmd/**"
  - "internal/**"
  - "migrations/**"
  - "features/**"
  - ".gremlins.yaml"
---

# Architecture layout, code standards, testing

Hexagonal / Ports & Adapters. Dependency rule: **domain depends on nothing;
application depends on domain; adapters depend on application/domain.** No
framework or SQL types in the domain layer. Enforced by an arch-go fitness
test (ADR-0007, `make arch-test`).

## Layer map (the non-obvious notes; the rest is discoverable with ls)

- `cmd/workforce/` OLTP composition root; `cmd/workforce-projector/`
  analytics writer (consumes analytics topic, projects);
  `cmd/workforce-reports/` analytics reader (read-only REST over the
  analytical DB); `cmd/mcp/` MCP server (Streamable HTTP), ADR-0008.
- `internal/domain/`: `associate` (AssociateShift aggregate: roster,
  certifications, breaks), `shiftplan` (ShiftPlan aggregate: committed
  headcount split across paths), `assignment` (LaborAssignment aggregate: one
  associate, one path, an interval), `pathcatalog` (process-path catalogue
  model, prefix-match Lookup, ADR-0013), `shared` (value objects: AssociateId,
  PathId, Certification, events).
- `internal/application/ports/` OUT ports: repos, EventPublisher,
  UnitOfWork, ProcessedEvents, Clock, MeasuredRateClient, IdleShareClient,
  InstalledCapacityClient, PathCatalogue. `usecases/`: one struct per use case
  (see domain-model.md).
- `internal/analytics/report/` Labor Utilization & Staffing read model +
  ports — depends on nothing.
- `internal/adapters/`: `inbound/http` (chi handlers for OLTP + reports,
  DTOs, RFC 7807 mapping), `inbound/kafka` (analytics consumer = projector),
  `inbound/mcp` (MCP tools incl. the curated labor-report tool),
  `outbound/postgres` (pgxpool repos + golang-migrate), `outbound/analyticsstore`
  (projection writer + read-only reader + memory store), `outbound/memory`,
  `outbound/events` (log/buffered + multi fan-out publisher), `kafka/cloudevents`
  (the ONLY CloudEvents 1.0 builder/decoder + exact type constants, ADR-0026),
  `outbound/kafka` (integration + analytics publishers, outbox relay sink,
  trace-context carrier), `outbound/fulfillmentexecution`
  (InstalledCapacityClient, ADR-0014), `outbound/laborperformance`
  (MeasuredRateClient, ADR-0012), `outbound/laborperformancecache`
  (event-fed measured-rate + idle-share cache, ADR-0019/0020),
  `outbound/filecatalog` (process-path catalogue YAML, ADR-0013),
  `outbound/kafkacatalog` (Kafka-sourced catalogue, `PATH_CATALOGUE_SOURCE=kafka`),
  `outbound/clock`, `outbound/telemetry` (OTel + trace-aware slog).
- `migrations/` OLTP SQL (incl. outbox table); `migrations/analytics/`
  analytical DB SQL, owned by the projector.

## Code standards

- Go 1.26, modules. Module path: `github.com/claudioed/workforce-management`.
- chi (`go-chi/chi/v5`), pgx/v5 + pgxpool, golang-migrate SQL migrations.
- Config via env (`DATABASE_URL`, `HTTP_ADDR`, see README.md's full env
  table for every service/adapter mode variable).
- Typed domain errors mapped to HTTP status + RFC 7807 `application/problem+json`
  in the adapter (ADR-0005) — never a bespoke error shape.
- JSON DTOs live in the http adapter; never leak domain structs directly.
- gofmt/go vet clean; every package has a doc comment.

## Testing

- Table-driven tests: domain + application (in-memory adapters); one
  `httptest` case per endpoint; build-tagged Postgres integration tests
  (`-tags=integration`, skipped without `DATABASE_URL`; outbox tests boot
  their own Postgres via testcontainers).
- BDD/acceptance: `features/*.feature` (Gherkin) run via godog against the
  real HTTP surface wired to in-memory adapters (`go test ./... -run
  TestFeatures -v`, ADR-0006). One feature file per invariant area:
  shift_plan, labor_assignment, breaks, staffing_gap.
- Mutation testing (gremlins, `.gremlins.yaml`): fast subset on
  `internal/domain/shiftplan` blocks CI; the full `internal/domain` run is
  scheduled, not blocking.
- Coverage gate: 90% over `internal/domain/...,internal/application/...`.

## Other gates

```bash
make vuln            # govulncheck — run after touching go.mod/go.sum
make mutation        # fast gremlins subset on internal/domain/shiftplan (blocks CI)
make mutation-full   # exhaustive gremlins over internal/domain (scheduled)
make integration     # needs Postgres/DATABASE_URL; outbox tests use testcontainers
```
