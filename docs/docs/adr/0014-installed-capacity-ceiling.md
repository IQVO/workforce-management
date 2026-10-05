---
id: 0014-installed-capacity-ceiling
slug: /adr/0014-installed-capacity-ceiling
title: 0014. Live installed-capacity ceiling on CommitShiftPlan, sourced from fulfillment-execution
sidebar_label: 0014. Installed-capacity ceiling (fulfillment-execution)
sidebar_position: 15
description: ADR 0014 — CommitShiftPlan enforces plannedHeads against a LIVE, fulfillment-execution-sourced installed-capacity ceiling, as a second independent check alongside the existing caller-supplied installedStations invariant, and fails the entire commit loud on any fetch failure.
---

# 0014. Installed-capacity ceiling, sourced from fulfillment-execution

## Status

Accepted.

## Context

`CommitShiftPlan` has always enforced `plannedHeads <= installedStations`
per path line — but `installedStations` is a number the CALLER supplies
in the request body. This context has no dependency on
fulfillment-execution (the bounded context that actually owns the
Station registry), so it has never had any way to verify that number
against physical reality. A caller supplying a stale or simply wrong
`installedStations` could commit a plan that either overcommits real
capacity or unnecessarily rejects a plan that would fit.

fulfillment-execution's own ADR-0018 closes exactly this gap from the
other side: it exposes `GET /capacity/{capability}`, a read-only
projection over its Station registry reporting how many stations are
physically registered with a given capability, live. This ADR is the
consuming half of that decision.

## Decision

**`CommitShiftPlan` fetches a LIVE installed-capacity ceiling from
fulfillment-execution for every line, and `shiftplan.CommitShiftPlan`
enforces it as a SECOND, INDEPENDENT check — never a replacement for the
existing caller-supplied `installedStations` invariant.** Both checks
must pass:

```go
// internal/domain/shiftplan/shift_plan.go
func CommitShiftPlan(buildingId, shiftId string, lines []PathPlan,
	installedStations map[shared.PathId]int, // caller-supplied, structural
	installedCapacity map[shared.PathId]int, // LIVE, fulfillment-execution-sourced
	maxHoursPerShift float64, at time.Time) (*ShiftPlan, error) {
	for _, line := range lines {
		if line.PlannedHeads > installedStations[line.PathId] {
			return nil, ErrPlannedHeadsExceedInstalled
		}
		// A missing entry defaults to a live capacity of 0 -- never
		// skipped -- so a caller cannot bypass this check by omitting
		// a path.
		if line.PlannedHeads > installedCapacity[line.PathId] {
			return nil, ErrExceedsInstalledCapacity
		}
		...
	}
}
```

We rejected collapsing these into one check (replacing
`installedStations` outright with the live fetch) because the two numbers
answer genuinely different questions and existing BDD fixtures /
`shiftplan` tests already depend on `installedStations` as a distinct,
structural input the caller is asserting. Keeping both as independent
checks means a caller-side lie ("I say there are 20 stations") is caught
by the structural check even before a network call, and a stale live
fetch is caught by the physical check even when the caller's number
looked plausible.

### Fail LOUD, not fail open — the key difference from `MeasuredRateClient`

This service already has one precedent for consulting an external
Supplier from inside a use case: `ProposePathPlan`'s
`MeasuredRateClient` (ADR-0012), which fails OPEN — on any error it
silently falls back to the caller-supplied `plannedRate`, because a
measured rate is a soft, optional enrichment to a mere PROPOSAL.

`InstalledCapacityClient` does the opposite:

```go
// internal/application/ports/ports.go
type InstalledCapacityClient interface {
	// capability, not path id -- see the addendum below.
	InstalledCapacity(ctx context.Context, capability shared.Capability) (int, error)
}
var ErrInstalledCapacityUnavailable = errors.New("installed capacity unavailable")
```

```go
// internal/application/usecases/commit_shift_plan.go
func (uc *CommitShiftPlan) Execute(ctx context.Context, buildingId, shiftId string,
	lines []shiftplan.PathPlan, installedStations map[shared.PathId]int) (*shiftplan.ShiftPlan, error) {
	installedCapacity := make(map[shared.PathId]int, len(lines))
	for _, line := range lines {
		// resolved via PathCatalogue -> requiredCapabilities -> MIN of
		// InstalledCapacity(capability); see the addendum below.
		capacity, err := uc.installedCapacityForPath(ctx, line.PathId, memo)
		if err != nil {
			return nil, err // FAILS THE ENTIRE COMMIT — no fallback
		}
		installedCapacity[line.PathId] = capacity
	}
	...
}
```

