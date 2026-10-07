package kafka

import (
	"context"
	"encoding/json"
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

// AnalyticsTopic is the dedicated topic the analytics data product consumes.
// It is separate from the integration topic (Topic) so the OLTP integration
// contract and the analytical read-model stream evolve independently
// (ADR-0010).
const AnalyticsTopic = "warehouse.workforce.analytics"

// analyticsSchemaVersion is the dataschema version stamped onto every
// analytics CloudEvent (urn:warehouse:workforce-management:analytics:<Event>:v1).
// It replaces the retired Envelope v1 schema_version field.
const analyticsSchemaVersion = 1

// AnalyticsPublisher publishes every workforce-management domain event onto
// AnalyticsTopic as a CloudEvents 1.0 structured-mode event (ADR-0026). It satisfies ports.EventPublisher and
// is a SEPARATE adapter from Publisher: the integration publisher (publisher.go)
// forwards only ShiftPlanCommitted and is left untouched.
//
// The message key is the aggregate id — AssociateId for associate-scoped events
// and PathId for path-scoped events — so per-aggregate ordering is preserved on
// the topic. The CloudEvents `subject` is the same id, except ShiftPlanCommitted
// whose subject is the ShiftPlan aggregate id "<buildingId>/<shiftId>" (its key
// stays the building id, unchanged).
type AnalyticsPublisher struct {
	Writer Writer
	NewId  func() string
}

// NewAnalyticsPublisher constructs an AnalyticsPublisher writing to
// AnalyticsTopic on brokers. newId mints the CloudEvents id.
func NewAnalyticsPublisher(brokers []string, newId func() string) *AnalyticsPublisher {
	return NewAnalyticsPublisherWithWriter(&segmentio.Writer{
		BatchTimeout:           syncWriterBatchTimeout,
		RequiredAcks:           syncWriterRequiredAcks,
		Addr:                   segmentio.TCP(brokers...),
		Topic:                  AnalyticsTopic,
		Balancer:               &segmentio.Hash{},
		AllowAutoTopicCreation: true,
	}, newId)
}

// NewAnalyticsPublisherWithWriter constructs an AnalyticsPublisher over an
// explicit Writer (a fake in tests). writer is expected to have Topic
// pinned to AnalyticsTopic, as NewAnalyticsPublisher does.
func NewAnalyticsPublisherWithWriter(writer Writer, newId func() string) *AnalyticsPublisher {
	return &AnalyticsPublisher{Writer: writer, NewId: newId}
}

// Encode turns every event in evts into a wire-ready CloudEvents message for
// AnalyticsTopic, keyed by aggregate id, carrying the content-type header. Events with no
// analytics payload (an unrecognised type) are skipped rather than
// erroring, so the caller can hand it the full event stream
// indiscriminately. The current span context (if any) is injected into
// each message's headers.
func (p *AnalyticsPublisher) Encode(ctx context.Context, evts ...shared.DomainEvent) ([]Encoded, error) {
	var out []Encoded
	propagator := otel.GetTextMapPropagator()
	for _, e := range evts {
		a, ok := marshalAnalyticsData(e)
		if !ok {
			continue
		}
		payload, err := cloudevents.New(cloudevents.Spec{
			ID:        p.NewId(),
			Entity:    a.entity,
			EventName: a.eventName,
			Subject:   a.subject,
			Time:      e.OccurredAt(),
			Stream:    cloudevents.StreamAnalytics,
			Version:   analyticsSchemaVersion,
			Data:      a.data,
		})
		if err != nil {
			return nil, fmt.Errorf("kafka: encode analytics cloudevent: %w", err)
		}
		enc := Encoded{
			Topic:     AnalyticsTopic,
			EventType: cloudevents.Type(a.entity, a.eventName),
			Key:       []byte(a.key),
			Value:     payload,
			Headers:   []segmentio.Header{cloudevents.ContentTypeHeader()},
		}
		propagator.Inject(ctx, propagation.TextMapCarrier(headerCarrier{headers: &enc.Headers}))
		out = append(out, enc)
	}
	return out, nil
}

// Publish emits every event in evts onto AnalyticsTopic, one broker write
// per message (see Encode for which events are skipped).
func (p *AnalyticsPublisher) Publish(ctx context.Context, evts ...shared.DomainEvent) error {
	encoded, err := p.Encode(ctx, evts...)
	if err != nil {
		return err
	}
	for _, enc := range encoded {
		if err := p.write(ctx, enc); err != nil {
			return err
		}
	}
	return nil
}

// analyticsEvent is one domain event's analytics projection: the CloudEvents
// entity/event name, the Kafka key, the CloudEvents subject and the snake_case
// JSON data payload.
type analyticsEvent struct {
	entity, eventName, key, subject string
	data                            json.RawMessage
}

func associateEvent(name string, id shared.AssociateId, data map[string]any) analyticsEvent {
	return analyticsEvent{entity: cloudevents.EntityAssociate, eventName: name, key: string(id), subject: string(id), data: mustMarshal(data)}
}

// marshalAnalyticsData maps a domain event to its analytics CloudEvent parts.
// The bool return is false for an event type outside the analytics contract,
// so Encode can skip it.
func marshalAnalyticsData(e shared.DomainEvent) (analyticsEvent, bool) {
	switch ev := e.(type) {
	case shared.AssociateShiftStarted:
		return associateEvent(ev.EventName(), ev.AssociateId, map[string]any{
			"associate_id": string(ev.AssociateId),
		}), true
	case shared.AssociateShiftEnded:
		return associateEvent(ev.EventName(), ev.AssociateId, map[string]any{
			"associate_id": string(ev.AssociateId),
		}), true
	case shared.AssociateBreakStarted:
		return associateEvent(ev.EventName(), ev.AssociateId, map[string]any{
			"associate_id": string(ev.AssociateId),
		}), true
	case shared.AssociateBreakEnded:
		return associateEvent(ev.EventName(), ev.AssociateId, map[string]any{
			"associate_id": string(ev.AssociateId),
		}), true
	case shared.AssociateCertified:
		return associateEvent(ev.EventName(), ev.AssociateId, map[string]any{
			"associate_id":  string(ev.AssociateId),
			"certification": string(ev.Certification),
		}), true
	case shared.LaborAssigned:
		return analyticsEvent{entity: cloudevents.EntityAssignment, eventName: ev.EventName(),
			key: string(ev.AssociateId), subject: string(ev.AssociateId),
			data: mustMarshal(map[string]any{
				"associate_id": string(ev.AssociateId),
				"path_id":      string(ev.PathId),
			})}, true
	case shared.LaborReassigned:
		return analyticsEvent{entity: cloudevents.EntityAssignment, eventName: ev.EventName(),
			key: string(ev.AssociateId), subject: string(ev.AssociateId),
			data: mustMarshal(map[string]any{
				"associate_id": string(ev.AssociateId),
				"from_path_id": string(ev.FromPathId),
				"to_path_id":   string(ev.ToPathId),
			})}, true
	case shared.PathUnderstaffed:
		data := map[string]any{
			"path_id":       string(ev.PathId),
			"planned_heads": ev.PlannedHeads,
			"active_heads":  ev.ActiveHeads,
		}
		// site_code is ADDITIVE on the v1 payload (ADR 0034): present only
		// when the gap was computed for one site, omitted for an unscoped
		// (fleet-wide) gap. Consumers must treat absence as "unscoped".
		if !ev.SiteCode.IsUnscoped() {
			data["site_code"] = string(ev.SiteCode)
		}
		return analyticsEvent{entity: cloudevents.EntityShiftPlan, eventName: ev.EventName(),
			key: string(ev.PathId), subject: string(ev.PathId),
			data: mustMarshal(data)}, true
	case shared.ShiftPlanProposed:
		return analyticsEvent{entity: cloudevents.EntityShiftPlan, eventName: ev.EventName(),
			key: string(ev.PathId), subject: string(ev.PathId),
			data: mustMarshal(map[string]any{
				"building_id":   ev.BuildingId,
				"path_id":       string(ev.PathId),
				"planned_heads": ev.PlannedHeads,
				"planned_rate":  ev.PlannedRate,
			})}, true
	case shared.ShiftPlanCommitted:
		return analyticsEvent{entity: cloudevents.EntityShiftPlan, eventName: ev.EventName(),
			key: ev.BuildingId, subject: shiftPlanKey(ev.BuildingId, ev.ShiftId),
			data: mustMarshal(map[string]any{
				"building_id": ev.BuildingId,
				"shift_id":    ev.ShiftId,
			})}, true
	default:
		return analyticsEvent{}, false
	}
}

// mustMarshal marshals a map whose shape is fully controlled by
// marshalAnalyticsData, so an error here is a programming mistake rather than a
// runtime condition.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("kafka: marshal analytics data: %v", err))
	}
	return b
}

