---
id: quickstart
title: Run it locally
sidebar_label: Quickstart
sidebar_position: 2
description: Bring up Postgres, run the service, and walk one shift end to end with curl.
---

# Run it locally

The OLTP service is `cmd/workforce`. It needs Postgres **and** Kafka: there is
no in-memory mode and no log sink, and it refuses to boot without
`DATABASE_URL` and `KAFKA_BROKERS`
([ADR 0038](../adr/0038-postgres-and-kafka-are-the-only-runtime.md)).

## 1. Start Postgres, Kafka and the service

`docker compose up -d --wait` starts a Postgres 16 and a single-node Kafka
(KRaft) that the values below point at. The process-path catalogue is not a
file any more: the service replays `process-path-management`'s topic
`warehouse.process-path-management.events` before it accepts traffic
([ADR 0013](../adr/0013-process-path-catalogue-validation.md),
[ADR 0030](../adr/0030-kafka-sourced-process-path-catalogue.md)). On a fresh
local broker that topic is empty, so every path id is rejected with
`400 unknown-path-id` until a `ProcessPathCreated` event exists. Publish one
(the same event `process-path-management` emits) after the service is up:

```bash
docker compose exec -T kafka sh -c "echo '{\"specversion\":\"1.0\",\"id\":\"seed-pack\",\"source\":\"/warehouse/process-path-management\",\"type\":\"com.warehouse.wes.process-path-management.processpath.ProcessPathCreated\",\"subject\":\"PACK\",\"time\":\"2026-10-19T00:00:00Z\",\"datacontenttype\":\"application/json\",\"data\":{\"path_id\":\"PACK\",\"match_prefix\":\"pack\",\"direct\":true,\"required_capabilities\":[\"pack\"]}}' | kafka-console-producer --bootstrap-server localhost:29092 --topic warehouse.process-path-management.events"
```

(Or point `KAFKA_BROKERS` at the kind cluster's broker, where the real
catalogue is already published.)

`CommitShiftPlan` also checks every line against the live installed capacity
in `fulfillment-execution`
([ADR 0014](../adr/0014-installed-capacity-ceiling.md)). In the default
`permissive` mode every commit fails with `503 installed-capacity-unavailable`.
To commit a plan, point the service at a `fulfillment-execution` that has
stations registered for the path:

```bash
docker compose up -d --wait   # Postgres 16 on :5432, Kafka on :9092

export DATABASE_URL="postgres://workforce:workforce@localhost:5432/workforce?sslmode=disable"
export KAFKA_BROKERS=localhost:9092
export INSTALLED_CAPACITY_MODE=http
export FULFILLMENT_EXECUTION_BASE_URL=http://localhost:8000/api/fulfillment-execution   # e.g. via the kind cluster's Kong
go run ./cmd/workforce   # applies migrations, then serves on :8080
```

## 2. Configuration

Everything is environment-driven — there is no config file.

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `DATABASE_URL` | **yes** | — | Postgres connection string; `DATABASE_URL is required: this context runs on real Postgres` without it |
| `KAFKA_BROKERS` | **yes** | — | comma-separated broker list; `KAFKA_BROKERS is required: this context runs on real Kafka` without it |
| `HTTP_ADDR` | no | `:8080` | listen address |
| `MIGRATIONS_PATH` | no | `migrations` | path to the golang-migrate SQL files |
| `MAX_HOURS_PER_SHIFT` | no | `8` | the configured max-hours-per-shift cap |
| `PATH_CATALOGUE_SOURCE` | no | `kafka` | the only source: `process-path-management`'s topic is replayed. `file` is a boot error |
| `INSTALLED_CAPACITY_MODE` | no | `permissive` | `http` or `permissive`. **Permissive rejects every `CommitShiftPlan` with `503`** (a REST sync edge, unchanged by ADR 0038) |
| `FULFILLMENT_EXECUTION_BASE_URL` | with `http` capacity mode | — | `fulfillment-execution` base URL |
| `LABOR_PERFORMANCE_MODE` | no | `kafka-cache` | `kafka-cache` (default) or `http` (REST rollback) — measured-rate / idle-share feed for `ProposePathPlan` and `GetStaffingGap`. `permissive` is a boot error |
| `EVENT_PUBLISHER` | no | `kafka` | kept for the chart: unset or `kafka`; `log` is a boot error — see [Integration](../ecosystem/integration.md) |

