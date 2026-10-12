---
id: configuration
title: Configuration reference
sidebar_label: Configuration
description: Every environment variable each of the four binaries reads, with defaults, whether it is required, and the source file that reads it.
---

# Configuration reference

Every setting is an environment variable. The repository has four binaries,
and each one reads its own set. This page has one table per binary, built from
the composition roots (`cmd/*/main.go`, `cmd/workforce/housekeeping.go`, `cmd/workforce/serve.go`) and
from the two adapters that read the environment themselves
(`internal/adapters/inbound/http/router.go`,
`internal/adapters/outbound/telemetry/telemetry.go`). The test-only variables
`DATABASE_URL`, `KAFKA_BROKERS` and `WAREHOUSE_INFRA_CATALOGUE_PATH` that some
`_test.go` files read are left out.

## How values are parsed

| Helper | Used for | Behaviour on a bad value |
|---|---|---|
| `requireEnv` (`cmd/workforce/main.go`) | `DATABASE_URL` in `cmd/workforce` | Unset or empty: logs `missing required env var` and exits 1. |
| `envOrDefault` / `getenv` | every string setting | An empty string counts as unset, so the default applies. |
| `envFloatOrDefault` | `MAX_HOURS_PER_SHIFT`, `IDLE_SHARE_TRIM_THRESHOLD` | A value that is not a number logs `invalid float env var` and exits 1. |
| `envDurationOrDefault` (`cmd/workforce/main.go`) | `OUTBOX_RELAY_INTERVAL` | A malformed, zero or negative value logs a warning and falls back to the default. It never stops the boot. |
| `envDurationAllowZero` (`cmd/workforce/housekeeping.go`) | `HOUSEKEEPING_INTERVAL`, `IDEMPOTENCY_KEY_TTL`, `OUTBOX_RETENTION`, `SHUTDOWN_DRAIN_DELAY` | `0` is valid and means "disabled" or "keep forever". A malformed or negative value logs a warning and falls back to the default. |

Durations use Go syntax (`500ms`, `30s`, `1h`, `168h`).

## `cmd/workforce`: OLTP REST service

