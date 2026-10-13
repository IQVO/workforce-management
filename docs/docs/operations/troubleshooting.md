---
id: troubleshooting
title: Troubleshooting
sidebar_label: Troubleshooting
description: Symptom, cause, check and fix for the failure modes visible in the workforce-management code.
---

# Troubleshooting

Each row starts from what you see, names the code path that produces it, and
gives the check and the fix. Log messages are quoted exactly as the code
writes them. For variable defaults see [Configuration](./configuration.md);
for metrics see [Observability](./observability.md).

## Boot and probes

| Symptom | Cause | Check | Fix |
|---|---|---|---|
| Pod exits at once and logs `missing required env var` with `key=DATABASE_URL` | `cmd/workforce` calls `requireEnv("DATABASE_URL")`. There is no in-memory mode. | `kubectl logs` of the first container start | Set `database.url` or `database.existingSecret` in the chart (the chart refuses to render without one). |
| Projector or reports exits with `ANALYTICS_DATABASE_URL is required` | Both analytics binaries need it | Analytics Secret keys `ANALYTICS_DATABASE_URL` / `ANALYTICS_READER_DATABASE_URL` | Set `analytics.database.*` or `analytics.database.existingSecret`. |
| Exit with `failed to load the process-path catalogue: …` | `PATH_CATALOGUE_FILE` is missing or malformed. The catalogue is required at boot ([ADR 0013](../adr/0013-process-path-catalogue-validation.md)). | Is the ConfigMap mounted at `pathCatalogue.mountPath`? Is it valid YAML with `paths[].id`? | Provide `pathCatalogue.content` or `existingConfigMap`. Locally, point `PATH_CATALOGUE_FILE` at a real file. |
| Exit with `PATH_CATALOGUE_SOURCE=kafka requires KAFKA_BROKERS to be set` (or the `LABOR_PERFORMANCE_MODE=kafka-cache` equivalent) | The consumers read `KAFKA_BROKERS` raw, with no `localhost:9092` default, unlike the publisher | `env` in the pod | Set `kafka.enabled=true` (templates `KAFKA_BROKERS`) or add it through `extraEnv`. |
| Exit with `process-path catalogue did not become ready within 1m0s` / `labor-performance measured rate cache did not become ready within 1m0s` | The 60 s `WaitReady` replay of the compacted topic did not reach the high-water mark | Topic exists and the broker is reachable from the pod. Look for `retrying` lines from `connect … kafka consumer`. | Fix broker reachability or topic retention. A very large topic may need more than 60 s; that timeout is a code constant. |
| Exit with `unknown EVENT_PUBLISHER "…" (want kafka or log)` | Typo in `EVENT_PUBLISHER` | `config.eventPublisher` | Use `log` or `kafka`. |
| Exit with `invalid float env var` | `MAX_HOURS_PER_SHIFT` or `IDLE_SHARE_TRIM_THRESHOLD` is not a number | `env` | Fix the value. These two crash; duration variables only warn and fall back. |
| Logs show `retrying` … `run migrations` / `open postgres pool`, then `… (after 5 attempts): …` and exit | Postgres unreachable for longer than the boot-retry budget: 5 attempts, 1+2+4+8 = **15 s** of waits ([ADR 0032](../adr/0032-bootretry-first-outbound-dial.md)). The source comment's "~31s" is wrong. | Last error in the log (DNS, auth, `connection refused`) | Fix the DSN or the network. One retry around ~10 s is normal under Istio (first-dial reset) and is not a fault. |
| `CrashLoopBackOff` with **exit code 143** during boot | The kubelet killed a pod that was still booting: the startup probe (30 × 2 s = 60 s) ran out | `kubectl describe pod` → `Startup probe failed`. Look at the boot log timeline. | Find the slow step (migrations, Kafka replay). For a legitimately slow boot, raise `startupProbe.failureThreshold`. |
| Migrations crash-loop on several replicas with `pq: unnamed prepared statement does not exist` or `canceling statement due to statement timeout` | golang-migrate's `pg_advisory_lock` running through PgBouncer transaction pooling ([ADR 0025](../adr/0025-migrations-direct-postgres-connection.md)) | Does the Secret have `MIGRATIONS_DATABASE_URL`? It is `optional: true`, so a missing key silently falls back to `DATABASE_URL`. | Provision a direct, non-pooled `MIGRATIONS_DATABASE_URL`. |
| Migration fails with `Dirty database version N` | A previous migration failed halfway | `SELECT * FROM schema_migrations;` | Repair by hand: apply or revert the partial change, then `migrate force <N-1>` against the direct DSN. |
| `/readyz` returns 503 | It only does so **after SIGTERM**. Readiness never waits on a dependency. | Look for `readiness flipped to not-ready` in the log | Expected during rollout or shutdown. If the pod is not terminating, it is a bug: report it. |
| `/readyz` never answers, but the process is running | The listener has not started: the boot is still in the catalogue or cache replay, or in migrations | The last boot log line | See the rows above. |

