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
| `loadBootConfig` (`cmd/workforce/config.go`, `cmd/mcp/main.go`) | `DATABASE_URL`, `KAFKA_BROKERS`, `EVENT_PUBLISHER`, `PATH_CATALOGUE_SOURCE`, `LABOR_PERFORMANCE_MODE` | Every problem is collected and reported at once, each message naming its variable. The process exits 1 before any connection and before the listener binds ([ADR 0038](../adr/0038-postgres-and-kafka-are-the-only-runtime.md)). |
| `envOrDefault` / `getenv` | every string setting | An empty string counts as unset, so the default applies. |
| `envFloatOrDefault` | `MAX_HOURS_PER_SHIFT`, `IDLE_SHARE_TRIM_THRESHOLD` | A value that is not a number logs `invalid float env var` and exits 1. |
| `envDurationOrDefault` (`cmd/workforce/main.go`) | `OUTBOX_RELAY_INTERVAL` | A malformed, zero or negative value logs a warning and falls back to the default. It never stops the boot. |
| `envNonNegativeIntOrDefault` (`cmd/workforce/main.go`) | `OUTBOX_MAX_ATTEMPTS` | `0` is valid ("unlimited"). A non-numeric or negative value logs a warning and falls back to the default. It never stops the boot. |
| `envDurationAllowZero` (`cmd/workforce/housekeeping.go`) | `HOUSEKEEPING_INTERVAL`, `IDEMPOTENCY_KEY_TTL`, `OUTBOX_RETENTION`, `SHUTDOWN_DRAIN_DELAY` | `0` is valid and means "disabled" or "keep forever". A malformed or negative value logs a warning and falls back to the default. |

Durations use Go syntax (`500ms`, `30s`, `1h`, `168h`).

## `cmd/workforce`: OLTP REST service