[Configuration](../operations/configuration.md) lists every remaining knob
per binary (`LABOR_PERFORMANCE_BASE_URL`, `IDLE_SHARE_TRIM_THRESHOLD`,
`OUTBOX_RELAY_INTERVAL`, `MIGRATIONS_DATABASE_URL`, `HOUSEKEEPING_INTERVAL`,
`IDEMPOTENCY_KEY_TTL`, `OUTBOX_RETENTION`, `SHUTDOWN_DRAIN_DELAY`,
`CORS_ALLOWED_ORIGINS`, the OTel variables, and the `cmd/mcp`,
`cmd/workforce-projector` and `cmd/workforce-reports` settings).

## 3. Walk one shift end to end

```bash
# 1. Someone clocks on, certified to pack
curl -X POST localhost:8080/associates/assoc-1/shift-starts \
  -d '{"certifications":["pack"],"siteCode":"WH1"}'

# 2. Give them a second qualification
curl -X POST localhost:8080/associates/assoc-1/certifications \
  -d '{"certification":"hazmat"}'

# 3. The software proposes: 100 units of charge at 30 units/hour needs 4 heads
curl -X POST localhost:8080/paths/pack/plan-proposals \
  -d '{"siteCode":"WH1","charge":100,"plannedRate":30}'

# 4. A human commits the split (this is the ShiftPlan). Needs
#    INSTALLED_CAPACITY_MODE=http and reported capacity >= 3 for pack;
#    otherwise 503 (capacity unavailable) or 409 (exceeds-installed-capacity).
#    The two creation POSTs require an Idempotency-Key header (ADR 0027):
#    without one the service answers 400 idempotency-key-required.
curl -X POST localhost:8080/shift-plans \
  -H 'Idempotency-Key: 6f1d2c3a-commit-wh1-shift-1' \
  -d '{"siteCode":"WH1","shiftId":"shift-1","lines":[
        {"pathId":"pack","plannedHeads":3,"plannedRate":30,"plannedHours":24,"installedStations":10}
      ]}'

# 5. Put the associate on the pack path
curl -X POST localhost:8080/associates/assoc-1/assignments \
  -H 'Idempotency-Key: 0b7e9a41-assign-assoc-1-pack' \
  -d '{"pathId":"pack"}'

# 6. Breaks gate assignment while they are open
curl -X POST localhost:8080/associates/assoc-1/break-starts
curl -X POST localhost:8080/associates/assoc-1/break-ends

# 7. Is the pack path short of its committed heads?
curl "localhost:8080/paths/pack/staffing-gap?siteCode=WH1&shiftId=shift-1"
# (siteCode is the canonical name of the plan key and also scopes the count to
# associates started at that site; the deprecated buildingId alias finds the same
# plan but is not scoped — ADR 0035)

# 8. Clock off — closes any active assignment first
curl -X POST localhost:8080/associates/assoc-1/shift-ends

curl localhost:8080/healthz   # liveness
curl localhost:8080/readyz    # readiness: 200 only while Postgres AND a Kafka broker answer; 503 once graceful shutdown starts
```

Step 7 returns the staffing-gap read model — planned versus active, and the
`understaffed` flag:

```json
{"pathId":"pack","plannedHeads":3,"activeHeads":1,"understaffed":true}
```

With `LABOR_PERFORMANCE_MODE=kafka-cache` (the default) the response can also carry
`observedIdleShare` (a fraction in [0, 1]; the deprecated `observedIdlePct`
carries the same value): the path's recently observed idle share, taken from
`labor-performance`. It is omitted when there is no signal
([ADR 0020](../adr/0020-idle-share-staffing-signal.md),
[ADR 0033](../adr/0033-docs-audit-corrections-2026-10.md)).

That flag is the whole point of this context's intra-shift half: it says the
gap exists. It does not move anybody. See
[Why it stops at the path boundary](../business-context/path-boundary.md).

## 4. Test it

```bash
go build ./...
go vet ./...
go test ./...
go test ./... -race
gofmt -l .                      # should print nothing

# Integration tests (build-tagged). Every one boots its own Postgres (and,
# where needed, Kafka) via testcontainers (Docker required); none reads
# DATABASE_URL and none is skip-gated.
go test -tags=integration ./... -race -count=1

# Gherkin acceptance specs, driven through the real HTTP surface
go test ./... -run TestFeatures -v
```

## 5. Regenerate this site's API reference

The REST pages under **API Reference → REST API** are generated from
`apis/openapi.yaml`, not hand-written. After changing the spec:

```bash
cd docs
npm run clean-api-docs
npm run gen-api-docs
npm run build
```
