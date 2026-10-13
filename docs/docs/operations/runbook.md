---
id: runbook
title: Runbook
sidebar_label: Runbook
description: Deploying and operating the four workforce-management binaries. Covers the Helm chart, probes, migrations, Kafka topics and consumer groups, the outbox relay, the DLQ, housekeeping, scaling and routine procedures.
---

# Runbook

Everything below comes from the Helm chart `charts/workforce-management`, the
composition roots under `cmd/` and the adapters they wire. For every
environment variable see [Configuration](./configuration.md).

## Deployables

All four binaries ship in one image (`image.repository`, default
`workforce-management`). Each Deployment selects its binary with `command`.

| Deployment (template) | Binary | Enabled by | Container port | Service | Probes |
|---|---|---|---|---|---|
| `<fullname>` (`deployment.yaml`), component `api` | default entrypoint, `cmd/workforce` | always | `http` = `service.targetPort` `8080` | `service.yaml`: ClusterIP `80` → `8080` | startup `/healthz` (2 s × 30), liveness `/healthz`, **readiness `/readyz`** |
| `<fullname>-mcp` (`mcp-deployment.yaml`) | `/app/mcp` | `mcp.enabled` (default `false`) | `8090` (`MCP_ADDR`) | `mcp-service.yaml`: ClusterIP `8090` → `8090` | startup, liveness, readiness all on `/healthz` |
| projector (`projector-deployment.yaml`), component `analytics-projector` | `/app/workforce-projector` | `analytics.enabled` (default `false`) | `admin` `8091` (`ADMIN_ADDR`) | none | startup, liveness, readiness all on `/healthz` on `admin` |
| reports (`reports-deployment.yaml`) | `/app/workforce-reports` | `analytics.enabled` | `8092` (`HTTP_ADDR`) | ClusterIP `80` → `8092`, in the same template | startup, liveness, readiness all on `/healthz` |
| frontend (`frontend-deployment.yaml`) | nginx-unprivileged serving `web/` | `frontend.enabled` (default `false`) | `8080` | `frontend-service.yaml`: ClusterIP `80` → `8080` | liveness, readiness `/healthz` |

Edge routing is owned by `warehouse-infra`. APIs are reached through Kong at
`:8000` under `/api/workforce-management`. The chart's own `ingress` and
`gatewayApi` HTTPRoute blocks are both disabled by default. The console remote
is served by the Nginx web gateway at `/mfes/workforce-management/`, never
through Kong (see the `frontend:` comment in `values.yaml`). REST and MCP are
unauthenticated.

The chart refuses to render without `database.url` or
`database.existingSecret` (`workforce-management.requireDatabase`), because
`cmd/workforce` exits at once without `DATABASE_URL`.

### What the chart templates, and what needs `extraEnv`

