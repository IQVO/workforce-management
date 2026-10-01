# Workforce Management

> **⚠️ Study project.** This repository is an educational exercise in
> Domain-Driven Design applied to warehouse management/execution systems. It
> follows real industry-standard patterns and terminology (WMS/WES/WCS,
> certification-gated assignment, CloudEvents, RFC 7807, hexagonal
> architecture) but is **not a production system** and is **not affiliated
> with, endorsed by, or representative of any real-world company**.

A Supporting bounded context that owns "who is on shift, on which process
path, at what rate; direct vs indirect hours." It covers the shift-start
planning horizon (a human commits a split of headcount across paths) and
intra-shift assignment tracking (moving people between paths as backlogs
deviate — the move itself stays a human call; this context makes the gap
legible, it does not decide).

## Documentation

Full documentation is published at
**<https://claudioed.github.io/workforce-management/>** — the business context
and the path-boundary reasoning, the DDD model (aggregates, invariants, all ten
domain events), an interactive REST API reference generated from
`apis/openapi.yaml`, the CloudEvents catalog from `apis/asyncapi.yaml`, the
ecosystem context map, and the Architecture Decision Records. Source lives in
[`docs/`](docs/) and deploys on every push to `main`.

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
    outbound/postgres/          pgxpool repos + golang-migrate migrations
    outbound/analyticsstore/    analytical projection (writer) + report (read-only reader) + memory store
    outbound/memory/            in-memory repos for tests/local
    outbound/events/            log/buffered publisher + multi (fan-out) publisher
    outbound/clock/             system clock
    outbound/kafka/             integration publisher + analytics publisher + outbox relay sink + trace-context header carrier
    outbound/fulfillmentexecution/  InstalledCapacityClient: GET /capacity/{capability} (ADR-0014)
    outbound/laborperformance/      MeasuredRateClient over HTTP (ADR-0012)
    outbound/laborperformancecache/ event-fed measured-rate + idle-share cache (ADR-0019, ADR-0020)
    outbound/filecatalog/           process-path catalogue from a YAML file (ADR-0013)
    outbound/kafkacatalog/          process-path catalogue fed by process-path-management's topic
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
- **`GetStaffingGap` takes `buildingId`/`shiftId` as query parameters**
  (`GET /paths/{pathId}/staffing-gap?buildingId=&shiftId=`) because
  `ShiftPlan` is keyed by building + shift, and a path's planned heads only
  make sense within one committed plan.
- **`PathPlan.plannedHours <= plannedHeads * maxHoursPerShift`** is how
  "sum of hours valid" is enforced on `ShiftPlan`: a path line can't commit
  more total hours than its planned heads could work within one shift's max.

## Run it

```bash
docker compose up -d                 # Postgres 16 on localhost:5432
export DATABASE_URL="postgres://workforce:workforce@localhost:5432/workforce?sslmode=disable"
export PATH_CATALOGUE_FILE=./process-paths.yaml   # required: see PATH_CATALOGUE_FILE below
go run ./cmd/workforce                # applies migrations, then serves on :8080
```

`cmd/workforce` refuses to start without a readable process-path catalogue
(copy `warehouse-infra`'s `config/process-paths/sortable-fc.yaml`, or see the
minimal example in the
[quickstart](docs/docs/overview/quickstart.md)).

Env vars:

