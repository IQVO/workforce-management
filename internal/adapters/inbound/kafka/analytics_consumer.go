// Package kafka provides the inbound Kafka adapter for the workforce analytics
// data product: a consumer that reads the analytics topic and applies each
// event to the Labor Utilization & Staffing projection, exactly once per
// event_id despite Kafka's at-least-once delivery.
//
// ADR-0022 (ported from order-management's ADR-0025) adds bounded
// in-process retry plus a dead-letter topic to this consumer: a
// genuinely poisoned event (one whose projection application keeps
// failing) is retried up to maxAnalyticsHandlerAttempts times with
// jittered backoff, then published raw to <topic>.dlq (analyticsDLQTopicSuffix)
// and its offset committed anyway — one poison message must never block
// every other event behind it on this partition.
//
// Run's retry loop deliberately does NOT simply call HandleMessage
// (envelope decode + MarkProcessed + apply, all in one) in a loop:
// ProcessedEvents.MarkProcessed is a one-shot "insert if absent" gate,
// not itself transactional with the projection Apply call that
// follows it, so calling MarkProcessed again on a retry of the SAME
// event_id would report isNew=false (already seen) and silently skip
// re-applying — turning a genuine transient projection failure into a
// falsely-successful no-op that never reaches the DLQ. Run's retry
// therefore marks-processed AT MOST ONCE per fetched message
// (retried on its OWN, since MarkProcessed can itself hit a transient
// infra error) and retries ONLY the apply step afterwards — see
// handleFetchedMessage below.
package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cenkalti/backoff/v4"
	segmentio "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/claudioed/workforce-management/internal/analytics/report"
	"github.com/claudioed/workforce-management/internal/application/ports"
)

// tracerName identifies this adapter's instrumentation scope.
const tracerName = "github.com/claudioed/workforce-management/internal/adapters/inbound/kafka"

// AnalyticsConsumerGroup is the Kafka consumer group the analytics projector
// reads under. It is distinct from any OLTP consumer group so the two pipelines
// track their offsets independently.
const AnalyticsConsumerGroup = "workforce-analytics"

// analyticsDLQTopicSuffix names the dead-letter topic this consumer
// publishes a poison message to, relative to its OWN source topic
// (never a fixed constant) — mirroring order-management's
// RepromiseConsumer dlqTopicSuffix convention exactly, so an isolated
// test topic gets its own isolated DLQ topic for free.
const analyticsDLQTopicSuffix = ".dlq"

// maxAnalyticsHandlerAttempts bounds each of Run's two retryable
// stages (MarkProcessed, and the projection apply step) at up to this
// many total attempts each (ADR-0022 §DLQ): 1 initial attempt plus up
// to 2 retries.
const maxAnalyticsHandlerAttempts = 3

const (
	analyticsRetryInitialInterval = 100 * time.Millisecond
	analyticsRetryMaxInterval     = 2 * time.Second
)

// headerCarrier adapts a kafka-go header slice to OTel's
// propagation.TextMapCarrier so the producer's trace context can be read off a
// consumed message. It mirrors the outbound adapter's carrier (kept local so
// this inbound adapter does not depend on the outbound one).
type headerCarrier struct {
	headers []segmentio.Header
}

func (c headerCarrier) Get(key string) string {
	for _, h := range c.headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func (c headerCarrier) Set(string, string) {}

func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(c.headers))
	for _, h := range c.headers {
		keys = append(keys, h.Key)
	}
	return keys
}

// analyticsEnvelope is the inbound decode shape of the Envelope v1 wrapper on
// the analytics topic. The data payload is left as a RawMessage and decoded per
// event_type. It is declared here (rather than imported from the outbound
// publisher) so this inbound adapter does not depend on an outbound adapter.
type analyticsEnvelope struct {
	EventId       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Source        string          `json:"source"`
	SchemaVersion int             `json:"schema_version"`
	Data          json.RawMessage `json:"data"`
}

// analyticsData is the union of fields the projecting event payloads carry.
// Each event_type populates the subset it needs.
type analyticsData struct {
	AssociateId string `json:"associate_id"`
	PathId      string `json:"path_id"`
	ToPathId    string `json:"to_path_id"`
}

// isProjectingEventType reports whether eventType is one of the
// utilization/staffing-moving events this consumer projects. The rest
// (ShiftPlanProposed, ShiftPlanCommitted, and anything unrecognized)
// are acknowledged without touching the read model or the processed
// set.
func isProjectingEventType(eventType string) bool {
	switch eventType {
	case "AssociateShiftStarted", "AssociateShiftEnded",
		"AssociateBreakStarted", "AssociateBreakEnded", "AssociateCertified",
		"LaborAssigned", "LaborReassigned", "PathUnderstaffed":
		return true
	default:
		return false
	}
}

