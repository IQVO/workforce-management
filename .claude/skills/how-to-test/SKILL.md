---
name: how-to-test
description: Write or review tests and diagnose a failing coverage, mutation, bdd or integration CI job: the four test layers, the 90% gate, gremlins threshold semantics, the testcontainers rule. Use when adding tests, killing a surviving mutant, or fixing a red check.
---

# How to test

Use when writing or reviewing tests in this repo, or diagnosing a failing
`coverage`/`mutation-fast`/`bdd`/`integration` CI job. This fleet's quality
bar is layered — passing `go test` is necessary but is the WEAKEST signal
of the four; mutation testing exists specifically because green tests can
assert nothing.

## The four layers, in order of what they actually prove

1. **Unit tests** (`go test ./... -race`) — prove the code runs without
   panicking and returns SOMETHING. Table-driven, in-memory adapters only
   (`internal/adapters/outbound/memory/`), never a real network/DB call.
2. **Coverage** (`make coverage`, 90% gate on
   `./internal/domain/...,./internal/application/...`) — proves lines
   executed. Proves nothing about whether the test asserted the right
   thing.
3. **Mutation testing** (`make mutation`, gremlins, scoped to
   `./internal/domain/shiftplan` for the fast BLOCKING CI job; `make
   mutation-full` on all of `./internal/domain`, scheduled weekly, not
   blocking) — proves the tests actually ASSERT, not merely execute. A
   mutant is a deliberately broken version of the code (`<` -> `<=`,
   `+` -> `-`, etc.); if the test suite still passes against the mutant,
   it "survived" (LIVED) — meaning no test would catch that exact bug in
   production.
4. **BDD / behaviour** (`make bdd`, godog) — proves the use case works
   end-to-end through the real HTTP surface (`features/*.feature`), not
   through a mocked port.

## Mutation testing: `<=` fails, not `>=`

`.gremlins.yaml` sets `efficacy: 99` / `mutant-coverage: 99` as a floor
gremlins fails on if the MEASURED value is `<=` the threshold — read the
comment at the top of this repo's `.gremlins.yaml`: it was set to 99
specifically because the measured baseline was 100.00%/100.00% on both
`./internal/domain/shiftplan` (10 mutants, all killed, 35s) and the
exhaustive `./internal/domain` run (23 mutants, all killed, 47s) as of
2026-08-23 — 99 locks in "already achieved" the same way the 90%
coverage gate does. When you deliberately lower coverage of a package
(rare, but happens when removing dead code), you may need to lower the
threshold in the SAME PR with a dated comment explaining why.

## The real boundary-guard pitfall this repo already hit once

`MUTATION.md` documents the one LIVED mutant this repo's baseline run
ever found: `shiftplan/shift_plan.go:71:24` —
`if line.PlannedHeads > installed`, mutated by gremlins'
`CONDITIONALS_BOUNDARY` mutator to `>=`, survived because no existing
test exercised the exact-equal boundary
(`plannedHeads == installedStations`). The invariant is explicitly
`plannedHeads(path) <= installedStations(path)`, so the boundary case
must be *accepted*, not rejected. Fixed by adding
`TestCommitShiftPlan_AllowsPlannedHeadsExactlyEqualToInstalledStations`.
**Any new `< 0`/`> 0`/`<=`/`>=` guard you add — in `shiftplan` or
elsewhere — needs an explicit test for the boundary value itself,** not
just one clearly-invalid and one clearly-valid case, or you will
reproduce this exact class of survivor.

## Two more general pitfalls worth knowing before they cost a CI run

### Zero/origin-value fixtures hide arithmetic mutants

A test built around zero-valued operands makes `a - b` and `a + b`
produce the same result, so a mutant flipping `-` to `+` survives even
though coverage looks complete. Any new value object with real
arithmetic (e.g. anything computing a trim like
`ProposePathPlan.applyIdleShareTrim`'s
`int(math.Ceil(float64(heads) * (1 - share)))`) needs fixture values
where every operand and every derived delta is distinct and non-zero,
asserting the exact expected value, not just "no error".

### Tie-break / near-equivalent mutants: know when NOT to chase them