| Var | Required | Default | Meaning |
|-----|----------|---------|---------|
| `DATABASE_URL` | yes | — | Postgres connection string |
| `HTTP_ADDR` | no | `:8080` | listen address |
| `MIGRATIONS_PATH` | no | `migrations` | path to golang-migrate SQL files |
| `MAX_HOURS_PER_SHIFT` | no | `8` | the configured max-hours-per-shift cap |
| `EVENT_PUBLISHER` | no | `log` | `log` or `kafka` — see [Integration](#integration). With `kafka`, events are written to the `outbox_events` table in the same transaction as the aggregate and relayed to both topics by an in-process relay ([ADR-0016](docs/docs/adr/0016-transactional-outbox.md)); they never go straight to the broker from a request |
| `KAFKA_BROKERS` | no | `localhost:9092` | comma-separated broker list, used when `EVENT_PUBLISHER=kafka` |
| `OUTBOX_RELAY_INTERVAL` | no | `1s` | how long the outbox relay sleeps between passes that found nothing to publish (Go duration; only meaningful with `EVENT_PUBLISHER=kafka`). A full batch is followed immediately by another pass |
| `LABOR_PERFORMANCE_MODE` | no | `permissive` | `http`, `kafka-cache`, or `permissive` — selects the `ProposePathPlan` measured-rate feed from labor-performance. `http` calls labor-performance synchronously (ADR-0012); `kafka-cache` replaces that call with a local, in-memory read model fed by labor-performance's `warehouse.labor-performance.events` integration topic (ADR-0019), requiring `KAFKA_BROKERS`, and additionally feeds the idle-share staffing signal (see `IDLE_SHARE_TRIM_THRESHOLD` below and ADR-0020); `permissive` never reaches the network |
| `LABOR_PERFORMANCE_BASE_URL` | when `LABOR_PERFORMANCE_MODE=http` | — | labor-performance's base URL |
| `IDLE_SHARE_TRIM_THRESHOLD` | no | `0.30` | Idle share (fraction, e.g. `0.3` = 30%) above which `ProposePathPlan` trims its proposed heads (idleness-as-staffing-signal, ADR-0020). Only takes effect when `LABOR_PERFORMANCE_MODE=kafka-cache` is also set — no other mode has an idle-share signal to trim against, so `ProposePathPlan` fails open (no trim) regardless of this value otherwise |
| `INSTALLED_CAPACITY_MODE` | no | `permissive` | `http` or `permissive` — selects the `CommitShiftPlan` live installed-capacity ceiling client from fulfillment-execution (ADR-0014). Unlike `LABOR_PERFORMANCE_MODE`'s fail-open `permissive` default, this one is fail-LOUD: every `CommitShiftPlan` call is rejected until `http` mode is set, since a shift-plan commit mutates real state |
| `FULFILLMENT_EXECUTION_BASE_URL` | when `INSTALLED_CAPACITY_MODE=http` | — | fulfillment-execution's base URL |
| `PATH_CATALOGUE_SOURCE` | no | `file` | `file` loads `PATH_CATALOGUE_FILE` once at boot; `kafka` instead replays `process-path-management`'s `warehouse.process-path-management.events` topic into an in-memory catalogue (requires `KAFKA_BROKERS`; the service waits up to 60s for the replay before serving) |
| `PATH_CATALOGUE_FILE` | no | `/etc/workforce-management/process-paths.yaml` | Path to the declared process-path catalogue YAML (see `warehouse-infra`'s `config/process-paths/sortable-fc.yaml`, the same file `fulfillment-execution` and `wes-work-planning` read). Loaded once at startup; a missing or invalid file is a fatal boot-time error — see [ADR-0013](docs/docs/adr/0013-process-path-catalogue-validation.md) |
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
(`warehouse.workforce.analytics`) when `EVENT_PUBLISHER=kafka`; the integration
topic and publisher are left untouched.

```bash
# 1. the fleet's shared broker reachable at localhost:9092 (the warehouse-infra
#    kind cluster exposes it there) and an analytical Postgres database
#    reachable via ANALYTICS_DATABASE_URL.

# 2. OLTP service, fanning events onto the analytics topic:
export EVENT_PUBLISHER=kafka
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

## API

All bodies are JSON. `{id}` and `{pathId}` are path parameters.

```bash
# Start an associate's shift with initial certifications
curl -X POST localhost:8080/associates/assoc-1/start-shift \
  -d '{"certifications":["pack"]}'

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
curl -X POST localhost:8080/paths/pack/plan/propose \
  -d '{"buildingId":"bldg-1","charge":100,"plannedRate":30}'
# -> {"pathId":"pack","proposedHeads":4,"resolvedRate":30,"rateSource":"caller"}

curl -X POST localhost:8080/paths/pack/plan/propose \
  -d '{"buildingId":"bldg-1","charge":100}'
# -> falls back to labor-performance's measured rate for PACK when
#    LABOR_PERFORMANCE_MODE=http and data exists; otherwise 0 proposed heads.

# Commit a shift plan (human-committed headcount split across paths).
# Requires INSTALLED_CAPACITY_MODE=http: in the default permissive mode every
# commit is rejected with 503 installed-capacity-unavailable (ADR-0014), and a
# line whose plannedHeads exceeds fulfillment-execution's live capacity is 409.
curl -X POST localhost:8080/shift-plans \
  -d '{"buildingId":"bldg-1","shiftId":"shift-1","lines":[
        {"pathId":"pack","plannedHeads":3,"plannedRate":30,"plannedHours":24,"installedStations":10}
      ]}'

# Assign an associate to a path (ends any prior active assignment)
curl -X POST localhost:8080/associates/assoc-1/assignments \
  -d '{"pathId":"pack"}'

