//go:build integration

package kafka_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"

	inboundkafka "github.com/claudioed/workforce-management/internal/adapters/inbound/kafka"
)

// blockingProjection parks every ApplyLaborAssigned call until the handler's
// context is cancelled, then reports that cancellation -- exactly what a real
// projection store does when the pod is told to stop mid-write. Every other
// projection method is the no-op recordingProjection's.
type blockingProjection struct {
	*recordingProjection
	entered chan struct{}
}

func (p *blockingProjection) ApplyLaborAssigned(ctx context.Context, _, _ string, _ time.Time) error {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestAnalyticsConsumer_ShutdownMidHandle_IsCleanAndDeadLettersNothing pins
// the shutdown contract: cancelling Run's context while a healthy message is
// being applied is a clean stop (Run returns nil), and that message is NOT
// dead-lettered -- its offset stays uncommitted, so it is redelivered on the
// next start and the processed-events dedupe absorbs the replay. Before the
// fix the retry wrapper's cancellation error was routed into
// deadLetterAndCommit as if the message were poison, and the failed DLQ
// publish on the cancelled context surfaced from Run as an error.
func TestAnalyticsConsumer_ShutdownMidHandle_IsCleanAndDeadLettersNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.6.1",
		tckafka.WithClusterID(fmt.Sprintf("wm-analytics-shutdown-itest-%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatalf("start Kafka container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminate Kafka container: %v", err)
		}
	})
	brokers, err := container.Brokers(ctx)
	if err != nil {
		t.Fatalf("resolve Kafka brokers: %v", err)
	}

	topic := fmt.Sprintf("warehouse.workforce.analytics.shutdown-itest-%d", time.Now().UnixNano())
	dlqTopic := topic + ".dlq"
	createAnalyticsTopic(t, ctx, brokers, topic)
	createAnalyticsTopic(t, ctx, brokers, dlqTopic)

	projection := &blockingProjection{recordingProjection: &recordingProjection{}, entered: make(chan struct{}, 1)}
	consumer := inboundkafka.NewAnalyticsConsumer(
		brokers, topic, "workforce-analytics-itest", projection, newFakeProcessedEvents(),
		slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	)
	defer func() { _ = consumer.Close() }()

	consumeCtx, consumeCancel := context.WithCancel(ctx)
	runErr := make(chan error, 1)
	go func() { runErr <- consumer.Run(consumeCtx) }()

	writer := &kafkago.Writer{Addr: kafkago.TCP(brokers...), Topic: topic}
	defer func() { _ = writer.Close() }()
	eventID := fmt.Sprintf("evt-shutdown-%d", time.Now().UnixNano())
	if err := writer.WriteMessages(ctx, kafkago.Message{
		Key:   []byte(eventID),
		Value: laborAssignedEventJSON(t, eventID, fmt.Sprintf("path-shutdown-%d", time.Now().UnixNano())),
	}); err != nil {
		t.Fatalf("publish LaborAssigned: %v", err)
	}

	select {
	case <-projection.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the consumer never started applying the message")
	}
	consumeCancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run after its context was cancelled mid-handle = %v, want nil (clean shutdown)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("consumer did not stop after context cancellation")
	}

	dlqReader := kafkago.NewReader(kafkago.ReaderConfig{Brokers: brokers, Topic: dlqTopic, Partition: 0, MinBytes: 1, MaxBytes: 10e6})
	defer func() { _ = dlqReader.Close() }()
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readCancel()
	if msg, err := dlqReader.ReadMessage(readCtx); err == nil {
		t.Fatalf("a healthy message was dead-lettered on shutdown: key=%q headers=%v", string(msg.Key), msg.Headers)
	}
}