A comparison whose mutation only diverges on an exact tie (two equal
`float64` idle-share values, say) is not always killable by any
reasonable test. Do NOT force an artificial tied fixture just to kill it
— that pins an arbitrary, currently-unspecified tie-break order as if it
were a real invariant. Document it in `MUTATION.md`'s triage section
instead, following the format that file already uses, and move on.

## Diagnosing a `mutation-fast` CI failure: diff against develop, don't chase every LIVED line

```bash
gremlins unleash ./internal/domain/shiftplan          # on your branch
git stash && git checkout origin/develop -- . && gremlins unleash ./internal/domain/shiftplan   # baseline
```

Only entries NEW on your branch are your regression. `MUTATION.md` is
this repo's permanent record of the ONE accepted/triaged survivor found
so far — confirming the survivor SET is unchanged from `origin/develop`
(currently: none survive at all, 100%/100%), not just that the
percentage cleared the `.gremlins.yaml` gate, is the real proof a fix
didn't just get lucky on the threshold.

Flag note from `MUTATION.md`: `gremlins unleash ./internal/domain`
(trailing `/...`) reports "No results to report" — gremlins wants a
plain directory path, not a Go package pattern. The default timeout
coefficient is also too tight on this machine (every mutant reports
`TIMED OUT`); `.gremlins.yaml`'s `timeout-coefficient: 30`/`workers: 1`
is what actually produces real kill/live verdicts — don't override these
locally without a reason, and don't run multiple parallel workers
(`workers: 1`'s comment explains they contend over the build cache on a
module this small and produce spurious TIMED OUT results).

## Kafka integration tests: testcontainers, never a skip-gate

A `-tags=integration` test touching Kafka MUST start its own broker via
`testcontainers-go`. Never gate on `os.Getenv("KAFKA_BROKERS")` +
`t.Skip(...)`, and never hardcode `localhost:9092`. This repo's CI
`integration` job (`.github/workflows/ci.yml`) provisions two Postgres
service containers (OLTP + analytics) but NO Kafka — a skip-gated Kafka
test silently skips in CI and proves nothing there, while testcontainers
actually exercises the assertions on the runner. This repo has TWO
working examples, both structured identically — see
`internal/adapters/outbound/laborperformancecache/consumer_integration_test.go`
and its sibling in `internal/adapters/outbound/kafkacatalog/`:

```go
//go:build integration

container, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.6.1",
    tckafka.WithClusterID("workforce-laborperformancecache-itest"))
defer testcontainers.TerminateContainer(container)
brokers, _ := container.Brokers(ctx)
```

Both real tests also cover the "two instances in a row both replay
fully" regression from how-to-add-an-integration-event.md's consumer-
group pitfall — `TestNewConsumerForTopic_TwoInstancesInARow_BothReplayFully`
is the concrete proof that the per-process-unique group id actually
prevents the shared-offset bug, not just an assertion that the code
compiles.

Pitfalls that cost real time here:

- **Create the topic explicitly, then WAIT for the partition leader.**
  Both real tests' shared `createTopicAndWaitForLeader` helper polls
  `conn.ReadPartitions(topic)` until a leader is assigned (15s deadline,
  100ms poll interval) before the first read/write — `conn.CreateTopics`
  returns before the broker has actually finished.
- **Give each test its own unique topic** (`fmt.Sprintf("...-itest-%d",
  time.Now().UnixNano())`) and reuse ONE container per package.
- Still run `go build -tags=integration ./...` and
  `go vet -tags=integration ./...` before pushing — integration files
  are invisible to the default build, so a signature change breaks only
  the CI integration job.

For a Postgres-touching integration test (the outbox), the same
testcontainers discipline applies —
`internal/adapters/outbound/postgres/outbox_integration_test.go` boots
its own `postgres:16-alpine`, never reads an external `DATABASE_URL`.

## Verify before opening the PR

```bash
make check-all   # check + coverage + arch-test + bdd (the full local gate)
```

`make mutation` (the fast blocking subset) and `make vuln` are NOT part
of `check-all` locally, but CI runs both (`mutation-fast` and `vuln`
jobs) on every push/PR — a PR can pass your local `check-all` and still
go red in CI on either of those; run them explicitly too before pushing
if gremlins/govulncheck are installed locally.
