---
id: quickstart
title: Run it locally
sidebar_label: Quickstart
sidebar_position: 2
description: Bring up Postgres, run the service, and walk one shift end to end with curl.
---

# Run it locally

The OLTP service is `cmd/workforce`. It needs Postgres and a process-path
catalogue file. Kafka is optional and off by default.

## 1. Start Postgres and the service

The service refuses to boot without a process-path catalogue
([ADR 0013](../adr/0013-process-path-catalogue-validation.md)). The default
path, `/etc/workforce-management/process-paths.yaml`, rarely exists on a
laptop, so point `PATH_CATALOGUE_FILE` at a local copy. The canonical file is
`warehouse-infra`'s `config/process-paths/sortable-fc.yaml`. A minimal
equivalent:

```yaml
# process-paths.yaml
building: local
paths:
  - id: PACK
    matchPrefix: pack
    requiredCapabilities: [pack]
  - id: PICK
    matchPrefix: pick
    requiredCapabilities: [pick]
```

`CommitShiftPlan` also checks every line against the live installed capacity
in `fulfillment-execution`
([ADR 0014](../adr/0014-installed-capacity-ceiling.md)). In the default
`permissive` mode every commit fails with `503 installed-capacity-unavailable`.
To commit a plan, point the service at a `fulfillment-execution` that has
stations registered for the path:

```bash
docker compose up -d   # Postgres 16 on localhost:5432

export DATABASE_URL="postgres://workforce:workforce@localhost:5432/workforce?sslmode=disable"
export PATH_CATALOGUE_FILE=./process-paths.yaml
export INSTALLED_CAPACITY_MODE=http
export FULFILLMENT_EXECUTION_BASE_URL=http://localhost:8000/api/fulfillment-execution   # e.g. via the kind cluster's Kong
go run ./cmd/workforce   # applies migrations, then serves on :8080
```

## 2. Configuration

Everything is environment-driven — there is no config file.

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `DATABASE_URL` | yes | — | Postgres connection string |
| `HTTP_ADDR` | no | `:8080` | listen address |
| `MIGRATIONS_PATH` | no | `migrations` | path to the golang-migrate SQL files |
| `MAX_HOURS_PER_SHIFT` | no | `8` | the configured max-hours-per-shift cap |
| `PATH_CATALOGUE_SOURCE` | no | `file` | `file` reads `PATH_CATALOGUE_FILE` once at boot; `kafka` replays `process-path-management`'s topic instead (requires `KAFKA_BROKERS`) |
| `PATH_CATALOGUE_FILE` | with `file` source | `/etc/workforce-management/process-paths.yaml` | process-path catalogue YAML; missing or invalid is a fatal boot error |
| `INSTALLED_CAPACITY_MODE` | no | `permissive` | `http` or `permissive`. **Permissive rejects every `CommitShiftPlan` with `503`** |
| `FULFILLMENT_EXECUTION_BASE_URL` | with `http` capacity mode | — | `fulfillment-execution` base URL |
| `LABOR_PERFORMANCE_MODE` | no | `permissive` | `http`, `kafka-cache` or `permissive` — measured-rate / idle-share feed for `ProposePathPlan` and `GetStaffingGap` |
| `EVENT_PUBLISHER` | no | `log` | `log` or `kafka` — see [Integration](../ecosystem/integration.md) |
| `KAFKA_BROKERS` | no | `localhost:9092` | comma-separated broker list, used when `EVENT_PUBLISHER=kafka` |

The README's env table lists the remaining knobs (`LABOR_PERFORMANCE_BASE_URL`,
`IDLE_SHARE_TRIM_THRESHOLD`, `OUTBOX_RELAY_INTERVAL`, `MIGRATIONS_DATABASE_URL`,
`HOUSEKEEPING_INTERVAL`, `IDEMPOTENCY_KEY_TTL`, `OUTBOX_RETENTION`,
`SHUTDOWN_DRAIN_DELAY`, `CORS_ALLOWED_ORIGINS`, OTel variables).

## 3. Walk one shift end to end

```bash
# 1. Someone clocks on, certified to pack
curl -X POST localhost:8080/associates/assoc-1/start-shift \
  -d '{"certifications":["pack"]}'

# 2. Give them a second qualification
curl -X POST localhost:8080/associates/assoc-1/certifications \
  -d '{"certification":"hazmat"}'

# 3. The software proposes: 100 units of charge at 30 units/hour needs 4 heads
curl -X POST localhost:8080/paths/pack/plan/propose \
  -d '{"buildingId":"bldg-1","charge":100,"plannedRate":30}'

# 4. A human commits the split (this is the ShiftPlan). Needs
#    INSTALLED_CAPACITY_MODE=http and reported capacity >= 3 for pack;
#    otherwise 503 (capacity unavailable) or 409 (exceeds-installed-capacity).
#    The two creation POSTs require an Idempotency-Key header (ADR 0027):
#    without one the service answers 400 idempotency-key-required.
curl -X POST localhost:8080/shift-plans \
  -H 'Idempotency-Key: 6f1d2c3a-commit-bldg-1-shift-1' \
  -d '{"buildingId":"bldg-1","shiftId":"shift-1","lines":[
        {"pathId":"pack","plannedHeads":3,"plannedRate":30,"plannedHours":24,"installedStations":10}
      ]}'

# 5. Put the associate on the pack path
curl -X POST localhost:8080/associates/assoc-1/assignments \
  -H 'Idempotency-Key: 0b7e9a41-assign-assoc-1-pack' \
  -d '{"pathId":"pack"}'

# 6. Breaks gate assignment while they are open
curl -X POST localhost:8080/associates/assoc-1/break/start
curl -X POST localhost:8080/associates/assoc-1/break/end

# 7. Is the pack path short of its committed heads?
curl "localhost:8080/paths/pack/staffing-gap?buildingId=bldg-1&shiftId=shift-1"

# 8. Clock off — closes any active assignment first
curl -X POST localhost:8080/associates/assoc-1/end-shift

curl localhost:8080/healthz   # liveness
curl localhost:8080/readyz    # readiness (503 once graceful shutdown starts)
```

Step 7 returns the staffing-gap read model — planned versus active, and the
`understaffed` flag:

```json
{"pathId":"pack","plannedHeads":3,"activeHeads":1,"understaffed":true}
```

With `LABOR_PERFORMANCE_MODE=kafka-cache` the response can also carry
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
