---
id: ddd-artifacts
title: DDD artifacts (ddd-crew)
sidebar_label: Artifact pack index
sidebar_position: 7
description: The ddd-crew artifact pack for workforce-management — core domain chart, bounded context canvas, context map, aggregate design canvas, domain message flows, EventStorming, ubiquitous language, UML class / ER / sequence diagrams and the domain-event catalog.
---

# DDD artifacts (ddd-crew)

The full [ddd-crew](https://github.com/ddd-crew) artifact pack for this
bounded context, plus UML and ER diagrams, all drawn as Mermaid and all
derived from the code on `develop`. Every diagram carries a **Source:** line
naming the files it was read from, and says what it leaves out.

| Artifact | Page | ddd-crew tool / notation |
| --- | --- | --- |
| Core Domain Chart | [Core domain chart](./core-domain-chart.md) | [core-domain-charts](https://github.com/ddd-crew/core-domain-charts) — Mermaid `quadrantChart` |
| Bounded Context Canvas | [Bounded context canvas](./bounded-context-canvas.md) | [bounded-context-canvas](https://github.com/ddd-crew/bounded-context-canvas) v5 |
| Context Map | [Ecosystem → Context map](../ecosystem/context-map.md) (kept in the Ecosystem section, not duplicated here) | [context-mapping](https://github.com/ddd-crew/context-mapping) — Mermaid `flowchart LR` |
| Aggregate Design Canvas | [Aggregate design canvas](./aggregate-design-canvas.md) | [aggregate-design-canvas](https://github.com/ddd-crew/aggregate-design-canvas) v1.1 — `stateDiagram-v2` per root |
| Domain Message Flow | [Domain message flow](./domain-message-flow.md) | [domain-message-flow-modelling](https://github.com/ddd-crew/domain-message-flow-modelling) — `sequenceDiagram` |
| EventStorming (design level) | [EventStorming](./eventstorming.md) | [eventstorming-glossary-cheat-sheet](https://github.com/ddd-crew/eventstorming-glossary-cheat-sheet) — `flowchart LR` with sticky colours |
| Ubiquitous Language | [Business Context → Ubiquitous language](../business-context/ubiquitous-language.md) (kept in Business Context, not duplicated here) | [welcome-to-ddd](https://github.com/ddd-crew/welcome-to-ddd) glossary, every term mapped to a Go identifier |
| UML class diagrams | [Class diagram](./class-diagram.md) | UML `classDiagram` with DDD stereotypes, plus a ports-and-adapters view |
| ER diagram | [Entity-relationship](./entity-relationship.md) | Mermaid `erDiagram` of the final schema after all migrations |
| UML sequence diagrams | [Sequence diagrams](./sequence-diagrams.md) | `sequenceDiagram` per command use case |
| Domain events | [Domain events](./domain-events.md) | Catalog: full CloudEvents `type`, topic, partition key, payload, producer, consumers |

The strategic pages that predate this pack stay where they are and are
referenced from it: [Subdomain classification](./subdomain-classification.md),
[Aggregates](./aggregates.md), [Invariants](./invariants.md) and
[Context relationships](./context-relationships.md).

## Sources of truth

Every artifact is derived from a source-of-truth file, never from older prose:

| Diagram kind | Read from |
| --- | --- |
| Aggregates, value objects, events, state transitions | `internal/domain/**` |
| Commands, use-case flows, error branches | `internal/application/usecases/*.go`, `internal/adapters/inbound/http/router.go`, `internal/adapters/inbound/mcp/tools.go` |
| Ports and adapters | `internal/application/ports/ports.go`, `internal/adapters/**`, `cmd/*/main.go` |
| Persistence | `migrations/*.up.sql` and `migrations/analytics/*.up.sql`, applied in order |
| Wire contracts | `internal/adapters/kafka/cloudevents/types.go`, `internal/adapters/outbound/kafka/*.go`, `apis/openapi.yaml`, `apis/asyncapi.yaml` |
| Relationships with siblings | the outbound adapter packages here, and the consumers in the sibling repos named on each page |
| Classification | [Subdomain classification](./subdomain-classification.md) and the ADRs |

Fleet-level artifacts — the EventStorming big picture and the DDD Starter
Modelling Process — live in the `warehouse-docs` aggregator, not here.

Throughput and size numbers on the aggregate design canvas are **estimates**
and are labelled as such. Everything else is a claim about the code; if the
code and a page disagree, the code is right and the page is a bug.