// AnalyticsConsumer reads analytics events off the analytics topic and applies
// each to the labor ProjectionStore, exactly once per event_id despite Kafka's
// at-least-once delivery.
type AnalyticsConsumer struct {
	Reader     *segmentio.Reader
	Projection report.ProjectionStore
	Processed  ports.ProcessedEvents
	Logger     *slog.Logger

	// dlqWriter publishes a poison message (ADR-0022 §DLQ) to
	// <topic>.dlq after Run's retries are exhausted. nil in a struct
	// literal built directly (as every existing HandleMessage-level
	// unit test does — they never reach Run's retry/DLQ path) —
	// dlqPublish itself guards against a nil writer so those tests
	// keep compiling and passing unchanged.
	dlqWriter *segmentio.Writer
}

// NewAnalyticsConsumer constructs an AnalyticsConsumer reading topic from
// brokers under AnalyticsConsumerGroup. The dead-letter topic is always
// derived as topic+analyticsDLQTopicSuffix.
func NewAnalyticsConsumer(brokers []string, topic string, projection report.ProjectionStore, processed ports.ProcessedEvents, logger *slog.Logger) *AnalyticsConsumer {
	if logger == nil {
		logger = slog.Default()
	}
	reader := segmentio.NewReader(segmentio.ReaderConfig{
		Brokers: brokers,
		Topic:   topic,
		GroupID: AnalyticsConsumerGroup,
		// Start a brand-new consumer group at the EARLIEST offset. The analytics
		// projection must see the full history of the topic (it is a replayable
		// read model, not a live integration reaction), so a fresh projector — or
		// a backfill into a new group — reads from the beginning rather than
		// kafka-go's default of the latest offset, which would silently drop
		// every event produced before the group first committed an offset. Once
		// the group has committed offsets, those take precedence and this only
		// affects the first join.
		StartOffset: segmentio.FirstOffset,
	})
	return &AnalyticsConsumer{
		Reader:     reader,
		Projection: projection,
		Processed:  processed,
		Logger:     logger,
		dlqWriter: &segmentio.Writer{
			Addr:  segmentio.TCP(brokers...),
			Topic: topic + analyticsDLQTopicSuffix,
		},
	}
}

// Run reads and handles messages until ctx is cancelled or the reader returns a
// fatal error. Each fetched message is handled (with retry/DLQ, see
// handleFetchedMessage) before its offset is committed, so one poison message
// can never wedge the partition. Only a commit failure or a DLQ publish
// failure aborts the loop.
func (c *AnalyticsConsumer) Run(ctx context.Context) error {
	for {
		msg, err := c.Reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := c.handleFetchedMessage(ctx, msg); err != nil {
			return err
		}
	}
}

// Close releases the underlying Kafka reader and, if configured, the DLQ
// writer.
func (c *AnalyticsConsumer) Close() error {
	readerErr := c.Reader.Close()
	if c.dlqWriter == nil {
		return readerErr
	}
	return errors.Join(readerErr, c.dlqWriter.Close())
}

