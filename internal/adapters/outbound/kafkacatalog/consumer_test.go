package kafkacatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	ce "github.com/cloudevents/sdk-go/v2/event"
	kafkago "github.com/segmentio/kafka-go"

	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
	"github.com/claudioed/workforce-management/internal/domain/pathcatalog"
)

// fakeReader replays a fixed sequence of messages, then blocks until ctx
// is cancelled -- mirrors a real Kafka reader that has caught up and is
// now waiting for new messages.
type fakeReader struct {
	messages []kafkago.Message
	pos      int
	closed   bool
}

func (r *fakeReader) ReadMessage(ctx context.Context) (kafkago.Message, error) {
	if r.pos < len(r.messages) {
		m := r.messages[r.pos]
		r.pos++
		return m, nil
	}
	<-ctx.Done()
	return kafkago.Message{}, ctx.Err()
}

func (r *fakeReader) Close() error {
	r.closed = true
	return nil
}

func eventMsg(t *testing.T, partition int, offset int64, eventType string, data any) kafkago.Message {
	t.Helper()
	return cloudEventMsg(t, partition, offset, fmt.Sprintf("p%d-o%d", partition, offset), eventType, data)
}

// cloudEventMsg builds a CloudEvents 1.0 structured-mode message as the
// upstream producer writes it (source/type/subject/time/dataschema set).
func cloudEventMsg(t *testing.T, partition int, offset int64, id, eventType string, data any) kafkago.Message {
	t.Helper()
	e := ce.New(ce.CloudEventsVersionV1)
	e.SetID(id)
	e.SetSource("/warehouse/process-path-management")
	e.SetType(eventType)
	e.SetSubject("subject-1")
	e.SetTime(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	e.SetDataSchema("urn:warehouse:process-path-management:events:ProcessPath:v1")
	if err := e.SetData("application/json", data); err != nil {
		t.Fatalf("set data: %v", err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal cloudevent: %v", err)
	}
	return kafkago.Message{Partition: partition, Offset: offset, Value: raw}
}

func newTestConsumer(reader Reader, target targetOffsets) *Consumer {
	c := &Consumer{
		Logger:  slog.New(slog.DiscardHandler),
		Reader:  reader,
		paths:   make(map[string]pathcatalog.PathDefinition),
		readyCh: make(chan struct{}),
		target:  target,
	}
	if len(target) == 0 {
		c.markReady()
	}
	return c
}

func TestConsumer_NoTargetOffsets_IsReadyImmediately(t *testing.T) {
	c := newTestConsumer(&fakeReader{}, targetOffsets{})
	if !c.Ready() {
		t.Fatal("expected a consumer with no readiness target to be ready immediately")
	}
}

func TestConsumer_Run_BecomesReadyAfterCatchingUpSinglePartition(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeProcessPathCreated, pathData{PathId: "PICK", MatchPrefix: "pick", Direct: true, RequiredCapabilities: []string{"pick"}}),
			eventMsg(t, 0, 1, cloudevents.TypeProcessPathCreated, pathData{PathId: "PACK", MatchPrefix: "pack", Direct: true, RequiredCapabilities: []string{"pack"}}),
		},
	}
	// target[0] = 2 means "caught up once offset 1 has been processed"
	// (last is exclusive: 2 messages at offsets 0 and 1).
	c := newTestConsumer(reader, targetOffsets{0: 2})

	if c.Ready() {
		t.Fatal("expected not ready before Run starts")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = c.Run(ctx)
		close(done)
	}()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}
	cancel()
	<-done

	if _, err := c.Lookup("pick"); err != nil {
		t.Fatalf("expected PICK to be looked up successfully, got: %v", err)
	}
	if _, err := c.Lookup("pack"); err != nil {
		t.Fatalf("expected PACK to be looked up successfully, got: %v", err)
	}
}

func TestConsumer_MultiPartition_ReadyOnlyAfterBothCaughtUp(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeProcessPathCreated, pathData{PathId: "PICK", MatchPrefix: "pick", Direct: true, RequiredCapabilities: []string{"pick"}}),
			// Partition 1 not yet caught up (target[1]=1, need offset 0).
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 1, 1: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err == nil {
		t.Fatal("expected WaitReady to time out -- partition 1 never caught up")
	}
}

func TestConsumer_Deactivated_RemovesPathFromCache(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeProcessPathCreated, pathData{PathId: "PICK", MatchPrefix: "pick", Direct: true, RequiredCapabilities: []string{"pick"}}),
			eventMsg(t, 0, 1, cloudevents.TypeProcessPathDeactivated, pathData{PathId: "PICK"}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 2})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}

	if _, err := c.Lookup("pick"); err == nil {
		t.Fatal("expected PICK to be unknown after deactivation")
	}
}

