---
id: 0030-kafka-sourced-process-path-catalogue
slug: /adr/0030-kafka-sourced-process-path-catalogue
title: 0030. Kafka-sourced process-path catalogue (PATH_CATALOGUE_SOURCE=kafka)
sidebar_label: 0030. Kafka-sourced path catalogue
sidebar_position: 31
description: "ADR 0030 — the process-path catalogue ADR-0013 validates path ids against can be sourced either from the boot-time YAML file or from process-path-management's compacted Kafka topic (PATH_CATALOGUE_SOURCE=kafka), with a boot-time replay gate; recorded retroactively by the 2026-10 ADR-conformance pass."
---

# 0030. Kafka-sourced process-path catalogue (`PATH_CATALOGUE_SOURCE=kafka`)

## Status

Accepted — recorded retroactively (2026-10 ADR-conformance pass) for a
source that shipped as an ADR-0013 extension without its own record.

## Context

ADR-0013 loads the fleet's published-language process-path catalogue from
`warehouse-infra`'s YAML file at boot. The file is a snapshot: every
consumer redeploys to see a catalogue change, and the fleet's catalogue
owner (process-path-management) already publishes every declaration and
compaction to a Kafka topic so consumers can track it live.

## Decision

`PATH_CATALOGUE_SOURCE` selects the catalogue's source:

- `file` (default): `filecatalog.Load(PATH_CATALOGUE_FILE)` — unchanged
  ADR-0013 behaviour.
- `kafka`: `kafkacatalog.NewConsumer` subscribes to process-path
  management's compacted topic, applies declarations/compactions in
  offset order into an in-memory catalogue, and exposes the same
  `ports.PathCatalogue` interface, so every validating handler and use
  case is source-agnostic.

Both sources share ONE fail-closed boot contract, enforced in
`composition.BuildCatalogue` (used by both cmd/workforce and cmd/mcp):
an unreadable file, unset `KAFKA_BROKERS`, or a topic that does not
replay its history within `WaitReadyTimeout` stops the process before it
serves — there is no partial or empty-catalogue operating mode, ever.

The consumer's group id is per-process-unique (`hostname+PID+timestamp`),
so N api replicas each replay the full history independently — safe by
design, and one of the facts ADR-0024's HPA table records.

## Consequences

- Catalogue changes reach every pod without a redeploy, in topic order.
- Kafka becomes a boot dependency when the kafka source is selected —
  deliberately, behind the same fail-closed gate as the file source.
- ADR-0013's validation semantics (case-insensitive prefix-family
  `Lookup`) are identical for both sources; only the load path differs.
