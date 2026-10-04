---
paths:
  - "internal/adapters/**"
  - "internal/domain/shared/**"
  - "apis/asyncapi*"
  - "features/**"
---

# CloudEvents 1.0 envelope — attributes, naming, this service's types

Every Kafka message this service produces or consumes (integration
`warehouse.<ctx>.events` AND analytics `warehouse.<ctx>.analytics`) is a
CloudEvents 1.0 event in structured content mode (ADR-0026). This is a hard
fleet rule, not a preference:

- No flat envelope (`event_id`/`event_type`/`occurred_at`), no dual-write,
  no dual-read, no envelope toggle env var (`EVENT_ENVELOPE_MODE` is gone).
- Build/validate/(un)marshal with `github.com/cloudevents/sdk-go/v2/event`
  via `internal/adapters/kafka/cloudevents/` (the ONLY builder/decoder);
  transport stays kafka-go.
- Kafka header `content-type: application/cloudevents+json; charset=UTF-8`.
- Required attributes: `specversion=1.0`, `id` (UUID, stable across outbox
  redelivery), `source=/warehouse/workforce-management`, `type`, `subject` (aggregate id), `time`
  (occurred-at, UTC), `datacontenttype=application/json`,
  `dataschema=urn:warehouse:workforce-management:<events|analytics>:<EventName>:v<N>`.
- `type` = `com.warehouse.<subdomain>.<bounded-context>.<entity>.<EventName>`;
  for this service: `com.warehouse.wes.workforce-management.<entity>.<EventName>`. Breaking payload
  change => new `.v2` type + new dataschema version, never mutate.
- Consumers dispatch on the FULL `type`, ignore unknown types, dedupe on
  `id`, and DLQ/skip (never crash, never parse a legacy shape) anything that
  fails CloudEvents validation.

Full standard and the fleet's cross-service type catalogue: ADR-0026
(`docs/docs/adr/`).

Entity segments: `shiftplan` (ShiftPlanCommitted/Proposed, PathUnderstaffed),
`associate` (AssociateShift*/Break*/Certified), `assignment`
(LaborAssigned/Reassigned). `ShiftPlanCommitted` is fanned out one message per
`PathPlan` line, each with its own `id`; `subject` = Kafka key =
`<buildingId>/<shiftId>`. Exact type strings live in
`internal/adapters/kafka/cloudevents/types.go`.

See also `integrations.md` (example payload, outbox, consumers).