`templates/configmap.yaml` renders `HTTP_ADDR`, `EVENT_PUBLISHER`,
`MIGRATIONS_PATH`, `MAX_HOURS_PER_SHIFT`, `OUTBOX_RELAY_INTERVAL`, the
housekeeping variables, `SHUTDOWN_DRAIN_DELAY` and (with `kafka.enabled`)
`KAFKA_BROKERS` from `config.*`, and the api Deployment mounts it with
`envFrom` (gap fix D14, 2026-10-12; the full key table is in
[Configuration](./configuration.md#how-the-helm-chart-delivers-cmdworkforces-variables)).
Changing a `config.*` value takes effect on the next rollout: the
`checksum/config` annotation rolls the pods. `deployment.yaml` also sets
`HTTP_ADDR`, `EVENT_PUBLISHER`, `MIGRATIONS_PATH`, `MAX_HOURS_PER_SHIFT`, the
housekeeping variables, `SHUTDOWN_DRAIN_DELAY`, `DATABASE_URL`,
`MIGRATIONS_DATABASE_URL` (secret key, `optional: true`), `LOG_LEVEL`,
`KAFKA_BROKERS` (only when `kafka.enabled`), the OTel variables (when
`otel.enabled`) and `PATH_CATALOGUE_FILE` (when `pathCatalogue.enabled`) as
explicit `env`, which wins over the ConfigMap for the same key. The projector,
reports and MCP Deployments do not mount the ConfigMap.
`charts/workforce-management/tests/render_test.sh` (CI job `chart-tests`)
fails if the mount disappears. These must come through `extraEnv`:

- `INSTALLED_CAPACITY_MODE=http` and `FULFILLMENT_EXECUTION_BASE_URL`. Without them **every `CommitShiftPlan` fails** with `503 installed-capacity-unavailable`.
- `LABOR_PERFORMANCE_MODE` and `LABOR_PERFORMANCE_BASE_URL`, when you want a measured-rate feed.
- `PATH_CATALOGUE_SOURCE=kafka`, to replay the catalogue from Kafka instead of the mounted file.
- `IDLE_SHARE_TRIM_THRESHOLD` and `CORS_ALLOWED_ORIGINS`.

`extraEnv` is also rendered into the MCP Deployment.

## Startup and readiness

`cmd/workforce` boots in this order (`cmd/workforce/main.go` `run`):

1. Telemetry (non-blocking; a missing collector only drops data).
2. The process-path catalogue. With `file`, the YAML is loaded and validated. With `kafka`, the compacted `warehouse.process-path-management.events` topic is replayed up to its current high-water mark, waiting at most 60 s. Failure stops the boot.
3. Migrations on `MIGRATIONS_DATABASE_URL`, then the pgxpool on `DATABASE_URL`. Each step runs under `bootretry`: 5 attempts, 15 s of total backoff.
4. With `LABOR_PERFORMANCE_MODE=kafka-cache`: replay of `warehouse.labor-performance.events`, waiting at most 60 s.
5. The housekeeping sweeper starts, then the HTTP listener and, in kafka mode, the outbox relay.

**`/readyz` does not wait for any dependency.** It returns `200 {"status":"ready"}` as soon as the
listener is up, and the listener only starts after steps 2 to 4 have
succeeded. `/readyz` turns to `503 {"status":"not_ready"}` at exactly one
point: the first step of graceful shutdown. `/healthz` is pure liveness and is
never flipped. The other three binaries have only `/healthz`.

The startup probe allows 30 × 2 s = 60 s before liveness takes over. A cold
start that needs the full boot-retry budget twice (migrations, then the pool)
plus a 60 s Kafka replay can exceed that. See
[Troubleshooting](./troubleshooting.md).

### Graceful shutdown (`cmd/workforce/serve.go`)

On SIGTERM: `/readyz` → 503, then a wait of `SHUTDOWN_DRAIN_DELAY` (10 s),
then `http.Server.Shutdown`, then the Kafka consumers stop, then the sweeper,
then the outbox relay finishes its in-flight pass. Everything after the drain
wait shares one 10 s deadline. The Postgres pool closes last. The chart's
`terminationGracePeriodSeconds: 30` covers 10 s drain + 10 s shutdown + 5 s
telemetry flush. `cmd/mcp`, the projector and reports shut down with a plain
10 s `Shutdown`.

The projector also stops on its own when its consumer loop exits without a
signal (a commit or DLQ write failure): it shuts the admin server down and
exits non-zero, so the pod restarts instead of serving a green `/healthz`
over a dead consumer (gap fix D13, `superviseConsumer` in
`cmd/workforce-projector/main.go`).

## Migrations

| Database | Files | Applied by | DSN |
|---|---|---|---|
| OLTP | `migrations/000001_init` … `000005_associate_shift_site_code` | `cmd/workforce` **and** `cmd/mcp` (when it has a `DATABASE_URL`), on every start | `MIGRATIONS_DATABASE_URL`, falling back to `DATABASE_URL` |
| Analytical | `migrations/analytics/0001_report` | `cmd/workforce-projector`, on every start | `ANALYTICS_DATABASE_URL` |

The OLTP migrations create `associate_shift`, `shift_plan`, `path_plan`,
`labor_assignment`, `labor_assignment_history` and `domain_event` (000001),
`outbox_events` (000002), the `version` columns (000003), `idempotency_keys`
(000004) and the associate shift's site code (000005). The analytical
migration creates `analytics_processed_events`, `analytics_consumed_events`,
`analytics_pending_breaks` and `labor_rollup`.

Migrations are golang-migrate. They take a session-scoped advisory lock, so
in a PgBouncer environment `MIGRATIONS_DATABASE_URL` **must** be the direct,
non-pooled DSN ([ADR 0025](../adr/0025-migrations-direct-postgres-connection.md)).
There is no separate migration Job. Concurrent replicas serialise on the lock,
and `cmd/workforce-reports` never migrates. To roll a migration back, run the
matching `.down.sql` by hand with the golang-migrate CLI against the direct
DSN. The binaries only ever migrate up.

## Kafka

One broker cluster for the whole platform (in-cluster Bitnami; external
access at `localhost:9092` from the kind cluster). Every message is a
CloudEvents 1.0 **structured-mode** event (`internal/adapters/kafka/cloudevents`).
There is no binary-mode toggle.

| Topic | Direction | Who | Consumer group | Notes |
|---|---|---|---|---|
| `warehouse.workforce.events` | produce | outbox relay in `cmd/workforce`. `cmd/mcp` produces directly only when it runs without a database. | n/a | Integration topic. Event types are in [Events](../api-reference/events.md). |
| `warehouse.workforce.analytics` | produce | same as above | n/a | Analytics fan-out of every domain event ([ADR 0010](../adr/0010-analytical-data-product.md)). |
| `warehouse.workforce.analytics` | consume | `cmd/workforce-projector` | `workforce-analytics` (stable, shared) | Idempotent on the CloudEvents `id` (`analytics_consumed_events`). |
| `warehouse.workforce.analytics.dlq` | produce | `cmd/workforce-projector` | n/a | Poison messages, see below. Auto-created on first write. |
| `warehouse.process-path-management.events` | consume | `cmd/workforce`, `cmd/mcp` when `PATH_CATALOGUE_SOURCE=kafka` | `workforce-management-process-path-catalogue-<host>-<pid>-<ns>` (unique per process) | Full replay from the first offset on every start. No DLQ: an invalid CloudEvent is logged at WARN and skipped. |
| `warehouse.labor-performance.events` | consume | `cmd/workforce` when `LABOR_PERFORMANCE_MODE=kafka-cache` | `workforce-management-labor-performance-cache-<host>-<pid>-<ns>` (unique per process) | Same replay and skip behaviour. Feeds the measured rate and the idle-share signal. |

### Outbox relay

With `EVENT_PUBLISHER=kafka` and Postgres, every use case writes its events
into `outbox_events` in the same transaction as its state change (two rows
per event, one per topic) ([ADR 0016](../adr/0016-transactional-outbox.md)).
Only `cmd/workforce` runs the relay (`internal/adapters/outbound/postgres/outbox_relay.go`):

- Each pass claims up to 100 unpublished rows (`FOR UPDATE SKIP LOCKED`, so overlapping pods during a rollout never double-send) and sends them one at a time in `id` order.
- After a full batch the next pass starts at once. After a partial or empty one the relay sleeps `OUTBOX_RELAY_INTERVAL` (1 s).
- On the first send failure the pass stops, the row gets `attempts`/`last_error`, rows already sent stay published, and the next pass retries the failed row. Ordering per key is preserved, so one stuck row holds back everything behind it.
- Lag is visible as `workforce.outbox.lag_seconds` (see [Observability](./observability.md)).

### Analytics DLQ

The projector retries each stage of a message (dedupe mark, projection apply)
up to 3 attempts, with jittered backoff from 100 ms to 2 s. A message that
does not decode as a CloudEvent goes straight to the DLQ. After the retries
are exhausted, the raw payload is written to `<topic>.dlq` with
`x-dlq-source-topic` and error-context headers, and the offset is
**committed anyway**, so one poison message never blocks the partition. Only
a DLQ write failure or a commit failure stops the consumer
(`internal/adapters/inbound/kafka/analytics_consumer.go`).

## Housekeeping

`cmd/workforce` runs one sweeper goroutine (`internal/adapters/outbound/postgres/sweeper.go`,
[ADR 0028](../adr/0028-housekeeping-sweeper-idempotency-keys-and-outbox.md)).
It runs once at boot and then every `HOUSEKEEPING_INTERVAL` (1 h). It deletes
in batches of 1000:

- `idempotency_keys` rows older than `IDEMPOTENCY_KEY_TTL` (24 h). This is also the replay window of the `Idempotency-Key` contract: a retry after that is a new request.
- `outbox_events` rows that are **published** and older than `OUTBOX_RETENTION` (7 days). Unpublished rows are never swept.

A failed pass is logged as `housekeeping sweep failed` and retried on the
next tick. Several replicas sweeping at once is safe: deletes are idempotent.

## Scaling

| Workload | HPA block (all default `enabled: false`) | Notes |
|---|---|---|
| api | `autoscaling.api`: 1 to 4, 70 % CPU | Stateless. Its Kafka caches use per-process groups, so N replicas each replay their own copy. |
| projector | `autoscaling.projector`: 1 to 2 | Shared group `workforce-analytics`. Partitions rebalance across replicas. |
| reports | `autoscaling.reports`: 1 to 3 | Read-only and stateless. |
| frontend | `autoscaling.frontend`: 1 to 3 | Static assets. |
| mcp | **none** | MCP Streamable HTTP keeps per-process session state keyed by `Mcp-Session-Id`, and the Service has no session affinity. Keep `mcp.replicaCount: 1`. |

When an HPA is enabled, the Deployment's own `replicas:` is omitted.
Connection budgets per process
([ADR 0024](../adr/0024-horizontal-autoscaling-and-pgxpool-tuning.md)): the
OLTP pool has `MaxConns` 10 and a 5 s `statement_timeout`
(`internal/adapters/outbound/postgres/pool.go`). The projector pool has 5
connections and 10 s; the reports pool has 5 connections, 15 s, and is
read-only (`internal/adapters/outbound/analyticsstore/pool.go`). Multiply by
the replica maximum when you size PgBouncer or `max_connections`.

## Routine procedures

| Task | How |
|---|---|
| Re-publish stuck outbox rows | Fix the broker or topic. The relay retries the oldest unpublished row on every pass, so nothing else is needed. Inspect the row with `SELECT id, topic, event_type, attempts, last_error FROM outbox_events WHERE published_at IS NULL ORDER BY id LIMIT 20;`. |
| Re-send already-published events | `UPDATE outbox_events SET published_at = NULL WHERE id BETWEEN … ;` while the rows are still inside `OUTBOX_RETENTION`. Consumers must dedupe on the CloudEvents `id`; the projector does. |
| Replay the analytics projection | Stop the projector, reset group `workforce-analytics` on `warehouse.workforce.analytics` (`kafka-consumer-groups.sh --reset-offsets --to-earliest --execute`), restart. Already-applied ids are skipped through `analytics_consumed_events`. To rebuild from scratch, truncate the analytical tables first. |
| Reprocess the analytics DLQ | Read `warehouse.workforce.analytics.dlq`, fix the cause, and produce the original payload back onto `warehouse.workforce.analytics`. There is no tooling in this repo for it. |
| Refresh the process-path catalogue | `file` source: update the ConfigMap (`pathCatalogue.content` or `existingConfigMap`). The pod template's `checksum/path-catalogue` annotation rolls the Deployment when `content` changes. With `existingConfigMap`, restart by hand. `kafka` source: the catalogue keeps consuming at runtime. |
| Rotate database credentials | Update the Secret (`database.existingSecret`, keys `DATABASE_URL` and `MIGRATIONS_DATABASE_URL`; the analytics Secret's `ANALYTICS_DATABASE_URL` / `ANALYTICS_READER_DATABASE_URL`), then `kubectl rollout restart` the Deployments. DSNs are read only at boot. |
| Disable housekeeping temporarily | Set `config.housekeepingInterval: "0"` and roll. |

## Analytics reports API (`cmd/workforce-reports`)

These routes are not in `apis/openapi.yaml`; they are served by the separate
reader binary (`internal/adapters/inbound/http/reports_handler.go`). The
request and response shapes are on [Labor report](../analytics/labor-report.md).

| Route | Query parameters | Response |
|---|---|---|
| `GET /healthz` | none | `200 {"status":"ok"}` |
| `GET /reports/labor` | `from`, `to` (RFC 3339, **required**), `pathId` (optional; empty means all paths), `granularity` (optional, only `hour`) | `200 {"rows":[{"pathId","hourBucket","shiftsStarted","shiftsEnded","breaks","avgBreakSeconds","certifications","laborAssigned","laborReassigned","understaffingEvents"}]}` from `labor_rollup`. A missing or malformed parameter returns 400 with problem type `invalid-report-query`. |
| `GET /reports/labor/freshness` | none | `200 {"lagSeconds": <float>}`: seconds since the newest `occurred_at` in `analytics_processed_events`. It is `0` when the read model is empty, so "0" does not always mean "fresh". |

A store failure on either report route returns 500 with problem type
`report-store-error`. A row with an empty `pathId` is the building-wide,
associate-scoped bucket (shifts, breaks, certifications).

`cmd/mcp` exposes the same report as the `get_workforce_labor_report` tool
only when `REPORTS_BASE_URL` (chart `mcp.reportsBaseUrl`, or the in-cluster
reports Service when `analytics.enabled`) is set.