| Variable | Default | Required | Meaning | Read in |
|---|---|---|---|---|
| `DATABASE_URL` | none | **yes** | Postgres DSN for the pgxpool that every request uses. In the cluster this goes through PgBouncer in transaction-pooling mode. Unset: `DATABASE_URL is required: this context runs on real Postgres`. | `cmd/workforce/config.go` (`loadBootConfig`) |
| `MIGRATIONS_DATABASE_URL` | value of `DATABASE_URL` | no | Direct, non-pooled DSN used only by the golang-migrate step. golang-migrate takes a session-scoped `pg_advisory_lock`, which PgBouncer transaction pooling cannot hold ([ADR 0025](../adr/0025-migrations-direct-postgres-connection.md)). | `cmd/workforce/main.go` (`envSettingsFromEnv`) |
| `MIGRATIONS_PATH` | `migrations` | no | Directory of the OLTP SQL migrations, relative to the working directory. They are applied on every start. | `cmd/workforce/main.go` |
| `HTTP_ADDR` | `:8080` | no | Listen address of the REST API, `/healthz` and `/readyz`. | `cmd/workforce/main.go` |
| `MAX_HOURS_PER_SHIFT` | `8.0` | no | Per-associate shift-hours ceiling enforced by `CommitShiftPlan`, `AssignLabor` and `EndAssociateShift`. Not a number: exit 1. | `cmd/workforce/main.go` |
| `EVENT_PUBLISHER` | unset (means `kafka`) | no | Kept only because the chart and warehouse-infra set it. Unset and `kafka` are accepted: each event goes into `outbox_events` in the use case's transaction (two rows: integration and analytics topic) and this process's outbox relay, which is always on, forwards them ([ADR 0016](../adr/0016-transactional-outbox.md)). `log` stops the boot with `EVENT_PUBLISHER=log is not supported: this context publishes to real Kafka`; any other value with `unknown EVENT_PUBLISHER`. | `internal/composition/config.go` (`CheckEventPublisher`), `internal/composition/publisher.go` |
| `KAFKA_BROKERS` | none | **yes** | Comma-separated bootstrap brokers for the outbox relay, the catalogue and labor-performance consumers and `/readyz`. Unset or blank: `KAFKA_BROKERS is required: this context runs on real Kafka`. | `cmd/workforce/config.go` (`loadBootConfig`) |
| `OUTBOX_RELAY_INTERVAL` | `1s` | no | Poll interval of the outbox relay. | `cmd/workforce/main.go` |
| `OUTBOX_MAX_ATTEMPTS` | `20` | no | Failed sends after which the outbox relay stops claiming a row, so one poison row cannot block the relay forever. The row stays in `outbox_events` with `attempts`/`last_error` and is logged once at ERROR; re-arm it with `UPDATE outbox_events SET attempts = 0 WHERE id = ...`. `0` = unlimited. A negative or non-numeric value logs a warning and uses `20` ([ADR 0036](../adr/0036-outbox-relay-attempt-cap.md)). | `cmd/workforce/main.go` (`publisherConfigFromEnv`), `internal/composition/publisher.go` |
| `PATH_CATALOGUE_SOURCE` | unset (means `kafka`) | no | The process-path catalogue is always replayed from the compacted process-path topic before the service accepts traffic (60 s `WaitReady` timeout) ([ADR 0013](../adr/0013-process-path-catalogue-validation.md), [ADR 0030](../adr/0030-kafka-sourced-process-path-catalogue.md)). `file` stops the boot with `PATH_CATALOGUE_SOURCE=file is not supported: this context runs on real Kafka`; any other value with `unknown PATH_CATALOGUE_SOURCE`. `PATH_CATALOGUE_FILE` and the YAML loader no longer exist. | `internal/composition/config.go` (`CheckCatalogueSource`), `internal/composition/catalogue.go` |
| `LABOR_PERFORMANCE_MODE` | `kafka-cache` | no | Measured-rate feed for `ProposePathPlan`. `kafka-cache` (default) builds a local read model from the labor-performance integration topic, waits up to 60 s for it to replay, and is the only mode that supplies the idle-share signal ([ADR 0019](../adr/0019-labor-performance-cache-consumer.md), [ADR 0020](../adr/0020-idle-share-staffing-signal.md)). `http` is the REST rollback of that edge (calls labor-performance behind a circuit breaker with retry; needs `LABOR_PERFORMANCE_BASE_URL`; [ADR 0012](../adr/0012-measured-rate-feed-for-propose-path-plan.md), R10 in `docs/rollback-switches.md`), a REST sync edge that ADR 0038 leaves alone. `permissive` stops the boot with `LABOR_PERFORMANCE_MODE=permissive is not supported: this context runs on real Kafka`. | `cmd/workforce/config.go` (`checkLaborPerformanceMode`), `cmd/workforce/main.go` (`newMeasuredRateClients`) |
| `LABOR_PERFORMANCE_BASE_URL` | empty | when the mode is `http` | Base URL of labor-performance's REST API. | `cmd/workforce/main.go` |
| `INSTALLED_CAPACITY_MODE` | `permissive` (binary); `http` (Helm chart, `config.installedCapacityMode`) | yes, set it to `http` in a real deployment | Installed-capacity ceiling for `CommitShiftPlan`. **`permissive` fails every shift-plan commit** (fail-loud, [ADR 0014](../adr/0014-installed-capacity-ceiling.md)). `http` calls fulfillment-execution behind a circuit breaker without retry. The chart sets `http` by default (gap fix D24). | `cmd/workforce/main.go` (`newOLTPHandler`) |
| `FULFILLMENT_EXECUTION_BASE_URL` | empty (binary); `http://fulfillment-execution.<release namespace>.svc.cluster.local:80` (Helm chart, `config.fulfillmentExecutionBaseUrl`) | when the mode is `http` | Base URL of fulfillment-execution's REST API. The chart default is fulfillment-execution's in-cluster Service. | `cmd/workforce/main.go` |
| `IDLE_SHARE_TRIM_THRESHOLD` | `0.30` (`usecases.DefaultIdleShareTrimThreshold`) | no | Idle-share fraction above which `ProposePathPlan` trims its proposed headcount. Only matters in `kafka-cache` mode. Not a number: exit 1. | `cmd/workforce/main.go` |
| `HOUSEKEEPING_INTERVAL` | `1h` | no | How often the housekeeping sweeper runs. `0` disables the sweeper ([ADR 0028](../adr/0028-housekeeping-sweeper-idempotency-keys-and-outbox.md)). | `cmd/workforce/housekeeping.go` |
| `IDEMPOTENCY_KEY_TTL` | `24h` | no | Age after which `idempotency_keys` rows are deleted. `0` keeps them forever. | `cmd/workforce/housekeeping.go` |
| `OUTBOX_RETENTION` | `168h` (7 days) | no | Age after which already-published `outbox_events` rows are deleted. `0` keeps them forever. | `cmd/workforce/housekeeping.go` |
| `SHUTDOWN_DRAIN_DELAY` | `10s` | no | On SIGTERM, how long the process keeps serving after `/readyz` flips to 503, before the HTTP listener closes. `0` skips the wait. The shutdown deadline after that is a fixed 10 s (`shutdownTimeout`) ([ADR 0022](../adr/0022-resilience-circuit-breakers-retry-dlq-shutdown.md)). | `cmd/workforce/serve.go` (`shutdownDrainDelayFromEnv`) |
| `CORS_ALLOWED_ORIGINS` | `http://localhost:5173,http://localhost:5185` | no | Comma-separated browser origins allowed to call the REST API: the console shell's dev port and this service's MFE remote dev port. Credentials are never allowed. | `internal/adapters/inbound/http/router.go` (`corsMiddleware`) |
| `LOG_LEVEL` | `info` | no | `debug`, `info`, `warn`/`warning` or `error` (case-insensitive). Anything else means `info`. | `cmd/workforce/main.go` (`newLogger`) |
| `OTEL_SERVICE_NAME` | `workforce-management` | no | OTel `service.name` and the HTTP metrics' service label. | `cmd/workforce/main.go` (`setupServiceTelemetry`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | no | OTLP/gRPC collector endpoint (insecure) for traces and metrics. The service still starts when no collector is listening. | `cmd/workforce/main.go` |
| `SERVICE_VERSION` | `dev` | no | OTel `service.version`. A build-time `-ldflags "-X main.version=..."` takes precedence. | `cmd/workforce/main.go` (`serviceVersion`) |
| `ENVIRONMENT` | `local` | no | OTel resource attribute `deployment.environment.name`. | `internal/adapters/outbound/telemetry/telemetry.go` (`Environment`) |