func TestConsumer_Revised_UpdatesMatchPrefix(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeProcessPathCreated, pathData{PathId: "PICK", MatchPrefix: "pick", Direct: true, RequiredCapabilities: []string{"pick"}}),
			eventMsg(t, 0, 1, cloudevents.TypeProcessPathUpdated, pathData{PathId: "PICK", MatchPrefix: "pick-zone-a", Direct: true, RequiredCapabilities: []string{"pick", "hazmat"}}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 2})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}

	def, err := c.Lookup("pick-zone-a")
	if err != nil {
		t.Fatalf("expected pick-zone-a to resolve after revision, got: %v", err)
	}
	if len(def.RequiredCapabilities) != 2 {
		t.Fatalf("expected 2 required capabilities after revision, got %d", len(def.RequiredCapabilities))
	}
}

// TestConsumer_DecodesDestinationLocationRole proves the previously-dropped
// destination_location_role field is decoded and carried into the local
// PathDefinition, so a caller reading it back via Lookup sees it.
func TestConsumer_DecodesDestinationLocationRole(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeProcessPathCreated, pathData{
				PathId: "PACK", MatchPrefix: "pack", Direct: true,
				RequiredCapabilities: []string{"pack"}, DestinationLocationRole: "Drop",
			}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}

	def, err := c.Lookup("pack")
	if err != nil {
		t.Fatalf("expected pack to resolve, got: %v", err)
	}
	if def.DestinationLocationRole != "Drop" {
		t.Fatalf("expected DestinationLocationRole %q, got %q", "Drop", def.DestinationLocationRole)
	}
}

// TestConsumer_NoDestinationLocationRole_DecodesEmpty proves the field is
// simply "" (not an error) when the producer omitted it -- the fully valid
// DestinationLocationRoleUnset case on process-path-management's side.
func TestConsumer_NoDestinationLocationRole_DecodesEmpty(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, cloudevents.TypeProcessPathCreated, pathData{
				PathId: "PICK", MatchPrefix: "pick", Direct: true,
				RequiredCapabilities: []string{"pick"},
			}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout, got: %v", err)
	}

	def, err := c.Lookup("pick")
	if err != nil {
		t.Fatalf("expected pick to resolve, got: %v", err)
	}
	if def.DestinationLocationRole != "" {
		t.Fatalf("expected empty DestinationLocationRole, got %q", def.DestinationLocationRole)
	}
}

func TestConsumer_UnknownEventType_IsIgnored(t *testing.T) {
	reader := &fakeReader{
		messages: []kafkago.Message{
			eventMsg(t, 0, 0, "SomeFutureEventType", map[string]any{}),
		},
	}
	c := newTestConsumer(reader, targetOffsets{0: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready before timeout even for an unrecognized event type, got: %v", err)
	}
}

// TestConsumer_LegacyFlatEnvelope_IsRejectedNotParsed proves the retired flat
// envelope (event_type/event_id/occurred_at) is NOT parsed: handle reports
// ErrNotCloudEvent, the catalogue stays empty, and Run still skips past it to
// readiness instead of wedging (ADR-0026).
func TestConsumer_LegacyFlatEnvelope_IsRejectedNotParsed(t *testing.T) {
	legacy := kafkago.Message{Partition: 0, Offset: 0, Value: []byte(`{"event_id":"e1","event_type":"ProcessPathCreated","occurred_at":"2026-09-01T00:00:00Z","source":"process-path-management","data":{"path_id":"PICK","match_prefix":"pick"}}`)}
	c := newTestConsumer(&fakeReader{messages: []kafkago.Message{legacy}}, targetOffsets{0: 1})

	if err := c.handle(legacy); !errors.Is(err, cloudevents.ErrNotCloudEvent) {
		t.Fatalf("handle(legacy) err = %v, want ErrNotCloudEvent", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("expected Ready after skipping a legacy message, got: %v", err)
	}
	if ids := c.Ids(); len(ids) != 0 {
		t.Fatalf("legacy message must not populate the catalogue, got %v", ids)
	}
}

// TestConsumer_ShortTypeName_IsIgnored proves dispatch is on the FULL type:
// a valid CloudEvent whose type is the bare short name is not acted on.
func TestConsumer_ShortTypeName_IsIgnored(t *testing.T) {
	msg := eventMsg(t, 0, 0, "ProcessPathCreated", pathData{PathId: "PICK", MatchPrefix: "pick"})
	c := newTestConsumer(&fakeReader{}, targetOffsets{})
	if err := c.handle(msg); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if ids := c.Ids(); len(ids) != 0 {
		t.Fatalf("short type name must not be dispatched, got %v", ids)
	}
}
