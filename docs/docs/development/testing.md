---
id: testing
title: Testing
sidebar_label: Testing
description: The test pyramid as it exists in workforce-management. Covers unit, BDD, integration (testcontainers), contract, MCP evals, architecture fitness and mutation tests, the make targets, and what every CI job runs.
---

# Testing

Every `make` target mirrors a CI job in `.github/workflows/ci.yml`, so a
local pass predicts a CI pass. The commands below are copied from the
`Makefile` and `ci.yml`.

## The pyramid

| Layer | What | Where | Command | Needs |
|---|---|---|---|---|
| Unit | Domain aggregates, use cases (fakes for every port), HTTP handlers through `httptest`, MCP server in memory, Kafka encoders and decoders, composition wiring | `*_test.go` next to the code | `make test` (`go test ./... -race`) | nothing |
| Coverage gate | Unit tests with `-coverpkg=./internal/domain/...,./internal/application/...` | same | `make coverage` | nothing. Fails below **90 %**. |
| BDD acceptance | godog/Gherkin scenarios driven through the real HTTP router with in-memory test doubles ([ADR 0006](../adr/0006-godog-bdd-acceptance-tests.md)) | `features/*.feature`, runners `features/features_test.go` and `features/features_wave2_test.go` (not at the repo root: `TestNoGoFilesAtRepoRoot`, [ADR 0038](../adr/0038-postgres-and-kafka-are-the-only-runtime.md)) | `make bdd` (`go test ./... -run TestFeatures -v`) | nothing |
| Architecture fitness | Hexagonal and fleet rules as tests ([ADR 0007](../adr/0007-arch-go-architecture-fitness-tests.md)), including `TestRuntimeBinaryHasNoInMemoryAdapters` and `TestNoGoFilesAtRepoRoot` | `internal/architecture/` | `make arch-test` | nothing |
| MCP evals | Tool schemas, wire conformance, behaviour | `internal/adapters/inbound/mcp/eval_*_test.go`, `evalsuite_test.go` | `go test ./internal/adapters/inbound/mcp/... -race -run '^TestEval\|^TestMCPEvalSuite' -v` | nothing |
| Integration | Real Postgres and real Kafka through testcontainers-go, including `TestRunOnRealPostgresAndKafka` (`cmd/workforce/e2e_integration_test.go`), which boots the real `run()` and asserts Postgres rows and the CloudEvents on both output topics | files with `//go:build integration` | `make integration` (`go test -tags=integration ./... -race -count=1`) | Docker |
| Contract | Schemathesis property-based tests of the running binary against `apis/openapi.yaml`, on a real Postgres and a real Kafka (the CI job uses service containers; `docker compose up -d --wait` locally) | `scripts/contract-test.sh` | `make contract` | `DATABASE_URL`, `KAFKA_BROKERS` and `st` (Schemathesis 4.28.0) |
| Mutation | gremlins on the domain | `.gremlins.yaml` | `make mutation` (fast) / `make mutation-full` | gremlins v0.6.0 |

Bundles: `make check` = `fmt-check vet build lint test`, the fast
pre-commit loop. `make check-all` = `check` + `coverage arch-test bdd`, to run
before pushing. Neither bundle includes `integration`, `contract` or
`mutation`. `make check-fast` (fmt-check, vet, arch-test, and the tests of the
packages changed against HEAD) is the agent stop-hook gate.

## BDD features

16 feature files hold **85** `Scenario`/`Scenario Outline` blocks (8 of them
outlines, which expand to more runs):

| Feature file | Scenarios |
|---|---|
| `associate_roster.feature` | 11 |
| `associate_shift_lifecycle.feature` | 3 |
| `break_rules.feature` | 8 |
| `break_state_transitions.feature` | 2 |
| `breaks.feature` | 1 |
| `labor_assignment.feature` | 4 |
| `labor_assignment_rules.feature` | 6 |
| `path_plan_proposal.feature` | 1 |
| `path_plan_proposal_rules.feature` | 11 |
| `probes.feature` | 2 |
| `shift_plan.feature` | 5 |
| `shift_plan_commit_rules.feature` | 14 |
| `shift_plan_hours_capacity.feature` | 1 |
| `staffing_gap.feature` | 3 |
| `staffing_gap_read_model.feature` | 2 |
| `staffing_gap_views.feature` | 11 |

