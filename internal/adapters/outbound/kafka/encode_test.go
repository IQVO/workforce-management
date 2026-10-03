package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	segmentio "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	"github.com/claudioed/workforce-management/internal/domain/shared"
	"github.com/claudioed/workforce-management/internal/domain/shiftplan"
)

// withRecordingSpan returns a ctx carrying a real (sampled) span and installs
// the W3C propagator so Encode's header injection has something to inject.
func withRecordingSpan(t *testing.T) context.Context {
	t.Helper()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "use-case")
	t.Cleanup(func() { span.End() })
	return ctx
}

func headerValue(headers []segmentio.Header, key string) (string, bool) {
	for _, h := range headers {
		if h.Key == key {
			return string(h.Value), true
		}
	}
	return "", false
}

func TestPublisherEncode_OneMessagePerLineOnIntegrationTopic(t *testing.T) {
	lines := []shiftplan.PathPlan{
		{PathId: "pack", PlannedHeads: 3, PlannedRate: 50, PlannedHours: 24},
		{PathId: "pick", PlannedHeads: 2, PlannedRate: 40, PlannedHours: 16},
	}
	sp := mustCommitPlan(t, lines, map[shared.PathId]int{"pack": 5, "pick": 5})
	events := sp.PullEvents()
	pub, fw := newTestPublisher(t, sp)

	encoded, err := pub.Encode(context.Background(), events...)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(fw.msgs) != 0 {
		t.Fatalf("Encode must never touch the writer, got %d writes", len(fw.msgs))
	}
	if len(encoded) != len(lines) {
		t.Fatalf("expected %d encoded messages, got %d", len(lines), len(encoded))
	}
	seen := map[string]bool{}
	for _, enc := range encoded {
		if enc.Topic != Topic {
			t.Errorf("topic = %q, want %q", enc.Topic, Topic)
		}
		if enc.EventType != cloudevents.TypeShiftPlanCommitted {
			t.Errorf("EventType = %q, want %q", enc.EventType, cloudevents.TypeShiftPlanCommitted)
		}
		if string(enc.Key) != "BLD1/SHIFT1" {
			t.Errorf("integration messages must be keyed by the ShiftPlan aggregate id (buildingId/shiftId), got %q", enc.Key)
		}
		_, data := assertCommittedLineEvent(t, enc.Value)
		if v, ok := headerValue(enc.Headers, "content-type"); !ok || v != cloudevents.MediaType {
			t.Errorf("content-type header = %q, want %q", v, cloudevents.MediaType)
		}
		seen[data.PathId] = true
	}
	for _, l := range lines {
		if !seen[string(l.PathId)] {
			t.Errorf("missing encoded message for %q", l.PathId)
		}
	}
}

func TestPublisherEncode_InjectsTraceHeadersWhenSpanActive(t *testing.T) {
	sp := mustCommitPlan(t, []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 1, PlannedRate: 10, PlannedHours: 8}}, map[shared.PathId]int{"pack": 5})
	events := sp.PullEvents()
	pub, _ := newTestPublisher(t, sp)

	encoded, err := pub.Encode(withRecordingSpan(t), events...)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) != 1 {
		t.Fatalf("expected 1 message, got %d", len(encoded))
	}
	if v, ok := headerValue(encoded[0].Headers, "traceparent"); !ok || v == "" {
		t.Fatalf("expected a traceparent header on the encoded message, got headers=%v", encoded[0].Headers)
	}
	if v, ok := headerValue(encoded[0].Headers, "content-type"); !ok || v != cloudevents.MediaType {
		t.Fatalf("expected the content-type header alongside the trace headers, got headers=%v", encoded[0].Headers)
	}
}

func TestPublisherEncode_MissingPlanFails(t *testing.T) {
	sp := mustCommitPlan(t, []shiftplan.PathPlan{{PathId: "pack", PlannedHeads: 1, PlannedRate: 10, PlannedHours: 8}}, map[shared.PathId]int{"pack": 5})
	sp.PullEvents()
	pub, _ := newTestPublisher(t, sp)

	_, err := pub.Encode(context.Background(), shared.NewShiftPlanCommitted(time.Now(), "OTHER", "S9"))
	if err == nil {
		t.Fatal("expected an error when the committed plan cannot be loaded")
	}
}

