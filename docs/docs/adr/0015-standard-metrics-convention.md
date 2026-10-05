---
id: 0015-standard-metrics-convention
slug: /adr/0015-standard-metrics-convention
title: 0015. Standard metrics convention across the fleet
sidebar_label: 0015. Standard metrics convention (fleet-wide)
sidebar_position: 16
description: ADR 0015 — a fleet-wide, two-tier metrics convention (mandatory telemetry.Setup + otelchi RED instrumentation, plus a shared naming/attribute shape for business counters) so metrics are predictable across services without reading each service's source first. workforce-management is already fully compliant.
---

# 0015. Standard metrics convention across the fleet

## Status

Accepted — workforce-management's own counter placement and attribute keys
were brought to this decision's Tier-2 shape by the 2026-10 ADR-conformance
pass (instrument moved from the application layer's global `otel.Meter` to
`ports.LaborMetrics` + the outbound telemetry adapter; attribute keys
renamed `workforce.assignment.outcome`/`workforce.assignment.reason` →
`outcome`/`reason`; instrument name `workforce.labor_assignments`
unchanged).

## Context

Every service in the fleet installs the same OTel pipeline (service pods
--OTLP/gRPC--> otel-collector --OTLP/gRPC--> Jaeger, with a `prometheus`
exporter re-publishing metrics for Prometheus/Grafana — see
warehouse-infra/terraform/observability.tf). That pipeline is a real,
enforced contract: the Collector's DNS name and OTLP port are hard-coded in
every repo's Helm values, and Grafana ships one dashboard
(`go-runtime.json`) built from the Go-runtime metrics
`go.opentelemetry.io/contrib/instrumentation/runtime` emits — the one thing
every instrumented service already reports identically.

Past that shared plumbing, nothing was standard. An audit across all eight
services found:

- Two services (`order-management`, `labor-performance`) had no
  `telemetry.Setup` at all — no MeterProvider, no TracerProvider, not even
  the Go-runtime metrics every other service gets for free. `order-management`
  additionally had no HTTP RED instrumentation (`otelchi`).
- Of the six services that do call `telemetry.Setup`, HTTP request
  rate/error/duration (RED) instrumentation via `otelchi` was present in five
  and absent in one (`labor-performance`).
- Every service that emits a business metric invented its own instrument
  name, unit, and attribute keys independently:
  `inventory.reservations` (attr `outcome`), `workforce.labor_assignments`
  (attrs `outcome`, `reason`, `path.id`... — actually the assignment attr key
  was `workforce.path.id`, not namespaced under `path_id` the way
  `wes-work-planning` names its own `path_id` attribute),
  `fulfillment.tasks.claimed` / `fulfillment.tasks.completed` (attr
  `task.type`), `facility.location_slot.registrations`. No two services
  agree on whether the bounded-context prefix is `<context>.<noun>` or
  `<context>.<aggregate>.<verb>`, whether an attribute key uses a dot or an
  underscore, or whether an outcome is spelled as an attribute on one
  instrument or as two separate instruments.

A metric no dashboard-builder can predict the shape of in advance is a
metric nobody actually queries across services. The Go-runtime dashboard
being the fleet's only dashboard, eleven weeks into having Prometheus
running, is the visible symptom.

`workforce-management` is one of the fleet's reference implementations for
this ADR: it calls `telemetry.Setup`, wires the three-line `otelchi` RED
middleware chain on its HTTP adapter, and emits the
`workforce.labor_assignments` business counter. (A 2026-10 conformance pass
moved that counter behind a `ports.LaborMetrics` port implemented by the
outbound telemetry adapter and renamed its attribute keys to the fleet's
`outcome`/`reason` shape — see the status note; before that pass this record
over-claimed that the repo "needed no change".)

## Decision

Every service's metrics fall into two tiers. Tier 1 is mechanical and the
same for all eight services. Tier 2 is business-specific per bounded
context but follows one shared naming/attribute convention so a human or a
dashboard can find any service's metrics without reading that service's
source first.

### Tier 1 — mandatory baseline, identical across every service

1. **Runtime metrics.** `telemetry.Setup` installs the MeterProvider and
   calls `runtime.Start(runtime.WithMeterProvider(...))`
   (`go.opentelemetry.io/contrib/instrumentation/runtime`). Already the
   fleet's de facto standard; this ADR makes it load-bearing — a service
   without it is non-compliant, not merely incomplete.
2. **HTTP RED metrics.** Every service that serves HTTP wires
   `otelchi.Middleware` for tracing AND
   `otelchimetric.NewServerRequestDuration` for the duration histogram, in
   that order, ahead of the request logger:
   ```go
   r.Use(middleware.RequestID)
   r.Use(otelchi.Middleware(serviceName, otelchi.WithChiRoutes(r)))
   r.Use(otelchimetric.NewServerRequestDuration(otelchimetric.NewBaseConfig(serviceName)))
   r.Use(RequestLogger(logger))
   r.Use(middleware.Recoverer)
   ```
   This is the ONLY sanctioned way to get request rate/error/duration — no
   service hand-rolls its own HTTP counter.
