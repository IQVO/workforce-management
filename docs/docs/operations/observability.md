---
id: observability
title: Observability
sidebar_label: Observability
description: Every OTel metric, span and log field the four binaries emit, how they are exported, the dashboard in warehouse-infra, and suggested alerts.
---

# Observability

All four binaries call `telemetry.Setup`
(`internal/adapters/outbound/telemetry/telemetry.go`). It installs one
TracerProvider and one MeterProvider, and both **push** over OTLP/gRPC
(insecure) to `OTEL_EXPORTER_OTLP_ENDPOINT` (default `localhost:4317`; chart
default `otel-collector.observability.svc.cluster.local:4317`). Metrics are
exported every 30 s (`metricExportInterval`). **No pod exposes a `/metrics`
scrape endpoint.** The OTel Collector re-exports to Prometheus, which turns
dots into underscores and adds unit suffixes
(`circuit_breaker.state` → `circuit_breaker_state`). Propagation is W3C
`traceparent` plus baggage.

The resource attributes on everything are `service.name` (`OTEL_SERVICE_NAME`:
`workforce-management`, `workforce-management-mcp`, `workforce-projector` or
`workforce-reports`), `service.version` and `deployment.environment.name`
(`ENVIRONMENT`). A collector that cannot be reached never blocks startup. The
data is dropped, and the final flush logs `telemetry shutdown did not flush
cleanly` or `opentelemetry shutdown failed`.

## Metrics

### Service-owned instruments

| Instrument | Type | Unit | Attributes | Emitted by | Source |
|---|---|---|---|---|---|
| `workforce.labor_assignments` | Int64Counter | `{assignment}` | `workforce.path.id`; `outcome` = `accepted`\|`rejected`; `reason` (rejected only) = `uncertified`, `on_break`, `shift_ended`, `max_hours_exceeded`, `associate_not_found`, `internal_error` | `cmd/workforce` (REST `AssignLabor`) and `cmd/mcp` (`assign_labor`) | `internal/adapters/outbound/telemetry/labor_metrics.go`, reasons in `internal/application/usecases/metrics.go` |
| `workforce.outbox.lag_seconds` | Float64ObservableGauge | `s` | none | `cmd/workforce`, only with `EVENT_PUBLISHER=kafka` (the relay owner) | `internal/adapters/outbound/postgres/outbox_metrics.go` |
| `circuit_breaker.state` | Int64Gauge | `{state}` | `dependency` = `labor-performance`\|`fulfillment-execution` | `cmd/workforce`, only for clients in `http` mode | `internal/adapters/outbound/telemetry/circuit_breaker_metrics.go` |

- `workforce.outbox.lag_seconds` is the age of the **oldest unpublished** `outbox_events` row (`now() - created_at`), and `0` when the outbox is drained. Each collection runs one `SELECT … WHERE published_at IS NULL ORDER BY id LIMIT 1`. A growing value means the relay is behind or stuck on one row. It does not say which.
- `circuit_breaker.state` is gobreaker's state: `0` closed, `1` half-open, `2` open. It is recorded on each transition only, so a breaker that never tripped has no series.
- Before the 2026-10 ADR-0015 conformance pass, the labor counter's attributes were `workforce.assignment.outcome`/`.reason`. They are now `outcome`/`reason` ([ADR 0015](../adr/0015-standard-metrics-convention.md)).

### Library instruments

| Instrument | Type | Attributes | Emitted by | Source |
|---|---|---|---|---|
| `http.server.request.duration` | Float64Histogram, `s` (buckets 5 ms … 10 s) | `http.method`, `http.scheme`, `http.route` (semconv v1.20). **No status-code attribute.** | `cmd/workforce` (REST router), `cmd/mcp` (`/healthz`, `/mcp`, `/`), `cmd/workforce-reports` | otelchi v0.12.3 `metric.NewServerRequestDuration`, wired in `internal/adapters/inbound/http/router.go`, `reports_handler.go`, `cmd/mcp/router.go` |
| `go.memory.used`, `go.goroutine.count` and the other Go runtime instruments | various | none | all four binaries | `go.opentelemetry.io/contrib/instrumentation/runtime` v0.71.0 (`runtime.Start` in `telemetry.Setup`) |
| `pgxpool.acquires`, `pgxpool.acquire_duration`, `pgxpool.acquired_connections`, `pgxpool.idle_connections`, `pgxpool.total_connections`, `pgxpool.max_connections`, `pgxpool.empty_acquire`, `pgxpool.empty_acquire_wait_time`, `pgxpool.canceled_acquires`, `pgxpool.constructing_connections`, `pgxpool.new_connections`, `pgxpool.max_idle_destroys`, `pgxpool.max_lifetime_destroys` | observable counters and gauges | `db.system.name=postgresql`, `db.client.connection.pool.name` | **only** `cmd/workforce-projector` and `cmd/workforce-reports` (`analyticsstore.RecordPoolStats`) | otelpgx v0.12.0 `RecordStats` |

The OLTP pool (`cmd/workforce`, `cmd/mcp`) is traced by otelpgx but does
**not** record `pgxpool.*` stats. The projector's admin server has no HTTP
instrumentation.

## Traces

