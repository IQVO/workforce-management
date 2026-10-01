# Project: Workforce Management (Supporting Bounded Context)

Owns "who is on shift, on which process path, at what rate; direct vs
indirect hours." Covers the **shift-start planning horizon** (a human commits
a headcount split across paths) plus **intra-shift assignment tracking**
(moving people between paths as backlogs deviate — the move itself is a human
call; this context makes the gap legible, it does not decide). It stops at
the **path boundary**: it never links an associate to a specific task —
individual task dispatch belongs to `fulfillment-execution`. Full narrative
docs (business context, DDD canvases, generated API reference) live at
<https://claudioed.github.io/workforce-management/> and in [`docs/`](docs/).

Source of truth for the domain model: `/Users/claudioed/docs/amazon-fulfillment-ddd.md`
and `/Users/claudioed/warehouse-systems-ddd.md`. Honor that ubiquitous language.

## Architecture (NON-NEGOTIABLE)

Hexagonal / Ports & Adapters. Strict dependency rule: **domain depends on
nothing; application depends on domain; adapters depend on application/domain.**
No framework or SQL types in the domain layer. Enforced by an arch-go fitness
test (ADR-0007, `make arch-test`).

```
cmd/workforce/                 OLTP composition root — main.go
cmd/workforce-projector/       analytics writer: consumes analytics topic, projects
cmd/workforce-reports/         analytics reader: read-only REST over the analytical DB
cmd/mcp/                       MCP server (Streamable HTTP), ADR-0008
internal/
  domain/
    associate/                  AssociateShift aggregate (roster, certifications, breaks)
    shiftplan/                  ShiftPlan aggregate (committed headcount split across paths)
    assignment/                 LaborAssignment aggregate (one associate, one path, an interval)
    pathcatalog/                process-path catalogue model (prefix-match Lookup), ADR-0013
    shared/                     value objects: AssociateId, PathId, Certification, events
  application/
    ports/                      OUT: repos, EventPublisher, UnitOfWork, ProcessedEvents,
                                 Clock, MeasuredRateClient, IdleShareClient, InstalledCapacityClient,
                                 PathCatalogue
    usecases/                   one struct per use case (see rules/domain-model.md)
  analytics/report/             Labor Utilization & Staffing read model + ports — depends on nothing
  adapters/
    inbound/http/                chi handlers (OLTP + reports), DTOs, RFC 7807 error mapping
    inbound/kafka/                analytics consumer (projector)
    inbound/mcp/                  MCP tools incl. the curated labor-report tool
    outbound/postgres/            pgxpool repos + golang-migrate migrations
    outbound/analyticsstore/      analytical projection writer + read-only reader + memory store
    outbound/memory/              in-memory repos for tests/local
    outbound/events/              log/buffered publisher + multi (fan-out) publisher
    kafka/cloudevents/            the ONLY CloudEvents 1.0 builder/decoder + exact type constants (ADR-0026)
    outbound/kafka/               integration + analytics publishers, outbox relay sink, trace-context carrier
    outbound/fulfillmentexecution/  InstalledCapacityClient HTTP client (ADR-0014)
    outbound/laborperformance/      MeasuredRateClient HTTP client (ADR-0012)
    outbound/laborperformancecache/ event-fed measured-rate + idle-share cache (ADR-0019, ADR-0020)
    outbound/filecatalog/           loads the process-path catalogue YAML (ADR-0013)
    outbound/kafkacatalog/          Kafka-sourced catalogue adapter (PATH_CATALOGUE_SOURCE=kafka)
    outbound/clock/                 system clock
    outbound/telemetry/             OTel setup (traces/metrics) + trace-aware slog handler
migrations/                    golang-migrate SQL files (OLTP, incl. outbox table)
migrations/analytics/          golang-migrate SQL files (analytical DB, owned by the projector)
web/                           workforce-mfe — Vite/React MFE remote, see rules/frontend.md
```

Deep-dive references, split out so this file stays a short index:

- **rules/domain-model.md** — ubiquitous language, aggregates & invariants,
  domain events, use cases, REST API surface.
- **rules/integrations.md** — outbound clients (measured rate, idle share,
  installed capacity), the process-path catalogue, the two opt-in consumed
  topics, Kafka events (CloudEvents 1.0) + transactional outbox, CORS.
- **rules/analytics-and-observability.md** — the analytics data product
  (ADR-0010), the MCP inbound adapter (ADR-0008), OTel traces/metrics/logs.
