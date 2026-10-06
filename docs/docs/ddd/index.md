---
id: index
slug: /ddd
title: Domain-Driven Design
sidebar_label: Overview
sidebar_position: 1
description: Subdomain classification, aggregates, invariants, domain events and context relationships.
---

# Domain-Driven Design

The tactical model of this bounded context, and its place in the strategic
picture.

| Page | Answers |
| --- | --- |
| [Subdomain classification](./subdomain-classification.md) | Core, Supporting or Generic — and on whose authority |
| [Aggregates](./aggregates.md) | The three aggregate roots, their consistency boundaries, and why they are drawn where they are |
| [Invariants](./invariants.md) | Every rule enforced in the domain layer, with the test that pins it down |
| [Domain events](./domain-events.md) | All ten events, what raises them, and which ones leave the process |
| [Context relationships](./context-relationships.md) | Upstream, downstream, and the deliberate non-relationships |
| [DDD artifacts (ddd-crew)](./ddd-artifacts.md) | The ddd-crew artifact pack — core domain chart, bounded context and aggregate design canvases, message flows, EventStorming — plus UML class, ER and sequence diagrams |

The two reference documents behind this model are the platform-level
`warehouse-systems-ddd.md` (WMS/WES/WCS layering, worker-assignment ownership,
context-mapping vocabulary) and `amazon-fulfillment-ddd.md` (subdomain
classification, aggregates, domain events, glossary). Where this page cites a
classification or a relationship pattern, it is citing those.