A separate godog suite, `internal/adapters/inbound/mcp/testdata/features/mcp_tools.feature`
(17 scenarios), drives the MCP tools. It runs as part of the MCP evals
(`TestMCPEvalSuite`), not `make bdd`.

## Integration tests (testcontainers)

Every integration test starts its own containers, so no `DATABASE_URL` or
`KAFKA_BROKERS` is read. Two fitness tests enforce this:
`TestPostgresIntegrationTestsUseTestcontainers` and
`TestKafkaIntegrationTestsUseTestcontainers`.

| Area | Files |
|---|---|
| Postgres repositories, `version` concurrency, site code | `internal/adapters/outbound/postgres/integration_test.go`, `version_integration_test.go`, `site_integration_test.go` |
| Outbox: atomicity, relay, attempt cap ([ADR 0036](../adr/0036-outbox-relay-attempt-cap.md)), lag gauge | `outbox_integration_test.go`, `outbox_lag_integration_test.go` (postgres package) |
| Outbox trace continuity (D27): the request's `traceparent` reaches Kafka through the outbox; an untraced row relays unchanged | `outbox_trace_integration_test.go` (postgres package, real Postgres and Kafka) |
| Pool limits (`MaxConns`, `statement_timeout`) | `pool_limits_integration_test.go` |
| Housekeeping | `internal/adapters/outbound/postgres/sweeper_integration_test.go`, `cmd/workforce/housekeeping_integration_test.go` |
| Idempotency-Key middleware, site-code conversion | `internal/adapters/inbound/http/idempotency_integration_test.go`, `siteconv_integration_test.go` |
| Use-case wiring over real Postgres | `internal/application/usecases/usecases_wiring_integration_test.go` |
| MCP over Streamable HTTP | `internal/adapters/inbound/mcp/streamable_http_integration_test.go` |
| MCP writes land in the outbox | `cmd/mcp/outbox_integration_test.go` |
| Kafka producer partition key | `internal/adapters/outbound/kafka/publisher_partition_integration_test.go` |
| Kafka replay consumers | `internal/adapters/outbound/kafkacatalog/consumer_integration_test.go`, `internal/adapters/outbound/laborperformancecache/consumer_integration_test.go` |
| Analytics projection and DLQ | `internal/adapters/outbound/analyticsstore/postgres_integration_test.go`, `internal/adapters/inbound/kafka/analytics_dlq_integration_test.go` |

The `integration` CI job and `make integration` both run
`go test -tags=integration ./...`, so every file above runs in CI.

A Kafka integration test creates every topic it reads or writes and waits for the
partition leader before the first produce (`conn.CreateTopics` returns before the
broker has elected one). Writing to a topic that was only "created" fails on a cold
broker with `Unknown Topic Or Partition`; the outbox trace test flaked that way until
its `ensureTraceTopic` helper did both steps.

## Contract tests

`scripts/contract-test.sh` builds `cmd/workforce` and starts it on
`127.0.0.1:18087` (`CONTRACT_PORT`) against `DATABASE_URL`, with a temporary
two-path catalogue (`PACK`, `PICK`). It waits for `/healthz` and then runs
`st run apis/openapi.yaml --max-examples 100 --workers 4 --generation-allow-x00=false`.
`CONTRACT_MAX_EXAMPLES` overrides the example count. Four operations are
excluded, and the script says where each is tested instead:

- `proposePathPlan`, `assignLabor`, `getStaffingGap`: Schemathesis generates path ids that the catalogue rightly rejects (`400 unknown-path-id`). That gate is tested per endpoint in `internal/adapters/inbound/http/router_test.go`.
- `commitShiftPlan`: the same reason, plus `INSTALLED_CAPACITY_MODE=permissive` returns `503` for every valid commit.

To run it locally:

```bash
docker run -d --rm --name wf-contract-pg -p 18097:5432 \
  -e POSTGRES_USER=workforce -e POSTGRES_PASSWORD=workforce \
  -e POSTGRES_DB=workforce postgres:16
export DATABASE_URL='postgres://workforce:workforce@localhost:18097/workforce?sslmode=disable'
make contract
```

The reports binary (`cmd/workforce-reports`) has its own spec,
`apis/openapi-reports.yaml` ([ADR 0037](../adr/0037-reports-api-openapi-contract.md)).
It is contract-tested in Go against the real router, without a database:
`TestReportsContract_RoutesMatchSpec` (every served route is in the spec and
vice versa) and `TestReportsContract_ResponsesMatchSpec` (every documented
status of every operation: status, media type, required and undocumented
properties) in `internal/adapters/inbound/http/reports_contract_test.go`.
They run in the `test` job.