`ErrInstalledCapacityUnavailable` on ANY line fails the whole commit.
There is no fallback path, because `CommitShiftPlan` mutates real state
(it commits a plan and publishes `ShiftPlanCommitted`), and this fleet's
own standing rule — already applied identically in order-management's
fail-loud `inventorystorage` client — is to fail loud for anything that
mutates real state. A soft proposal can afford to guess; a commit cannot.

### Adapters, mirroring `laborperformance`'s shape

```go
// internal/adapters/outbound/fulfillmentexecution/client.go
type Client struct{ baseURL string; doer HTTPDoer }
func (c *Client) InstalledCapacity(ctx context.Context, capability shared.Capability) (int, error) {
	// GET {baseURL}/capacity/{capability}, capability sent verbatim.
	// CommitShiftPlan resolves the capability from the process-path
	// catalogue first -- see the addendum below; the ORIGINAL text of
	// this ADR sent the path id here, which was wrong.
}
```

```go
// internal/adapters/outbound/fulfillmentexecution/permissive.go
type PermissiveClient struct{}
func (PermissiveClient) InstalledCapacity(_ context.Context, _ shared.Capability) (int, error) {
	return 0, ports.ErrInstalledCapacityUnavailable
}
```

Selected at boot via `INSTALLED_CAPACITY_MODE` (`http`|`permissive`,
default `permissive`) and `FULFILLMENT_EXECUTION_BASE_URL`, the same
env-selected-adapter pattern `LABOR_PERFORMANCE_MODE` already uses. The
default is deliberately the FAIL-LOUD permissive client, not a silent
no-op: an operator who forgets to set `INSTALLED_CAPACITY_MODE=http`
gets every `CommitShiftPlan` call rejected loudly (logged as a `Warn` at
boot with an explicit hint), never a quiet capacity check that always
passes.

## Consequences

### Easier

- A shift-plan commit can no longer silently overcommit past
  fulfillment-execution's REAL Station registry — the exact class of gap
  this fleet's process-path model review flagged.
- The two checks (`installedStations`, `installedCapacity`) stay legible
  independently: a test or an operator reading a rejection knows
  immediately which one failed (`ErrPlannedHeadsExceedInstalled` vs
  `ErrExceedsInstalledCapacity`).

### Harder

- `CommitShiftPlan` now has a REQUIRED runtime dependency on
  fulfillment-execution being reachable — a new failure mode this
  context did not previously have. This is a deliberate tradeoff, not an
  oversight: the alternative (silently trusting a caller-supplied number)
  is the exact bug this ADR closes.
- One extra network round-trip per distinct path in a commit (deduped —
  a plan with two lines for the same path fetches once, not twice).

## Verification

`internal/domain/shiftplan/shift_plan_test.go`: 4 new tests
(`TestCommitShiftPlan_RejectsPlanExceedingInstalledCapacity`,
`_AllowsPlannedHeadsExactlyEqualToInstalledCapacity`,
`_MissingInstalledCapacityEntryDefaultsToZeroCeiling`, plus every
existing test updated for the new parameter) — `make mutation` (the
CI-gated fast subset, `./internal/domain/shiftplan`) and `make
mutation-full` (the exhaustive `./internal/domain` run) both pass at
100.00% efficacy / 100.00% mutator coverage, unchanged from before this
feature.

`internal/application/usecases/usecases_test.go`: 3 new tests
(`TestCommitShiftPlan_RejectsPlanExceedingLiveInstalledCapacity`,
`_FailsLoudWhenInstalledCapacityUnavailable`,
`_FetchesInstalledCapacityOncePerDistinctPath`) plus 7 existing
`CommitShiftPlan`-dependent tests updated across the application, HTTP,
MCP, and outbound-kafka test suites.

`internal/adapters/outbound/fulfillmentexecution/`: `client_test.go` (6
tests: published-contract shape, verbatim lowercase PathId-as-capability,
zero-is-a-real-answer, every failure mode maps to
`ErrInstalledCapacityUnavailable`, unreachable-server) and
`permissive_test.go` (1 test).

`go test ./... -race` (all packages, including `internal/architecture`'s
hexagonal fitness tests), `golangci-lint run ./...` (0 issues),
`go build -tags=integration ./...` / `go vet -tags=integration ./...`
(caught and fixed two stale `shiftplan.CommitShiftPlan` call sites this
signature change broke under the separate `integration` build tag — the
same pitfall class this fleet has hit before), a real Postgres
integration test (`TestShiftPlanRepo_*`, run against a live local
Postgres via `-tags=integration`) proving the persistence layer is
unaffected, and `cd docs && npm run build` all pass.

