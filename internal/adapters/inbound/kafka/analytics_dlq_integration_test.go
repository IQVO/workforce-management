//go:build integration

package kafka_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"

	inboundkafka "github.com/claudioed/workforce-management/internal/adapters/inbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
)

// alwaysFailingProjection wraps a real report.ProjectionStore so
// ApplyLaborAssigned fails with a genuine infrastructure error for
// exactly poisonPathId, on EVERY call, while every other call is
// delegated unchanged -- letting one poison message coexist in the SAME
// test with a normal, successfully-projected message on the SAME
// partition. This mirrors order-management's
// alwaysFailingProcessedEventsFor pattern exactly, adapted to this
// consumer's own dependency shape.
type alwaysFailingProjection struct {
	*recordingProjection
	poisonPathId string
}

func (p *alwaysFailingProjection) ApplyLaborAssigned(ctx context.Context, eventId, pathId string, at time.Time) error {
	if pathId == p.poisonPathId {
		return fmt.Errorf("simulated poison-message infrastructure failure for path %s", pathId)
	}
	return p.recordingProjection.ApplyLaborAssigned(ctx, eventId, pathId, at)
}

// recordingProjection is a minimal, real (non-mocked) report.ProjectionStore
// that records every successful apply, so this test can assert the healthy
// message was actually applied without needing a live analytics database.
// Guarded by mu: ApplyLaborAssigned is called from the consumer's own Run
// goroutine while the test goroutine concurrently polls applied via
// appliedPathIds (waitForApplied) and the final assertion -- both must go
// through the same mutex.
type recordingProjection struct {
	mu      sync.Mutex
	applied []string
}

func (p *recordingProjection) appliedPathIds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.applied...)
}

func (p *recordingProjection) ApplyShiftStarted(context.Context, string, string, time.Time) error {
	return nil
}
func (p *recordingProjection) ApplyShiftEnded(context.Context, string, string, time.Time) error {
	return nil
}
func (p *recordingProjection) ApplyBreakStarted(context.Context, string, string, time.Time) error {
	return nil
}
func (p *recordingProjection) ApplyBreakEnded(context.Context, string, string, time.Time) error {
	return nil
}
func (p *recordingProjection) ApplyCertified(context.Context, string, string, time.Time) error {
	return nil
}
func (p *recordingProjection) ApplyLaborAssigned(_ context.Context, _ string, pathId string, _ time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applied = append(p.applied, pathId)
	return nil
}
func (p *recordingProjection) ApplyLaborReassigned(context.Context, string, string, time.Time) error {
	return nil
}
func (p *recordingProjection) ApplyPathUnderstaffed(context.Context, string, string, time.Time) error {
	return nil
}

// fakeProcessedEvents is an in-memory ports.ProcessedEvents, matching this
// package's own unit-test fake exactly (kept local to this _test package
// since integration tests build with a separate tag and cannot share
// unexported test helpers across build-tag boundaries).
type fakeProcessedEvents struct {
	seen map[string]bool
}

func newFakeProcessedEvents() *fakeProcessedEvents {
	return &fakeProcessedEvents{seen: map[string]bool{}}
}

func (p *fakeProcessedEvents) MarkProcessed(_ context.Context, eventId string) (bool, error) {
	if p.seen[eventId] {
		return false, nil
	}
	p.seen[eventId] = true
	return true, nil
}

