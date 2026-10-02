package kafka

import (
	"context"
	"fmt"

	segmentio "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// RelaySink writes already-encoded outbox messages to whichever topic each
// one names. It is what postgres.OutboxRelay drains into (ADR 0016).
//
// Unlike Publisher and AnalyticsPublisher its Writer has NO fixed Topic:
// kafka-go requires exactly one of Writer.Topic / Message.Topic to be set,
// and a single relay has to serve both warehouse.workforce.events and
// warehouse.workforce.analytics from one outbox table, so the topic rides on
// each message instead.
type RelaySink struct {
	writer Writer
}

// NewRelaySink constructs a RelaySink over brokers with a topic-less writer.
func NewRelaySink(brokers []string) *RelaySink {
	return NewRelaySinkWithWriter(&segmentio.Writer{
		BatchTimeout:           syncWriterBatchTimeout,
		Addr:                   segmentio.TCP(brokers...),
		Balancer:               &segmentio.Hash{},
		AllowAutoTopicCreation: true,
	})
}

// NewRelaySinkWithWriter constructs a RelaySink over an explicit Writer (a
// fake in tests). The writer must NOT have a Topic set.
func NewRelaySinkWithWriter(writer Writer) *RelaySink {
	return &RelaySink{writer: writer}
}

// Send writes msgs in one WriteMessages call, each addressed to its own
// Encoded.Topic. Headers persisted with the outbox row (the W3C trace
// context captured when the event was raised) are forwarded untouched, so
// the consumer's span still parents onto the originating request's trace
// rather than onto the relay's.
func (s *RelaySink) Send(ctx context.Context, msgs ...Encoded) error {
	if len(msgs) == 0 {
		return nil
	}
	ctx, span := otel.Tracer(tracerName).Start(ctx, "kafka.publish outbox",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			semconv.MessagingSystemKafka,
			semconv.MessagingOperationName("publish"),
			semconv.MessagingBatchMessageCount(len(msgs)),
		),
	)
	defer span.End()

	out := make([]segmentio.Message, len(msgs))
	for i, m := range msgs {
		if m.Topic == "" {
			err := fmt.Errorf("kafka relay sink: message %d (%s) has no topic", i, m.EventType)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
		out[i] = m.message(true)
	}
	if err := s.writer.WriteMessages(ctx, out...); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("kafka relay sink: write %d message(s): %w", len(out), err)
	}
	return nil
}

// Close releases the underlying Kafka writer.
func (s *RelaySink) Close() error {
	if w, ok := s.writer.(*segmentio.Writer); ok {
		return w.Close()
	}
	return nil
}