| Variable | Default | Required | Meaning | Read in |
|---|---|---|---|---|
| `DATABASE_URL` | none | **yes** | Postgres DSN for the pgxpool that every request uses. In the cluster this goes through PgBouncer in transaction-pooling mode. | `cmd/workforce/main.go` (`databaseURLFromEnv`) |
| `MIGRATIONS_DATABASE_URL` | value of `DATABASE_URL` | no | Direct, non-pooled DSN used only by the golang-migrate step. golang-migrate takes a session-scoped `pg_advisory_lock`, which PgBouncer transaction pooling cannot hold ([ADR 0025](../adr/0025-migrations-direct-postgres-connection.md)). | `cmd/workforce/main.go` (`envSettingsFromEnv`) |
| `MIGRATIONS_PATH` | `migrations` | no | Directory of the OLTP SQL migrations, relative to the working directory. They are applied on every start. | `cmd/workforce/main.go` |
| `HTTP_ADDR` | `:8080` | no | Listen address of the REST API, `/healthz` and `/readyz`. | `cmd/workforce/main.go` |
| `MAX_HOURS_PER_SHIFT` | `8.0` | no | Per-associate shift-hours ceiling enforced by `CommitShiftPlan`, `AssignLabor` and `EndAssociateShift`. Not a number: exit 1. | `cmd/workforce/main.go` |
| `EVENT_PUBLISHER` | `log` | no | `log` writes domain events to the JSON log only. `kafka` writes each event into `outbox_events` in the use case's transaction (two rows: integration and analytics topic), and this process's outbox relay forwards them ([ADR 0016](../adr/0016-transactional-outbox.md)). Any other value stops the boot with `unknown EVENT_PUBLISHER`. | `cmd/workforce/main.go` (`publisherConfigFromEnv`), `internal/composition/publisher.go` |
| `KAFKA_BROKERS` | `localhost:9092` for the publisher; no default for the consumers | when a Kafka mode is on | Comma-separated bootstrap brokers. The publisher falls back to `localhost:9092`. `PATH_CATALOGUE_SOURCE=kafka` and `LABOR_PERFORMANCE_MODE=kafka-cache` read the raw value and stop the boot when it is empty. | `cmd/workforce/main.go` (three reads) |
| `OUTBOX_RELAY_INTERVAL` | `1s` | no | Poll interval of the outbox relay. Used only when `EVENT_PUBLISHER=kafka`. | `cmd/workforce/main.go` |
| `PATH_CATALOGUE_SOURCE` | `file` | no | Where the process-path catalogue comes from. `kafka` replays the compacted process-path topic before the service accepts traffic (60 s `WaitReady` timeout). Any other value selects the file ([ADR 0013](../adr/0013-process-path-catalogue-validation.md), [ADR 0030](../adr/0030-kafka-sourced-process-path-catalogue.md)). | `cmd/workforce/main.go` (`catalogueConfigFromEnv`), `internal/composition/catalogue.go` |
| `PATH_CATALOGUE_FILE` | `/etc/workforce-management/process-paths.yaml` | when the source is `file` | YAML catalogue of valid path ids. A missing or malformed file stops the boot; there is no empty-catalogue fallback. | `cmd/workforce/main.go` |
| `LABOR_PERFORMANCE_MODE` | `permissive` | no | Measured-rate feed for `ProposePathPlan`. `permissive` returns no measured rate (fail-open). `http` calls labor-performance behind a circuit breaker with retry. `kafka-cache` builds an in-memory read model from the labor-performance integration topic, waits up to 60 s for it to replay, and is also the only mode that supplies the idle-share signal ([ADR 0012](../adr/0012-measured-rate-feed-for-propose-path-plan.md), [ADR 0019](../adr/0019-labor-performance-cache-consumer.md), [ADR 0020](../adr/0020-idle-share-staffing-signal.md)). | `cmd/workforce/main.go` (`newMeasuredRateClients`) |
| `LABOR_PERFORMANCE_BASE_URL` | empty | when the mode is `http` | Base URL of labor-performance's REST API. | `cmd/workforce/main.go` |
| `INSTALLED_CAPACITY_MODE` | `permissive` | yes, set it to `http` in a real deployment | Installed-capacity ceiling for `CommitShiftPlan`. **`permissive` fails every shift-plan commit** (fail-loud, [ADR 0014](../adr/0014-installed-capacity-ceiling.md)). `http` calls fulfillment-execution behind a circuit breaker without retry. | `cmd/workforce/main.go` (`newOLTPHandler`) |
| `FULFILLMENT_EXECUTION_BASE_URL` | empty | when the mode is `http` | Base URL of fulfillment-execution's REST API. | `cmd/workforce/main.go` |
| `IDLE_SHARE_TRIM_THRESHOLD` | `0.30` (`usecases.DefaultIdleShareTrimThreshold`) | no | Idle-share fraction above which `ProposePathPlan` trims its proposed headcount. Only matters in `kafka-cache` mode. Not a number: exit 1. | `cmd/workforce/main.go` |
| `HOUSEKEEPING_INTERVAL` | `1h` | no | How often the housekeeping sweeper runs. `0` disables the sweeper ([ADR 0028](../adr/0028-housekeeping-sweeper-idempotency-keys-and-outbox.md)). | `cmd/workforce/housekeeping.go` |
| `IDEMPOTENCY_KEY_TTL` | `24h` | no | Age after which `idempotency_keys` rows are deleted. `0` keeps them forever. | `cmd/workforce/housekeeping.go` |
| `OUTBOX_RETENTION` | `168h` (7 days) | no | Age after which already-published `outbox_events` rows are deleted. `0` keeps them forever. | `cmd/workforce/housekeeping.go` |
| `SHUTDOWN_DRAIN_DELAY` | `10s` | no | On SIGTERM, how long the process keeps serving after `/readyz` flips to 503, before the HTTP listener closes. `0` skips the wait. The shutdown deadline after that is a fixed 10 s (`shutdownTimeout`) ([ADR 0022](../adr/0022-resilience-circuit-breakers-retry-dlq-shutdown.md)). | `cmd/workforce/serve.go` (`shutdownDrainDelayFromEnv`) |
| `CORS_ALLOWED_ORIGINS` | `http://localhost:5173,http://localhost:5185` | no | Comma-separated browser origins allowed to call the REST API: the console shell's dev port and this service's MFE remote dev port. Credentials are never allowed. | `internal/adapters/inbound/http/router.go` (`corsMiddleware`) |
| `LOG_LEVEL` | `info` | no | `debug`, `info`, `warn` or `error`. Anything else, including `warning`, means `info` in this binary. | `cmd/workforce/main.go` (`newLogger`) |
| `OTEL_SERVICE_NAME` | `workforce-management` | no | OTel `service.name` and the HTTP metrics' service label. | `cmd/workforce/main.go` (`setupServiceTelemetry`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | no | OTLP/gRPC collector endpoint (insecure) for traces and metrics. The service still starts when no collector is listening. | `cmd/workforce/main.go` |
| `SERVICE_VERSION` | `dev` | no | OTel `service.version`. A build-time `-ldflags "-X main.version=..."` takes precedence. | `cmd/workforce/main.go` (`serviceVersion`) |
| `ENVIRONMENT` | `local` | no | OTel resource attribute `deployment.environment.name`. | `internal/adapters/outbound/telemetry/telemetry.go` (`Environment`) |

