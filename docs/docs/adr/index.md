---
id: index
slug: /adr
title: Architecture Decision Records
sidebar_label: About ADRs
sidebar_position: 1
description: Why these records exist, the template they follow, and how to propose a new one.
---

# Architecture Decision Records

An **Architecture Decision Record** captures one architecturally significant
decision: the forces that produced it, the decision itself, and what it costs.
Not a design document, not a spec — a record of a choice, written once, never
edited after acceptance.

## Why they exist

Code shows *what* was built. Git history shows *when*. Neither reliably shows
*why*, and "why" is what determines whether a future change is safe.

Two examples from this repo where the reasoning is genuinely non-obvious from
the code:

- `LaborAssignment` is keyed by `AssociateId`, not by an `AssignmentId`. Reading
  the aggregate, that looks like a modelling shortcut. It is the mechanism that
  makes "exactly one active assignment per associate" structurally
  impossible to violate. Without
  [ADR 0003](./0003-certification-gated-single-active-assignment.md), a future
  refactor to "proper" per-assignment identity would silently downgrade a hard
  invariant into a hopeful query.
- There is no `taskId` anywhere in this service. That reads as incompleteness.
  It is [ADR 0002](./0002-stop-at-the-path-boundary.md), and it is the most
  consequential decision in the codebase.

## The template

These records use [Michael Nygard's
format](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions),
the de facto standard: one Markdown file per decision, numbered `0001-`,
`0002-`, …, with five sections.

```markdown
# NNNN. Short noun phrase naming the decision

## Status
Proposed | Accepted | Deprecated | Superseded by [ADR-NNNN](./nnnn-....md)

## Context
The forces at play: technical, organisational, domain. Written in
value-neutral language — describe the situation, do not argue for the
outcome yet.

## Decision
"We will ..." — active voice, one clear choice.

## Consequences
What becomes easier, what becomes harder, what is now true that was not
before. Include the costs. An ADR with only benefits is marketing.
```

Records are **immutable once accepted**. A decision that no longer holds is not
edited — it is marked `Superseded by ADR-NNNN`, and the new record explains what
changed.

## Proposing a new one

1. Copy the template above into `docs/docs/adr/NNNN-short-title.md`, taking the
   next free number.
2. Write it with status `Proposed`.
3. Add it to the `Architecture Decision Records` category in
   `docs/sidebars.ts`.
4. Open a pull request. The discussion happens on the PR, not in the document.
5. On merge, flip the status to `Accepted`.

If a decision turns out to be wrong, write a new ADR that supersedes it. Do not
quietly delete the old one — the fact that it was tried is itself information.

## The records

