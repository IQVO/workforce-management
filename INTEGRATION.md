# Cross-service integration (additive — Task 7, do NOT touch existing domain code)

> **Historical brief.** This is the original Task 7 specification. Its flat
> envelope is RETIRED: every message is now a CloudEvents 1.0 event
> (ADR-0026, shown below). The "does not need to consume
> anything" and `docker-compose.kafka.yml` statements below describe that
> round only. For the current edges (two opt-in consumed topics, two
> synchronous sibling reads, the transactional outbox, the in-cluster shared
> broker), see `docs/docs/ecosystem/integration.md` and
> `.claude/rules/integrations.md`.

This service PUBLISHES `ShiftPlanCommitted` over Kafka to a shared broker. This
round it does not need to consume anything. Strictly additive: new adapter
only, no change to existing aggregates, invariants, or use cases.

## Envelope (CloudEvents 1.0, mandatory — ADR-0026)

```json
{
  "specversion": "1.0",
  "id": "uuid-v4",
  "source": "/warehouse/workforce-management",
  "type": "com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted",
  "subject": "<building_id>/<shift_id>",
  "datacontenttype": "application/json",
  "dataschema": "urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1",
  "time": "2026-08-21T22:00:00Z",
  "data": {
    "building_id": "...",
    "shift_id": "...",
    "path_id": "...",
    "planned_heads": 3,
    "planned_rate": 30,
    "planned_hours": 24
  }
}
```

Note: a `ShiftPlan` has multiple `PathPlan` lines. Publish one
`ShiftPlanCommitted` event PER path line (i.e. `CommitShiftPlan` with 3 path
lines emits 3 Kafka messages, one per path), each carrying that single path's
`planned_heads/rate/hours`. This matches how the downstream consumer
(wes-work-planning) keys its read model — by `path_id`, one row per path.

## Kafka

- Client library: `github.com/segmentio/kafka-go`.
- Broker: `KAFKA_BROKERS` env var (default `localhost:9092`). A shared broker
  already runs via `~/warehouse-systems/docker-compose.kafka.yml` — connect to
  it, do not add your own Kafka service to this repo's docker-compose.yml.
- New adapter package `internal/adapters/outbound/kafka/` implementing the
  existing `ports.EventPublisher` interface. Select via env
  (`EVENT_PUBLISHER=kafka|log`, default `log` so existing tests are unaffected).
- Topic: `warehouse.workforce.events`.
- Publish `ShiftPlanCommitted` (one message per path line, as above) when
  `CommitShiftPlan` succeeds.

Downstream consumer: wes-work-planning projects these into its own
`LaborPlanObserved` read model, by `path_id`.

## Definition of done for Task 7

- New Kafka publisher adapter compiles and is unit-tested (e.g. against an
  in-memory kafka-go writer fake, or by asserting the CloudEvents shape and the
  one-message-per-path-line fan-out).
- Existing full suite (`go build ./...`, `go vet ./...`, `go test ./...`,
  `go test ./... -race`) still green, unchanged.
- README gains an "Integration" section: topic published, exact JSON schema
  above (including the one-event-per-path-line note), the
  `KAFKA_BROKERS`/`EVENT_PUBLISHER` env vars.
- Do a REAL smoke test: with the shared broker running and `EVENT_PUBLISHER=kafka`,
  actually call `POST /shift-plans` with 2+ path lines against the running
  binary and confirm 2+ messages land on `warehouse.workforce.events` via
  `kafka-console-consumer.sh --from-beginning` (or an equivalent one-off Go
  consumer) before declaring done.
