# Build Tasks — Workforce Management

Build the full bounded context described in CLAUDE.md, in order. Keep
`go build ./...` and `go test ./...` green throughout. Read
/Users/claudioed/docs/amazon-fulfillment-ddd.md for the domain model first.

## Task 0 — Skeleton
- `go mod init github.com/claudioed/workforce-management`; create the layout;
  .gitignore (bin/, .env); add chi + pgx deps.

## Task 1 — Domain (pure Go)
- shared: AssociateId, PathId, Certification value objects; DomainEvent + the
  10 named events.
- associate: AssociateShift aggregate (roster entry, certifications set, break
  state, hours logged; cannot assign while on break; hours <= max-per-shift).
- shiftplan: ShiftPlan aggregate with PathPlan lines (plannedHeads <=
  installedStations invariant; sum of hours valid).
- assignment: LaborAssignment aggregate (one associate, one path, an interval;
  exactly one ACTIVE assignment per associate; requires certification match).
- Unit tests for EVERY invariant, including the failing paths (four named in
  CLAUDE.md's Definition of Done).

## Task 2 — Application
- ports: AssociateRepo, ShiftPlanRepo, AssignmentRepo, EventPublisher, Clock.
- usecases: the 8 use cases from CLAUDE.md, depending only on domain + ports.
  ProposePathPlan is a pure computation (ceil(charge/rate)) with no persistence.
  AssignLabor must end any prior ACTIVE assignment for that associate before
  creating the new one (or reject — pick one behavior and document it in the
  README; either is defensible, but the double-booking invariant must hold).
- Unit-test against the in-memory adapter.

## Task 3 — Outbound adapters
- memory: thread-safe implementations of every port.
- postgres: pgxpool-backed repos + migrations (associate_shift, shift_plan,
  path_plan, labor_assignment, events tables). Build-tagged integration test
  (skip w/o DATABASE_URL).
- events: log/buffered EventPublisher; leave a kafka-ready interface.

## Task 4 — Inbound HTTP
- chi router, handlers for every endpoint in CLAUDE.md, request/response DTOs,
  domain-error -> HTTP status mapping, /healthz.
- httptest integration test per endpoint wired to in-memory repos.

## Task 5 — Composition root & ops
- cmd/workforce/main.go wires config (env) -> adapters -> use cases -> router.
- docker-compose.yml (Postgres 16). README.md with run steps + curl examples,
  plus the explicit "stops at the path boundary" design note.

## Task 6 — Verify
- `go build ./...`, `go vet ./...`, `go test ./...`, `go test ./... -race` all
  green. `gofmt -l .` empty. Confirm the four named invariants each have a
  red-path test. Do the smoke test: run the compiled binary and curl /healthz
  plus at least one write endpoint before declaring done. Do not stop until the
  Definition of Done in CLAUDE.md is met.

## Task 7 — Cross-service integration (additive, see INTEGRATION.md)
- Read INTEGRATION.md in this repo first.
- Add `github.com/segmentio/kafka-go` dependency.
- New Kafka outbound publisher adapter implementing the existing EventPublisher
  port, publishing one ShiftPlanCommitted message per PathPlan line to
  warehouse.workforce.events, selected via EVENT_PUBLISHER env (default "log").
- Unit test the envelope shape + the one-message-per-path-line fan-out.
  (Envelope since superseded: CloudEvents 1.0 is mandatory, ADR-0026.)
- README gains an Integration section. REAL smoke test against the shared
  broker (docker-compose.kafka.yml in ~/warehouse-systems, localhost:9092):
  call POST /shift-plans with 2+ path lines and confirm 2+ messages land on
  the topic before declaring done.
- Full existing suite (build/vet/test/-race) must still be green afterward.

## Task 10 — Quality engineering: linting, coverage, integration tests, mutation tests, CI
Full spec in QUALITY.md at the repo root. Five ordered stages, each gates the
next: (1) golangci-lint clean via the committed .golangci.yml, (2) unit test
coverage >= 90% on internal/domain/... + internal/application/... combined,
(3) real integration tests against live Postgres for every outbound Postgres
adapter, (4) gremlins mutation testing on internal/domain/... only
(exploratory, triaged not gated), (5) .github/workflows/ci.yml — lint+unit+
integration blocking on every push/PR, mutation testing on a weekly schedule/
manual dispatch only, never blocking PRs. Do not stop until every stage's
Definition of Done in QUALITY.md is met, then report the final numbers.

## Task 11 — REST API hardening + OpenAPI 3.0.3 docs + Spectral CI gate
Full spec in REST_API_TASK.md at the repo root. Four ordered stages: (1) audit
this service's HTTP adapter against REST/HTTP Level 2 maturity and fix real
violations (resource nouns, correct verbs/status codes, Location headers,
input validation), (2) migrate all error responses from the bespoke
{"error":...} shape to RFC 7807 application/problem+json, (3) write a very
detailed openapi.yaml (3.0.3) covering every route with full request/response
schemas and real domain-grounded examples, (4) add a new openapi-lint job to
the existing .github/workflows/ci.yml using Spectral, blocking on every
push/PR. Do not stop until every stage's Definition of Done in
REST_API_TASK.md is met, then report the final numbers.

## Task 12 — CI workflow restructure (user-provided template)
Full spec in CI_RESTRUCTURE_TASK.md at the repo root. Rewrite
.github/workflows/ci.yml to the given 4-job structure (lint, test,
integration, mutation) plus top-level permissions/concurrency/defaults,
while preserving Task 11's openapi-lint job as a 5th job. Requires adapting
placeholders to this repo's real values (postgres creds, DATABASE_URL,
whether integration tests self-migrate, current gremlins version), not
copy-pasting blindly. Every job's commands must be verified locally against
this repo before pushing, and the real GitHub Actions run must be confirmed
green via gh run watch. Do not stop until every requirement in
CI_RESTRUCTURE_TASK.md is met.