| # | Title | Status |
| --- | --- | --- |
| [0001](./0001-hexagonal-ports-and-adapters.md) | Hexagonal (ports and adapters) architecture | Accepted |
| [0002](./0002-stop-at-the-path-boundary.md) | Stop at the path boundary — no associate-to-task link | Accepted |
| [0003](./0003-certification-gated-single-active-assignment.md) | Certification-gated assignment with exactly one active assignment per associate | Accepted |
| [0004](./0004-kafka-integration-events-and-cloudevents-catalog.md) | Kafka for integration events, with a CloudEvents catalog ahead of the wire format | Accepted; envelope superseded by [0026](./0026-cloudevents-mandatory-event-envelope.md) |
| [0005](./0005-rfc-7807-problem-details.md) | RFC 7807 Problem Details for every error response | Accepted |
| [0006](./0006-godog-bdd-acceptance-tests.md) | godog acceptance specs driven through the real HTTP surface | Accepted |
| [0007](./0007-arch-go-architecture-fitness-tests.md) | arch-go fitness tests to make the layering rule executable | Accepted |
| [0008](./0008-mcp-inbound-adapter.md) | Model Context Protocol as an inbound adapter, not a new service | Accepted; auth/static-bearer sections superseded by [0018](./0018-remove-fleet-rest-identity.md) |
| [0009](./0009-hazmat-certification-via-existing-path-gating.md) | Hazmat handling via the existing path-name-equals-certification-name gate | Accepted; superseded in part by [0013](./0013-process-path-catalogue-validation.md) |
| [0010](./0010-analytical-data-product.md) | Per-service analytical data product (report) via a separate analytics topic | Accepted; Envelope v1 superseded by [0026](./0026-cloudevents-mandatory-event-envelope.md) |
| [0011](./0011-adopt-fleet-mfe-console-architecture.md) | Adopt the fleet-wide micro-frontend console architecture (workforce-mfe) | Accepted; deferred all-paths endpoint recorded in [0029](./0029-all-paths-staffing-gap-endpoint.md) |
| [0012](./0012-measured-rate-feed-for-propose-path-plan.md) | Close-the-loop measured rate feed from labor-performance into ProposePathPlan | Accepted |
| [0013](./0013-process-path-catalogue-validation.md) | Process-path catalogue validation, mirroring fulfillment-execution's ADR-0017 | Accepted |
| [0014](./0014-installed-capacity-ceiling.md) | Live installed-capacity ceiling on CommitShiftPlan, sourced from fulfillment-execution | Accepted |
| [0015](./0015-standard-metrics-convention.md) | Standard metrics convention across the fleet | Accepted |
| [0016](./0016-transactional-outbox.md) | Transactional outbox feeding both Kafka topics from one table | Accepted |
| [0017](./0017-adopt-fleet-rest-identity.md) | Adopt the fleet REST identity — static bearer keys with read/read-write scopes | Superseded by [0018](./0018-remove-fleet-rest-identity.md) |
| [0018](./0018-remove-fleet-rest-identity.md) | Remove the fleet REST identity layer (revert ADR-0017) | Accepted |
| [0019](./0019-labor-performance-cache-consumer.md) | Replace the synchronous labor-performance MeasuredRateClient with an event-fed local cache | Accepted |
| [0020](./0020-idle-share-staffing-signal.md) | Consume labor-performance's idle-share signal for staffing surfacing and proposal trim | Accepted |
| [0021](./0021-optimistic-concurrency-version-column.md) | Optimistic concurrency (version column) on AssociateShift and LaborAssignment | Accepted |
| [0022](./0022-resilience-circuit-breakers-retry-dlq-shutdown.md) | Per-dependency circuit breakers, read-only retry, Kafka DLQ, and graceful shutdown hardening | Accepted |
| [0023](./0023-kafka-integration-producer-partition-key.md) | Key the integration Kafka publisher by ShiftPlan aggregate id | Accepted |
| [0024](./0024-horizontal-autoscaling-and-pgxpool-tuning.md) | Per-workload HorizontalPodAutoscaler and pgxpool MaxConns/statement_timeout tuning | Accepted |
| [0025](./0025-migrations-direct-postgres-connection.md) | Run golang-migrate against a direct Postgres connection, not PgBouncer | Accepted |
| [0026](./0026-cloudevents-mandatory-event-envelope.md) | CloudEvents 1.0 as the mandatory event envelope | Accepted |
| [0027](./0027-idempotency-key-middleware.md) | Transactional Idempotency-Key middleware on the creation POSTs | Accepted |
| [0028](./0028-housekeeping-sweeper-idempotency-keys-and-outbox.md) | Housekeeping sweeper for idempotency keys and published outbox rows | Accepted |
| [0029](./0029-all-paths-staffing-gap-endpoint.md) | All-paths staffing-gap list endpoint for a building/shift | Accepted |
| [0030](./0030-kafka-sourced-process-path-catalogue.md) | Kafka-sourced process-path catalogue (PATH_CATALOGUE_SOURCE=kafka) | Accepted |
| [0031](./0031-kafka-writer-durability.md) | Kafka writer durability: RequireAll acks and a 10ms batch timeout | Accepted |
| [0032](./0032-bootretry-first-outbound-dial.md) | Boot-time retry for the first outbound dial (Istio sidecar warm-up) | Accepted |
| [0033](./0033-docs-audit-corrections-2026-10.md) | Corrections from the 2026-10-05 docs audit (idempotent-request atomicity, measured-rate unit, idle-share naming, /readyz) | Accepted |
| [0034](./0034-site-scoped-staffing-gap.md) | Site-scoped staffing gap (optional canonical siteCode) | Accepted |
| [0035](./0035-sitecode-converges-building-id.md) | siteCode is the canonical name of the ShiftPlan key; buildingId is a deprecated alias | Accepted |
