//go:build integration

// Gap fix D27: the W3C trace context of the transaction that wrote an
// outbox row crosses the outbox to Kafka. This repo already stores it
// (outbox_events.headers, migration 000002: the Encoders Inject from the use
// case ctx, OutboxPublisher persists the headers, the relay hands them to
// RelaySink unchanged, and the analytics consumer Extracts before it starts
// its span). These tests pin that path end to end with a real Postgres and a
// real Kafka broker (testcontainers), never KAFKA_BROKERS or localhost.
package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/testcontainers/testcontainers-go"
	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	outboundkafka "github.com/claudioed/workforce-management/internal/adapters/outbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/outbound/postgres"
	"github.com/claudioed/workforce-management/internal/application/usecases"
)

// installW3CTracing installs a real SDK tracer provider and the W3C
// trace-context propagator globally (what telemetry.Setup does in
// production), restoring the previous globals when the test ends.
func installW3CTracing(t *testing.T) trace.Tracer {
	t.Helper()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return tp.Tracer("outbox-trace-itest")
}

// startTraceKafka boots a disposable broker and creates every destination
// topic, waiting for its partition leader, before the relay's first write.
// conn.CreateTopics returns before the broker has elected the leader, so on a
// cold broker a relay that wrote right away failed with "Unknown Topic Or
// Partition" (the TestOutboxTrace_TraceparentCrossesTheOutboxToKafka flake).
func startTraceKafka(t *testing.T, ctx context.Context, topics ...string) []string {
	t.Helper()
	container, err := tckafka.Run(ctx, "confluentinc/confluent-local:7.6.1",
		tckafka.WithClusterID("workforce-outbox-trace-itest"))
	if err != nil {
		t.Fatalf("start Kafka container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate Kafka container: %v", err)
		}
	})
	brokers, err := container.Brokers(ctx)
	if err != nil {
		t.Fatalf("resolve Kafka brokers: %v", err)
	}
	for _, topic := range topics {
		ensureTraceTopic(t, ctx, brokers[0], topic)
	}
	return brokers
}

// ensureTraceTopic creates topic (retrying the connection resets a just-booted
// broker gives; TopicAlreadyExists after such a reset is success) and then
// polls until its single partition has a leader, so the first produce cannot
// see "Unknown Topic Or Partition".
func ensureTraceTopic(t *testing.T, ctx context.Context, broker, topic string) {
	t.Helper()
	cfg := kafkago.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if lastErr = createAndCheckLeader(ctx, broker, cfg); lastErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("topic %q not ready on %s: %v", topic, broker, lastErr)
}

// createAndCheckLeader is one attempt: dial, create (idempotently), and
// report an error until the topic's partition has an elected leader.
func createAndCheckLeader(ctx context.Context, broker string, cfg kafkago.TopicConfig) error {
	conn, err := kafkago.DialContext(ctx, "tcp", broker)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.CreateTopics(cfg); err != nil && !errors.Is(err, kafkago.TopicAlreadyExists) {
		return err
	}
	partitions, err := conn.ReadPartitions(cfg.Topic)
	if err != nil {
		return err
	}
	if len(partitions) != cfg.NumPartitions || partitions[0].Leader.ID < 0 {
		return fmt.Errorf("partition leader not elected yet (%d partitions)", len(partitions))
	}
	return nil
}

func readOneMessage(t *testing.T, ctx context.Context, brokers []string, topic string) kafkago.Message {
	t.Helper()
	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers: brokers, Topic: topic, StartOffset: kafkago.FirstOffset, MaxWait: 2 * time.Second,
	})
	defer func() { _ = reader.Close() }()
	msg, err := reader.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read %s: %v", topic, err)
	}
	return msg
}

func kafkaHeaderValue(headers []kafkago.Header, key string) (string, bool) {
	for _, h := range headers {
		if h.Key == key {
			return string(h.Value), true
		}
	}
	return "", false
}

