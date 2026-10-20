---
id: 0006-godog-bdd-acceptance-tests
title: 0006. godog acceptance specs driven through the real HTTP surface
sidebar_label: 0006. godog acceptance specs
sidebar_position: 7
description: Gherkin scenarios that exercise the published contract rather than internal wiring.
---

# 0006. godog acceptance specs driven through the real HTTP surface

## Status

Accepted. Added in commit `e623950` ("Add godog (Cucumber/Gherkin) BDD
acceptance tests, wire bdd CI job").

## Context

By the time this decision was taken the service already had a dense test suite:
domain unit tests, application tests against in-memory adapters, an `httptest`
test per endpoint, build-tagged Postgres integration tests, 98.2% coverage on
domain plus application, and mutation testing.

What none of them did was state the *rules of the business* in language a
non-programmer could check. Four invariants are named in this context's
Definition of Done, and every one of them is a sentence about how a fulfillment
centre works:

- an associate without the path's certification cannot be assigned to it;
- an associate on a logged break cannot be assigned;
- an associate cannot hold two active assignments at once;
- a path cannot be planned for more heads than it has installed stations.

Those sentences existed in `CLAUDE.md`, in the README, and — encoded — in Go
test function names. They did not exist in an executable form that a shift
manager could read and confirm.

The risk this creates is specific and not hypothetical: a rule can hold at the
domain layer while an adapter quietly bypasses it. Domain unit tests cannot
catch that, by construction, because they never go through the adapter.

## Decision

**Add executable specifications in Gherkin under `features/`, run with
[godog](https://github.com/cucumber/godog)** — the official Cucumber
implementation for Go — and **drive them through the real REST API**.

Each scenario:

- wires the real chi router to the in-memory adapters (memory repos, a buffered
  event publisher, a fixed clock);
- serves it over an `httptest.Server`;
- exercises it with real `net/http` calls.

Nothing reaches past the HTTP boundary. The scenarios document the **published
contract**, not internal wiring. Every scenario gets a fresh server and fresh
repositories, so they are independent and order-free.

Four feature files, mapped to the four named invariants and the read model:

| Feature file | Covers |
| --- | --- |
| `features/shift_plan.feature` | `CommitShiftPlan` — within capacity, and rejected when `plannedHeads` exceed installed stations |
| `features/labor_assignment.feature` | `AssignLabor` — certified assignment, uncertified rejection, no double-booking, rejection while on break |
| `features/breaks.feature` | `StartBreak` / `EndBreak` — break state gates assignment, then releases it |
| `features/staffing_gap.feature` | `GetStaffingGap` — a path below plan is flagged `PathUnderstaffed` |

Step definitions and the suite entry point (`TestFeatures`) live in
`features/features_test.go` (not at the repo root, ADR 0038). CI runs them as a dedicated blocking `bdd`
job.

Scenarios assert on the **RFC 7807 problem type**, not on message prose:

```gherkin
Scenario: Assigning an uncertified associate to a path is rejected
  Given an AssociateShift is started for associate "assoc-2" with certifications "pick"
  When associate "assoc-2" is assigned to path "pack"
  Then the assignment is rejected with status 409 and problem type "certification-required"
```

That is only possible because of
[ADR 0005](./0005-rfc-7807-problem-details.md) — with the old bespoke error
shape the assertion would have been a string match on English.

## Consequences

**Easier**

- The four named invariants are stated in domain language, executably, at the
  layer clients actually use. A rule that holds in the domain but is bypassed by
  an adapter now fails a test.
- The feature files double as the most readable available description of what
  the service does. They open with the domain narrative, not with setup code.
- Ubiquitous language is enforced by usage: the steps say `AssociateShift`,
  `LaborAssignment`, `ShiftPlan`, `PathUnderstaffed`.
- Running against in-memory adapters keeps the suite fast and infrastructure-free
  — a direct benefit of [ADR 0001](./0001-hexagonal-ports-and-adapters.md).

**Harder**

- A second test vocabulary to maintain alongside table-driven Go tests, plus
  ~500 lines of step definitions that are pure glue.
- Gherkin encourages over-specification. The discipline is one scenario per
  *rule*, not per code path — exhaustive branch coverage stays in the Go tests.
- The specs run against in-memory adapters, so they prove the contract holds
  over the real router but say nothing about the Postgres adapters. That is what
  the build-tagged integration tests are for.

**Now true**

- CI has a dedicated blocking `bdd` job.
- New invariants are expected to arrive with a scenario, not only a unit test.
- `go test ./... -run TestFeatures -v` is the fastest way for a newcomer to see
  what this service guarantees.
