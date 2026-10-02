// Package kafka provides a Kafka-backed ports.EventPublisher implementation.
// It publishes to the shared cross-service broker described in
// INTEGRATION.md: one ShiftPlanCommitted CloudEvent per PathPlan line, on topic
// warehouse.workforce.events (ADR-0026).
package kafka

import (
	"context"
	"crypto/rand"
	"fmt"

	segmentio "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	"github.com/claudioed/workforce-management/internal/application/ports"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// Topic is the shared warehouse-systems topic this service publishes to.
const Topic = "warehouse.workforce.events"

// tracerName identifies this adapter's instrumentation scope.
const tracerName = "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"

// integrationSchemaVersion is the dataschema version of the integration
// ShiftPlanCommitted payload (urn:warehouse:workforce-management:events:ShiftPlanCommitted:v1).
const integrationSchemaVersion = 1

// shiftPlanCommittedData is the CloudEvents `data` payload of one
// ShiftPlanCommitted PathPlan-line message — byte-for-byte the payload shape
// this topic has always carried.
type shiftPlanCommittedData struct {
	BuildingId   string  `json:"building_id"`
	ShiftId      string  `json:"shift_id"`
	PathId       string  `json:"path_id"`
	PlannedHeads int     `json:"planned_heads"`
	PlannedRate  float64 `json:"planned_rate"`
	PlannedHours float64 `json:"planned_hours"`
}

// Publisher implements ports.EventPublisher by publishing to Kafka. The
// domain-level ShiftPlanCommitted event carries only buildingId/shiftId (a
// ShiftPlan's identity); ShiftPlans is used to load the committed plan's
// PathPlan lines so one message can be fanned out per line, as
// INTEGRATION.md requires.
//
// Because Encode reads ShiftPlans, the transactional outbox (ADR 0016)
// calls Encode INSIDE the use case's transaction — that is the only way the
// lookup sees the plan the same transaction just saved.
type Publisher struct {
	writer     Writer
	shiftPlans ports.ShiftPlanRepo
	// newID mints each line message's CloudEvents id (NewEventID in
	// production; pinned in golden tests).
	newID func() string
}

// NewPublisher constructs a Publisher writing to brokers on Topic.
func NewPublisher(brokers []string, shiftPlans ports.ShiftPlanRepo) *Publisher {
	return NewPublisherWithWriter(&segmentio.Writer{
		BatchTimeout:           syncWriterBatchTimeout,
		Addr:                   segmentio.TCP(brokers...),
		Topic:                  Topic,
		Balancer:               &segmentio.Hash{},
		AllowAutoTopicCreation: true,
	}, shiftPlans)
}

// NewPublisherWithWriter constructs a Publisher over an explicit Writer
// (a fake in tests). writer is expected to have Topic pinned to Topic, as
// NewPublisher does; Encode leaves Message.Topic empty accordingly.
func NewPublisherWithWriter(writer Writer, shiftPlans ports.ShiftPlanRepo) *Publisher {
	return &Publisher{writer: writer, shiftPlans: shiftPlans, newID: NewEventID}
}

// Close releases the underlying Kafka writer's resources.
func (p *Publisher) Close() error {
	if w, ok := p.writer.(*segmentio.Writer); ok {
		return w.Close()
	}
	return nil
}

// Encode fans ShiftPlanCommitted events out into one wire-ready CloudEvents
// 1.0 structured-mode message per PathPlan line (ADR-0026). Every line message
// gets its OWN freshly minted CloudEvents `id` (minted here, once, so the
// outbox persists and later republishes the exact same id), `type`
// com.warehouse.wes.workforce-management.shiftplan.ShiftPlanCommitted, and
// `subject` = the ShiftPlan aggregate id "<buildingId>/<shiftId>" — the same
// value as the Kafka key; the line's path_id stays in `data`. Other event types are ignored: this round only publishes
// ShiftPlanCommitted, per INTEGRATION.md. Every message is keyed with
// shiftPlanKey(buildingId, shiftId) — the ShiftPlan aggregate's identity
// (ShiftPlan is keyed by building + shift, see internal/domain/shiftplan) —
// so every PathPlan-line message for the same commit, and every subsequent
// commit for the same building/shift, is routed to the same partition and
// consumed in order regardless of the topic's partition count (ADR-0023;
// mirrors AnalyticsPublisher's existing aggregate-id keying).
//
// The current span context (if any) is injected into every message's
// headers so downstream services' consume spans are children of the span
// active when the event was raised — whether the message is written
// straight away by Publish or persisted to the outbox and written later
// by the relay.
func (p *Publisher) Encode(ctx context.Context, events ...shared.DomainEvent) ([]Encoded, error) {
	var out []Encoded
	propagator := otel.GetTextMapPropagator()
	for _, e := range events {
		committed, ok := e.(shared.ShiftPlanCommitted)
		if !ok {
			continue
		}
		sp, err := p.shiftPlans.FindByBuildingAndShift(ctx, committed.BuildingId, committed.ShiftId)
		if err != nil {
			return nil, fmt.Errorf("kafka publisher: load committed shift plan: %w", err)
		}
		key := []byte(shiftPlanKey(committed.BuildingId, committed.ShiftId))
		for _, line := range sp.Lines() {
			b, err := cloudevents.New(cloudevents.Spec{
				ID:        p.newID(),
				Entity:    cloudevents.EntityShiftPlan,
				EventName: committed.EventName(),
				Subject:   string(key),
				Time:      committed.OccurredAt(),
				Stream:    cloudevents.StreamEvents,
				Version:   integrationSchemaVersion,
				Data: shiftPlanCommittedData{
					BuildingId:   committed.BuildingId,
					ShiftId:      committed.ShiftId,
					PathId:       string(line.PathId),
					PlannedHeads: line.PlannedHeads,
					PlannedRate:  line.PlannedRate,
					PlannedHours: line.PlannedHours,
				},
			})
			if err != nil {
				return nil, fmt.Errorf("kafka publisher: encode cloudevent: %w", err)
			}
			enc := Encoded{
				Topic:     Topic,
				EventType: cloudevents.TypeShiftPlanCommitted,
				Key:       key,
				Value:     b,
				Headers:   []segmentio.Header{cloudevents.ContentTypeHeader()},
			}
			propagator.Inject(ctx, propagation.TextMapCarrier(headerCarrier{headers: &enc.Headers}))
			out = append(out, enc)
		}
	}
	return out, nil
}

// Publish encodes events (see Encode) and writes the result to Topic in one
// broker round-trip, inside a `kafka.publish <topic>` producer span.
func (p *Publisher) Publish(ctx context.Context, events ...shared.DomainEvent) error {
	encoded, err := p.Encode(ctx, events...)
	if err != nil {
		return err
	}
	if len(encoded) == 0 {
		return nil
	}
	return p.writeMessages(ctx, encoded)
}

// writeMessages wraps the broker write in a `kafka.publish <topic>` span (per
// the OTel messaging semantic conventions) and injects that span's context
// into every outgoing message's headers, superseding the headers Encode
// captured (the publish span is a child of the same trace, so nothing is
// lost for the consumer).
func (p *Publisher) writeMessages(ctx context.Context, encoded []Encoded) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "kafka.publish "+Topic,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			semconv.MessagingSystemKafka,
			semconv.MessagingOperationName("publish"),
			semconv.MessagingDestinationName(Topic),
			semconv.MessagingBatchMessageCount(len(encoded)),
		),
	)
	defer span.End()

	propagator := otel.GetTextMapPropagator()
	msgs := make([]segmentio.Message, len(encoded))
	for i, enc := range encoded {
		msgs[i] = enc.message(false)
		propagator.Inject(ctx, propagation.TextMapCarrier(headerCarrier{headers: &msgs[i].Headers}))
	}

	if err := p.writer.WriteMessages(ctx, msgs...); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// shiftPlanKey builds the Kafka partition key for a ShiftPlan aggregate.
// ShiftPlan has no single-field id — it is keyed by (buildingId, shiftId)
// everywhere else in this codebase (see internal/domain/shiftplan and the
// postgres repo's composite primary key) — so the message key mirrors that
// same composite identity rather than inventing a new one.
func shiftPlanKey(buildingId, shiftId string) string {
	return buildingId + "/" + shiftId
}

// NewEventID generates a random UUID v4 without pulling in a UUID
// dependency. It is exported so a composition root
// can supply it as the analytics publisher's CloudEvents id minter without
// duplicating the generator.
func NewEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Compile-time assertions: Publisher is both a direct publisher and an
// outbox-feeding Encoder.
var (
	_ ports.EventPublisher = (*Publisher)(nil)
	_ Encoder              = (*Publisher)(nil)
)