## Requests

| Symptom | Cause | Check | Fix |
|---|---|---|---|
| **Every** `POST /shift-plans` returns `503 installed-capacity-unavailable` | `INSTALLED_CAPACITY_MODE` is `permissive` (the binary default; the Helm chart defaults to `http` since gap fix D24). It fails loud on purpose ([ADR 0014](../adr/0014-installed-capacity-ceiling.md)). | WARN at boot: `fulfillment-execution installed capacity client configured` with `mode=permissive` | Chart: `config.installedCapacityMode=http` (the default) and, when fulfillment-execution is not `fulfillment-execution` in the same namespace, `config.fulfillmentExecutionBaseUrl`. Elsewhere: `INSTALLED_CAPACITY_MODE=http`, `FULFILLMENT_EXECUTION_BASE_URL=…`. |
| Shift-plan commits return 503 after working before | fulfillment-execution is down, or the breaker is open | `circuit_breaker_state{dependency="fulfillment-execution"} == 2` | Restore fulfillment-execution. The breaker half-opens on its own. There is deliberately no retry on this call. |
| `409 exceeds-installed-capacity` / `planned-heads-exceed-installed` / `planned-hours-exceed-capacity` | A domain invariant of `CommitShiftPlan` | `detail` in the problem body | Lower the planned heads or hours, or register stations in fulfillment-execution. |
| `400 unknown-path-id` | The path id is not in the loaded catalogue | Boot log `process-path catalogue loaded` lists the ids | Use a catalogue id, or roll the pod after you change the catalogue. |
| `409 certification-required`, `associate-on-break`, `associate-shift-ended`, `max-hours-exceeded` | `AssignLabor` invariants | Also visible as `workforce_labor_assignments_total{outcome="rejected", reason=…}` | Business outcome, not a fault. Certify the associate, end the break, and so on. |
| `409 associate-already-on-break` / `associate-not-on-break` | Break state machine | Current associate state | Call `break/end` or `break/start` as appropriate. |
| `409 concurrent-modification` | Optimistic-concurrency `version` check lost a race ([ADR 0021](../adr/0021-optimistic-concurrency-version-column.md)) | Several writers on the same associate or plan | Reload and retry. |
| `400 idempotency-key-required` | `POST /shift-plans` or `POST /associates/{id}/assignments` sent without `Idempotency-Key` while Postgres is wired ([ADR 0027](../adr/0027-idempotency-key-middleware.md)) | Request headers | Send a unique `Idempotency-Key` per logical request. |
| `422 idempotency-key-reused` | Same key, different body | Client key generation | Use a new key for a different request. A same-key, same-body retry replays the stored response. |
| A retry after a day does something new instead of replaying | The key's row was swept after `IDEMPOTENCY_KEY_TTL` (24 h) | `config.idempotencyKeyTtl` | Expected: the TTL is the replay window. |
| `400 missing-building-id` / `422 conflicting-site-and-building` on staffing-gap | `siteCode` missing, or `siteCode` and the deprecated `buildingId` disagree ([ADR 0035](../adr/0035-sitecode-converges-building-id.md)) | Query string | Send `siteCode` only. The slug keeps its legacy name on purpose. |
| Browser call blocked by CORS | Origin not in `CORS_ALLOWED_ORIGINS` (default `http://localhost:5173,http://localhost:5185`) | Browser devtools preflight | Add the origin. Behind Kong on the same origin, CORS does not apply. |
| `ProposePathPlan` never uses a measured rate | `LABOR_PERFORMANCE_MODE=permissive` (default, fail-open), or the breaker is open in `http` mode | Boot log `labor-performance measured rate client configured` with its `mode`; `circuit_breaker_state{dependency="labor-performance"}` | Set `http` plus the base URL, or `kafka-cache`. |
| Staffing gap or proposal ignores idleness | The idle-share signal only exists in `kafka-cache` mode | Same boot log | `LABOR_PERFORMANCE_MODE=kafka-cache` ([ADR 0020](../adr/0020-idle-share-staffing-signal.md)). |