`cmd/workforce-projector` and `cmd/mcp` boot the same way: mandatory configuration
is validated first, each error names its variable, and nothing is opened before it passes.

## `cmd/mcp`: MCP server (Streamable HTTP)

`cmd/mcp` serves the MCP tools at `/` and `/mcp`, plus `GET /healthz`
(`cmd/mcp/router.go`). It reads and writes **the same Postgres as
`cmd/workforce`** and requires it, exactly like `cmd/workforce` ([ADR 0038](../adr/0038-postgres-and-kafka-are-the-only-runtime.md)):
there is no in-memory repository set. Its events go into `outbox_events`; only
`cmd/workforce` relays them to Kafka.

| Variable | Default | Required | Meaning | Read in |
|---|---|---|---|---|
| `MCP_ADDR` | `:8090` | no | Listen address of the MCP server. | `cmd/mcp/main.go` |
| `DATABASE_URL` | none | **yes** | Postgres DSN; the process migrates and then opens a pgxpool on the same OLTP database as `cmd/workforce`. Unset: `DATABASE_URL is required: this context runs on real Postgres`. | `cmd/mcp/main.go` (`loadBootConfig`) |
| `MIGRATIONS_DATABASE_URL` | value of `DATABASE_URL` | no | Direct DSN for the migration step, as in `cmd/workforce`. | `cmd/mcp/main.go` |
| `MIGRATIONS_PATH` | `migrations` | no | OLTP migrations directory. This binary also migrates on start. | `cmd/mcp/main.go` |
| `MAX_HOURS_PER_SHIFT` | `8.0` | no | Same ceiling for the `assign_labor` tool. Not a number: exit 1. | `cmd/mcp/main.go` |
| `EVENT_PUBLISHER` | unset (means `kafka`) | no | Same rule as `cmd/workforce`: unset or `kafka`; `log` and anything else stop the boot. | `internal/composition/config.go` |
| `KAFKA_BROKERS` | none | **yes** | Same meaning as in `cmd/workforce`: the catalogue consumer and the outbox publisher need it. Unset: `KAFKA_BROKERS is required: this context runs on real Kafka`. | `cmd/mcp/main.go` (`loadBootConfig`) |
| `PATH_CATALOGUE_SOURCE` | unset (means `kafka`) | no | Same catalogue as `cmd/workforce` (replayed from Kafka), so MCP path ids are validated exactly like REST ones. `file` stops the boot. | `internal/composition/config.go` |
| `REPORTS_BASE_URL` | empty | no | When set, registers the `get_workforce_labor_report` tool, which calls `cmd/workforce-reports` over REST. The MCP server never opens the analytical database. | `cmd/mcp/main.go` |
| `LOG_LEVEL` | `info` | no | `debug`, `info`, `warn`/`warning` or `error`. | `cmd/mcp/main.go` |
| `OTEL_SERVICE_NAME` | `workforce-management-mcp` | no | OTel `service.name`. | `cmd/mcp/main.go` (`setupTelemetry`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | no | OTLP/gRPC collector endpoint. | `cmd/mcp/main.go` |
| `SERVICE_VERSION` | `dev` | no | OTel `service.version`, unless ldflags set it. | `cmd/mcp/main.go` |
| `ENVIRONMENT` | `local` | no | `deployment.environment.name`. | `internal/adapters/outbound/telemetry/telemetry.go` |

`cmd/mcp` does not read `OUTBOX_RELAY_INTERVAL`, `OUTBOX_MAX_ATTEMPTS`, the `LABOR_PERFORMANCE_*` or
`INSTALLED_CAPACITY_*` variables, `IDLE_SHARE_TRIM_THRESHOLD`, the
housekeeping variables or `CORS_ALLOWED_ORIGINS`. Its `ProposePathPlan` has no
measured-rate client, and it builds no `CommitShiftPlan`.

## `cmd/workforce-projector`: analytics writer

| Variable | Default | Required | Meaning | Read in |
|---|---|---|---|---|
| `ANALYTICS_DATABASE_URL` | none | **yes** | DSN of the analytical Postgres database. When unset, the process exits with `ANALYTICS_DATABASE_URL is required: the projector writes only to the analytical database`. The projector is the only writer and owns this schema ([ADR 0010](../adr/0010-analytical-data-product.md)). | `cmd/workforce-projector/main.go` |
| `ANALYTICS_MIGRATIONS_PATH` | `migrations/analytics` | no | Analytical schema migrations, applied on start against `ANALYTICS_DATABASE_URL`. There is no separate migrations DSN for this database. | `cmd/workforce-projector/main.go` |
| `KAFKA_BROKERS` | none | **yes** | Brokers for the analytics-topic consumer. Unset: `KAFKA_BROKERS is required: the projector consumes the analytics topic`. | `cmd/workforce-projector/main.go` |
| `ANALYTICS_CONSUMER_GROUP` | none | **yes** | The projector's stable, shared Kafka consumer group id (the chart default is `workforce-analytics`, the group it has always used). There is no default in the binary: unset, the process exits with `ANALYTICS_CONSUMER_GROUP is required: a stable Kafka group id for the projector; there is no default` ([ADR 0038](../adr/0038-postgres-and-kafka-are-the-only-runtime.md)). | `cmd/workforce-projector/main.go` |
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
catalogue and labor-performance consumers, run under `bootretry.Retry`
(`internal/adapters/outbound/bootretry/bootretry.go`,
[ADR 0032](../adr/0032-bootretry-first-outbound-dial.md)). It makes 5 attempts
with a backoff that starts at 1 s and doubles. Only four waits happen between
the five attempts, so the total wait is **1 + 2 + 4 + 8 = 15 s** before the
process gives up and exits with the last error (the loop breaks after the
fifth attempt without sleeping a 16 s step; the package comment says so since
gap fix D22). None of this is configurable through the environment.

## Minimal local set

For a local `cmd/workforce` run you need `DATABASE_URL` and `KAFKA_BROKERS`
(`docker compose up -d --wait` starts a Postgres and a single-node Kafka that
fit the values in the [Quickstart](../overview/quickstart.md)); nothing else is
required to boot. Add `INSTALLED_CAPACITY_MODE=http` plus
`FULFILLMENT_EXECUTION_BASE_URL` before you try to commit a shift plan (the
Helm chart sets both by default). The process-path catalogue is empty until
`process-path-management` publishes (or the quickstart's seed event is
produced). See the
[Quickstart](../overview/quickstart.md) for the walk-through.

## How the Helm chart delivers `cmd/workforce`'s variables

`charts/workforce-management/templates/configmap.yaml` renders `config.*` (plus
`kafka.brokers`) into a ConfigMap named after the release. The chart refuses to
render, naming the value, without `database.url`/`database.existingSecret`
(`DATABASE_URL`) and without `kafka.brokers` (`KAFKA_BROKERS`, no default); with
`config.eventPublisher` other than `kafka`; with `kafka.enabled=false` (the key
is a legacy no-op, `true` still renders); with `pathCatalogue.enabled=true` (the
file catalogue is gone); with a `terminationGracePeriodSeconds` under 25; with
`EVENT_PUBLISHER`, `PATH_CATALOGUE_FILE`, `PATH_CATALOGUE_SOURCE` or
`LABOR_PERFORMANCE_MODE` set to an unsupported value, or any other `*_MODE`, in
`extraEnv` (`INSTALLED_CAPACITY_MODE` and `LABOR_PERFORMANCE_MODE=kafka-cache`/`http`
stay allowed); and, with `analytics.enabled`, without an analytical DSN source or
`analytics.projector.consumerGroup`.

| ConfigMap key | Chart value | Chart default |
|---|---|---|
| `HTTP_ADDR` | `config.httpAddr` | `:8080` |
| `EVENT_PUBLISHER` | `config.eventPublisher` | `kafka` (the only accepted value) |
| `MIGRATIONS_PATH` | `config.migrationsPath` | `migrations` |
| `MAX_HOURS_PER_SHIFT` | `config.maxHoursPerShift` | `8.0` |
| `OUTBOX_RELAY_INTERVAL` | `config.outboxRelayInterval` | `1s` |
| `OUTBOX_MAX_ATTEMPTS` | `config.outboxMaxAttempts` | `20` |
| `HOUSEKEEPING_INTERVAL` | `config.housekeepingInterval` | `1h` |
| `IDEMPOTENCY_KEY_TTL` | `config.idempotencyKeyTtl` | `24h` |
| `OUTBOX_RETENTION` | `config.outboxRetention` | `168h` |
| `SHUTDOWN_DRAIN_DELAY` | `config.shutdownDrainDelay` | `10s` |
| `INSTALLED_CAPACITY_MODE` | `config.installedCapacityMode` | `http` |
| `FULFILLMENT_EXECUTION_BASE_URL` | `config.fulfillmentExecutionBaseUrl` | empty, rendered as `http://fulfillment-execution.<release namespace>.svc.cluster.local:80` |
| `KAFKA_BROKERS` | `kafka.brokers` (always rendered) | none: required |

The api Deployment mounts it with `envFrom: configMapRef` (gap fix D14,
2026-10-12), so every `config.*` value reaches `cmd/workforce`. Explicit `env`
entries in `deployment.yaml` (the same values, plus the Secret-backed DSNs and the
OTel variables) and `extraEnv` win over the ConfigMap
for the same key. The projector, reports and MCP Deployments do not mount the
ConfigMap: their variables are set explicitly in their own templates (the
reports binary would otherwise inherit `HTTP_ADDR=:8080`).
`INSTALLED_CAPACITY_MODE` and `FULFILLMENT_EXECUTION_BASE_URL` live only in the
ConfigMap, never in an explicit `env` entry, so an `extraEnv` entry for the
same name (warehouse-infra sets both) wins without rendering a duplicate env
key, which would break ArgoCD's diff.
`charts/workforce-management/tests/render_test.sh` fails when the mount
disappears, when a tuned value is not rendered, or when another workload mounts
the ConfigMap.
