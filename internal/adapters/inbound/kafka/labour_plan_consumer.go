package kafka

import (
	"context"
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

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	"github.com/claudioed/workforce-management/internal/application/usecases"
	"github.com/claudioed/workforce-management/internal/domain/labourplan"
	"github.com/claudioed/workforce-management/internal/domain/shared"
)

// LabourPlanTopic is warehouse-planning's integration topic (its ADR 0023).
// It also carries CapacityPlan*, Shortage/Bottleneck, WorkloadForecastReceived
// and LabourPlanDriftDetected: the labour-plan consumer acts only on
// cloudevents.TypeLabourPlanCommitted and commits past every other type.
const LabourPlanTopic = "warehouse.warehouse-planning.events"

// LabourPlanDLQTopicSuffix names the dead-letter topic relative to the
// consumed topic (<topic>.dlq), the same convention as the analytics
// consumer and the fleet's other durable consumers.
const LabourPlanDLQTopicSuffix = ".dlq"

// maxLabourPlanAttempts bounds the apply (claim + upsert, one unit of work):
// 1 attempt plus 2 retries. Retrying the whole unit is safe because a failed
// attempt rolls its claim back with it.
const maxLabourPlanAttempts = 3

// labourPlanReader is the subset of *segmentio.Reader the consumer uses, so
// tests can drive the loop without a broker.
type labourPlanReader interface {
	FetchMessage(ctx context.Context) (segmentio.Message, error)
	CommitMessages(ctx context.Context, msgs ...segmentio.Message) error
	Close() error
}

// labourPlanDLQ is the subset of *segmentio.Writer the dead-letter path uses.
type labourPlanDLQ interface {
	WriteMessages(ctx context.Context, msgs ...segmentio.Message) error
	Close() error
}

// labourPlanCommittedData is the CloudEvents data of LabourPlanCommitted
// (warehouse-planning ADR 0023 decision 2, snake_case). installed_stations
// and the optional shift_start are part of the contract but not kept here
// (ADR 0043 decision 1 stores heads, rate, hours, source and version), so
// they are not decoded. PlannedHeads is a pointer: a missing value is a
// broken contract, never zero heads.
type labourPlanCommittedData struct {
	SiteId           string  `json:"site_id"`
	ShiftId          string  `json:"shift_id"`
	PathId           string  `json:"path_id"`
	PlannedHeads     *int    `json:"planned_heads"`
	RateUnitsPerHour float64 `json:"rate_units_per_hour"`
	Hours            float64 `json:"hours"`
	HeadsSource      string  `json:"heads_source"`
	Version          int     `json:"version"`
}

// labourPlanMessage is one decoded message: relevant is false for every type
// other than LabourPlanCommitted.
type labourPlanMessage struct {
	id       string
	req      labourplan.Requirement
	relevant bool
}

// LabourPlanConsumer keeps the local copy of warehouse-planning's
// LabourPlanCommitted (ADR 0043 decision 1) on a stable, shared consumer group
// (LABOUR_PLAN_CONSUMER_GROUP, no default). Each message is handled before its
// offset is committed (at-least-once); the claim on the CloudEvents id makes a
// redelivery a no-op. A message that is not a valid CloudEvents event or that
// breaks the published contract (missing site/shift/path or heads, negative
// heads, version < 1) is dead-lettered to <topic>.dlq and committed past: the
// repo's convention for its durable consumers (the analytics projector), since
// a skipped requirement would silently leave the copy stale.
type LabourPlanConsumer struct {
	reader labourPlanReader
	dlq    labourPlanDLQ
	record *usecases.RecordLabourRequirement
	logger *slog.Logger
}

// NewLabourPlanConsumer reads topic from brokers under group, starting a new
// group at the earliest offset so a fresh copy sees the whole history. An
// empty group is a programming error: the composition root refuses to boot
// without LABOUR_PLAN_CONSUMER_GROUP.
func NewLabourPlanConsumer(brokers []string, topic, group string, record *usecases.RecordLabourRequirement, logger *slog.Logger) *LabourPlanConsumer {
	reader := segmentio.NewReader(segmentio.ReaderConfig{
		Brokers:     brokers,
		Topic:       topic,
		GroupID:     group,
		StartOffset: segmentio.FirstOffset,
	})
	return newLabourPlanConsumer(reader, newLabourPlanDLQWriter(brokers, topic+LabourPlanDLQTopicSuffix), record, logger)
}

func newLabourPlanConsumer(reader labourPlanReader, dlq labourPlanDLQ, record *usecases.RecordLabourRequirement, logger *slog.Logger) *LabourPlanConsumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &LabourPlanConsumer{reader: reader, dlq: dlq, record: record, logger: logger}
}

// newLabourPlanDLQWriter builds the synchronous dead-letter writer, with the
// same settings and reasons as the analytics one (auto-create the rarely
// written topic, 10ms batch, acks from every in-sync replica, ADR-0031).
func newLabourPlanDLQWriter(brokers []string, topic string) *segmentio.Writer {
	return &segmentio.Writer{
		Addr:                   segmentio.TCP(brokers...),
		Topic:                  topic,
		AllowAutoTopicCreation: true,
		BatchTimeout:           10 * time.Millisecond,
		RequiredAcks:           dlqRequiredAcks,
	}
}

