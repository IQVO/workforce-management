---
id: 0024-horizontal-autoscaling-and-pgxpool-tuning
slug: /adr/0024-horizontal-autoscaling-and-pgxpool-tuning
title: 0024. Per-workload HorizontalPodAutoscaler and pgxpool MaxConns/statement_timeout tuning
sidebar_label: 24. HPA + pgxpool tuning
sidebar_position: 25
description: "ADR 0024 — Phase 3 (scalability) for workforce-management, ported from order-management's ADR-0026 (PR #110): an autoscaling/v2 HorizontalPodAutoscaler per independently-assessed workload (api max 4, analytics-projector max 2, analytics-reports max 3, frontend max 3; mcp explicitly excluded for a real in-memory-session reason), all default-disabled via values.yaml so this PR changes nothing on merge; plus explicit pgxpool.Config MaxConns caps and per-pool statement_timeout values, sized against the shared Postgres instance's real max_connections=100 ceiling, with PgBouncer (warehouse-infra PR #43) already in front of every service's OLTP DATABASE_URL."
---

# 24. Per-workload HorizontalPodAutoscaler and pgxpool MaxConns/statement_timeout tuning

## Status

Accepted — implemented in the same change that introduces this record.
This is Phase 3 (scalability) of the fleet production-readiness plan,
ported from order-management's ADR-0026 (PR #110), the reference the
other fleet repos in this phase copy — the same role order-management's
ADR-0025 played for Phase 2's resilience wave in this repo's own
ADR-0022. Do not redesign this pattern per service; mirror the reference
and document only real deviations (this repo has none of substance: same
workload shape — api / analytics-projector / analytics-reports / frontend
/ mcp — same shared-Postgres-instance topology, same PgBouncer front end).

## Context

Before this change, workforce-management's Helm chart had exactly one
`HorizontalPodAutoscaler` template, unconditionally targeting the `api`
Deployment only, wired to a flat `autoscaling.enabled/minReplicas/
maxReplicas/targetCPUUtilizationPercentage` block (`80%` target, max 5) —
untested against the other four Deployments this chart renders (`mcp`,
`frontend`, `analytics-projector`, `analytics-reports`), and never
assessed for whether scaling `analytics-projector` past 1 replica was
even safe given its Kafka consumer-group membership. `replicaCount` was
hardcoded to 1 with no per-workload HPA anywhere in the chart.

