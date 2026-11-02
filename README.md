# Workforce Management

[![CI](https://github.com/IQVO/workforce-management/actions/workflows/ci.yml/badge.svg?branch=develop)](https://github.com/IQVO/workforce-management/actions/workflows/ci.yml?query=branch%3Adevelop) [![coverage gate](https://img.shields.io/badge/coverage%20gate-90%25-brightgreen)](https://github.com/IQVO/workforce-management/blob/develop/Makefile) [![Go](https://img.shields.io/badge/go-1.26.10-00ADD8?logo=go&logoColor=white)](https://github.com/IQVO/workforce-management/blob/develop/go.mod) [![release](https://img.shields.io/github/v/release/IQVO/workforce-management?sort=semver)](https://github.com/IQVO/workforce-management/releases) [![docs](https://img.shields.io/badge/docs-iqvo.github.io-blue)](https://iqvo.github.io/workforce-management/) [![license](https://img.shields.io/badge/license-MIT-blue)](https://github.com/IQVO/workforce-management/blob/develop/LICENSE)

A Supporting bounded context that owns "who is on shift, on which process
path, at what rate; direct vs indirect hours." It covers the shift-start
planning horizon (a human commits a split of headcount across paths) and
intra-shift assignment tracking (moving people between paths as backlogs
deviate — the move itself stays a human call; this context makes the gap
legible, it does not decide).

## Disclaimer

This project is a domain-driven design study. It is not associated with,
endorsed by or affiliated with any company, and it uses no company's code,
data, logos or trademarks. All names, figures and examples are illustrative
or synthetic.

## Documentation

Full documentation is published at
**<https://iqvo.github.io/workforce-management/>** — the business context
and the path-boundary reasoning, the DDD model (aggregates, invariants, all ten
domain events), the ddd-crew DDD artifact pack (core domain chart, bounded
context canvas, aggregate design canvas, domain message flows, EventStorming,
UML class / ER / sequence diagrams — see
[`docs/docs/ddd/ddd-artifacts.md`](docs/docs/ddd/ddd-artifacts.md)), an
interactive REST API reference generated from `apis/openapi.yaml`, the
CloudEvents catalog from `apis/asyncapi.yaml`, the ecosystem context map, and
the Architecture Decision Records. Source lives in [`docs/`](docs/) and deploys
on every push to `main`.

| Area | Pages |
| --- | --- |
| Overview | [Introduction](docs/docs/overview/index.md), [Architecture](docs/docs/overview/architecture.md), [Quickstart](docs/docs/overview/quickstart.md) |
| Operations | [Configuration](docs/docs/operations/configuration.md) (every env var, per binary), [Runbook](docs/docs/operations/runbook.md), [Observability](docs/docs/operations/observability.md), [Troubleshooting](docs/docs/operations/troubleshooting.md) |
| Development | [Testing](docs/docs/development/testing.md) (test pyramid, make targets, CI jobs) |
| DDD | [Use cases](docs/docs/ddd/use-cases.md), [Subdomain classification](docs/docs/ddd/subdomain-classification.md), [DDD artifact pack](docs/docs/ddd/ddd-artifacts.md) |
| Ecosystem | [Integration](docs/docs/ecosystem/integration.md), [Context map](docs/docs/ecosystem/context-map.md) |
| MCP | [Tools, resources and prompts](docs/docs/mcp/tools.md), [Governance charter](docs/docs/mcp/governance-charter.md) |
| A2A | [A2A inbound adapter (ADR-0039)](docs/docs/adr/0039-a2a-inbound-adapter.md) |
| Analytics | [Labor report](docs/docs/analytics/labor-report.md) |
| Decisions | [ADR index](docs/docs/adr/index.md) |

## Why this context stops at the path boundary

Workforce Management never links an associate to a specific task — it only
tracks which **path** (pack, stow, pick, ...) an associate is currently
assigned to, at the granularity of a shift-length interval. Dispatching an
individual unit of work to a claiming station is a different problem with a
different cadence: workforce assignment changes over minutes to hours as
headcount is rebalanced across paths, while task dispatch changes over
seconds as work is claimed off a queue. Fusing the two into one aggregate
would force task-dispatch policy changes to touch workforce planning code
(and vice versa) even though nothing about the labor picture actually
changed. Keeping the seam here — LaborAssignment ends at "this associate is
on this path," full stop — lets Fulfillment Execution own task dispatch
independently, on its own release cadence, against this context's read model
(`GetStaffingGap`) rather than its write model.

## Architecture

Hexagonal / Ports & Adapters. Domain depends on nothing; application depends
on domain; adapters depend on application/domain.

```
cmd/workforce/                composition root (OLTP)
cmd/workforce-projector/      analytics writer: consumes the analytics topic, projects
cmd/workforce-reports/        analytics reader: read-only REST over the analytical DB
cmd/mcp/                       MCP server (Streamable HTTP)
cmd/a2a/                       A2A agent (JSON-RPC over HTTP, Agent Card at /.well-known/agent-card.json)
internal/
  domain/
    associate/                 AssociateShift aggregate
    shiftplan/                 ShiftPlan aggregate
    assignment/                LaborAssignment aggregate
    pathcatalog/               process-path catalogue model (prefix-match Lookup), ADR-0013
    shared/                    value objects + domain events
  application/
    ports/                     repos, EventPublisher, UnitOfWork, ProcessedEvents, Clock,
                               MeasuredRateClient, IdleShareClient, InstalledCapacityClient, PathCatalogue
    usecases/                  one struct per use case
  analytics/
    report/                    Labor Utilization & Staffing read model + ports (depends on nothing)
  adapters/
    inbound/http/               chi handlers (OLTP + reports), DTOs, error mapping
    inbound/kafka/              analytics consumer (projector)
    inbound/mcp/                MCP tools incl. the curated labor-report tool
    inbound/a2a/                A2A skills: staffing_status (read), assign_labor (write)
    outbound/postgres/          pgxpool repos + golang-migrate migrations
    outbound/analyticsstore/    analytical projection (writer) + report (read-only reader) + memory store
    outbound/memory/            in-memory test doubles for unit and BDD tests ONLY; never imported by cmd/** (ADR-0038, sensor TestRuntimeBinaryHasNoInMemoryAdapters)
    outbound/events/            buffered/log publisher (test doubles) + multi (fan-out) publisher
    outbound/clock/             system clock
    outbound/kafka/             integration publisher + analytics publisher + outbox relay sink + trace-context header carrier
    outbound/fulfillmentexecution/  InstalledCapacityClient: GET /capacity/{capability} (ADR-0014)
    outbound/laborperformance/      MeasuredRateClient over HTTP (ADR-0012)
    outbound/laborperformancecache/ event-fed measured-rate + idle-share cache (ADR-0019, ADR-0020)
    outbound/kafkacatalog/          process-path catalogue fed by process-path-management's topic (ADR-0013, ADR-0030)
    outbound/telemetry/         OTel setup (traces/metrics) + trace-aware slog handler
migrations/                   golang-migrate SQL files (OLTP)
migrations/analytics/         golang-migrate SQL files (analytical DB, owned by the projector)
```

## Design decisions worth calling out

- **AssignLabor ends the prior assignment rather than rejecting the call.**
  The "exactly one ACTIVE assignment per associate" invariant is enforced by
  construction: `LaborAssignment` is one aggregate per associate holding a
  single optional active interval, so assigning a second path always closes
  the first (raising `LaborReassigned`) instead of needing a reject-on-conflict
  check. This matches how rebalancing actually happens on the floor — a
  supervisor moves someone, they don't first have to "unassign."
- **Path → required certification is a naming convention**, not a separate
  concept: a path's required certification is the `Certification` with the
  same name as its `PathId` (path `"pack"` requires certification `"pack"`).
  This context has no `AssignLabor`-adjacent port for path requirements in its
  spec, so introducing one would be scope creep; the convention is documented
  here instead.
- **`installedStations` is supplied by the caller** on `CommitShiftPlan`,
  not looked up from Work Planning; this context enforces
  `plannedHeads <= installedStations` independently (per its own invariant).
  Since [ADR-0014](docs/docs/adr/0014-installed-capacity-ceiling.md) each line
  is **also** checked against the live installed capacity fetched from
  fulfillment-execution (`GET /capacity/{capability}`). This is a second,
  independent ceiling that fails loud (503) when it cannot be verified.
  The capability is **resolved through the process-path catalogue**, never
  taken from the path id: path `PICK` (or `pick-zone-a`) declares
  `requiredCapabilities: [pick]`, so the ceiling is the count of stations
  registered with `pick`. A path requiring several capabilities is capped by
  the scarcest one (MIN); a path the catalogue does not declare is rejected
  `400 unknown-path-id` before fulfillment-execution is called.
- **`GetStaffingGap` takes `siteCode`/`shiftId` as query parameters**
  (`GET /paths/{pathId}/staffing-gap?siteCode=&shiftId=`) because
  `ShiftPlan` is keyed by site + shift, and a path's planned heads only
  make sense within one committed plan. **Site is the ubiquitous-language
  term**: `siteCode` (the facility-layout Site code) is the canonical name of
  the plan key and `buildingId` its deprecated alias — same value, same stored
  column, nothing removed ([ADR 0035](docs/docs/adr/0035-sitecode-converges-building-id.md)).
- **`PathPlan.plannedHours <= plannedHeads * maxHoursPerShift`** is how
  "sum of hours valid" is enforced on `ShiftPlan`: a path line can't commit
  more total hours than its planned heads could work within one shift's max.

## Run it

Postgres **and** Kafka are the only runtime ([ADR-0038](docs/docs/adr/0038-postgres-and-kafka-are-the-only-runtime.md)):
there is no in-memory mode, no log sink and no file catalogue. Missing
`DATABASE_URL` or `KAFKA_BROKERS` is a boot error that names the variable,
raised before the listener binds.

```bash
docker compose up -d --wait          # Postgres 16 on :5432 and a single-node Kafka (KRaft) on :9092
export DATABASE_URL="postgres://workforce:workforce@localhost:5432/workforce?sslmode=disable"
export KAFKA_BROKERS=localhost:9092
go run ./cmd/workforce                # applies migrations, then serves on :8080
```

The process-path catalogue is replayed from `process-path-management`'s Kafka
topic (`warehouse.process-path-management.events`); on a fresh local broker it
is empty until that context publishes (the minimal way to seed it is shown in
the [quickstart](docs/docs/overview/quickstart.md)). `/readyz` is `200` only
while Postgres answers a ping **and** a Kafka broker answers; `/healthz` stays
liveness only.

### Configuration
Env vars:

| Var | Required | Default | Meaning |
|-----|----------|---------|---------|
| `DATABASE_URL` | **yes** | — | Postgres connection string; boot error `DATABASE_URL is required: this context runs on real Postgres` without it |
| `KAFKA_BROKERS` | **yes** | — | comma-separated broker list; boot error `KAFKA_BROKERS is required: this context runs on real Kafka` without it. Used by the outbox relay, the catalogue and labor-performance consumers and `/readyz` |
| `HTTP_ADDR` | no | `:8080` | listen address |
| `MIGRATIONS_PATH` | no | `migrations` | path to golang-migrate SQL files |
| `MIGRATIONS_DATABASE_URL` | no | `DATABASE_URL` | direct (non-PgBouncer) connection string used ONLY by the boot-time golang-migrate step ([ADR-0025](docs/docs/adr/0025-migrations-direct-postgres-connection.md)); the request pool always uses `DATABASE_URL` |
| `MAX_HOURS_PER_SHIFT` | no | `8` | the configured max-hours-per-shift cap |
| `HOUSEKEEPING_INTERVAL` | no | `1h` | how often the housekeeping sweeper runs ([ADR-0028](docs/docs/adr/0028-housekeeping-sweeper-idempotency-keys-and-outbox.md)); `0` disables it |
| `IDEMPOTENCY_KEY_TTL` | no | `24h` | how long an `idempotency_keys` row is kept; `0` keeps them forever |
| `OUTBOX_RETENTION` | no | `168h` | how long a PUBLISHED `outbox_events` row is kept; `0` keeps them forever |
| `SHUTDOWN_DRAIN_DELAY` | no | `10s` | how long the process keeps serving after `/readyz` flips to 503 on SIGTERM ([ADR-0022](docs/docs/adr/0022-resilience-circuit-breakers-retry-dlq-shutdown.md)); `0` disables the wait |
| `EVENT_PUBLISHER` | no | `kafka` | kept only because the chart and warehouse-infra set it: unset or `kafka` are accepted; `log` fails the boot with `EVENT_PUBLISHER=log is not supported: this context publishes to real Kafka`, anything else with `unknown EVENT_PUBLISHER`. Events are written to the `outbox_events` table in the same transaction as the aggregate and relayed to both topics by an in-process relay that is always on ([ADR-0016](docs/docs/adr/0016-transactional-outbox.md), [ADR-0038](docs/docs/adr/0038-postgres-and-kafka-are-the-only-runtime.md)); they never go straight to the broker from a request |
| `OUTBOX_RELAY_INTERVAL` | no | `1s` | how long the outbox relay sleeps between passes that found nothing to publish (Go duration). A full batch is followed immediately by another pass |
| `LABOR_PERFORMANCE_MODE` | no | `kafka-cache` | `kafka-cache` (default) replaces the labor-performance call with a local read model fed by labor-performance's `warehouse.labor-performance.events` integration topic (ADR-0019), and additionally feeds the idle-share staffing signal (see `IDLE_SHARE_TRIM_THRESHOLD` below and ADR-0020). `http` is the REST rollback of that edge (needs `LABOR_PERFORMANCE_BASE_URL`; `docs/rollback-switches.md` R10), a REST sync edge ADR-0038 leaves alone. `permissive` is gone: `LABOR_PERFORMANCE_MODE=permissive is not supported: this context runs on real Kafka` |
| `LABOR_PERFORMANCE_BASE_URL` | when `LABOR_PERFORMANCE_MODE=http` | — | labor-performance's base URL |
| `IDLE_SHARE_TRIM_THRESHOLD` | no | `0.30` | Idle share (fraction, e.g. `0.3` = 30%) above which `ProposePathPlan` trims its proposed heads (idleness-as-staffing-signal, ADR-0020). Only takes effect when `LABOR_PERFORMANCE_MODE=kafka-cache` is also set — no other mode has an idle-share signal to trim against, so `ProposePathPlan` fails open (no trim) regardless of this value otherwise |
| `INSTALLED_CAPACITY_MODE` | no | `permissive` | `http` or `permissive` — selects the `CommitShiftPlan` live installed-capacity ceiling client from fulfillment-execution (ADR-0014). This one is fail-LOUD: every `CommitShiftPlan` call is rejected until `http` mode is set, since a shift-plan commit mutates real state. It is a REST sync edge, so ADR-0038 deliberately leaves it unchanged (follow-up) |
| `FULFILLMENT_EXECUTION_BASE_URL` | when `INSTALLED_CAPACITY_MODE=http` | — | fulfillment-execution's base URL |
| `PATH_CATALOGUE_SOURCE` | no | `kafka` | the only source: the process-path catalogue is replayed from `process-path-management`'s `warehouse.process-path-management.events` topic into a local catalogue (the service waits up to 60s for the replay before serving). `file` fails the boot with `PATH_CATALOGUE_SOURCE=file is not supported: this context runs on real Kafka`; `PATH_CATALOGUE_FILE` and the YAML loader are gone ([ADR-0038](docs/docs/adr/0038-postgres-and-kafka-are-the-only-runtime.md), [ADR-0030](docs/docs/adr/0030-kafka-sourced-process-path-catalogue.md)) |
| `CORS_ALLOWED_ORIGINS` | no | `http://localhost:5173,http://localhost:5185` | comma-separated browser origins allowed by the CORS middleware |
| `LOG_LEVEL` | no | `info` | `debug`\|`info`\|`warn`\|`error` (case-insensitive) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | `localhost:4317` | OTel Collector gRPC endpoint — see [Observability](#observability) |
| `OTEL_SERVICE_NAME` | no | `workforce-management` | `service.name` resource attribute |
| `SERVICE_VERSION` | no | `dev` | `service.version` resource attribute (overridable at build time with `-ldflags "-X main.version=..."`) |
| `ENVIRONMENT` | no | `local` | `deployment.environment.name` resource attribute |

## Analytics data product (Labor Utilization & Staffing report)

An additive, isolated analytical read model built from this service's own domain
events — a lightweight data mesh with no central platform. It runs as **two more
processes and a separate analytical database**; the OLTP write path is untouched.
See [ADR-0010](docs/docs/adr/0010-analytical-data-product.md) and the
[report contract](docs/docs/analytics/labor-report.md).

The OLTP service fans domain events onto a **separate** analytics topic
(`warehouse.workforce.analytics`) through the same always-on outbox relay; the
integration topic and publisher are left untouched.

```bash
# 1. the fleet's shared broker reachable at localhost:9092 (the warehouse-infra
#    kind cluster exposes it there; so does `docker compose up -d --wait`) and
#    an analytical Postgres database reachable via ANALYTICS_DATABASE_URL.

# 2. OLTP service, fanning events onto the analytics topic:
export KAFKA_BROKERS=localhost:9092
go run ./cmd/workforce

# 3. Projector — the ONLY writer of the analytical DB. Runs migrations/analytics
#    on start, consumes the analytics topic from the earliest offset, projects:
export ANALYTICS_DATABASE_URL="postgres://workforce:***@localhost:5432/workforce_analytics?sslmode=disable"
go run ./cmd/workforce-projector      # admin/health on :8091

# 4. Reports — read-only reader over the analytical DB, serves REST:
export ANALYTICS_DATABASE_URL="postgres://workforce_ro:***@localhost:5432/workforce_analytics?sslmode=disable"
go run ./cmd/workforce-reports        # :8092

# 5. Query the report:
curl 'http://localhost:8092/reports/labor?from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z'
curl 'http://localhost:8092/reports/labor/freshness'
```

Analytics env vars (projector + reports):

| Var | Required | Default | Meaning |
|-----|----------|---------|---------|
| `ANALYTICS_DATABASE_URL` | yes | — | analytical Postgres connection string (a **read-only** role for `workforce-reports`) |
| `ANALYTICS_MIGRATIONS_PATH` | no | `migrations/analytics` | projector-only: path to the analytical migrations |
| `KAFKA_BROKERS` | no | `localhost:9092` | projector-only: broker list for the analytics topic |
| `ADMIN_ADDR` | no | `:8091` | projector-only: health endpoint listen address |
| `HTTP_ADDR` | no | `:8092` | reports-only: REST listen address |

Optionally expose the curated read-only MCP tool `get_workforce_labor_report`
by setting `REPORTS_BASE_URL` (e.g. `http://localhost:8092`) on `cmd/mcp`; it
calls the reports REST and never opens the analytical database itself.

## Running the MCP server in Kubernetes

The MCP server ([ADR-0008](docs/docs/adr/0008-mcp-inbound-adapter.md)) is a
fourth binary in the same image, `/app/mcp`, and the Helm chart deploys it as a
separate Deployment + ClusterIP Service (`<release>-mcp`, port 8090) when
`mcp.enabled=true` (default `false`, so existing releases are unaffected). It
runs the same use cases over the same `DATABASE_URL` secret as the HTTP
service and reads `MCP_ADDR` (default `:8090`). `GET /healthz` serves
liveness/readiness probes; the MCP Streamable HTTP endpoint is mounted at both
`/` and `/mcp` (so `http://<release>-mcp:8090/mcp` is the in-cluster endpoint
to hand to warehouse-ops-agent). When `analytics.enabled` is also true,
`REPORTS_BASE_URL` defaults to the in-cluster reports Service so
`get_workforce_labor_report` works without extra values; override it with
`mcp.reportsBaseUrl`.

```bash
helm upgrade --install workforce-management charts/workforce-management \
  --set database.url="postgres://..." \
  --set mcp.enabled=true
```

## Running the A2A agent in Kubernetes

The A2A agent ([ADR-0039](docs/docs/adr/0039-a2a-inbound-adapter.md)) is a
fifth binary in the same image, `/app/a2a`, and the Helm chart deploys it as a
separate Deployment + ClusterIP Service (`<release>-a2a`, port 8093) when
`a2a.enabled=true` (default `false`). It runs the same use cases over the same
`DATABASE_URL` secret as the HTTP service and reads `A2A_ADDR` (default
`:8093`) and the mandatory `A2A_AGENT_URL`, the absolute URL advertised on the
Agent Card (the chart defaults it to the in-cluster Service DNS). `GET /healthz`
serves the probes; the Agent Card is at `/.well-known/agent-card.json` and the
JSON-RPC endpoint is mounted at both `/` and `/a2a`
(`http://<release>-a2a:8093/a2a` is the fleet's `*_A2A_ENDPOINT` convention).

Two skills, called as a `SendMessage` data part
`{"skill": "<id>", "arguments": {...}}`:

- `staffing_status` (read): `{siteCode, shiftId, pathId[, charge, plannedRate]}`
  returns planned vs active heads and whether the path is understaffed, plus the
  headcount needed for the charge when both optional arguments are given.
- `assign_labor` (write): `{associateId, pathId}` assigns an associate to a path
  and publishes `LaborAssigned` / `LaborReassigned` through the outbox. Not
  idempotent: do not blindly retry an ambiguous failure.

Domain rejections come back as an in-band RFC 7807 `problem` with the same
status classification as REST. A2A carries delegation only; the facts still
leave as CloudEvents on Kafka.

## API

All bodies are JSON. `{id}` and `{pathId}` are path parameters.

```bash
# Start an associate's shift with initial certifications. siteCode is OPTIONAL
# (ADR-0034): the canonical facility-layout Site code the associate works at.
curl -X POST localhost:8080/associates/assoc-1/shift-starts \
  -d '{"certifications":["pack"],"siteCode":"WH1"}'

# Add a certification
curl -X POST localhost:8080/associates/assoc-1/certifications \
  -d '{"certification":"hazmat"}'

# Propose heads for a path (pure computation, ceil(charge/resolvedRate); not committed).
# plannedRate is OPTIONAL: omit it (or send <= 0) to fall back to a real
# measured rate fed back from labor-performance for pick/pack/slam paths
# (feature: close-the-loop measured rate, ADR-0012). Response includes
# resolvedRate + rateSource ("caller" or "measured") so it's always clear
# where the number came from. When LABOR_PERFORMANCE_MODE=kafka-cache is
# set and the observed idle share for this path's task type exceeds
# IDLE_SHARE_TRIM_THRESHOLD (default 0.30), proposedHeads is further
# trimmed (floored at 1 head) and trimReason explains why
# (idleness-as-staffing-signal, ADR-0020).
curl -X POST localhost:8080/paths/pack/plan-proposals \
  -d '{"siteCode":"WH1","charge":100,"plannedRate":30}'
# -> {"pathId":"pack","proposedHeads":4,"resolvedRate":30,"rateSource":"caller"}
# (siteCode is canonical; "buildingId" is a deprecated alias with the same
#  value, and sending both with different values is 422
#  conflicting-site-and-building -- ADR-0035)

curl -X POST localhost:8080/paths/pack/plan-proposals \
  -d '{"siteCode":"WH1","charge":100}'
# -> falls back to labor-performance's measured rate for PACK when
#    LABOR_PERFORMANCE_MODE=http and data exists; otherwise 0 proposed heads.

# Commit a shift plan (human-committed headcount split across paths).
# Requires INSTALLED_CAPACITY_MODE=http: in the default permissive mode every
# commit is rejected with 503 installed-capacity-unavailable (ADR-0014), and a
# line whose plannedHeads exceeds fulfillment-execution's live capacity is 409.
# EVERY POST requires an Idempotency-Key header (ADR-0027, ADR-0040): a
# missing key is 400 idempotency-key-required, a key that is not 1-128
# printable ASCII is 400 idempotency-key-invalid, the same key with a
# different method/path/body is 409 idempotency-key-reused, and a retry with
# the same key + request replays the stored response with
# "Idempotency-Replayed: true". The key is global (not per route).
curl -X POST localhost:8080/shift-plans \
  -H 'Idempotency-Key: 6f1d2c3a-commit-wh1-shift-1' \
  -d '{"siteCode":"WH1","shiftId":"shift-1","lines":[
        {"pathId":"pack","plannedHeads":3,"plannedRate":30,"plannedHours":24,"installedStations":10}
      ]}'

# Assign an associate to a path (ends any prior active assignment)
curl -X POST localhost:8080/associates/assoc-1/assignments \
  -H 'Idempotency-Key: 0b7e9a41-assign-assoc-1-pack' \
  -d '{"pathId":"pack"}'

# Break start/end
curl -X POST localhost:8080/associates/assoc-1/break-starts
curl -X POST localhost:8080/associates/assoc-1/break-ends

# Staffing gap read model for a path within a committed plan
# -> {"pathId":"pack","plannedHeads":3,"activeHeads":1,"understaffed":true}
#    plus observedIdleShare (a 0..1 fraction; observedIdlePct is its deprecated
#    alias, same value) when LABOR_PERFORMANCE_MODE=kafka-cache has a signal
#    for the path (ADR-0020, ADR-0033). Unknown path ids are 400 unknown-path-id.
curl "localhost:8080/paths/pack/staffing-gap?siteCode=WH1&shiftId=shift-1"

# siteCode is the plan key AND the associate scope: only associates with an
# active shift at that site are counted (ADR-0034, ADR-0035); the response
# echoes "siteCode". The deprecated buildingId alias finds the same plan but a
# call giving ONLY buildingId is not scoped (fleet-wide count, exactly as
# before). Also available on the MCP get_staffing_gap tool.
curl "localhost:8080/paths/pack/staffing-gap?buildingId=WH1&shiftId=shift-1"

# Staffing gap for EVERY path planned in one committed shift plan (ADR-0029)
# -> a JSON array of the same per-path objects (canonical route, ADR-0035)
# Paged: {"items":[...],"nextCursor":"..."}; pass ?limit=1-500 and ?cursor=
# (the previous nextCursor) until nextCursor is absent.
curl "localhost:8080/sites/WH1/shifts/shift-1/staffing-gap?limit=100"
# Deprecated alias (answers "Deprecation: true"): /buildings/WH1/shifts/shift-1/staffing-gap

# End an associate's shift (closes any active assignment first)
curl -X POST localhost:8080/associates/assoc-1/shift-ends

curl localhost:8080/healthz   # liveness
curl localhost:8080/readyz    # readiness: 503 {"status":"not_ready"} once shutdown begins (ADR-0022)
```

## Errors

Every error response is [RFC 7807](https://www.rfc-editor.org/rfc/rfc7807)
`application/problem+json`, not a bespoke shape:

```bash
curl -i -X POST localhost:8080/associates/ghost/certifications \
  -d '{"certification":"hazmat"}'
```

```
HTTP/1.1 404 Not Found
Content-Type: application/problem+json

{
  "type": "https://errors.workforce-management.warehouse-systems.dev/resource-not-found",
  "title": "Resource not found",
  "status": 404,
  "detail": "not found",
  "instance": "/associates/ghost/certifications"
}
```

`type` identifies the error CATEGORY (fixed per sentinel domain error, not
per occurrence) and does not need to resolve; `title` is the fixed
category-level summary; `detail` carries the specific `err.Error()` text for
this occurrence; `instance` is the request path.

## Integration

This service can publish `ShiftPlanCommitted` to the shared warehouse-systems
Kafka broker so other bounded contexts can react to committed headcount
without calling back into this service. Two consume it today:
`wes-work-planning` (projects it into its own `LaborPlanObserved` read model,
keyed by `path_id`) and `warehouse-planning` (its labor-capacity consumer,
group `warehouse-planning-labor-capacity`). Inbound,
Kafka is the only event runtime: `KAFKA_BROKERS` is mandatory (the service
refuses to boot without it) and the process-path catalogue
(`warehouse.process-path-management.events`) and `labor-performance`'s
`warehouse.labor-performance.events` (`LABOR_PERFORMANCE_MODE=kafka-cache`, the
default) are always consumed,
each into a local cache rebuilt from the earliest offset under a
per-process consumer group. It also makes two synchronous calls:
fulfillment-execution's `GET /capacity/{capability}` (`INSTALLED_CAPACITY_MODE=http`)
and, only as the REST rollback, labor-performance's `GET /task-types/{taskType}/performance`
(`LABOR_PERFORMANCE_MODE=http`). See
[docs/docs/ecosystem/integration.md](docs/docs/ecosystem/integration.md) for
the full edge list.

- **Topic**: `warehouse.workforce.events`
- **Broker**: `KAFKA_BROKERS` (mandatory, no default) — the fleet's one
  shared broker (in the `warehouse-infra` kind cluster, exposed on the host
  at `localhost:9092`); this repo's own `docker-compose.yml` also runs a
  single-node broker for local runs.
- **Selection**: none. The only publisher is Kafka through the outbox
  (`EVENT_PUBLISHER` is kept only because the chart sets it: unset or `kafka`;
  `log` is a boot error). The in-memory/log publisher is a test double and
  never reaches a binary ([ADR-0038](docs/docs/adr/0038-postgres-and-kafka-are-the-only-runtime.md)).
- **Delivery** ([ADR-0016](docs/docs/adr/0016-transactional-outbox.md)): the
  use case inserts the already-encoded message(s)
  for BOTH topics into `outbox_events` inside the same Postgres transaction
  as the aggregate, and a relay goroutine in `cmd/workforce` drains that
  table onto Kafka every `OUTBOX_RELAY_INTERVAL`. The store and the topics
  therefore cannot diverge; delivery is at-least-once, per-key ordered, and
  lands within one relay interval of the HTTP response.
- **Fan-out**: a `ShiftPlan` has multiple `PathPlan` lines. `CommitShiftPlan`
  with 3 path lines publishes **3** Kafka messages — one per path line, each
  carrying that single path's `planned_heads`/`planned_rate`/`planned_hours`.
  This matches how `wes-work-planning` keys its read model, by `path_id`.
- **Envelope**: CloudEvents 1.0, mandatory, structured content mode
  ([ADR-0026](docs/docs/adr/0026-cloudevents-mandatory-event-envelope.md)).
  Every message carries the Kafka header
  `content-type: application/cloudevents+json; charset=UTF-8`; each fanned-out
  line has its own `id`, and `subject` equals the Kafka key:

```json
{
  "specversion": "1.0",
  "id": "uuid-v4",
  "source": "/warehouse/workforce-management",
  "type": "com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted",
  "subject": "<site_code>/<shift_id>",
  "datacontenttype": "application/json",
  "dataschema": "urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1",
  "time": "2026-08-21T22:00:00Z",
  "data": {
    "site_code": "...",
    "building_id": "... (deprecated, same value as site_code)",
    "shift_id": "...",
    "path_id": "...",
    "planned_heads": 3,
    "planned_rate": 30,
    "planned_hours": 24
  }
}
```

Smoke test against the shared broker:

```bash
# shared broker reachable at localhost:9092 (warehouse-infra kind cluster)
export DATABASE_URL="postgres://workforce:workforce@localhost:5432/workforce?sslmode=disable"
export KAFKA_BROKERS=localhost:9092
go run ./cmd/workforce

curl -X POST localhost:8080/shift-plans \
  -d '{"siteCode":"WH1","shiftId":"shift-1","lines":[
        {"pathId":"pack","plannedHeads":3,"plannedRate":30,"plannedHours":24,"installedStations":10},
        {"pathId":"pick","plannedHeads":2,"plannedRate":25,"plannedHours":16,"installedStations":10}
      ]}'

# in another terminal, confirm 2 messages landed on the topic (any Kafka CLI
# pointed at the shared broker, e.g. kubectl exec into kafka-controller-0):
kafka-console-consumer.sh \
  --bootstrap-server localhost:9092 --topic warehouse.workforce.events --from-beginning --max-messages 2
```

## Observability

Traces and metrics are exported over **OTLP/gRPC** to an OpenTelemetry
Collector; logs are structured JSON on stdout, correlated to traces by
`trace_id`/`span_id`. There is no `/metrics` scrape endpoint on this service
— the Collector does Prometheus exposition, so the pod only pushes.

A Collector is expected at `OTEL_EXPORTER_OTLP_ENDPOINT` (default
`localhost:4317`). In the `warehouse-infra` kind cluster the Helm chart points
this at the in-cluster Collector Service
(`otel-collector.observability.svc.cluster.local:4317`, see `otel.endpoint` in
`charts/workforce-management/values.yaml`).

**If no Collector is reachable, nothing breaks.** The OTLP exporters are
non-blocking — no `grpc.WithBlock()` dial option — so telemetry is silently
dropped while the service starts and serves exactly as it otherwise would.
That is enforced by a test
(`TestSetupDoesNotBlockWithoutACollector`), not just by convention.

### What gets exported

| Signal | What |
|--------|------|
| Traces | one server span per HTTP request, named by **route pattern** (`POST /associates/{id}/assignments`), not the raw path, so span-name cardinality stays bounded by the route table |
| Traces | a client child span per Postgres round-trip via `otelpgx`, carrying the **parameterized** SQL (`$1`, `$2`, … — literal values are never recorded) |
| Traces | with `EVENT_PUBLISHER=kafka`, the W3C trace context is captured into each outbox row's headers when the event is raised; the relay's `kafka.publish outbox` producer span forwards those headers untouched, so a consumer's span parents onto the originating request's trace |
| Metrics | `http.server.request.duration` (histogram, seconds, OTel HTTP semconv) |
| Metrics | `workforce.labor_assignments` (counter) — every `AssignLabor` attempt, attributed `workforce.assignment.outcome=accepted\|rejected`, `workforce.path.id`, and on rejection `workforce.assignment.reason` (`uncertified`, `on_break`, `shift_ended`, `max_hours_exceeded`, `associate_not_found`, `internal_error`) |
| Metrics | Go runtime metrics (goroutines, GC, memory) via `contrib/instrumentation/runtime` |
| Logs | JSON to stdout via `log/slog`, level from `LOG_LEVEL`; request-scoped lines carry `trace_id`, `span_id`, `route` and `request_id` |

`workforce.labor_assignments` is instrumented **in the use case**, not in the
HTTP handler, so it counts real domain outcomes rather than requests. Note
there is no `double_booked` rejection reason: this context resolves a second
active assignment by ending the prior one and raising `LaborReassigned`, so
double-booking is prevented by construction rather than rejected — see
`LaborAssignment.Assign`.

### Seeing it locally

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317   # or leave unset
go run ./cmd/workforce
curl -s localhost:8080/healthz
```

Each request emits a log line like:

```json
{"time":"...","level":"INFO","msg":"http request","method":"POST",
 "path":"/associates/A-100/assignments","route":"/associates/{id}/assignments",
 "status":201,"duration_ms":4,"bytes":60,"request_id":"...",
 "trace_id":"60b0a70a6ebad03fb2e4b4d05246c4ba","span_id":"3c745e4adb43be47"}
```

Paste that `trace_id` into Jaeger/Tempo to jump straight from the log line to
the trace, including its Postgres child spans.

## Local development / quality gate

Every CI sensor is also a `make` target, so the same feedback is available
locally, before you commit. `make help` lists them all.

```sh
make check        # fast pre-commit loop: fmt-check, vet, build, lint, test (-race)
make check-all    # before pushing: check + coverage gate (90%), arch-test, bdd
make vuln         # govulncheck ./... — known CVEs in deps and the Go stdlib
make mutation     # fast gremlins subset (blocks in CI); mutation-full = exhaustive
make integration  # repo/outbox tests; outbox tests boot their own Postgres via testcontainers (Docker needed)
```

Git hooks are managed with [lefthook](https://github.com/evilmartians/lefthook)
and configured in [`lefthook.yml`](lefthook.yml) — `pre-commit` runs
`make fmt-check vet lint`, `pre-push` runs `make check`. Hooks live in
`.git/hooks/`, which is not tracked, so activate them once per clone:

```sh
brew install lefthook     # or: go install github.com/evilmartians/lefthook@latest
lefthook install
```

`make lint` expects `golangci-lint` on your PATH at the version CI pins:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1
go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0
go install golang.org/x/vuln/cmd/govulncheck@latest
```

## Test

```bash
go build ./...
go vet ./...
go test ./...
go test ./... -race
gofmt -l .                      # should print nothing

# Integration tests (build-tagged). Every one boots its own Postgres (and,
# where needed, Kafka) via testcontainers — Docker is required, no
# DATABASE_URL is read and nothing is skip-gated.
go test -tags=integration ./... -race -count=1
```

The four invariants named in this context's Definition of Done each have a
dedicated failing-path test, at both the domain and use-case/HTTP layers:

- `plannedHeads > installedStations` rejected on `ShiftPlan` commit —
  `shiftplan.TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations`,
  `usecases.TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations`,
  `http.TestCommitShiftPlan_RejectsPlannedHeadsExceedingInstalledStations`.
- Double-booking rejected — `assignment.TestAssign_SecondAssignmentEndsPriorAndRaisesReassigned`
  proves the invariant holds by construction (a second active assignment can
  never coexist with the first).
- Assignment without required certification rejected —
  `assignment.TestAssign_RejectsMissingCertification`,
  `usecases.TestAssignLabor_RejectsMissingCertification`,
  `http.TestAssignLabor_RejectsMissingCertification`.
- Assignment while on an active break rejected —
  `associate.TestCanBeAssigned_RejectsWhileOnBreak`,
  `usecases.TestAssignLabor_RejectsWhileOnBreak`,
  `http.TestAssignLabor_RejectsWhileOnBreak`.

## BDD / Acceptance tests

Executable specifications live in [`features/`](features/) as Gherkin
`.feature` files and are run with [godog](https://github.com/cucumber/godog),
the official Cucumber implementation for Go. They drive the **real REST API**
end-to-end: the chi router is wired to the in-memory test doubles (memory repos, a
buffered event publisher, a fixed clock; the shipped binaries never import them,
ADR-0038), served over an `httptest.Server`, and
exercised with real `net/http` calls — nothing reaches past the HTTP boundary,
so the scenarios document the published contract rather than internal wiring.
Each scenario gets a fresh server and fresh repositories, so they are
independent and order-free.

```bash
go test ./... -run TestFeatures -v
```

| Feature file | Covers |
| --- | --- |
| `features/shift_plan.feature` | `CommitShiftPlan` — within capacity, rejected when `plannedHeads` exceed installed stations, live capacity resolved by the path's required capability (`PICK` → `pick`), unknown path rejected |
| `features/shift_plan_hours_capacity.feature` | `CommitShiftPlan` — `plannedHours` bounded by `plannedHeads × MAX_HOURS_PER_SHIFT` |
| `features/path_plan_proposal.feature` | `ProposePathPlan` — pure computation, nothing committed |
| `features/labor_assignment.feature` | `AssignLabor` — certified assignment, uncertified rejection, no double-booking, and rejection while on break |
| `features/breaks.feature` | `StartBreak` / `EndBreak` — break state gates assignment, then releases it |
| `features/break_state_transitions.feature` | `StartBreak` / `EndBreak` — discrete transitions (`associate-already-on-break`, `associate-not-on-break`), not upserts |
| `features/associate_shift_lifecycle.feature` | `EndAssociateShift` — closes the active assignment, ending an ended shift is a no-op, an ended shift cannot be assigned |
| `features/staffing_gap.feature` | `GetStaffingGap` — a path below plan is flagged `PathUnderstaffed` |
| `features/staffing_gap_read_model.feature` | `GetStaffingGap` — a fully staffed path is not flagged; no committed `ShiftPlan` is rejected |

Step definitions and the suite entry point (`TestFeatures`) are in
[`features/features_test.go`](features/features_test.go) (and
`features_wave2_test.go`) next to the `.feature` files: the repository root
holds no Go file (`TestNoGoFilesAtRepoRoot`). CI runs them as a
dedicated `bdd` job.

## Helm chart

`charts/workforce-management` deploys the OLTP service with optional components behind
values flags: the analytics projector and reports reader (`analytics.enabled`), the read-only MCP server (`mcp.enabled`) and the operator micro-frontend (`frontend.enabled`). Mandatory values are the Postgres secret and the Kafka
broker list; the deployed consumer-group ids and mode flags come from
`warehouse-infra`'s helm-values.

The chart is linted in CI (`helm-lint`, `chart-tests`) and its invariants are
asserted by the tests in `charts/workforce-management/tests/` — `render_test.sh`,
`test_service_selectors.py` (every Service selects exactly one Deployment) and
`test_kong_config.py`.
## Operator micro-frontend (`web/`)

`web/` is `workforce_mfe`, this context's Module Federation remote. It talks only to
this service's own REST API and is never part of `make check`.

**Standalone development** is unchanged:

```bash
cd web && npm install && npm run dev     # http://localhost:5185
```

**Deployed to the kind cluster**, it is built into a static bundle and served
by its own `nginx-unprivileged` pod:

```bash
cd web
docker build --build-context uikit=../../warehouse-ui-kit \
  -t warehouse/workforce-management-frontend:local .
```

The cluster's localhost topology separates the two kinds of traffic onto two
independent entrypoints, and neither proxies to the other:

| URL | Served by | Carries |
|---|---|---|
| `http://localhost/mfes/workforce-management/` | Nginx web gateway → this remote's nginx pod | HTML, JS, CSS, fonts, `remoteEntry.js` |
| `http://localhost:8000/api/workforce-management/` | Kong | this service's REST API |

Kong never serves frontend assets, and the Nginx gateway never proxies an API.
Enable the workload with `frontend.enabled=true` in the Helm chart; the Service
is deliberately `ClusterIP` with no Ingress/HTTPRoute, because frontend path
routing belongs to the Nginx web gateway in `warehouse-infra`.

Because one image must work in more than one environment, the remote reads its
API origin at runtime from `window.__WAREHOUSE_CONFIG__.apiOrigin` (published
by the console shell) rather than baking a hostname in at build time. A
production build with no runtime config **fails loudly** instead of silently
falling back to a developer port; standalone `npm run dev` still uses
`http://localhost:8085`. See `web/src/config.ts`.

Chart invariants are asserted by:

```bash
python3 charts/workforce-management/tests/test_service_selectors.py
```

which proves every Service selects exactly one Deployment — the OLTP Service
must never select the frontend, analytics or MCP pods.

## Architecture Decision Records

The ADR index lives at [docs/docs/adr/index.md](docs/docs/adr/index.md) — 38 decisions, from the
hexagonal layout to the CloudEvents-only envelope and the transactional
outbox.

## License

MIT — see [LICENSE](LICENSE).