## `cmd/mcp`: MCP server (Streamable HTTP)

`cmd/mcp` serves the MCP tools at `/` and `/mcp`, plus `GET /healthz`
(`cmd/mcp/router.go`). Unlike `cmd/workforce`, it **does not require a
database**: with no `DATABASE_URL` it uses in-memory repositories, and that
state is separate from any other process.

| Variable | Default | Required | Meaning | Read in |
|---|---|---|---|---|
| `MCP_ADDR` | `:8090` | no | Listen address of the MCP server. | `cmd/mcp/main.go` |
| `DATABASE_URL` | empty (in-memory) | no | Postgres DSN. When set, the process migrates and then opens a pgxpool on the same OLTP database as `cmd/workforce`. | `cmd/mcp/main.go` |
| `MIGRATIONS_DATABASE_URL` | value of `DATABASE_URL` | no | Direct DSN for the migration step, as in `cmd/workforce`. | `cmd/mcp/main.go` |
| `MIGRATIONS_PATH` | `migrations` | no | OLTP migrations directory. This binary also migrates on start. | `cmd/mcp/main.go` |
| `MAX_HOURS_PER_SHIFT` | `8.0` | no | Same ceiling for the `assign_labor` tool. Not a number: exit 1. | `cmd/mcp/main.go` |
| `EVENT_PUBLISHER` | `log` | no | `log` or `kafka`. With `kafka` and a database, events go into `outbox_events` and **only `cmd/workforce` relays them**. With `kafka` and no database, events go straight to both topics. | `cmd/mcp/main.go` (`publisherConfigFromEnv`) |
| `KAFKA_BROKERS` | `localhost:9092` for the publisher; no default for the catalogue | when a Kafka mode is on | Same meaning as in `cmd/workforce`. | `cmd/mcp/main.go` |
| `PATH_CATALOGUE_SOURCE` | `file` | no | Same catalogue as `cmd/workforce`, so MCP path ids are validated exactly like REST ones. | `cmd/mcp/main.go` |
| `PATH_CATALOGUE_FILE` | `/etc/workforce-management/process-paths.yaml` | when the source is `file` | Same as `cmd/workforce`. | `cmd/mcp/main.go` |
| `REPORTS_BASE_URL` | empty | no | When set, registers the `get_workforce_labor_report` tool, which calls `cmd/workforce-reports` over REST. The MCP server never opens the analytical database. | `cmd/mcp/main.go` |
| `LOG_LEVEL` | `info` | no | `debug`, `info`, `warn`/`warning` or `error`. | `cmd/mcp/main.go` |
| `OTEL_SERVICE_NAME` | `workforce-management-mcp` | no | OTel `service.name`. | `cmd/mcp/main.go` (`setupTelemetry`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | no | OTLP/gRPC collector endpoint. | `cmd/mcp/main.go` |
| `SERVICE_VERSION` | `dev` | no | OTel `service.version`, unless ldflags set it. | `cmd/mcp/main.go` |
| `ENVIRONMENT` | `local` | no | `deployment.environment.name`. | `internal/adapters/outbound/telemetry/telemetry.go` |

`cmd/mcp` does not read `OUTBOX_RELAY_INTERVAL`, the `LABOR_PERFORMANCE_*` or
`INSTALLED_CAPACITY_*` variables, `IDLE_SHARE_TRIM_THRESHOLD`, the
housekeeping variables or `CORS_ALLOWED_ORIGINS`. Its `ProposePathPlan` has no
measured-rate client, and it builds no `CommitShiftPlan`.

## `cmd/workforce-projector`: analytics writer

| Variable | Default | Required | Meaning | Read in |
|---|---|---|---|---|
| `ANALYTICS_DATABASE_URL` | none | **yes** | DSN of the analytical Postgres database. When unset, the process exits with `ANALYTICS_DATABASE_URL is required`. The projector is the only writer and owns this schema ([ADR 0010](../adr/0010-analytical-data-product.md)). | `cmd/workforce-projector/main.go` |
| `ANALYTICS_MIGRATIONS_PATH` | `migrations/analytics` | no | Analytical schema migrations, applied on start against `ANALYTICS_DATABASE_URL`. There is no separate migrations DSN for this database. | `cmd/workforce-projector/main.go` |
| `KAFKA_BROKERS` | `localhost:9092` | no | Brokers for the analytics-topic consumer. | `cmd/workforce-projector/main.go` |
| `ADMIN_ADDR` | `:8091` | no | Admin listener. It serves only `GET /healthz`. | `cmd/workforce-projector/main.go` |
| `LOG_LEVEL` | `info` | no | `debug`, `info`, `warn`/`warning` or `error`. | `cmd/workforce-projector/main.go` |
| `OTEL_SERVICE_NAME` | `workforce-projector` | no | OTel `service.name`. | `cmd/workforce-projector/main.go` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | no | OTLP/gRPC collector endpoint. | `cmd/workforce-projector/main.go` |
| `SERVICE_VERSION` | `dev` | no | OTel `service.version`, unless ldflags set it. | `cmd/workforce-projector/main.go` |
| `ENVIRONMENT` | `local` | no | `deployment.environment.name`. | `internal/adapters/outbound/telemetry/telemetry.go` |

## `cmd/workforce-reports`: analytics reader

| Variable | Default | Required | Meaning | Read in |
|---|---|---|---|---|
| `ANALYTICS_DATABASE_URL` | none | **yes** | DSN of the analytical database, opened through a read-only pool. Use a read-only database role as well. When unset, the process exits with `ANALYTICS_DATABASE_URL is required`. | `cmd/workforce-reports/main.go` |
| `HTTP_ADDR` | `:8092` | no | Listen address for `GET /healthz`, `GET /reports/labor` and `GET /reports/labor/freshness` (see [Labor report](../analytics/labor-report.md)). | `cmd/workforce-reports/main.go` |
| `LOG_LEVEL` | `info` | no | `debug`, `info`, `warn`/`warning` or `error`. | `cmd/workforce-reports/main.go` |
| `OTEL_SERVICE_NAME` | `workforce-reports` | no | OTel `service.name` and the reports router's metrics label. | `cmd/workforce-reports/main.go` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | no | OTLP/gRPC collector endpoint. | `cmd/workforce-reports/main.go` |
| `SERVICE_VERSION` | `dev` | no | OTel `service.version`, unless ldflags set it. | `cmd/workforce-reports/main.go` |
| `ENVIRONMENT` | `local` | no | `deployment.environment.name`. | `internal/adapters/outbound/telemetry/telemetry.go` |

The reader runs no migrations and does not consume Kafka.

## Boot retry budget

Each binary's first connections to Postgres, and to Kafka for the
`kafka`/`kafka-cache` consumers, run under `bootretry.Retry`
(`internal/adapters/outbound/bootretry/bootretry.go`,
[ADR 0032](../adr/0032-bootretry-first-outbound-dial.md)). It makes 5 attempts
with a backoff that starts at 1 s and doubles. Only four waits happen between
the five attempts, so the total wait is **1 + 2 + 4 + 8 = 15 s** before the
process gives up and exits with the last error. The package comment says
"~31s total (1+2+4+8+16)", but the loop breaks after the fifth attempt without
sleeping the 16 s step. None of this is configurable through the environment.

## Minimal local set

For a local `cmd/workforce` run you need `DATABASE_URL` and a
`PATH_CATALOGUE_FILE` that exists. Add `INSTALLED_CAPACITY_MODE=http` plus
`FULFILLMENT_EXECUTION_BASE_URL` before you try to commit a shift plan. See the
[Quickstart](../overview/quickstart.md) for the walk-through.