## Architecture fitness tests

`internal/architecture/`:

| Test | Rule |
|---|---|
| `TestHexagonalDependencyRule` | Dependencies point inward only. `domain` depends on nothing outside `domain`, and adapters never depend on each other directly. |
| `TestPortsPackageIsInterfacesOnly` | `internal/application/ports` declares the out-ports and never accumulates structs or helper functions. |
| `TestMCPAdapterDependencyRule` | The MCP adapter depends only on `application` and `domain` (never on outbound adapters or `cmd`), and nothing else depends on it ([ADR 0008](../adr/0008-mcp-inbound-adapter.md)). |
| `TestNoAuthMiddlewareReintroduced` | REST and MCP stay unauthenticated (bearer auth was removed, [ADR 0018](../adr/0018-remove-fleet-rest-identity.md)). |
| `TestEventCatalogueMatchesContract` / `TestEventCatalogueDetector` | The CloudEvents types the code publishes match `apis/asyncapi.yaml`. |
| `TestCloudEventsOnly` | Every Kafka message goes through the CloudEvents envelope ([ADR 0026](../adr/0026-cloudevents-mandatory-event-envelope.md)). |
| `TestReplayConsumersSetCommitInterval` | Replay consumers configure their commit interval. |
| `TestKafkaConsumerGroupNeverHardcodedInline` | No `GroupID: "literal"`. Groups are named constants or per-process generators. |
| `TestKafkaIntegrationTestsUseTestcontainers` / `TestPostgresIntegrationTestsUseTestcontainers` / `TestPostgresIntegrationSensorFailsOnBadFixtures` | Integration tests never skip on an environment variable and never hard-code `localhost:9092`. The sensor proves it catches bad fixtures. |
| `TestMCPOptionalToolInputsCarryOmitempty` (+ `TestMCPOptionalToolInputsSensorFailsOnBadFixtures`) | Every MCP tool input field whose `jsonschema` description says "optional" carries `omitempty`, or the SDK marks it required and calls without it fail (gap fix D16). |
| `TestOutboxRelaysPassAttemptCap` (+ `TestOutboxRelayCapSensorFailsOnBadFixtures`) | Every non-test `NewOutboxRelay` call under `cmd/` or `internal/composition/` passes `WithMaxAttempts` ([ADR 0036](../adr/0036-outbox-relay-attempt-cap.md)), so one poison row cannot block the relay forever. |
| `TestKafkaWritersSetRequiredAcks` (+ `TestKafkaWriterAcksSensorBites`) | Every kafka-go `Writer` composite literal in a non-test file under `internal/` or `cmd/` sets `RequiredAcks` (the import alias is resolved per file). kafka-go defaults it to `RequireNone` (acks=0): the broker never confirms the write, so a broker blip silently drops the message while `WriteMessages` returns nil. The twin proves the sensor flags a bad fixture under any alias. |
| `TestDeadLetterCallsGuardCancellation` (+ `TestDeadLetterShutdownSensorBites`, `kafka_dlq_shutdown_fitness_test.go`) | Every function in a non-test file under `internal/adapters/inbound/kafka` that sends to the dead-letter topic must reference `ctx.Err()` (any context-named value) or `context.Canceled` in its own body, or carry a `//dlq:deterministic <reason>` comment on the call line or the line above it. A handler that fails because the process is stopping must not dead-letter a good message and commit past it (network-fulfillment #89); a guard in the caller does not count. The sensor test proves the detector flags the pre-fix shape. |
| `TestKafkaWriterTopicNotSetTwice` (+ `TestKafkaWriterTopicSensorBites`, `kafka_writer_topic_fitness_test.go`) | kafka-go rejects every write whose `Message.Topic` is set when the `Writer` also has a `Topic`. Per Go package under `internal/` and `cmd/`: once any kafka-go `Message` literal sets `Topic`, every `Writer` literal in that package must leave `Topic` empty, unless it carries `//kafka:fixed-topic-writer <reason>` and no Topic-setting Message is provably written through it. Writers configured after construction and `WriterConfig` are not covered. (Escape: warehouse-planning's domain DLQ writer, where no dead letter ever landed.) |

## MCP evals

The `evals-tests` CI job runs
`go test ./internal/adapters/inbound/mcp/... -race -run '^TestEval|^TestMCPEvalSuite' -v`:

- **E1 governance** (`eval_governance_test.go`): input schemas resolve and constrain, and every parameter is described. The tool registry matches `testdata/tool_registry.golden`. Tool names are unique fleet-wide (`testdata/fleet_tool_snapshot.golden`). `get_workforce_labor_report` registers only with a reports client.
- **E2 wire conformance** (`eval_conformance_test.go`): the initialize handshake. Unknown tools, wrongly typed arguments and extra arguments are rejected. Resource templates, unknown resources and prompts are checked, and closing the session ends calls.
- **E3 behaviour** (`evalsuite_test.go`): the godog `mcp_tools.feature` suite.

## Mutation testing

`.gremlins.yaml`: `workers: 1`, `timeout-coefficient: 30`, thresholds
**`efficacy: 99`** and **`mutant-coverage: 99`**. gremlins fails when a value is
at or below its threshold, so a single surviving mutant fails the run. On
2026-08-23 the file records 100 % on both the fast subset
(`./internal/domain/shiftplan`, 10 mutants) and the full domain (23 mutants).

## CI jobs (`.github/workflows/ci.yml`)

Triggers: push and pull_request on `main` and `develop`, a weekly cron
(Monday 06:00 UTC), and `workflow_dispatch`.

| Job | Runs | When |
|---|---|---|
| `lint` | golangci-lint v2.13.1 | every run |
| `guide-lint` | `scripts/harness/guide_lint.py`, `repo_lint.py`, and the harness self-tests | every run |
| `complexity` | golangci-lint with only `gocyclo,gocognit,cyclop,funlen,nestif` (gocyclo gate 15), plus an informational `gocyclo -over 10` report | every run |
| `test` | `go build`, `go vet`, then `make coverage` (`go test ./... -race` with the Makefile's coverage scope and 90 % gate) | every run |
| `bdd` | `go test ./... -run TestFeatures -v` | every run |
| `contract` | Schemathesis 4.28.0 through `scripts/contract-test.sh` against a `postgres:16` service container | every run |
| `evals-tests` | MCP E1, E2, E3 evals | every run |
| `integration` | `go test -tags=integration ./... -race -count=1` (testcontainers) | every run |
| `mutation-fast` | `gremlins unleash ./internal/domain/shiftplan` | every run |
| `mutation` | `gremlins unleash ./internal/domain --workers 1 --timeout-coefficient 30`. Opens or closes a `harness:red` issue. | schedule / dispatch only |
| `drift` | `deadcode -test`, `go mod tidy -diff`, knip on `web/`, a coverage-quality mutant report (all informational) | schedule / dispatch only |
| `api-lint` | Spectral on `apis/openapi.yaml`, `apis/openapi-reports.yaml` and `apis/asyncapi.yaml` (`--fail-severity=warn`) | every run |
| `vuln` | govulncheck v1.8.0 | every run |
| `arch-test` | `go test ./internal/architecture/... -v` | every run |
| `docs-api-drift` | `npm run clean-api-docs && npm run gen-api-docs` in `docs/` (both specs), then `git status --porcelain` on `docs/api-reference/rest` and `rest-reports` (untracked pages fail too) | every run |
| `web` | `npm ci`, lint, `tsc -b`, `npm test`, `npm run build` for the `web/` remote | every run |
| `helm-lint` | `ct lint` on `charts/workforce-management` | PRs into `main` only |
| `chart-tests` | `bash charts/workforce-management/tests/render_test.sh` (ConfigMap `envFrom` mount, `config.*` values, installed-capacity defaults) and `python3 charts/workforce-management/tests/test_service_selectors.py` (each Service selects exactly one Deployment; PyYAML pinned, the script fails without it). Locally: `make chart-test`. | every run |
| `trivy-scan` | image vulnerability scan | PRs into `main` only |
| `docker-publish` | build, push to GHCR, cosign sign and SBOM attest. Needs all gating jobs. | push to `main` only |
| `release` | semver tag, image retag, Helm chart package and publish | push to `main` only, after `docker-publish` |

The docs site is built by `.github/workflows/docs.yml` on every pull request
that touches `docs/**`, `apis/**` or the workflow (build only, no Pages
artifact, no deploy; gap fix D25), and on push to `main`, which also
publishes. Run `npm ci && npm run build` in `docs/` to reproduce it locally.