// write publishes one already-encoded message inside a
// "kafka.publish <topic>" producer span, injecting that span's context into the
// message headers (via the shared headerCarrier) so the projector's consume
// span becomes its child.
func (p *AnalyticsPublisher) write(ctx context.Context, enc Encoded) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "kafka.publish "+AnalyticsTopic,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			semconv.MessagingSystemKafka,
			semconv.MessagingOperationName("publish"),
			semconv.MessagingDestinationName(AnalyticsTopic),
		),
	)
	defer span.End()

	msg := enc.message(false)
	otel.GetTextMapPropagator().Inject(ctx, propagation.TextMapCarrier(headerCarrier{headers: &msg.Headers}))

	if err := p.Writer.WriteMessages(ctx, msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("kafka: publish %s analytics event: %w", enc.EventType, err)
	}
	return nil
}

// Close releases the underlying Kafka writer.
func (p *AnalyticsPublisher) Close() error {
	if w, ok := p.Writer.(*segmentio.Writer); ok {
		return w.Close()
	}
	return nil
}

// Compile-time assertions that AnalyticsPublisher satisfies the outbound
// event-publishing port and is an outbox-feeding Encoder.
var (
	_ ports.EventPublisher = (*AnalyticsPublisher)(nil)
	_ Encoder              = (*AnalyticsPublisher)(nil)
)