Separately, no pool in the codebase set an explicit `pgxpool.Config.
MaxConns`, so every pool — the OLTP pool (`internal/adapters/outbound/
postgres/pool.go`, used by `cmd/workforce` and `cmd/mcp`) and the two
analytics pools (`internal/adapters/outbound/analyticsstore/pool.go`'s
`NewPool` used by `cmd/workforce-projector`, and `NewReadOnlyPool` used
by `cmd/workforce-reports`) — ran on pgx's library default, `max(4,
runtime.NumCPU())` connections per process. No pool set a
`statement_timeout` either, so a single runaway query (a lock wait, an
unbounded `report.ReportStore` date range) could hold a pooled connection
indefinitely, with nothing to cancel it.

This matters together, not separately: turning on HPA for a
Postgres-backed workload without an explicit, bounded `MaxConns` means
the service's real connection ceiling becomes "however many CPUs the
node happens to have, times however many replicas the HPA happens to
have scaled to" — an unbounded, indirect function of cluster
autoscaling and CPU load, not a number anyone chose. This PR does both
together on purpose, exactly as order-management's reference PR did.

### Finding: shared Postgres, PgBouncer already in front, and the real max_connections

Verified fleet-wide (not re-derived per service — this is one shared
instance, not one per repo): **all 10 backend services share ONE
Postgres server instance**, each with its own logical database/role, on
`max_connections=100` — the unmodified Bitnami chart default, never
deliberately sized for the fleet's real connection demand
(`warehouse-infra/terraform/postgres.tf` has zero explicit
`max_connections` override).

**PgBouncer (warehouse-infra PR #43, merged) now sits in front of that
shared instance in transaction-pooling mode.** Every service's OLTP
`DATABASE_URL` Secret — including this repo's — is **already re-pointed**
at PgBouncer; no code change was needed on this side, since `pgxpool`
just points at a different `host:port` behind the same DSN shape.
Analytics DSNs (`cmd/workforce-projector`, `cmd/workforce-reports`) stay
pointed **directly** at Postgres, mirroring PgBouncer PR #43's own
reasoning: low QPS, a single Kafka-consumer connection each (projector)
or a small HPA-scaled fan-out (reports) — no pooling benefit, and no risk
of PgBouncer's transaction-pooling mode breaking a session-level feature
these pools don't use anyway.

Because PgBouncer now absorbs the real server-side connection
multiplexing, `pgxpool.MaxConns` on the client side is sized generously
per service — matching order-management's numbers rather than
artificially shrinking them — since the number that actually bounds
Postgres's own `max_connections` exposure is PgBouncer's own `pool_size`
configuration at the infra layer, not the sum of every service's
client-side `MaxConns`. This ADR still documents the fleet-wide
worst-case client-side arithmetic below for honesty, but PgBouncer is
what makes those numbers safe in aggregate, not the individual `MaxConns`
values alone.

## Decision

### 1. HorizontalPodAutoscaler — one per independently-assessed workload

Assessed each of the five Deployments this chart renders on its own
merits, mirroring order-management's per-workload table rather than
blanket-enabling HPA everywhere:

| Deployment | HPA? | min | max | target CPU | Why |
|---|---|---|---|---|---|
| `api` (`cmd/workforce`) | Yes | 1 | 4 | 70% | Stateless OLTP HTTP. Its two per-process caches — `kafkacatalog`'s process-path catalogue consumer (`PATH_CATALOGUE_SOURCE=kafka`) and `laborperformancecache`'s measured-rate/idle-share cache (`LABOR_PERFORMANCE_MODE=kafka-cache`) — both build their consumer group id via `uniqueConsumerGroup()` (hostname+PID+timestamp, see `internal/adapters/outbound/kafkacatalog/consumer.go` and `internal/adapters/outbound/laborperformancecache/consumer.go`), a genuinely **per-process-unique** group — safe to run N independent copies of by design, each replaying its own full copy from `FirstOffset`. Unlike order-management, this service has **no** inbound OLTP-mutating Kafka consumer analogous to `RepromiseConsumer`, so there is no shared-group case to verify on this Deployment at all — every Kafka-touching adapter on the `api` path is already per-process-unique. Nothing here breaks at N>1. |
| `analytics-projector` (`cmd/workforce-projector`) | Yes, capped at **2**, not `api`'s 4 | 1 | 2 | 70% | Verified BEFORE enabling: its analytics Kafka consumer group, `kafka.AnalyticsConsumerGroup = "workforce-analytics"` (`internal/adapters/inbound/kafka/analytics_consumer.go`), is a **stable, shared** group with no per-instance uniqueness — the fleet's normal horizontally-scalable pattern (N replicas share partitions via ordinary Kafka group rebalancing), the same safe shape as order-management's projector. The publisher partitions by aggregate id (`AssociateId`/`PathId`, `kafka.AnalyticsPublisher.Encode` in `internal/adapters/outbound/kafka/analytics_publisher.go`), so 2 replicas legitimately share the topic's partitions. Capped at 2 rather than left at 4 for the same two reasons as the reference PR, not correctness: (a) aggregate-id-keyed partitioning means ordering is only guaranteed per-aggregate, so a wide fan-out buys little for what is a lightweight idempotent-upsert workload; (b) every write is already idempotent on `event_id` via `ConsumedEventsRepo.MarkProcessed` (ADR-0010), so correctness does not regress at 2, but there is no throughput case yet that justifies more. |
| `analytics-reports` (`cmd/workforce-reports`) | Yes | 1 | 3 | 70% | Stateless read-only REST reader over its own read-only pgxpool (`analyticsstore.NewReadOnlyPool`) — no in-memory state, no Kafka consumption. Same treatment as `api`. |
| `frontend` (nginx-unprivileged serving the built SPA bundle) | Yes | 1 | 3 | 70% | Pure static-asset serving. No server-side session, no per-request state whatsoever. The most trivially horizontally-scalable workload in this chart. |
| `mcp` (`cmd/mcp`) | **No — deliberately excluded, not just disabled** | — | — | — | Verified directly against the code (not assumed from order-management's finding): `internal/adapters/inbound/mcp/server.go` wraps `github.com/modelcontextprotocol/go-sdk/mcp`'s `NewStreamableHTTPHandler`, which keeps **per-process, in-memory session state** keyed by the MCP protocol's own `Mcp-Session-Id` header (a real multi-request session, not just a TCP/HTTP connection) — same SDK version (`v1.8.0`) and same wiring shape as order-management's `cmd/mcp`. `charts/workforce-management/templates/mcp-service.yaml` is a plain `ClusterIP` Service with no `sessionAffinity` configured, so under >1 replica a second request carrying the same `Mcp-Session-Id` (e.g. a `tools/call` following an earlier `initialize`) could land on a different pod than the one that created the session, which has never heard of it and would reject or silently start a new one. Fixing it for real needs either `sessionAffinity: ClientIP` (a partial mitigation only) or wiring the SDK's `StreamableHTTPOptions.EventStore` to a shared/external session store — a real code change, out of scope for this chart-and-pool-tuning PR. `mcp.replicaCount` stays a plain, manually-set value; no `autoscaling.mcp` block exists in `values.yaml` at all. Revisit if/when `cmd/mcp` adopts an external session store or the SDK's stateless mode. |

Every enabled block is namespaced under a single top-level `autoscaling:`
key in `values.yaml`
(`autoscaling.<api|projector|reports|frontend>.{enabled,minReplicas,
maxReplicas,targetCPUUtilizationPercentage}`), **every `enabled` value
defaults to `false`**. This replaces the previous flat
`autoscaling.enabled/minReplicas/maxReplicas/targetCPUUtilizationPercentage`
shape (which only ever targeted `api`, at max 5 / 80% CPU — those old
values are superseded by the per-workload `autoscaling.api` block at max
4 / 70%, matching the reference PR's numbers rather than this repo's
previous ad hoc ones). This PR makes per-workload HPA possible and
verified-correct; it deliberately does not turn any of it on — the fleet
enables each workload's HPA later, once, as a conscious rollout decision.

**No replicas-vs-HPA fight.** Each Deployment template guards its
`spec.replicas` field with `{{- if not .Values.autoscaling.<x>.enabled
}}` — when a workload's HPA is enabled, its Deployment renders with NO
`replicas` field at all. Verified directly with `helm template`:

- Default values → 0 `HorizontalPodAutoscaler` resources render, every
  Deployment keeps its static `replicas:` field.
- All four `autoscaling.*.enabled=true` (with `analytics.enabled=true`,
  `frontend.enabled=true`, `mcp.enabled=true` so every Deployment
  renders) → exactly 4 `HorizontalPodAutoscaler` resources render (one
  per scalable workload, `mcp` has none by design), and none of those
  four Deployments has a `replicas:` field — `mcp`'s Deployment still
  does.
- Only `autoscaling.api.enabled=true` → exactly 1 HPA renders, only the
  `api` Deployment loses its `replicas:` field; `projector`/`reports`/
  `frontend` keep theirs untouched. Mixed enablement is safe and
  independent per workload, as designed.

`helm lint` passes; the existing chart selector-conformance test
(`charts/workforce-management/tests/test_service_selectors.py`) still
passes unchanged — this PR does not touch any Service selector, only
Deployment `replicas:` fields and a new `HorizontalPodAutoscaler`
template. `go vet`/`gofmt`/`make check`/`make arch-test` all pass
unchanged too (chart changes affect no Go code path directly; the pgxpool
change below does).

### 2. pgxpool MaxConns

All three pools now set an explicit `pgxpool.Config.MaxConns` instead of
inheriting the CPU-derived library default (`max(4, runtime.NumCPU())`),
matching order-management's reference numbers exactly (same workload
shape, same shared instance, same PgBouncer front end for OLTP):

| Pool | Used by | `MaxConns` | Reasoning |
|---|---|---|---|
| OLTP (`postgres.NewPool`) | `cmd/workforce` (`api`), `cmd/mcp` (`mcp`) | **10** | `api`'s proposed HPA ceiling of 4 replicas × 10 = 40 connections. Sits behind PgBouncer (transaction-pooling), so this is the client-side ceiling PgBouncer multiplexes down to a much smaller real server-side count — not a direct draw on Postgres's `max_connections=100`. Matches order-management's OLTP value; no deviation. |
| Analytics writer (`analyticsstore.NewPool`) | `cmd/workforce-projector` | **5** | The projector's proposed HPA caps at 2 replicas (not 4 — see the table above) and does single-row `ON CONFLICT` upserts against one row at a time; a small, flat pool is enough. Analytics DSN stays direct to Postgres (no PgBouncer), matching PgBouncer PR #43's own low-QPS/single-consumer reasoning. Matches order-management's value. |
| Analytics reader (`analyticsstore.NewReadOnlyPool`) | `cmd/workforce-reports` | **5** (`ReportsMaxConns`) | `reports` IS HPA-scalable (max 3); at that ceiling, 3 × 5 = 15 connections against the analytical database, direct (no PgBouncer). Matches order-management's value. |

Worst case across every workload simultaneously at its proposed HPA
maximum, **direct-to-Postgres connections only** (the OLTP pool's 40 goes
through PgBouncer first, so it is not a direct draw on the shared
instance's 100-connection ceiling the way the analytics pools are):
`projector` fixed at 1 × 5 = 5 today (no HPA enabled by default), or up
to 2 × 5 = 10 at its proposed HPA ceiling; `reports` up to 3 × 5 = 15 at
its ceiling. That is at most **25** direct connections from this ONE
service against the shared instance's 100, even at every proposed
analytics HPA ceiling simultaneously — comfortably inside the ceiling on
its own. The OLTP path's 40 (at `api`'s proposed max of 4 replicas × 10)
is real client-side pool capacity, but is absorbed by PgBouncer's own
`pool_size`, sized at the infra layer independently of any one service's
`MaxConns` — a fleet-wide 10-service `40 (api) + up-to-25 (analytics)`
worst case has NOT been recomputed against `max_connections=100` here,
because PgBouncer is precisely the mechanism that makes that recompute
unnecessary for the OLTP share; it remains a genuinely open, fleet-wide
question for the DIRECT-to-Postgres analytics pools alone once more of
the other 9 services' own Phase 3 PRs land (each contributing an
`analytics-projector`/`analytics-reports` share of the same 100-connection
ceiling) — flagged here as an honest residual, not claimed solved.

### 3. statement_timeout

All three pools set `statement_timeout` via `pgxpool.Config.
AfterConnect`, running `SET statement_timeout = '<value>'` on every new
physical connection as it's established (survives connection reuse
across pooled acquisitions). Values match order-management's reference
exactly — same query shapes, same reasoning:

| Pool | `statement_timeout` | Reasoning |
|---|---|---|
| OLTP (`postgres.StatementTimeout`) | **5s** | Every OLTP query (`ShiftPlan`/`AssociateShift`/`LaborAssignment` reads/writes) is a single-aggregate operation keyed by id, normally low-single-digit milliseconds. 5s is roughly 1000x that — generous headroom for real transient contention without ever being a normal-path concern, while bounding the absolute worst case tightly since this is the interactive, latency-sensitive path and (behind PgBouncer) the pool with the most client-side connections to protect. |
| Analytics writer (`analyticsstore.StatementTimeout`) | **10s** | A Kafka consumer replaying a backlog after a redeploy issues upserts in a tight loop; a transient lock wait here doesn't need to be as tight as an interactive OLTP request. Still bounded — an unbounded query here could stall the whole analytics pipeline if the projector isn't scaled out. |
| Analytics reader (`analyticsstore.ReportsStatementTimeout`) | **15s** | The Labor Utilization & Staffing report aggregates rows across a caller-chosen date range — wider than the OLTP side's always-single-aggregate-by-id shape — so it gets more headroom, but still a hard ceiling. |

Verified with a real Postgres via testcontainers
(`internal/adapters/outbound/postgres/pool_limits_integration_test.go`,
`-tags=integration`), ported directly from order-management's own test of
the same name:

- `TestNewPool_AppliesStatementTimeoutToNewConnections` — opens a pool
  against a real `postgres:16-alpine` container with a short test-only
  timeout (200ms, via the shared `NewPoolWithLimits` the production
  `NewPool` wraps), confirms `SHOW statement_timeout` reads back `200ms`
  on a freshly acquired connection, then runs `SELECT pg_sleep(2)` and
  asserts Postgres itself cancels it (SQLSTATE `57014`, "canceling
  statement due to statement timeout") rather than letting it run the
  full 2s, and finally confirms the pool is still usable afterward.
- `TestNewPool_AppliesMaxConns` — acquires exactly `maxConns` connections
  from a pool configured with `MaxConns=2`, then asserts a further
  `Acquire` blocks until `context.DeadlineExceeded`, proving `MaxConns`
  is the pool's real, enforced ceiling rather than advisory.

Both tests pass locally against a real Postgres container.

## Consequences

- HPA is now possible, correct, and independently verified per workload
  for four of this chart's five Deployments — but **off by default
  everywhere**. Merging this PR changes nothing about production replica
  counts; `MaxConns`/`statement_timeout` are the only behavior change
  that takes effect on deploy, and both are conservative relative to
  today's unbounded defaults (they can only reduce, never increase,
  worst-case connection usage and hung-query duration).
- `mcp` remains explicitly un-autoscaled, with the exact reason
  (in-memory session state, no sticky routing, verified against this
  repo's own `internal/adapters/inbound/mcp/server.go`) recorded here and
  in `values.yaml`'s comments, so a future contributor doesn't
  mechanically copy `api`'s HPA block onto it without re-solving the
  session-affinity problem first.
- Unlike order-management's own ADR-0026 (written before PgBouncer
  existed), this ADR does not need to treat the OLTP path's worst-case
  connection count as a fleet-wide risk requiring PgBouncer as a future
  remedy — PgBouncer is **already deployed** and this service's
  `DATABASE_URL` Secret already points at it. The residual open question
  here is narrower: the DIRECT-to-Postgres analytics pools' fleet-wide
  worst-case sum, which PgBouncer deliberately does not cover (per PR
  #43's own low-QPS reasoning) and which this ADR does not claim to have
  closed.
- A read replica for the analytics/reports read path is explicitly OUT
  OF SCOPE for this PR — not evaluated, not designed, not decided.
  Revisit only once actually needed and confirmed by the person
  requesting it.
- `max_connections=100` itself remains an unexamined Bitnami chart
  default. This ADR treats it as a hard external constraint to work
  within, not something in scope to change.

## References

- order-management PR #110 / ADR-0026 — the reference design this PR
  ports verbatim (per-workload HPA table shape, pgxpool MaxConns/
  statement_timeout numbers and testcontainers proof pattern).
- warehouse-infra PR #43 — PgBouncer deployment in front of the shared
  Postgres instance (transaction-pooling mode, OLTP-only re-pointing,
  analytics DSNs left direct); the reason this ADR's connection-budget
  section differs from order-management's (written pre-PgBouncer).
- This repo's own ADR-0022 (Phase 2 resilience) — the previous fleet
  production-readiness phase, whose "port the reference PR verbatim,
  document only real deviations" discipline this ADR follows again.