3. **Health endpoint.** `GET /healthz` returning `{"status":"ok"}`. Already
   universal; noted here because Prometheus's own liveness assumptions and
   the Kong route depend on it existing, so it is now part of the same
   contract as the metrics above, not a separate one.

### Tier 2 — business metrics: naming convention, not a fixed metric set

A service picks whichever business events actually matter to it (this ADR
does not mandate what to measure — that stays a domain decision per
bounded context), but every business instrument follows this shape:

- **Instrument name:** `<bounded_context>.<aggregate_or_noun>.<verb_or_state>`,
  all lowercase, dot-separated, no abbreviations
  (`inventory.reservations.created`, not `inv.rsv.create`). Prefer ONE
  counter with an `outcome` attribute over two separately-named counters
  for the success/failure split of the same event — `inventory.reservations`
  with `outcome=created|revoked` is the fleet's best existing example of
  this and is the pattern to copy, not to rename away from.
- **Attribute keys:** dot-separated, matching OTel semantic-convention style
  (`task.type`, not `taskType` or `task_type`). An attribute that identifies
  *why* something happened is always named `reason`; an attribute that
  identifies *what happened to it* is always named `outcome`. Don't invent a
  third word for either concept.
- **Unit:** every counter declares `metric.WithUnit(...)` using UCUM curly-
  brace instrument syntax (`{reservation}`, `{task}`, `{assignment}`) — this
  is what already differs least across the fleet and should stay that way.
- **Description:** every instrument's `metric.WithDescription(...)` states
  what business condition the metric is a proxy for (see
  `inventory-storage`'s `reservationCounterName` comment for the standard
  this ADR expects: not just what the number counts, but what a human
  should conclude when it moves).
- **Placement:** the instrument lives in the SAME adapter tier as
  `telemetry.Setup` (an outbound `telemetry` adapter implementing an
  application-layer port, e.g. `ports.ReservationMetrics`), never called
  directly from `internal/domain`. A use case takes the port as a
  dependency and calls it once per outcome, exactly the shape
  `inventory-storage`'s `ReservationMetrics` already establishes.

This ADR does NOT require renaming metrics that already fit the shape
(`inventory.reservations`, `fulfillment.tasks.claimed`,
`facility.location_slot.registrations`, and this repo's own
`workforce.labor_assignments` are compliant as written). It DOES require:
- `order-management` and `labor-performance` to gain a `telemetry.Setup`
  (Tier 1, item 1) and `otelchi` HTTP instrumentation (Tier 1, item 2), and
  at least one Tier-2 business counter each, following the shape above.
- `labor-performance` (which already has `telemetry.Setup` but no `otelchi`)
  to gain Tier 1, item 2.
- Any NEW business metric added anywhere in the fleet from this point on to
  follow the Tier 2 convention, reviewed the same way the RFC 7807 error
  shape or the Kafka envelope format are reviewed — a metric name is a
  public contract with every dashboard and alert built on it, not
  internal detail a PR reviewer can wave through unchecked.

## Consequences

- A new service added to the fleet has an unambiguous starting checklist:
  copy `telemetry.Setup` from any compliant repo verbatim, wire the three
  `otelchi` middleware lines, and name its first business counter
  `<context>.<noun>.<verb>` with an `outcome`/`reason` attribute pair where
  applicable.
- Grafana can eventually get a second, genuinely cross-service dashboard
  (e.g. "fleet throughput") because instrument names are predictable rather
  than requiring per-service tribal knowledge to query.
- This is additive: no existing instrument is renamed, no existing
  dashboard breaks, and both gap-closing services keep failing OPEN on a
  missing Collector exactly like every other service (`Setup`'s exporters
  are non-blocking; see `inventory-storage`'s `telemetry.go` header comment
  on why a service must not fail to start over absent telemetry).
- The convention is documentation and review discipline, not a CI gate.
  Nothing currently enforces instrument-name shape mechanically (unlike the
  hexagonal dependency rule, which arch-go does enforce) — a future ADR
  could add a lint if drift recurs, but is not scoped here.
- `workforce-management` requires no code change: `telemetry.Setup`, the
  `otelchi` RED middleware chain, and the `workforce.labor_assignments`
  counter (attrs `workforce.assignment.outcome`, `workforce.assignment.reason`,
  `workforce.path.id`) already conform and now serve as one of the fleet's
  reference implementations of this convention.
  _Amended 2026-10: the claim above was wrong on two counts — the counter
  lived on the application layer's global `otel.Meter` (Tier-2 placement
  says behind a port) and its outcome/reason keys carried a service-specific
  prefix. The conformance pass moved the instrument into
  `internal/adapters/outbound/telemetry` behind `ports.LaborMetrics` and
  renamed the keys to `outcome`/`reason`; the instrument name
  `workforce.labor_assignments` (and `workforce.path.id`) is unchanged, so
  dashboards keyed on the series name keep working._