- **rules/frontend.md** — the `web/` micro-frontend remote.

## Events: CloudEvents 1.0 is MANDATORY

Every Kafka message this service produces or consumes (integration
`warehouse.<ctx>.events` AND analytics `warehouse.<ctx>.analytics`) is a
CloudEvents 1.0 event in structured content mode. This is a hard fleet rule,
not a preference:

- No flat envelope (`event_id`/`event_type`/`occurred_at`), no dual-write,
  no dual-read, no envelope toggle env var (`EVENT_ENVELOPE_MODE` is gone).
- Build/validate/(un)marshal with `github.com/cloudevents/sdk-go/v2/event`
  via `internal/adapters/kafka/cloudevents/`; transport stays kafka-go.
- Kafka header `content-type: application/cloudevents+json; charset=UTF-8`.
- Required attributes: `specversion=1.0`, `id` (UUID, stable across outbox
  redelivery), `source=/warehouse/workforce-management`, `type`, `subject` (aggregate id), `time`
  (occurred-at, UTC), `datacontenttype=application/json`,
  `dataschema=urn:warehouse:workforce-management:<events|analytics>:<EventName>:v<N>`.
- `type` = `com.warehouse.<subdomain>.<bounded-context>.<entity>.<EventName>`;
  for this service: `com.warehouse.wes.workforce-management.<entity>.<EventName>`. Breaking payload
  change => new `.v2` type + new dataschema version, never mutate.
- Consumers dispatch on the FULL `type`, ignore unknown types, dedupe on
  `id`, and DLQ/skip (never crash, never parse a legacy shape) anything that
  fails CloudEvents validation.

Full standard and the fleet's cross-service type catalogue: ADR-0026
(`docs/docs/adr/`).

Entity segments: `shiftplan` (ShiftPlanCommitted/Proposed, PathUnderstaffed),
`associate` (AssociateShift*/Break*/Certified), `assignment`
(LaborAssigned/Reassigned). `ShiftPlanCommitted` is fanned out one message per
`PathPlan` line, each with its own `id`; `subject` = Kafka key =
`<buildingId>/<shiftId>`. Exact type strings live in
`internal/adapters/kafka/cloudevents/types.go`.

## Key Commands

```bash
# Run the OLTP service (Postgres required)
docker compose up -d
export DATABASE_URL="postgres://workforce:workforce@localhost:5432/workforce?sslmode=disable"
export PATH_CATALOGUE_FILE=./process-paths.yaml   # required; see docs quickstart
go run ./cmd/workforce                # :8080, applies migrations on boot

# Fast pre-commit loop (no DB needed, ~1 min)
make check        # fmt-check, vet, build, lint, test (-race)

# Before pushing
make check-all    # check + coverage gate (90%) + arch-test + bdd

# Other gates
make vuln         # govulncheck — run after touching go.mod/go.sum
make mutation     # fast gremlins subset on internal/domain/shiftplan (blocks CI)
make mutation-full   # exhaustive gremlins over internal/domain (scheduled)
make integration  # needs Postgres/DATABASE_URL; outbox tests use testcontainers

# Docs site (Docusaurus, generates REST reference from apis/openapi.yaml)
cd docs && npm ci && npm run gen-api-docs && npm run build
```

`make help` lists every target; each mirrors a `.github/workflows/ci.yml` job
so local feedback matches CI. `lefthook install` wires `make check`/lint into
git hooks (pre-commit/pre-push) — optional, run `make check` proactively
regardless since hooks are per-clone.

## Code Standards

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

## Definition of Done

- `go build ./...`, `go vet ./...`, `go test ./...` (and `-race`) all green;
  gofmt clean.
- README.md updated: run steps, endpoints w/ curl examples, layering note,
  and the "stops at the path boundary" rationale if touched.
- Each of these four invariants has a dedicated failing-path test at domain
  + use-case + HTTP layers: `plannedHeads > installedStations` rejected on
  `ShiftPlan` commit; double-booking (second ACTIVE assignment for the same
  associate) rejected; assignment without required certification rejected;
  assignment while on an active break rejected.
- If `apis/openapi.yaml` changed, regenerate the docs site reference pages:
  `cd docs && npm run gen-api-docs` and commit the result (CI's
  `docs-api-drift` job fails on any diff) — see
  `docs/package.json`'s `gen-api-docs` script. `apis/asyncapi.yaml` has no
  generated pages today; its narrative counterpart is
  `docs/docs/ecosystem/integration.md` — update both together.