## Task 13 — Docker Hub publish job in CI
Added a `docker-publish` job to .github/workflows/ci.yml, gated on
`needs: [lint, test, integration, openapi-lint]` and
`if: github.event_name == 'push' && github.ref == 'refs/heads/main'` (never
runs on PRs, only after all quality gates pass on a real push to main).
Builds and pushes the existing repo-root Dockerfile to Docker Hub under the
`claudioed` namespace, tagged `latest` + short git SHA, linux/amd64 only,
using GitHub Actions cache (type=gha) to speed up rebuilds. Requires the
repo to have `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` secrets configured
(user-provided, not committed anywhere).

## Task 14 — Architecture fitness tests (ArchUnit equivalent: arch-go)
Full spec in ARCH_TEST_TASK.md at the repo root. Add
internal/architecture/architecture_test.go using github.com/arch-go/arch-go
to encode the hexagonal dependency rule as executable Go tests (domain
depends on nothing internal, application depends only on domain, inbound and
outbound adapters never depend on each other, only cmd/ wires everything).
New arch-test CI job, added to docker-publish's needs list. Strictly
additive -- do not touch existing production code; if a real architecture
violation is found, report it explicitly rather than silently working around
it. Do not stop until every requirement in ARCH_TEST_TASK.md is met.

## Task 15 — Helm chart lint CI job
Added a `helm-lint` job to .github/workflows/ci.yml using
helm/chart-testing-action@v2.8.0 (`ct lint`) against
charts/workforce-management. Runs on every push/PR (no gating condition, always
blocking), and wired into `docker-publish`'s `needs` list alongside the
existing gates -- a chart that fails helm/Chart.yaml validation or YAML
style rules never reaches Docker Hub either. Verified locally with
`ct lint --charts charts/workforce-management --validate-maintainers=false
--check-version-increment=false` before pushing (chart-testing v3.14.0,
yamllint installed via Homebrew).