| Span name | Kind | Where | Attributes |
|---|---|---|---|
| chi route pattern, e.g. `/associates/{id}/start-shift` | server | otelchi middleware on every REST, reports and MCP request | HTTP semconv |
| otelpgx query spans | client | every Postgres round-trip (OLTP and analytics pools) | sanitized SQL statement |
| `kafka.publish warehouse.workforce.events` | producer | direct (no-database) integration publisher | `messaging.system=kafka`, `messaging.operation.name=publish`, `messaging.destination.name`, `messaging.batch.message_count` |
| `kafka.publish warehouse.workforce.analytics` | producer | direct analytics publisher | same |
| `kafka.publish outbox` | producer | outbox relay sink (`internal/adapters/outbound/kafka/relay_sink.go`) | `messaging.system`, `messaging.operation.name`, `messaging.batch.message_count` |
| `kafka.consume warehouse.workforce.analytics` | consumer | projector, per message (`analytics_consumer.go`) | `messaging.system`, `messaging.destination.name`, `messaging.operation.name=process` |
| `mcp.tool <tool name>`, e.g. `mcp.tool assign_labor` | internal | every MCP tool call (`internal/adapters/inbound/mcp/tools.go`) | `mcp.tool.name`. Error status on failure. |

In outbox mode the W3C trace context is captured when the use case raises the
event and stored in the row's `headers`. The relay forwards it untouched, so
the projector's `kafka.consume` span becomes a child of the **originating HTTP
or MCP request**, not of the relay pass.

## Logs

JSON to stdout through `slog.NewJSONHandler`, wrapped in
`telemetry.NewTraceHandler`. Every `*Context` log call made while a span is
active gets `trace_id` and `span_id`. Standard keys are `time`, `level`, `msg`.
`LOG_LEVEL` sets the level (see [Configuration](./configuration.md)).

Messages worth searching for:

| Message | Binary | Meaning |
|---|---|---|
| `retrying` (`op`, `attempt`, `in`, `err`) / `succeeded after retry` | all | Boot retry around the first Postgres or Kafka dial. |
| `missing required env var` | workforce | `DATABASE_URL` unset. Exit 1. |
| `fulfillment-execution installed capacity client configured` at WARN with `mode=permissive` | workforce | Every shift-plan commit will fail. |
| `waiting for the process-path catalogue to replay …` / `process-path catalogue is ready` | workforce, mcp | Kafka catalogue boot gate. |
| `outbox relay pass failed` | workforce | A relay pass hit an error. The row is retried next pass. |
| `housekeeping sweep` / `housekeeping sweep failed` | workforce | Sweeper results (`idempotency_keys_deleted`, `outbox_events_deleted`). |
| `readiness flipped to not-ready; draining before closing the listener` | workforce | SIGTERM received. |
| `… skipping message that is not a valid CloudEvents 1.0 event` | workforce, mcp | Catalogue or labor-performance cache skipped a message. |
| `analytics: exhausted retries, sending to dead-letter topic` (`topic`, `dlq_topic`, `attempts`, `error`) | projector | A message went to `warehouse.workforce.analytics.dlq`. |
| `analytics consumer stopped; exiting so the pod restarts` | projector | The consumer loop exited (commit or DLQ write failure, or any return without a shutdown signal). The process shuts its admin server down and exits non-zero, so Kubernetes restarts it (gap fix D13). |

## Dashboard

`warehouse-infra` ships the Grafana dashboard **"Warehouse — Workforce
Management"** (`terraform/dashboards/contexts/workforce-management.json`). Its
rows:

- Kong north-south: request rate, 5xx rate, p95 latency.
- Service HTTP RED on `http_server_request_duration_seconds`.
- `workforce_labor_assignments_total`.
- Go runtime (goroutines, memory).
- Loki log panels.

Two of its queries do not match what the code emits:

- The labor panel groups by `workforce_assignment_outcome`. The attribute has been `outcome` since the ADR-0015 conformance pass, so the panel shows one unlabelled series.
- The "Error rate (HTTP 5xx, by process)" panel filters on `http_response_status_code` / `http_status_code`. otelchi v0.12.3 records neither, so the panel is always empty. Use the Kong 5xx panel or traces instead.

There are no panels yet for `workforce.outbox.lag_seconds`,
`circuit_breaker.state` or the analytics `pgxpool.*` series.

## Suggested alerts

| Alert | Expression (Prometheus names after the collector) | Why |
|---|---|---|
| Outbox stuck | `max(workforce_outbox_lag_seconds) > 60` for 5 m | The relay is behind or a row is failing. Downstream contexts stop seeing events. |
| Breaker open | `max by (dependency) (circuit_breaker_state) == 2` for 2 m | labor-performance open: proposals lose the measured rate (fail-open). fulfillment-execution open: every shift-plan commit fails. |
| Assignment rejections climbing | `sum(rate(workforce_labor_assignments_total{outcome="rejected"}[15m])) / sum(rate(workforce_labor_assignments_total[15m])) > 0.5` | The floor is being asked to staff paths it cannot. |
| Analytics DLQ growth | broker-side end offset of `warehouse.workforce.analytics.dlq` increasing | Poison events are being skipped. The report under-counts. |
| Analytics consumer lag | consumer-group lag of `workforce-analytics` on `warehouse.workforce.analytics` | The projector is slow, or restarting: a stopped consumer loop ends the process (pod restarts count up). |
| Report staleness | poll `GET /reports/labor/freshness` → `lagSeconds` above your SLO | Same symptom, seen from the read side. |
| Slow API | `histogram_quantile(0.95, sum by (le, http_route) (rate(http_server_request_duration_seconds_bucket{service_name="workforce-management"}[5m]))) > 0.5` | Latency SLO. |