// Run consumes until ctx is cancelled (a clean stop, nil) or a commit or
// dead-letter write fails. A message interrupted by cancellation is left
// uncommitted and redelivered on the next start.
func (c *LabourPlanConsumer) Run(ctx context.Context) error {
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := c.handleFetched(ctx, msg); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// Close releases the reader and the dead-letter writer.
func (c *LabourPlanConsumer) Close() error {
	return errors.Join(c.reader.Close(), c.dlq.Close())
}

// handleFetched decodes msg once, ignores every other type, applies a
// LabourPlanCommitted with bounded retry and commits the offset. A decode or
// contract failure is dead-lettered at once (no retry can fix it); an apply
// that keeps failing is dead-lettered after its retries, unless the failure
// is only the shutdown cancelling ctx.
func (c *LabourPlanConsumer) handleFetched(ctx context.Context, msg segmentio.Message) error {
	ctx, span := startLabourPlanSpan(ctx, msg)
	defer span.End()

	m, err := decodeLabourPlan(msg.Value)
	if err != nil {
		return c.deadLetterLabourPlan(ctx, span, msg, err)
	}
	if !m.relevant {
		return c.reader.CommitMessages(ctx, msg)
	}
	applied, err := c.recordWithRetry(ctx, m)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // shutting down: never dead-letter a message only cancellation interrupted
		}
		return c.deadLetterLabourPlan(ctx, span, msg, err)
	}
	c.logger.DebugContext(ctx, "labour plan: message handled", "event_id", sanitizeLog(m.id), "applied", applied,
		"site_code", sanitizeLog(m.req.Key.SiteCode), "shift_id", sanitizeLog(m.req.Key.ShiftId), "path_id", sanitizeLog(string(m.req.Key.PathId)), "version", m.req.Version)
	return c.reader.CommitMessages(ctx, msg)
}

// recordWithRetry runs the claim + upsert unit of work up to
// maxLabourPlanAttempts times with jittered backoff, bounded by ctx.
func (c *LabourPlanConsumer) recordWithRetry(ctx context.Context, m labourPlanMessage) (bool, error) {
	policy := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(analyticsRetryInitialInterval),
		backoff.WithMaxInterval(analyticsRetryMaxInterval),
	)
	bounded := backoff.WithContext(backoff.WithMaxRetries(policy, maxLabourPlanAttempts-1), ctx)
	return backoff.RetryWithData(func() (bool, error) {
		applied, err := c.record.Execute(ctx, m.id, m.req)
		if errors.Is(err, labourplan.ErrInvalidRequirement) {
			return false, backoff.Permanent(err)
		}
		return applied, err
	}, bounded)
}

// deadLetterLabourPlan records cause, writes the raw message (plus error
// headers) to the dead-letter topic and commits past it.
func (c *LabourPlanConsumer) deadLetterLabourPlan(ctx context.Context, span trace.Span, msg segmentio.Message, cause error) error {
	span.RecordError(cause)
	span.SetStatus(codes.Error, cause.Error())
	c.logger.ErrorContext(ctx, "labour plan: sending message to the dead-letter topic",
		"topic", msg.Topic, "dlq_topic", msg.Topic+LabourPlanDLQTopicSuffix, "offset", msg.Offset, "partition", msg.Partition, "error", sanitizeErr(cause))
	headers := append([]segmentio.Header{}, msg.Headers...)
	headers = append(headers,
		segmentio.Header{Key: "x-dlq-source-topic", Value: []byte(msg.Topic)},
		segmentio.Header{Key: "x-dlq-error", Value: []byte(cause.Error())},
		segmentio.Header{Key: "x-dlq-failed-at", Value: []byte(time.Now().UTC().Format(time.RFC3339))},
	)
	if err := c.dlq.WriteMessages(ctx, segmentio.Message{Key: msg.Key, Value: msg.Value, Headers: headers}); err != nil {
		return fmt.Errorf("labour plan: publish to dead-letter topic: %w", err)
	}
	return c.reader.CommitMessages(ctx, msg)
}

// decodeLabourPlan decodes raw as a CloudEvents 1.0 event. Any other type is
// returned as not relevant; a LabourPlanCommitted is mapped to a validated
// labourplan.Requirement (wire site_id stored as site_code, ADR 0035).
func decodeLabourPlan(raw []byte) (labourPlanMessage, error) {
	e, err := cloudevents.Decode(raw)
	if err != nil {
		return labourPlanMessage{}, fmt.Errorf("labour plan: decode event: %w", err)
	}
	if e.Type() != cloudevents.TypeLabourPlanCommitted {
		return labourPlanMessage{}, nil
	}
	var d labourPlanCommittedData
	if err := e.DataAs(&d); err != nil {
		return labourPlanMessage{}, fmt.Errorf("%w: decode %s data: %w", labourplan.ErrInvalidRequirement, e.Type(), err)
	}
	if d.PlannedHeads == nil {
		return labourPlanMessage{}, fmt.Errorf("%w: planned_heads is missing", labourplan.ErrInvalidRequirement)
	}
	req := labourplan.Requirement{
		Key:              labourplan.Key{SiteCode: d.SiteId, ShiftId: d.ShiftId, PathId: shared.PathId(d.PathId)},
		PlannedHeads:     *d.PlannedHeads,
		RateUnitsPerHour: d.RateUnitsPerHour,
		Hours:            d.Hours,
		HeadsSource:      labourplan.HeadsSource(d.HeadsSource),
		Version:          d.Version,
	}
	if err := req.Validate(); err != nil {
		return labourPlanMessage{}, err
	}
	return labourPlanMessage{id: e.ID(), req: req, relevant: true}, nil
}

// startLabourPlanSpan continues the producer's trace from msg's headers in a
// "kafka.consume <topic>" consumer span.
func startLabourPlanSpan(ctx context.Context, msg segmentio.Message) (context.Context, trace.Span) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.TextMapCarrier(headerCarrier{headers: msg.Headers}))
	return otel.Tracer(tracerName).Start(ctx, "kafka.consume "+msg.Topic,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			semconv.MessagingSystemKafka,
			semconv.MessagingDestinationName(msg.Topic),
			semconv.MessagingOperationName("process"),
		),
	)
}
