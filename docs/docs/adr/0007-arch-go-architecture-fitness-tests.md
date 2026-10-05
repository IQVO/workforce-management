---
id: 0007-arch-go-architecture-fitness-tests
title: 0007. arch-go fitness tests to make the layering rule executable
sidebar_label: 0007. arch-go fitness tests
sidebar_position: 8
description: The hexagonal dependency rule as a failing test rather than a review comment.
---

# 0007. arch-go fitness tests to make the layering rule executable

## Status

Accepted. Added in commit `84d2305` ("Task 14: add architecture fitness tests
(arch-go) and CI gate").

## Context

[ADR 0001](./0001-hexagonal-ports-and-adapters.md) establishes the dependency
rule that everything else in this service depends on:

> domain depends on nothing; application depends on domain; adapters depend on
> application and domain.

Until this decision, that rule was enforced by three things, all of them
fallible: a paragraph in `CLAUDE.md`, the directory layout, and whoever was
reviewing the pull request.

Architectural erosion does not arrive as a bad commit. It arrives as a small,
locally reasonable one — a `pgx` type imported into a domain package to avoid a
conversion; an inbound handler reaching directly into a Postgres repo to skip a
use case for one read-only endpoint. Each is a two-line diff that looks fine in
isolation and is invisible in review a year later.

And the erosion is expensive here specifically, because every other quality
property of this service is *downstream* of the layering. Fast unit tests,
98.2% coverage without infrastructure, in-memory acceptance specs, adding the
Kafka publisher without touching a single use case — none of that survives a
domain package that imports `pgxpool`.

Java has ArchUnit for exactly this. Go's equivalent is
[arch-go](https://github.com/arch-go/arch-go), which loads the real package
graph and checks declared dependency rules against it.

## Decision

**Encode the hexagonal dependency rule as executable Go tests** in
`internal/architecture/architecture_test.go`, using
`github.com/arch-go/arch-go`, and **run them as a blocking CI job**.

`TestHexagonalDependencyRule` runs the rules as separate subtests, so a failure
names the specific rule that broke:

| Rule | Assertion |
| --- | --- |
| Domain purity | `internal/domain/...` imports nothing else from this module |
| Application boundary | `internal/application/...` imports only `internal/domain/...` |
| Adapter isolation (inbound) | `internal/adapters/inbound/...` never imports outbound adapters |
| Adapter isolation (outbound) | `internal/adapters/outbound/...` never imports inbound adapters |
| Composition root | only `cmd/` is permitted to wire everything together |

Failures are reported through a `reportFailure` helper that walks the arch-go
result and emits the **offending package and import** —

```
package "..." violates rule "...": ...
```

— rather than a bare "architecture test failed," so a CI failure points straight
at the import that caused it.

The job is wired into `docker-publish`'s `needs` list. An image with a layering
violation never reaches Docker Hub.

Strictly additive: no production code was changed. All subtests passed on their
first run — the codebase had zero existing violations, which is the outcome that
makes the rules worth locking in.

## Consequences

**Easier**

- The dependency rule is now a **property of the build**. It cannot be eroded by
  a reasonable-looking diff, and it does not depend on a reviewer remembering.
- Everything downstream of the layering is protected: the fast unit suite, the
  coverage number, the in-memory acceptance specs, adapter swappability.
- Onboarding gets a machine-checkable answer to "where does this code go?"
- The rule and its documentation cannot drift, because the rule *is* the test.

**Harder**

- One more CI job, and one more dependency.
- arch-go's glob patterns are permissive — `.` as a segment separator translates
  to a regex that also matches `/` — so the rules must be written and verified
  carefully rather than assumed. Getting a rule subtly wrong yields a test that
  passes without checking anything, which is worse than no test.
- arch-go loads packages with `Tests: false`, so `_test.go` imports are not
  analysed. Cross-layer imports in test files do not count as violations. That
  is usually what you want — a test may legitimately reach for an adapter — but
  it means the guarantee covers production code only, and that limit should be
  known rather than assumed away.
- A genuinely necessary exception would need a rule change, which is friction by
  design.

**Now true**

- CI has a blocking `arch-test` job, alongside `lint`, `test`, `integration`,
  `bdd`, `api-lint` (Spectral over apis/*.yaml) and `docs-api-drift`;
  `helm-lint` runs only on pull requests targeting `main`. (_Job names
  amended 2026-10 to match .github/workflows/ci.yml: the OpenAPI/AsyncAPI
  Spectral job is `api-lint`, and helm-lint is PRs-to-main only._)
- [ADR 0001](./0001-hexagonal-ports-and-adapters.md) is no longer merely
  aspirational; it is enforced.