## Addendum: query by the path's required capability, not by its path id

### What was wrong

The original decision (and the code shipped with it) sent the line's
`PathId` verbatim as the `{capability}` segment, on the stated assumption
that "fulfillment-execution's Station capabilities and this repo's PathId
already share the same lowercase convention". They do not. The fleet's
canonical process-path ids are upper-case (`PICK`, `PACK`, `REBIN`, `SLAM`
in `process-path-management` and `warehouse-infra`'s `sortable-fc.yaml`),
real path ids carry suffixes (`pick-zone-a`), and stations are registered
with the lower-case capability strings the catalogue declares in
`requiredCapabilities` (`pick`, `pack`, ...). fulfillment-execution's
`GET /capacity/{capability}` matches that string exactly and answers an
unknown capability with a valid `0`, so the mismatch never surfaced as an
error. Live evidence against the develop builds:

```text
GET  fulfillment-execution /capacity/pick   -> {"installed":41}
GET  fulfillment-execution /capacity/PICK   -> {"installed":0}
POST workforce-management /shift-plans  (pathId PICK, plannedHeads 4)
     -> 409 exceeds-installed-capacity       (always, for every canonical path)
```

### Decision

`CommitShiftPlan` resolves each line's path to the capabilities it requires
through the existing `ports.PathCatalogue` (ADR-0013 — the same file- or
Kafka-sourced catalogue the HTTP adapter already validates path ids
against), and checks `plannedHeads` against the stations able to serve that
path:

- **Port contract.** `InstalledCapacityClient.InstalledCapacity` takes a new
  `shared.Capability` value type instead of `shared.PathId`, so the contract
  says what fulfillment-execution actually counts. A `PathId` can no longer
  be passed by accident; the only way to obtain a `Capability` is from the
  catalogue's `requiredCapabilities`.
- **Ceiling = MIN over required capabilities.** A station can serve a path
  only if it holds every capability the path requires, and
  fulfillment-execution counts one capability at a time, so the tightest
  bound it can give is the smallest of those counts. Every path the fleet
  declares today requires exactly one capability, for which this is just
  that capability's count.
- **No capabilities declared ⇒ ceiling 0 (fail closed).** Not
  "unconstrained", and not "lower-case the path id": this ADR already treats
  a missing capacity entry as a 0 ceiling rather than a skipped check, and
  guessing a capability from the path id is exactly the bug being fixed.
  Both catalogue sources reject an empty `requiredCapabilities`
  (`filecatalog`'s loader and process-path-management's
  `ErrNoRequiredCapabilities`), so this is defensive only.
- **Unknown path ⇒ `400 unknown-path-id`.** `pathcatalog.ErrUnknownPath`
  (already mapped to RFC 7807 `unknown-path-id` by ADR-0013) is returned
  from the use case before fulfillment-execution is called, so the rule
  holds for every inbound adapter, not only the HTTP handler's pre-check.
- **Catalogue required.** `CommitShiftPlan.Catalogue` must be wired; a nil
  catalogue fails every commit (500) rather than guessing.
- Each distinct capability is fetched once per commit (`pick` and
  `pick-zone-a` share one call). The fail-loud contract is unchanged: any
  `ErrInstalledCapacityUnavailable` still fails the whole commit with `503
  installed-capacity-unavailable`.

### Verification

`internal/application/usecases/usecases_test.go`:
`TestCommitShiftPlan_QueriesInstalledCapacityByRequiredCapability` (table:
`PICK`/`pick`/`pick-zone-a` → queried as `pick`; over-capacity rejection;
multi-capability MIN; no-capability fail-closed; unknown path rejected
before any call), `_PathIdIsNeverSentAsCapability` (the live symptom),
`_UnreachableFulfillmentExecutionStillFailsLoud`, `_FetchesEachCapabilityOnce`,
`_RequiresCatalogue`. HTTP: `TestCommitShiftPlan_CanonicalPathIdIsCheckedAgainstItsCapability`.
Client: `TestInstalledCapacitySendsCapabilityVerbatim`. BDD
(`features/shift_plan.feature`): a fake station registry keyed by exact
capability string proves `PICK` commits against 41 `pick` stations,
`pack-station-3` is rejected against 3 `pack` stations, and an undeclared
path is `400 unknown-path-id`.
