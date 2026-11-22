package kafka

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	segmentio "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/trace"
)

// crlfLabourReader commits everything and fetches nothing.
type crlfLabourReader struct{}

func (crlfLabourReader) FetchMessage(ctx context.Context) (segmentio.Message, error) {
	<-ctx.Done()
	return segmentio.Message{}, ctx.Err()
}
func (crlfLabourReader) CommitMessages(context.Context, ...segmentio.Message) error { return nil }
func (crlfLabourReader) Close() error                                               { return nil }

// crlfLabourDLQ accepts every dead letter.
type crlfLabourDLQ struct{}

func (crlfLabourDLQ) WriteMessages(context.Context, ...segmentio.Message) error { return nil }
func (crlfLabourDLQ) Close() error                                              { return nil }

// TestLabourPlanConsumerLogsNoEventDerivedLineBreaks dead-letters a labour
// plan message whose key and value carry CR/LF with a use-case error quoting
// the event: the dead-letter log must carry no line break.
func TestLabourPlanConsumerLogsNoEventDerivedLineBreaks(t *testing.T) {
	rec := newCRLFRecorder()
	c := newLabourPlanConsumer(crlfLabourReader{}, crlfLabourDLQ{}, nil, slog.New(rec))
	ctx := context.Background()
	msg := segmentio.Message{Topic: "logsafe.topic", Key: []byte("key" + crlfForged), Value: []byte("value" + crlfForged)}
	if err := c.deadLetterLabourPlan(ctx, trace.SpanFromContext(ctx), msg, errors.New("shift refused"+crlfForged)); err != nil {
		t.Fatal(err)
	}
	rec.assertNoLineBreaks(t, 1)
}
