---
id: 0038-postgres-and-kafka-are-the-only-runtime
slug: /adr/0038-postgres-and-kafka-are-the-only-runtime
title: 0038. Postgres and Kafka are the only runtime
sidebar_label: 0038. Postgres and Kafka only
sidebar_position: 39
description: "ADR 0038: every cmd binary runs on real Postgres and real Kafka. No in-memory repositories, no log publisher, no file catalogue, no permissive consumer mode, no optional relay. Mandatory configuration is a boot error naming the variable; /readyz checks Postgres and a Kafka broker; sensors and the chart keep it true."
---

# 0038. Postgres and Kafka are the only runtime

## Status

Accepted (2026-10-19). Supersedes the in-memory fallback of `cmd/mcp`, the log
publisher, the direct (non-outbox) publisher, the file process-path catalogue,
the `permissive` labor-performance mode and the "relay only with
`EVENT_PUBLISHER=kafka`" rule that
[ADR 0008](./0008-mcp-inbound-adapter.md),
[ADR 0013](./0013-process-path-catalogue-validation.md),
[ADR 0016](./0016-transactional-outbox.md),
[ADR 0019](./0019-labor-performance-cache-consumer.md) and
[ADR 0030](./0030-kafka-sourced-process-path-catalogue.md) allowed. Fleet
decision "everything must use real database and broker"; workforce-management
follows pick-face-replenishment, task-interleaving and product-master
(their ADR 0010). No migration and no contract change: REST, MCP and the
CloudEvents envelope ([ADR 0026](./0026-cloudevents-mandatory-event-envelope.md))
are unchanged; only the readiness of `/readyz` grows.

## Context

The four binaries could start in states that cannot serve the cluster, and
did so silently:

- `cmd/mcp` without `DATABASE_URL` served its tools from its own empty
  in-memory repositories: an `assign_labor` call succeeded and vanished on
  restart, and nothing it wrote was visible over REST.
- `cmd/workforce` defaulted `EVENT_PUBLISHER` to `log`: the outbox relay was
  not even started and events only reached the JSON log, so
  `warehouse.workforce.events` and `warehouse.workforce.analytics` stayed
  empty while `/readyz` said ready. `KAFKA_BROKERS` was optional (the
  publisher fell back to `localhost:9092`).
- `PATH_CATALOGUE_SOURCE` defaulted to `file`, a boot-time YAML that the
  cluster replaced by the Kafka replay ([ADR 0030](./0030-kafka-sourced-process-path-catalogue.md))
  and that nobody kept in sync.
- `LABOR_PERFORMANCE_MODE` defaulted to `permissive`: no measured rate and no
  idle-share signal, silently.
- The projector hard-coded its consumer group.
- `/readyz` was only a drain gate: it said "ready" while Postgres or Kafka was
  unreachable.

The chart and the troubleshooting guide documented these as normal states
("events logged, not sent"). A fallback that works makes a mis-set
environment look like a healthy deployment.

## Decision

1. **One runtime.** `cmd/workforce` and `cmd/mcp` run on real Postgres;
   `cmd/workforce`, `cmd/mcp` and `cmd/workforce-projector` run on real
   Kafka. There is no in-memory mode, no log sink, no file catalogue and no
   optional relay; the outbox relay and both consumers start with the
   service. `cmd/workforce-reports` already read only the analytical Postgres
   and is unchanged.
2. **Missing configuration is a boot error that names the variable**, raised
   before telemetry, any connection and the listener, with every problem
   reported at once:
   `DATABASE_URL is required: this context runs on real Postgres`,
   `KAFKA_BROKERS is required: this context runs on real Kafka`,
   `ANALYTICS_CONSUMER_GROUP is required` (projector, no default in the
   binary). `EVENT_PUBLISHER` stays only because the chart and warehouse-infra
   set it: unset and `kafka` are accepted, `log` and anything else stop the
   boot. `PATH_CATALOGUE_SOURCE` is `kafka` (unset means `kafka`; `file` is
   `PATH_CATALOGUE_SOURCE=file is not supported`), and `LABOR_PERFORMANCE_MODE`
   defaults to `kafka-cache` (`permissive` is
   `LABOR_PERFORMANCE_MODE=permissive is not supported`).
3. **The composition roots build real adapters only.** `cmd/workforce/config.go`
   turns the environment into a validated `bootConfig`; `internal/composition`
   holds the shared validation and the publisher/catalogue builders. The
   `filecatalog` adapter and the direct/log publisher paths are deleted. The
   `memory` adapters stay in `internal/adapters/outbound/memory` as test
   doubles for unit and BDD tests and are never imported by a binary.
