# Integrations — outbound clients, process-path catalogue, events, CORS

## Outbound HTTP clients (`internal/application/ports/ports.go`)

- **`MeasuredRateClient`** (ADR-0012) — queries `labor-performance` for a
  real, measured mean task duration per path, so `ProposePathPlan` can
  propose headcount against reality instead of always requiring a
  caller-supplied `plannedRate`. **Fail-open**: any failure (unreachable
  service, malformed response, genuinely no data yet) collapses to the
  single sentinel `ErrMeasuredRateUnavailable`, and the use case falls back
  to the caller-supplied rate. Selected via `LABOR_PERFORMANCE_MODE`
  (`http`|`kafka-cache`|`permissive`, default `permissive` — never reaches
  the network) and `LABOR_PERFORMANCE_BASE_URL` (http mode).
  `kafka-cache` (ADR-0019) swaps the HTTP call for
  `outbound/laborperformancecache`: an in-memory cache fed by
  `warehouse.labor-performance.events` (`TaskPerformanceRecorded`), replayed
  from the earliest offset under a per-process-unique consumer group, with a
  60s `WaitReady` gate at boot. Requires `KAFKA_BROKERS`.
- **`IdleShareClient`** (ADR-0020) — the observed idle share per path's task
  type. **Only `kafka-cache` provides one**; `http`/`permissive` leave it nil.
  `GetStaffingGap` surfaces it as `observedIdlePct`; `ProposePathPlan` trims
  the proposal when it exceeds `IDLE_SHARE_TRIM_THRESHOLD` (default 0.30).
  Fail-open via `ErrIdleShareUnavailable` (surface nothing / trim nothing).
- **`InstalledCapacityClient`** (ADR-0014 + its addendum) — queries
  `fulfillment-execution` (`GET /capacity/{capability}`) for the real, live
  count of registered stations holding a **capability** (`shared.Capability`,
  never a `PathId`), so `CommitShiftPlan` enforces `plannedHeads` against
  physical reality, not just a caller-supplied `installedStations` count.
  `CommitShiftPlan` resolves each line's path through `PathCatalogue.Lookup`
  to its `requiredCapabilities` and uses the MIN station count across them
  (a station must hold all of them to serve the path). fulfillment-execution
  matches the capability string exactly and stations register lower-case
  (`pick`), so sending the canonical path id (`PICK`) would count 0 — never
  cast a `PathId` to a `Capability`. An undeclared path is `400
  unknown-path-id`; a path declaring no capabilities gets a 0 ceiling
  (fail-closed). `CommitShiftPlan.Catalogue` is required. **Fail-LOUD**
  (unlike the measured-rate client): any failure collapses to
  `ErrInstalledCapacityUnavailable` and the ENTIRE commit is rejected (503,
  `installed-capacity-unavailable`) with no fallback, since a commit mutates
  real state. A real `0` capacity is a valid answer, not an error — any
  `plannedHeads > 0` for that path is then rejected (409,
  `exceeds-installed-capacity`). Selected via `INSTALLED_CAPACITY_MODE`
  (`http`|`permissive`, default `permissive` — but permissive here means
  fail-loud-always, the opposite sense of the measured-rate default) and
  `FULFILLMENT_EXECUTION_BASE_URL`.

Both interfaces deliberately expose only one error sentinel each: the use
case has exactly one handling branch per client and never needs to
distinguish failure subtypes.

## Process-path catalogue (ADR-0013)

`internal/domain/pathcatalog` models the fleet's declared process-path
catalogue: which path families are declared and what capabilities each
requires. Lookup is a **case-insensitive prefix match**, not exact — real
`path_id` values carry a station/zone/scenario suffix (e.g. `pick-zone-a`
matches the declared prefix `pick`). This package mirrors
`fulfillment-execution`'s and `wes-work-planning`'s own pathcatalog packages
byte-for-byte in matching semantics, since all three read the same
published-language file (`warehouse-infra/config/process-paths/*.yaml`).

- `outbound/filecatalog` — loads the YAML from `PATH_CATALOGUE_FILE`
  (default `/etc/workforce-management/process-paths.yaml`) once at startup.
  A missing/invalid file is a **fatal boot-time error**.
- `outbound/kafkacatalog` — an alternative, Kafka-sourced catalogue adapter
  implementing the same `ports.PathCatalogue` interface, selected with
  `PATH_CATALOGUE_SOURCE=kafka` (default `file`). It replays
  `process-path-management`'s `warehouse.process-path-management.events`
  (`ProcessPathCreated`/`Updated`/`Deactivated`) from the earliest offset under
  a per-process consumer group, and blocks boot up to 60s (`WaitReady`).
  Requires `KAFKA_BROKERS`.