// TestAnalyticsConsumer_PoisonMessage_GoesToDeadLetterTopicWithoutBlockingPartition
// is the ADR-0022 §DLQ acceptance test, run against a real Kafka broker
// (testcontainers, no skip / no hardcoded localhost): a LaborAssigned
// message whose projection application ALWAYS fails must, after exactly
// maxAnalyticsHandlerAttempts (3) in-process retries, land on
// "<topic>.dlq" with the raw original payload plus error context, and
// the consumer must commit past it and keep processing -- a
// well-formed message published right after the poison one must be
// handled without delay, proving the partition was never blocked on the
// one bad message.
func TestAnalyticsConsumer_PoisonMessage_GoesToDeadLetterTopicWithoutBlockingPartition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.6.1",
		tckafka.WithClusterID(fmt.Sprintf("wm-analytics-dlq-itest-%d", time.Now().UnixNano())))
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
	topic := fmt.Sprintf("warehouse.workforce.analytics.dlq-itest-%d", time.Now().UnixNano())
	dlqTopic := topic + ".dlq"
	createAnalyticsTopic(t, ctx, brokers, topic)
	createAnalyticsTopic(t, ctx, brokers, dlqTopic)

	poisonPathId := fmt.Sprintf("poison-path-%d", time.Now().UnixNano())
	poisonEventId := fmt.Sprintf("evt-dlq-poison-%d", time.Now().UnixNano())
	healthyPathId := fmt.Sprintf("healthy-path-%d", time.Now().UnixNano())

	projection := &alwaysFailingProjection{recordingProjection: &recordingProjection{}, poisonPathId: poisonPathId}
	processed := newFakeProcessedEvents()

	consumer := inboundkafka.NewAnalyticsConsumer(
		brokers, topic, projection, processed,
		slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	)
	defer func() { _ = consumer.Close() }()

	consumeCtx, consumeCancel := context.WithCancel(ctx)
	runErr := make(chan error, 1)
	go func() { runErr <- consumer.Run(consumeCtx) }()

	// Start reading the DLQ topic BEFORE publishing, so the poison
	// message's eventual dead-letter write is never missed to a race.
	dlqReader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:     brokers,
		Topic:       dlqTopic,
		GroupID:     fmt.Sprintf("dlq-reader-%d", time.Now().UnixNano()),
		StartOffset: kafkago.FirstOffset,
	})
	defer func() { _ = dlqReader.Close() }()

	writer := &kafkago.Writer{Addr: kafkago.TCP(brokers...), Topic: topic}
	defer func() { _ = writer.Close() }()
	if err := writer.WriteMessages(ctx, kafkago.Message{
		Key:   []byte(poisonEventId),
		Value: laborAssignedEventJSON(t, poisonEventId, poisonPathId),
	}); err != nil {
		t.Fatalf("publish poison LaborAssigned: %v", err)
	}

	// Assert the poison message lands on the DLQ topic with the raw
	// payload and error context, after the retry budget is exhausted.
	dlqCtx, dlqCancel := context.WithTimeout(ctx, 60*time.Second)
	defer dlqCancel()
	dlqMsg, err := dlqReader.ReadMessage(dlqCtx)
	if err != nil {
		t.Fatalf("read DLQ message: %v", err)
	}
	if string(dlqMsg.Key) != poisonEventId {
		t.Errorf("DLQ message key = %q, want %q (raw key preserved)", string(dlqMsg.Key), poisonEventId)
	}
	var dlqPayload map[string]any
	if err := json.Unmarshal(dlqMsg.Value, &dlqPayload); err != nil {
		t.Fatalf("DLQ message value is not the raw original JSON payload: %v", err)
	}
	if dlqPayload["id"] != poisonEventId {
		t.Errorf("DLQ payload id = %v, want %q -- payload must be byte-identical to the original for manual replay", dlqPayload["id"], poisonEventId)
	}
	assertHeader(t, dlqMsg.Headers, "x-dlq-source-topic", topic)
	if h := headerValue(dlqMsg.Headers, "x-dlq-error"); h == "" {
		t.Error("DLQ message missing x-dlq-error header with failure context")
	}
	if h := headerValue(dlqMsg.Headers, "x-dlq-failed-at"); h == "" {
		t.Error("DLQ message missing x-dlq-failed-at header")
	}

	// Now publish a well-formed message right after the poison one, and
	// confirm it is applied without delay -- proving the partition was
	// not blocked behind the poison message.
	if err := writer.WriteMessages(ctx, kafkago.Message{
		Key:   []byte(fmt.Sprintf("evt-dlq-good-%d", time.Now().UnixNano())),
		Value: laborAssignedEventJSON(t, fmt.Sprintf("evt-dlq-good-%d", time.Now().UnixNano()), healthyPathId),
	}); err != nil {
		t.Fatalf("publish well-formed LaborAssigned: %v", err)
	}
	waitForApplied(t, ctx, projection, healthyPathId)

	// A retired flat-envelope message is a deterministic poison message: it
	// must be dead-lettered byte-identical, never parsed (ADR-0026).
	legacy := []byte(`{"event_id":"legacy-1","event_type":"LaborAssigned","occurred_at":"2026-05-01T08:00:00Z","source":"workforce-management","schema_version":1,"data":{"associate_id":"a1","path_id":"legacy-path"}}`)
	if err := writer.WriteMessages(ctx, kafkago.Message{Key: []byte("legacy-1"), Value: legacy}); err != nil {
		t.Fatalf("publish legacy flat message: %v", err)
	}
	legacyDLQ, err := dlqReader.ReadMessage(dlqCtx)
	if err != nil {
		t.Fatalf("read legacy DLQ message: %v", err)
	}
	if string(legacyDLQ.Value) != string(legacy) {
		t.Errorf("legacy DLQ value = %s, want the raw original bytes", legacyDLQ.Value)
	}
	if h := headerValue(legacyDLQ.Headers, "x-dlq-error"); !strings.Contains(h, "CloudEvents") {
		t.Errorf("legacy DLQ x-dlq-error = %q, want a CloudEvents validation error", h)
	}

	consumeCancel()
	select {
	case err := <-runErr:
		if err != nil && ctx.Err() == nil {
			t.Errorf("run consumer: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("consumer did not stop after context cancellation")
	}

	// The healthy path's apply must be the ONLY recorded projection --
	// the poison message never got to apply anything, no matter how
	// many times it was retried.
	if applied := projection.appliedPathIds(); len(applied) != 1 || applied[0] != healthyPathId {
		t.Fatalf("applied = %v, want exactly [%q] (only the healthy message)", applied, healthyPathId)
	}
}

func laborAssignedEventJSON(t *testing.T, eventId, pathId string) []byte {
	t.Helper()
	b, err := cloudevents.New(cloudevents.Spec{
		ID:        eventId,
		Entity:    cloudevents.EntityAssignment,
		EventName: "LaborAssigned",
		Subject:   "a1",
		Time:      time.Now().UTC(),
		Stream:    cloudevents.StreamAnalytics,
		Version:   1,
		Data:      map[string]any{"associate_id": "a1", "path_id": pathId},
	})
	if err != nil {
		t.Fatalf("build cloudevent: %v", err)
	}
	return b
}

func waitForApplied(t *testing.T, ctx context.Context, projection *alwaysFailingProjection, wantPathId string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range projection.appliedPathIds() {
			if p == wantPathId {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled while waiting for %q to be applied", wantPathId)
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("path %q was never applied within the deadline", wantPathId)
}

func createAnalyticsTopic(t *testing.T, ctx context.Context, brokers []string, topic string) {
	t.Helper()
	conn, err := kafkago.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		t.Fatalf("dial Kafka controller: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.CreateTopics(kafkago.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}); err != nil {
		t.Fatalf("create topic %q: %v", topic, err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		partitions, err := conn.ReadPartitions(topic)
		if err == nil && len(partitions) == 1 && partitions[0].Leader.ID >= 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("topic %q did not obtain a partition leader", topic)
}

func assertHeader(t *testing.T, headers []kafkago.Header, key, want string) {
	t.Helper()
	got := headerValue(headers, key)
	if got != want {
		t.Errorf("header %q = %q, want %q", key, got, want)
	}
}

func headerValue(headers []kafkago.Header, key string) string {
	for _, h := range headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// testWriter adapts *testing.T.Log to an io.Writer, so the consumer's slog
// output surfaces in `go test -v` rather than being lost.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
