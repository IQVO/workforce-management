# Analytics data product, MCP adapter, observability

## Analytics data product — Labor Utilization & Staffing (ADR-0010)

Additive read side built from this service's OWN domain events. The OLTP
domain/application layers are NOT modified and must NOT import the analytics
store (arch-test enforces this). `internal/analytics/report/` depends on
nothing. (`ProcessedEvents` in `application/ports` is the shared idempotency
gate, used only by the analytics projector — the OLTP write path does not
depend on it.)

- Events are fanned to a SEPARATE topic `warehouse.workforce.analytics` by
  the outbound Kafka adapter; the integration topic/publisher are untouched.
  Selected by `EVENT_PUBLISHER=kafka` (fan-out alongside the integration
  publisher, same transactional-outbox delivery as rules/integrations.md).
- Separate analytical Postgres (`ANALYTICS_DATABASE_URL`), own migrations
  (`migrations/analytics/`), read-only reader role for `workforce-reports`.
- **Three processes**:
  - `cmd/workforce` — OLTP.
  - `cmd/workforce-projector` — the ONLY writer of the analytical DB;
    consumes from FirstOffset, CloudEvents 1.0 only (ADR-0026), idempotent
    on the CloudEvents `id`; invalid events go to the `.dlq` topic; runs
    `migrations/analytics` on start; admin/health on `:8091`.
  - `cmd/workforce-reports` — read-only reader, `GET /reports/...`, serves on
    `:8092`.
- **Report**: Labor Utilization & Staffing, keyed per path/shift × hour
  (shifts started/ended, break time, labor assigned/reassigned,
  understaffing).
- `GET /reports/.../freshness` reports projection lag.
- Full contract: `docs/docs/analytics/labor-report.md`.

```bash
# 2. OLTP fanning events onto the analytics topic
export EVENT_PUBLISHER=kafka KAFKA_BROKERS=localhost:9092
go run ./cmd/workforce

# 3. Projector (only writer of the analytical DB)
export ANALYTICS_DATABASE_URL="postgres://workforce:***@localhost:5432/workforce_analytics?sslmode=disable"
go run ./cmd/workforce-projector

# 4. Reports (read-only)
export ANALYTICS_DATABASE_URL="postgres://workforce_ro:***@localhost:5432/workforce_analytics?sslmode=disable"
go run ./cmd/workforce-reports

curl 'http://localhost:8092/reports/labor?from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z'
curl 'http://localhost:8092/reports/labor/freshness'
```

Optionally expose the curated read-only MCP tool `get_workforce_labor_report`
by setting `REPORTS_BASE_URL` on `cmd/mcp`; it calls the reports REST and
never opens the analytical database directly.

## MCP inbound adapter (ADR-0008)

`cmd/mcp` is a fourth binary in the same image, `/app/mcp`. Streamable HTTP,
mounted at both `/` and `/mcp`. `GET /healthz` serves liveness/readiness. The
Helm chart deploys it as a separate Deployment + ClusterIP Service
(`<release>-mcp`, port 8090) when `mcp.enabled=true` (default `false`). Runs
the same use cases over the same `DATABASE_URL` as the HTTP service; reads
`MCP_ADDR` (default `:8090`). When `analytics.enabled` is also true,
`REPORTS_BASE_URL` defaults to the in-cluster reports Service.

```bash
helm upgrade --install workforce-management charts/workforce-management \
  --set database.url="postgres://..." \
  --set mcp.enabled=true
```

## Observability

Traces and metrics export over **OTLP/gRPC** to an OpenTelemetry Collector
(`OTEL_EXPORTER_OTLP_ENDPOINT`, default `localhost:4317`; in-cluster default
is `otel-collector.observability.svc.cluster.local:4317`, see `otel.endpoint`
in `charts/workforce-management/values.yaml`). Logs are structured JSON on
stdout, correlated to traces by `trace_id`/`span_id`.

**No `/metrics` scrape endpoint** — the Collector does Prometheus exposition,
the pod only pushes. **If no Collector is reachable, nothing breaks** — the
OTLP exporters use no `grpc.WithBlock()`, so telemetry is silently dropped;
enforced by `TestSetupDoesNotBlockWithoutACollector`, not just convention.

| Signal | What |
|---|---|
| Traces | one server span per HTTP request, named by **route pattern** (`POST /associates/{id}/assignments`), not raw path — bounds span-name cardinality |
| Traces | a client child span per Postgres round-trip via `otelpgx`, carrying **parameterized** SQL only (never literal values) |
| Traces | with `EVENT_PUBLISHER=kafka`, W3C trace context is captured into each outbox row's headers when the event is raised; the relay's `kafka.publish outbox` producer span forwards those headers untouched, so a consumer's span parents onto the originating request's trace (ADR-0016) |
| Metrics | `http.server.request.duration` (histogram, seconds, OTel HTTP semconv) |
| Metrics | `workforce.labor_assignments` (counter) — every `AssignLabor` attempt, attributed `workforce.assignment.outcome=accepted\|rejected`, `workforce.path.id`, and on rejection `workforce.assignment.reason` (`uncertified`, `on_break`, `shift_ended`, `max_hours_exceeded`, `associate_not_found`, `internal_error`) |
| Metrics | Go runtime metrics (goroutines, GC, memory) |
| Logs | JSON via `log/slog`, level from `LOG_LEVEL`; request-scoped lines carry `trace_id`, `span_id`, `route`, `request_id` |

`workforce.labor_assignments` is instrumented **in the use case**, not the
HTTP handler, so it counts real domain outcomes rather than requests. There
is no `double_booked` rejection reason: a second active assignment ends the
prior one and raises `LaborReassigned` (prevented by construction), so it is
never rejected — see `LaborAssignment.Assign`.