## Kafka integration events + transactional outbox (ADR-0004, ADR-0016)

This service publishes `ShiftPlanCommitted` to the shared warehouse-systems
Kafka broker; `wes-work-planning` projects it into its own
`LaborPlanObserved` read model, keyed by `path_id`. On the consuming side,
the only inbound topics are the two opt-in cache feeds above
(`kafkacatalog`, `laborperformancecache`); neither writes to Postgres.

- **Topic**: `warehouse.workforce.events`. **Broker**: `KAFKA_BROKERS`
  (default `localhost:9092`, the fleet's one shared broker — in-cluster in
  the `warehouse-infra` kind cluster; this repo's own `docker-compose.yml`
  only runs Postgres).
- **Selection**: `EVENT_PUBLISHER=kafka` to publish; default `log` keeps
  tests/local runs broker-free.
- **Delivery** (ADR-0016, transactional outbox): with `EVENT_PUBLISHER=kafka`
  the use case inserts the already-encoded message(s) for BOTH the
  integration topic and the analytics topic into the `outbox_events` table
  inside the SAME Postgres transaction as the aggregate. A relay goroutine in
  `cmd/workforce` drains that table onto Kafka every `OUTBOX_RELAY_INTERVAL`
  (default `1s`; a full batch is followed immediately by another pass). The
  store and the topics therefore cannot diverge — delivery is at-least-once,
  per-key ordered.
- **Fan-out**: a `ShiftPlan` has multiple `PathPlan` lines — `CommitShiftPlan`
  with 3 lines publishes 3 Kafka messages, one per line, each carrying that
  line's `planned_heads`/`planned_rate`/`planned_hours` plus the plan's
  `building_id`/`shift_id`. Consumers must expect N messages per commit.
- **Envelope**: CloudEvents 1.0, MANDATORY, structured content mode, for
  every message produced or consumed (ADR-0026). No flat envelope, no
  dual-read/write, no envelope toggle. Build/decode only through
  `internal/adapters/kafka/cloudevents` (`New`/`Decode`/`ContentTypeHeader`,
  on `github.com/cloudevents/sdk-go/v2/event`); every produced message carries
  `content-type: application/cloudevents+json; charset=UTF-8` next to the
  trace headers:

```json
{
  "specversion": "1.0",
  "id": "uuid-v4",
  "source": "/warehouse/workforce-management",
  "type": "com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted",
  "subject": "bldg-1/shift-1",
  "datacontenttype": "application/json",
  "dataschema": "urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1",
  "time": "2026-08-21T22:00:00Z",
  "data": {
    "building_id": "bldg-1",
    "shift_id": "shift-1",
    "path_id": "pack",
    "planned_heads": 3,
    "planned_rate": 30,
    "planned_hours": 24
  }
}
```

`id` is a UUID v4 minted once per message at Encode time (each fanned-out
line gets its own) and persisted with the outbox row, so a relay retry
republishes the same id. `subject` is the aggregate id — for
`ShiftPlanCommitted` the ShiftPlan id `<buildingId>/<shiftId>`, identical to
the Kafka key. `time` is the domain occurred-at, UTC. `dataschema` is
`...:events:...` on the integration topic and `...:analytics:...` on the
analytics topic. Consumers dispatch on the FULL `type`, ignore unknown types,
dedupe on `id`, and DLQ (analytics projector) or WARN-and-skip (catalogue /
labor-performance caches) any message that fails CloudEvents validation.

The **full CloudEvents domain-event catalog** (all ten events, reverse-DNS
`type` naming `com.warehouse.wes.workforce-management.<entity>.<EventName>`)
lives in `apis/asyncapi.yaml`. Only `ShiftPlanCommitted` is on the integration
topic; all ten are on the analytics topic. Keep `apis/asyncapi.yaml` and
`docs/docs/ecosystem/integration.md` (the narrative counterpart) in sync when
the publication set changes.

## CORS

`go-chi/cors` middleware is enabled on every route, allowing
`CORS_ALLOWED_ORIGINS` (env, default
`http://localhost:5173,http://localhost:5185` — the `warehouse-console`
shell and this service's own `workforce-mfe` remote). This service is not
part of the fleet's cross-service Order Lifecycle read model — CORS exists
solely for `workforce-mfe` (see rules/frontend.md).