// storedTraceparent returns the traceparent persisted on the only outbox row
// for eventType ("" when there is none).
func storedTraceparent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventType string) string {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT headers FROM outbox_events WHERE event_type = $1`, eventType).Scan(&raw); err != nil {
		t.Fatalf("read stored headers: %v", err)
	}
	var hs []map[string]string
	if err := json.Unmarshal(raw, &hs); err != nil {
		t.Fatalf("decode stored headers %s: %v", raw, err)
	}
	for _, h := range hs {
		if h["key"] == "traceparent" {
			return h["value"]
		}
	}
	return ""
}

// relayOnceTo drains the outbox once onto a real RelaySink over brokers, on
// a span-less context: continuity must come from the stored row, not from
// the caller.
func relayOnceTo(t *testing.T, pool *pgxpool.Pool, brokers []string) {
	t.Helper()
	sink := outboundkafka.NewRelaySink(brokers)
	t.Cleanup(func() { _ = sink.Close() })
	relay := postgres.NewOutboxRelay(pool, sink, slog.Default(), postgres.WithMaxAttempts(20))
	if n, err := relay.RelayOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("relay: n=%d err=%v", n, err)
	}
}

// TestOutboxTrace_TraceparentCrossesTheOutboxToKafka: a use case runs inside
// a request span; the outbox row stores that span's traceparent; the relay
// (a later pass, no span of its own) sends it as a Kafka header; and a
// consumer extracting it with the global propagator lands on the request's
// trace and span.
func TestOutboxTrace_TraceparentCrossesTheOutboxToKafka(t *testing.T) {
	tracer := installW3CTracing(t)
	pool := outboxDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	brokers := startTraceKafka(t, ctx, outboundkafka.AnalyticsTopic)

	d := newDeps(pool)
	start := &usecases.StartAssociateShift{Associates: d.associates, Events: d.pub, Clock: fixedClock{t: time.Now().UTC()}, UnitOfWork: d.uow}
	reqCtx, reqSpan := tracer.Start(ctx, "POST /associates/{id}/shift-starts", trace.WithSpanKind(trace.SpanKindServer))
	if _, err := start.Execute(reqCtx, "A1", nil); err != nil {
		t.Fatalf("start shift: %v", err)
	}
	reqSpan.End()
	want := reqSpan.SpanContext()
	wantTraceparent := fmt.Sprintf("00-%s-%s-01", want.TraceID(), want.SpanID())

	if got := storedTraceparent(t, ctx, pool, cloudevents.TypeAssociateShiftStarted); got != wantTraceparent {
		t.Fatalf("outbox row traceparent = %q, want the request span's %q", got, wantTraceparent)
	}

	relayOnceTo(t, pool, brokers)

	msg := readOneMessage(t, ctx, brokers, outboundkafka.AnalyticsTopic)
	got, ok := kafkaHeaderValue(msg.Headers, "traceparent")
	if !ok || got != wantTraceparent {
		t.Fatalf("Kafka traceparent = %q (present=%v), want %q", got, ok, wantTraceparent)
	}
	if ct, _ := kafkaHeaderValue(msg.Headers, "content-type"); ct != cloudevents.MediaType {
		t.Fatalf("content-type header lost on the outbox path: %+v", msg.Headers)
	}

	consumerCtx := otel.GetTextMapPropagator().Extract(context.Background(), kafkaHeaderCarrier(msg.Headers))
	parent := trace.SpanContextFromContext(consumerCtx)
	if parent.TraceID() != want.TraceID() || parent.SpanID() != want.SpanID() || !parent.IsRemote() {
		t.Fatalf("consumer would parent on %s/%s (remote=%v), want the request span %s/%s",
			parent.TraceID(), parent.SpanID(), parent.IsRemote(), want.TraceID(), want.SpanID())
	}
}

// TestOutboxTrace_RowWithoutTraceHeadersRelaysUnchanged pins the other half:
// a row written with no trace context (no active span, or a row that
// predates trace capture, headers left at their '[]' default) relays with exactly its stored key
// and value and carries no traceparent a consumer could mis-parent onto.
func TestOutboxTrace_RowWithoutTraceHeadersRelaysUnchanged(t *testing.T) {
	installW3CTracing(t)
	pool := outboxDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const topic = "warehouse.workforce.itest-no-trace"
	brokers := startTraceKafka(t, ctx, topic)

	if _, err := pool.Exec(ctx, `
		INSERT INTO outbox_events (topic, event_type, key, value)
		VALUES ($1, 'com.example.Untraced', 'associate-untraced', '{}')`, topic); err != nil {
		t.Fatalf("insert untraced outbox row: %v", err)
	}

	relayOnceTo(t, pool, brokers)

	msg := readOneMessage(t, ctx, brokers, topic)
	if string(msg.Key) != "associate-untraced" || string(msg.Value) != `{}` {
		t.Fatalf("row relayed changed: key=%q value=%q", msg.Key, msg.Value)
	}
	if len(msg.Headers) != 0 {
		t.Fatalf("a row without stored headers must relay with none, got %+v", msg.Headers)
	}
}

// kafkaHeaderCarrier adapts kafka-go headers to the OTel text-map carrier,
// as the analytics consumer does (headerCarrier in
// internal/adapters/inbound/kafka).
type kafkaHeaderCarrier []kafkago.Header

func (c kafkaHeaderCarrier) Get(key string) string {
	v, _ := kafkaHeaderValue(c, key)
	return v
}

func (c kafkaHeaderCarrier) Set(string, string) {}

func (c kafkaHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for _, h := range c {
		keys = append(keys, h.Key)
	}
	return keys
}