// Handle processes one consumed message inside a "kafka.consume <topic>" span
// whose parent is the producer's span, read from the message headers, WITHOUT
// the retry/DLQ/commit wrapping Run applies — kept exactly as before ADR-0022
// so the propagation can be tested without a live broker. Run itself calls
// handleFetchedMessage below, which wraps this same span+decode shape with
// retry, DLQ publish, and CommitMessages.
func (c *AnalyticsConsumer) Handle(ctx context.Context, msg segmentio.Message) error {
	ctx, span := c.startConsumeSpan(ctx, msg)
	defer span.End()

	if err := c.HandleMessage(ctx, msg.Value); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// handleFetchedMessage is Run's per-message handler (ADR-0022 §DLQ). It
// decodes the envelope once (a malformed/unparseable envelope is dead-
// lettered immediately — no amount of retrying changes a decode error), then
// for a projecting event type marks it processed AT MOST ONCE (retried on
// its own, since MarkProcessed can itself hit a transient infra error, but
// NEVER called again once it has successfully recorded an answer — see the
// package doc comment for why re-calling it on a retry would silently skip
// re-applying a genuinely failed apply), and finally retries ONLY the
// projection-apply step up to maxAnalyticsHandlerAttempts times. Either a
// success or an exhausted-retries DLQ publish commits the offset — a bad
// message must never block every event behind it on this partition. A
// commit failure or a DLQ publish failure is the only thing that still
// aborts Run's loop.
func (c *AnalyticsConsumer) handleFetchedMessage(ctx context.Context, msg segmentio.Message) error {
	ctx, span := c.startConsumeSpan(ctx, msg)
	defer span.End()

	env, decodeErr := decodeAnalyticsEnvelope(msg.Value)
	if decodeErr != nil {
		return c.deadLetterAndCommit(ctx, span, msg, decodeErr)
	}
	if !isProjectingEventType(env.EventType) {
		return c.commit(ctx, msg)
	}

	isNew, markErr := c.markProcessedWithRetry(ctx, env.EventId)
	if markErr != nil {
		return c.deadLetterAndCommit(ctx, span, msg, markErr)
	}
	if !isNew {
		return c.commit(ctx, msg)
	}

	if applyErr := c.applyWithRetry(ctx, env); applyErr != nil {
		return c.deadLetterAndCommit(ctx, span, msg, applyErr)
	}
	return c.commit(ctx, msg)
}

// deadLetterAndCommit records cause on span, logs it at ERROR level, and
// publishes msg to the dead-letter topic before committing its offset
// either way.
func (c *AnalyticsConsumer) deadLetterAndCommit(ctx context.Context, span trace.Span, msg segmentio.Message, cause error) error {
	span.RecordError(cause)
	span.SetStatus(codes.Error, cause.Error())
	c.Logger.ErrorContext(ctx, "analytics: exhausted retries, sending to dead-letter topic",
		"topic", msg.Topic, "dlq_topic", msg.Topic+analyticsDLQTopicSuffix, "attempts", maxAnalyticsHandlerAttempts, "error", cause)

	if dlqErr := c.dlqPublish(ctx, msg, cause); dlqErr != nil {
		return fmt.Errorf("analytics: publish to dead-letter topic: %w", dlqErr)
	}
	return c.commit(ctx, msg)
}

// markProcessedWithRetry retries Processed.MarkProcessed up to
// maxAnalyticsHandlerAttempts times with jittered backoff — a momentary
// infra hiccup on the idempotency-gate insert heals itself without ever
// reaching the DLQ. It is safe to retry: MarkProcessed has not yet
// SUCCEEDED (recorded an answer) on any of the failed attempts, so a retry
// after an error is a fresh, correct attempt, not a duplicate mark.
func (c *AnalyticsConsumer) markProcessedWithRetry(ctx context.Context, eventId string) (bool, error) {
	policy := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(analyticsRetryInitialInterval),
		backoff.WithMaxInterval(analyticsRetryMaxInterval),
	)
	bounded := backoff.WithContext(backoff.WithMaxRetries(policy, maxAnalyticsHandlerAttempts-1), ctx)

	return backoff.RetryNotifyWithData(func() (bool, error) {
		return c.Processed.MarkProcessed(ctx, eventId)
	}, bounded, nil)
}

// applyWithRetry retries applyEnvelope up to maxAnalyticsHandlerAttempts
// times with jittered backoff, bounded by ctx's own deadline/cancellation —
// a transient blip in the projection store heals itself without ever
// reaching the DLQ. This is called EXACTLY ONCE per message by
// handleFetchedMessage, after MarkProcessed has already succeeded with
// isNew=true, so retrying it here never risks a duplicate MarkProcessed
// call.
func (c *AnalyticsConsumer) applyWithRetry(ctx context.Context, env analyticsEnvelope) error {
	policy := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(analyticsRetryInitialInterval),
		backoff.WithMaxInterval(analyticsRetryMaxInterval),
	)
	bounded := backoff.WithContext(backoff.WithMaxRetries(policy, maxAnalyticsHandlerAttempts-1), ctx)

	return backoff.Retry(func() error {
		return c.applyEnvelope(ctx, env)
	}, bounded)
}

// dlqPublish writes the raw, unmodified message payload plus error context
// (as headers, so the raw body stays byte-identical for a manual replay tool)
// to the dead-letter topic. A nil dlqWriter (a struct literal built directly
// by a HandleMessage-level unit test, which never exercises this path) is a
// documented no-op rather than a nil-pointer panic.
func (c *AnalyticsConsumer) dlqPublish(ctx context.Context, msg segmentio.Message, cause error) error {
	if c.dlqWriter == nil {
		return nil
	}
	headers := append([]segmentio.Header{}, msg.Headers...)
	headers = append(headers,
		segmentio.Header{Key: "x-dlq-source-topic", Value: []byte(msg.Topic)},
		segmentio.Header{Key: "x-dlq-error", Value: []byte(cause.Error())},
		segmentio.Header{Key: "x-dlq-failed-at", Value: []byte(time.Now().UTC().Format(time.RFC3339))},
	)
	return c.dlqWriter.WriteMessages(ctx, segmentio.Message{
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,
	})
}