4. **`/readyz` tells the truth.** It is `200` only while the drain gate is open
   AND Postgres answers a ping AND a Kafka broker answers. Otherwise `503`
   (`{"status":"not_ready"}` while draining; a `problem+json` of type
   `dependency-unavailable` naming `postgres` and/or `kafka` when a dependency
   fails). The verdict is cached 3 s and one round is bounded to 2 s, so
   probes cannot hammer the database or the broker, and a cancelled caller
   does not poison the cached verdict. `/healthz` stays a pure liveness check.
5. **Readiness checks the broker, not the topics.** The broker auto-creates
   topics on the first write (fleet convention) and this service never creates
   topics through an admin API. A not-ready pod receives no traffic and so
   could never make the first write: requiring the topics would deadlock a
   cold cluster. The check dials a fresh connection per call and asks for the
   broker list with an empty, non-nil topic list; kafka-go's `Client.Metadata`
   is served from a cached layout after the first answer and would not notice
   a broker going away. A missing output topic therefore shows as one
   `outbox relay pass failed ... Unknown Topic Or Partition` ERROR on the
   first publish of a cold broker (the retry succeeds once the broker has
   auto-created it) and a missing input topic as `connect ... kafka consumer`
   retries at boot, never as a crash loop that outlives the topic.
6. **Sensors keep it true.** `TestRuntimeBinaryHasNoInMemoryAdapters`
   (`internal/architecture`, with `TestRuntimeBinarySensorBites`) fails when a
   non-test file under `cmd/**` imports `internal/adapters/outbound/memory`,
   and `TestNoGoFilesAtRepoRoot` keeps the repository root free of Go files:
   the godog runner moved from the root to `features/`.
7. **The deployment mirrors it.** The chart fails the render, naming the value,
   without `database.url`/`database.existingSecret` or `kafka.brokers` (no
   default), with `config.eventPublisher` other than `kafka`, with
   `kafka.enabled=false`, with `pathCatalogue.enabled=true`, with a
   `*_MODE`/`EVENT_PUBLISHER`/`PATH_CATALOGUE_*` entry in `extraEnv` that
   selects a retired mode, with a grace period under 25 s, and with analytics
   enabled but no analytical DSN source or `analytics.projector.consumerGroup`
   (default `workforce-analytics`, the group the projector has always used, so
   its committed offsets survive the rollout). `KAFKA_BROKERS` is always
   rendered. `kafka.enabled` is still accepted (warehouse-infra sets it to
   `true`) but is no longer a switch. The CI `contract` job (Schemathesis)
   boots the real binary on a Postgres and a Kafka service container instead
   of a Postgres and a seeded catalogue file.

## Consequences

- A mis-set environment fails at boot with a message that names the missing
  variable; it no longer degrades into a pod that is up, empty and silent.
- Local `go run ./cmd/workforce` needs a Postgres and a Kafka broker:
  `docker compose up -d --wait` provides both. Fast feedback stays in unit
  and BDD tests over the memory doubles.
- A broker outage takes the api pods out of the Service (readiness `503`)
  where the chart's readiness probe is `/readyz`, not out of the node:
  liveness is unaffected, so nothing restarts. Writes that were accepted stay
  in the transactional outbox and are relayed when the broker returns
  ([ADR 0016](./0016-transactional-outbox.md),
  [ADR 0036](./0036-outbox-relay-attempt-cap.md)).
- The local process-path catalogue of a fresh broker is empty until
  `process-path-management` publishes; path ids are rejected with
  `400 unknown-path-id` until then (the quickstart shows the seed event).
- Tests: `TestRunOnRealPostgresAndKafka`
  (`cmd/workforce/e2e_integration_test.go`, testcontainers) boots the real
  `run()` with nothing but the deployment's environment, drives REST and
  Kafka, asserts the Postgres rows, the CloudEvents envelope on both output
  topics, replay idempotency, an in-process restart (no loss, no duplicate)
  and `/readyz` following the dependencies (green with no output topic, `503`
  naming the dependency when it goes away). The broker check also has unit
  tests over a fake broker (answers, lists no broker, dead address, asks the
  live broker every time), and every boot error is a table test
  (`cmd/workforce/bootconfig_test.go`, `cmd/mcp/bootconfig_test.go`,
  `cmd/workforce-projector/config_test.go`).

## Out of scope

The REST sync edges are unchanged and tracked as a follow-up:
`INSTALLED_CAPACITY_MODE=permissive|http` (fulfillment-execution,
[ADR 0014](./0014-installed-capacity-ceiling.md)) and the
`LABOR_PERFORMANCE_MODE=http` rollback of the labor-performance edge
([ADR 0012](./0012-measured-rate-feed-for-propose-path-plan.md)); neither is a
database or a broker. warehouse-infra's readiness probe still points at
`/healthz`; switching it to `/readyz` is an infra follow-up.