func TestAnalyticsPublisherEncode_TopicKeyTypeAndHeaders(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p := NewAnalyticsPublisherWithWriter(&fakeWriter{}, func() string { return "evt-1" })

	encoded, err := p.Encode(withRecordingSpan(t),
		shared.NewLaborAssigned(at, "a6", "pack"),
		unknownForEncode{},
		shared.NewPathUnderstaffed(at, "pack", 5, 3),
	)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) != 2 {
		t.Fatalf("expected the unknown event to be skipped (2 encoded), got %d", len(encoded))
	}
	first := encoded[0]
	if first.Topic != AnalyticsTopic || first.EventType != cloudevents.TypeLaborAssigned || string(first.Key) != "a6" {
		t.Errorf("unexpected first message: topic=%q type=%q key=%q", first.Topic, first.EventType, first.Key)
	}
	e, err := cloudevents.Decode(first.Value)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.ID() != "evt-1" || e.DataSchema() != "urn:warehouse:workforce-management:analytics:LaborAssigned:v1" || !e.Time().Equal(at) {
		t.Errorf("unexpected event %+v", e)
	}
	if _, ok := headerValue(first.Headers, "traceparent"); !ok {
		t.Errorf("expected traceparent header, got %v", first.Headers)
	}
	if v, ok := headerValue(first.Headers, "content-type"); !ok || v != cloudevents.MediaType {
		t.Errorf("expected content-type header, got %v", first.Headers)
	}
	if encoded[1].EventType != cloudevents.TypePathUnderstaffed || string(encoded[1].Key) != "pack" {
		t.Errorf("unexpected second message: type=%q key=%q", encoded[1].EventType, encoded[1].Key)
	}
}

type unknownForEncode struct{}

func (unknownForEncode) EventName() string     { return "Unknown" }
func (unknownForEncode) OccurredAt() time.Time { return time.Time{} }

type failingWriter struct{ err error }

func (f failingWriter) WriteMessages(context.Context, ...segmentio.Message) error { return f.err }

func TestRelaySink_SetsTopicPerMessageAndWritesOnce(t *testing.T) {
	fw := &fakeWriter{}
	sink := NewRelaySinkWithWriter(fw)
	err := sink.Send(context.Background(),
		Encoded{Topic: Topic, EventType: "ShiftPlanCommitted", Value: []byte(`{"a":1}`), Headers: []segmentio.Header{{Key: "traceparent", Value: []byte("00-x")}}},
		Encoded{Topic: AnalyticsTopic, EventType: "LaborAssigned", Key: []byte("a1"), Value: []byte(`{"b":2}`)},
	)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(fw.msgs) != 2 {
		t.Fatalf("expected 2 messages written, got %d", len(fw.msgs))
	}
	if fw.msgs[0].Topic != Topic || fw.msgs[1].Topic != AnalyticsTopic {
		t.Errorf("topics not applied per message: %q, %q", fw.msgs[0].Topic, fw.msgs[1].Topic)
	}
	if v, _ := headerValue(fw.msgs[0].Headers, "traceparent"); v != "00-x" {
		t.Errorf("persisted headers must be forwarded untouched, got %v", fw.msgs[0].Headers)
	}
	if string(fw.msgs[1].Key) != "a1" || string(fw.msgs[1].Value) != `{"b":2}` {
		t.Errorf("key/value not forwarded: %q %q", fw.msgs[1].Key, fw.msgs[1].Value)
	}
}

func TestRelaySink_RejectsTopiclessMessageAndPropagatesWriterError(t *testing.T) {
	fw := &fakeWriter{}
	if err := NewRelaySinkWithWriter(fw).Send(context.Background(), Encoded{EventType: "X", Value: []byte("v")}); err == nil {
		t.Fatal("expected an error for a message without a topic")
	}
	if len(fw.msgs) != 0 {
		t.Fatalf("nothing must be written when validation fails, got %d", len(fw.msgs))
	}
	if err := NewRelaySinkWithWriter(fw).Send(context.Background()); err != nil {
		t.Fatalf("empty send must be a no-op, got %v", err)
	}
	boom := errors.New("broker down")
	err := NewRelaySinkWithWriter(failingWriter{err: boom}).Send(context.Background(), Encoded{Topic: Topic, Value: []byte("v")})
	if !errors.Is(err, boom) {
		t.Fatalf("expected the writer error to be wrapped, got %v", err)
	}
}

func TestPublishers_WithWriterCtorsAndCloseOnFakes(t *testing.T) {
	if err := NewPublisherWithWriter(&fakeWriter{}, nil).Close(); err != nil {
		t.Errorf("close on a fake writer must be a no-op, got %v", err)
	}
	if err := NewAnalyticsPublisherWithWriter(&fakeWriter{}, NewEventID).Close(); err != nil {
		t.Errorf("close on a fake writer must be a no-op, got %v", err)
	}
	if err := NewRelaySinkWithWriter(&fakeWriter{}).Close(); err != nil {
		t.Errorf("close on a fake writer must be a no-op, got %v", err)
	}
}