// commit acknowledges msg so it is never redelivered. Only a commit failure
// itself aborts Run's loop.
func (c *AnalyticsConsumer) commit(ctx context.Context, msg segmentio.Message) error {
	return c.Reader.CommitMessages(ctx, msg)
}

// startConsumeSpan extracts the producer's trace context from msg's headers
// and starts this adapter's "kafka.consume <topic>" child span.
func (c *AnalyticsConsumer) startConsumeSpan(ctx context.Context, msg segmentio.Message) (context.Context, trace.Span) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.TextMapCarrier(headerCarrier{headers: msg.Headers}))
	return otel.Tracer(tracerName).Start(ctx,
		"kafka.consume "+msg.Topic,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			semconv.MessagingSystemKafka,
			semconv.MessagingDestinationName(msg.Topic),
			semconv.MessagingOperationName("process"),
		),
	)
}

// decodeAnalyticsEnvelope unmarshals raw as an analyticsEnvelope. Split out
// from HandleMessage/applyEnvelope so Run's retry logic can decode ONCE
// (a malformed envelope is a permanent, not transient, failure — retrying
// it would never succeed) while still retrying the genuinely transient
// apply step separately.
func decodeAnalyticsEnvelope(raw []byte) (analyticsEnvelope, error) {
	var env analyticsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return analyticsEnvelope{}, fmt.Errorf("analytics: decode envelope: %w", err)
	}
	return env, nil
}

// applyEnvelope decodes env.Data and applies the matching projection method
// for env.EventType. Callers must have already confirmed env.EventType is a
// projecting type (isProjectingEventType) and that MarkProcessed reported
// isNew=true for env.EventId — this method does not re-check either.
func (c *AnalyticsConsumer) applyEnvelope(ctx context.Context, env analyticsEnvelope) error {
	var data analyticsData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return fmt.Errorf("analytics: decode data: %w", err)
	}

	switch env.EventType {
	case "AssociateShiftStarted":
		return c.Projection.ApplyShiftStarted(ctx, env.EventId, data.AssociateId, env.OccurredAt)
	case "AssociateShiftEnded":
		return c.Projection.ApplyShiftEnded(ctx, env.EventId, data.AssociateId, env.OccurredAt)
	case "AssociateBreakStarted":
		return c.Projection.ApplyBreakStarted(ctx, env.EventId, data.AssociateId, env.OccurredAt)
	case "AssociateBreakEnded":
		return c.Projection.ApplyBreakEnded(ctx, env.EventId, data.AssociateId, env.OccurredAt)
	case "AssociateCertified":
		return c.Projection.ApplyCertified(ctx, env.EventId, data.AssociateId, env.OccurredAt)
	case "LaborAssigned":
		return c.Projection.ApplyLaborAssigned(ctx, env.EventId, data.PathId, env.OccurredAt)
	case "LaborReassigned":
		return c.Projection.ApplyLaborReassigned(ctx, env.EventId, data.ToPathId, env.OccurredAt)
	case "PathUnderstaffed":
		return c.Projection.ApplyPathUnderstaffed(ctx, env.EventId, data.PathId, env.OccurredAt)
	default:
		return nil
	}
}

// HandleMessage decodes raw as an analyticsEnvelope and applies the matching
// projection method for its event_type. Event types outside the projection
// contract are ignored (and not marked processed). For a projecting event it
// dedupes on event_id via ProcessedEvents before applying, so a redelivery is
// a no-op. It is exported separately from Run so tests can feed raw envelopes
// without a live broker.
//
// This single call does its own one-shot decode + route + MarkProcessed +
// apply, unchanged from before ADR-0022 — Run's own handleFetchedMessage
// does NOT call this method in its retry loop (see the package doc comment
// for why re-calling MarkProcessed on every retry would be wrong); it
// reimplements the same steps with the mark and apply stages retried
// independently instead.
func (c *AnalyticsConsumer) HandleMessage(ctx context.Context, raw []byte) error {
	env, err := decodeAnalyticsEnvelope(raw)
	if err != nil {
		return err
	}

	if !isProjectingEventType(env.EventType) {
		return nil
	}

	isNew, err := c.Processed.MarkProcessed(ctx, env.EventId)
	if err != nil {
		return fmt.Errorf("analytics: mark processed: %w", err)
	}
	if !isNew {
		return nil
	}

	return c.applyEnvelope(ctx, env)
}