# Break start/end
curl -X POST localhost:8080/associates/assoc-1/break/start
curl -X POST localhost:8080/associates/assoc-1/break/end

# Staffing gap read model for a path within a committed plan
# -> {"pathId":"pack","plannedHeads":3,"activeHeads":1,"understaffed":true}
#    plus observedIdlePct when LABOR_PERFORMANCE_MODE=kafka-cache has a signal
#    for the path (ADR-0020). Unknown path ids are 400 unknown-path-id.
curl "localhost:8080/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1"

# End an associate's shift (closes any active assignment first)
curl -X POST localhost:8080/associates/assoc-1/end-shift

curl localhost:8080/healthz
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
Kafka broker so other bounded contexts (e.g. `wes-work-planning`, which
projects these into its own `LaborPlanObserved` read model, keyed by
`path_id`) can react to committed headcount without calling back into this
service. Inbound, it can optionally consume `process-path-management`'s
catalogue topic (`PATH_CATALOGUE_SOURCE=kafka`) and `labor-performance`'s
`warehouse.labor-performance.events` (`LABOR_PERFORMANCE_MODE=kafka-cache`),
each into an in-memory cache rebuilt from the earliest offset under a
per-process consumer group. It also makes two synchronous calls:
fulfillment-execution's `GET /capacity/{capability}` (`INSTALLED_CAPACITY_MODE=http`)
and labor-performance's `GET /task-types/{taskType}/performance`
(`LABOR_PERFORMANCE_MODE=http`). See
[docs/docs/ecosystem/integration.md](docs/docs/ecosystem/integration.md) for
the full edge list.

- **Topic**: `warehouse.workforce.events`
- **Broker**: `KAFKA_BROKERS` (default `localhost:9092`) — the fleet's one
  shared broker (in the `warehouse-infra` kind cluster, exposed on the host
  at `localhost:9092`); this repo's own `docker-compose.yml` only runs
  Postgres.
- **Selection**: `EVENT_PUBLISHER=kafka` to publish to Kafka, `EVENT_PUBLISHER=log`
  (default) to keep publishing to the in-memory/log publisher used by tests
  and local runs that don't need cross-service integration.
- **Delivery** ([ADR-0016](docs/docs/adr/0016-transactional-outbox.md)): with
  `EVENT_PUBLISHER=kafka` the use case inserts the already-encoded message(s)
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
  "subject": "<building_id>/<shift_id>",
  "datacontenttype": "application/json",
  "dataschema": "urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1",
  "time": "2026-08-21T22:00:00Z",
  "data": {
    "building_id": "...",
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
export EVENT_PUBLISHER=kafka
export KAFKA_BROKERS=localhost:9092
go run ./cmd/workforce

curl -X POST localhost:8080/shift-plans \
  -d '{"buildingId":"bldg-1","shiftId":"shift-1","lines":[
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

# Postgres integration tests (build-tagged). The repo tests need DATABASE_URL;
# the outbox tests (-run Outbox) start their own Postgres via testcontainers.
docker compose up -d
export DATABASE_URL="postgres://workforce:workforce@localhost:5432/workforce?sslmode=disable"
go test -tags=integration ./internal/adapters/outbound/postgres/...
go test -tags=integration ./internal/adapters/outbound/postgres/ -run Outbox -race -count=1
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
end-to-end: the chi router is wired to the in-memory adapters (memory repos, a
buffered event publisher, a fixed clock), served over an `httptest.Server`, and
exercised with real `net/http` calls — nothing reaches past the HTTP boundary,
so the scenarios document the published contract rather than internal wiring.
Each scenario gets a fresh server and fresh repositories, so they are
independent and order-free.

```bash
go test ./... -run TestFeatures -v
```

| Feature file | Covers |
| --- | --- |
| `features/shift_plan.feature` | `CommitShiftPlan` — within capacity, and rejected when `plannedHeads` exceed installed stations |
| `features/labor_assignment.feature` | `AssignLabor` — certified assignment, uncertified rejection, no double-booking, and rejection while on break |
| `features/breaks.feature` | `StartBreak` / `EndBreak` — break state gates assignment, then releases it |
| `features/staffing_gap.feature` | `GetStaffingGap` — a path below plan is flagged `PathUnderstaffed` |

Step definitions and the suite entry point (`TestFeatures`) are in
[`features_test.go`](features_test.go) at the repo root. CI runs them as a
dedicated `bdd` job.

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