## Events and analytics

| Symptom | Cause | Check | Fix |
|---|---|---|---|
| No events on `warehouse.workforce.events` | `EVENT_PUBLISHER=log` (the default): events only go to the log | `config.eventPublisher` | Set `kafka` and `kafka.enabled=true`. |
| `workforce_outbox_lag_seconds` keeps growing | The relay is failing on the head row and retries it every `OUTBOX_RELAY_INTERVAL` (ordering), or a row reached `OUTBOX_MAX_ATTEMPTS` (default 20) and is parked: later rows publish, but the parked row keeps the gauge growing ([ADR 0036](../adr/0036-outbox-relay-attempt-cap.md)). | `outbox relay pass failed` logs; `outbox relay: row reached the attempt cap and is no longer claimed` (ERROR, with `outbox_id`, `topic`, `event_type`, `key`). `SELECT id, event_type, key, attempts, last_error FROM outbox_events WHERE published_at IS NULL ORDER BY id LIMIT 5;` | Fix the broker or topic. Below the cap the relay resumes on its own. A parked row needs `UPDATE outbox_events SET attempts = 0 WHERE id = <id>;`. Do not delete it unless you accept losing that event. |
| Events written by `cmd/mcp` never reach Kafka | `cmd/mcp` only inserts into `outbox_events`; only `cmd/workforce` relays | Is `cmd/workforce` running with `EVENT_PUBLISHER=kafka` on the same database? | Run the API with the kafka publisher. |
| `outbox_events` table keeps growing | Sweeper disabled (`HOUSEKEEPING_INTERVAL=0`), or the rows are **unpublished** (never swept) | `housekeeping sweep` logs; count of unpublished rows | Re-enable the sweeper, or fix the relay first. |
| `warehouse.workforce.analytics.dlq` is growing | The projector cannot decode a message (not a CloudEvent), or keeps failing to apply it after 3 attempts | `analytics: exhausted retries, sending to dead-letter topic` with `error`. DLQ headers carry the source topic and the error. | Fix the producer or the projection, then re-produce the payload onto the source topic ([Runbook](./runbook.md#routine-procedures)). |
| Labor report stale; `GET /reports/labor/freshness` lag rising; projector pod restarting | Projector consumer stopped. A commit or DLQ write error ends `Run`; since gap fix D13 the projector then exits non-zero (it no longer keeps `/healthz` at 200 over a dead consumer), so the pod restarts and may crash-loop while the cause persists. | `analytics consumer stopped; exiting so the pod restarts` in the projector log (`kubectl logs --previous`); pod restart count; lag of group `workforce-analytics` | Fix the broker error. The restarted projector resumes from the committed offset. |
| `GET /reports/labor/freshness` returns `0` but the report is empty | `lagSeconds` is `0` both when caught up and when `analytics_processed_events` is empty | `SELECT count(*) FROM analytics_processed_events;` | Check that the API publishes with `EVENT_PUBLISHER=kafka` and that the projector consumes. |
| `GET /reports/labor` returns `400 invalid-report-query` | `from`/`to` missing or not RFC 3339, or `granularity` is not `hour` | Query string | `?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z`. |
| MCP has no `get_workforce_labor_report` tool | `REPORTS_BASE_URL` unset | `reports data-product tool enabled` absent from the MCP boot log | Set `mcp.reportsBaseUrl`, or enable `analytics` so the chart defaults it. |
| MCP client gets session errors with 2+ MCP replicas | Per-process `Mcp-Session-Id` state and no session affinity | `mcp.replicaCount` | Keep 1 replica (see [Runbook](./runbook.md#scaling)). |

## Local development

| Symptom | Cause | Fix |
|---|---|---|
| Logs fill with OTLP `connection refused` | No collector at `localhost:4317` | Harmless. Run a collector, or point `OTEL_EXPORTER_OTLP_ENDPOINT` elsewhere. |
| State created through MCP is not visible over REST | `cmd/mcp` without `DATABASE_URL` uses its own in-memory repositories | Give both binaries the same `DATABASE_URL`. |
