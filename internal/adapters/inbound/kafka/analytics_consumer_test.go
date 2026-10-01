package kafka_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	ce "github.com/cloudevents/sdk-go/v2/event"

	inboundkafka "github.com/claudioed/workforce-management/internal/adapters/inbound/kafka"
	"github.com/claudioed/workforce-management/internal/adapters/kafka/cloudevents"
)

// call captures one projection-store method invocation.
type call struct {
	method  string
	eventId string
	id      string // associateId or pathId, depending on the event
	at      time.Time
}

// fakeProjection records the calls the consumer makes so a test can assert the
// envelope was routed to the right method with the right fields.
type fakeProjection struct {
	calls []call
}

func (f *fakeProjection) ApplyShiftStarted(_ context.Context, eventId, associateId string, at time.Time) error {
	f.calls = append(f.calls, call{"shiftStarted", eventId, associateId, at})
	return nil
}
func (f *fakeProjection) ApplyShiftEnded(_ context.Context, eventId, associateId string, at time.Time) error {
	f.calls = append(f.calls, call{"shiftEnded", eventId, associateId, at})
	return nil
}
func (f *fakeProjection) ApplyBreakStarted(_ context.Context, eventId, associateId string, at time.Time) error {
	f.calls = append(f.calls, call{"breakStarted", eventId, associateId, at})
	return nil
}
func (f *fakeProjection) ApplyBreakEnded(_ context.Context, eventId, associateId string, at time.Time) error {
	f.calls = append(f.calls, call{"breakEnded", eventId, associateId, at})
	return nil
}
func (f *fakeProjection) ApplyCertified(_ context.Context, eventId, associateId string, at time.Time) error {
	f.calls = append(f.calls, call{"certified", eventId, associateId, at})
	return nil
}
func (f *fakeProjection) ApplyLaborAssigned(_ context.Context, eventId, pathId string, at time.Time) error {
	f.calls = append(f.calls, call{"laborAssigned", eventId, pathId, at})
	return nil
}
func (f *fakeProjection) ApplyLaborReassigned(_ context.Context, eventId, toPathId string, at time.Time) error {
	f.calls = append(f.calls, call{"laborReassigned", eventId, toPathId, at})
	return nil
}
func (f *fakeProjection) ApplyPathUnderstaffed(_ context.Context, eventId, pathId string, at time.Time) error {
	f.calls = append(f.calls, call{"pathUnderstaffed", eventId, pathId, at})
	return nil
}

// fakeProcessed is an in-memory ports.ProcessedEvents.
type fakeProcessed struct {
	seen map[string]bool
}

func newFakeProcessed() *fakeProcessed { return &fakeProcessed{seen: map[string]bool{}} }

func (p *fakeProcessed) MarkProcessed(_ context.Context, eventId string) (bool, error) {
	if p.seen[eventId] {
		return false, nil
	}
	p.seen[eventId] = true
	return true, nil
}

// cloudEvent builds the CloudEvents 1.0 structured-mode bytes the analytics
// publisher writes, via the same helper package production uses.
func cloudEvent(t *testing.T, eventId, entity, eventName string, at time.Time, data map[string]any) []byte {
	t.Helper()
	b, err := cloudevents.New(cloudevents.Spec{
		ID:        eventId,
		Entity:    entity,
		EventName: eventName,
		Subject:   "subject-1",
		Time:      at,
		Stream:    cloudevents.StreamAnalytics,
		Version:   1,
		Data:      data,
	})
	if err != nil {
		t.Fatalf("build cloudevent: %v", err)
	}
	return b
}

func TestAnalyticsConsumer_RoutesEachEventType(t *testing.T) {
	at := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		entity     string
		eventName  string
		data       map[string]any
		wantMethod string
		wantId     string
	}{
		{"shiftStarted", cloudevents.EntityAssociate, "AssociateShiftStarted", map[string]any{"associate_id": "a1"}, "shiftStarted", "a1"},
		{"shiftEnded", cloudevents.EntityAssociate, "AssociateShiftEnded", map[string]any{"associate_id": "a1"}, "shiftEnded", "a1"},
		{"breakStarted", cloudevents.EntityAssociate, "AssociateBreakStarted", map[string]any{"associate_id": "a1"}, "breakStarted", "a1"},
		{"breakEnded", cloudevents.EntityAssociate, "AssociateBreakEnded", map[string]any{"associate_id": "a1"}, "breakEnded", "a1"},
		{"certified", cloudevents.EntityAssociate, "AssociateCertified", map[string]any{"associate_id": "a1", "certification": "hazmat"}, "certified", "a1"},
		{"laborAssigned", cloudevents.EntityAssignment, "LaborAssigned", map[string]any{"associate_id": "a1", "path_id": "pack"}, "laborAssigned", "pack"},
		{"laborReassigned", cloudevents.EntityAssignment, "LaborReassigned", map[string]any{"associate_id": "a1", "from_path_id": "pick", "to_path_id": "pack"}, "laborReassigned", "pack"},
		{"pathUnderstaffed", cloudevents.EntityShiftPlan, "PathUnderstaffed", map[string]any{"path_id": "pack", "planned_heads": 5, "active_heads": 3}, "pathUnderstaffed", "pack"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proj := &fakeProjection{}
			processed := newFakeProcessed()
			c := &inboundkafka.AnalyticsConsumer{Projection: proj, Processed: processed, Logger: slog.Default()}

			raw := cloudEvent(t, "e-"+tt.name, tt.entity, tt.eventName, at, tt.data)
			if err := c.HandleMessage(context.Background(), raw); err != nil {
				t.Fatalf("HandleMessage: %v", err)
			}
			if len(proj.calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(proj.calls))
			}
			got := proj.calls[0]
			if got.method != tt.wantMethod {
				t.Errorf("method = %q, want %q", got.method, tt.wantMethod)
			}
			if got.id != tt.wantId {
				t.Errorf("id = %q, want %q", got.id, tt.wantId)
			}
			if !got.at.Equal(at) {
				t.Errorf("at = %v, want %v", got.at, at)
			}
		})
	}
}

func TestAnalyticsConsumer_Idempotent(t *testing.T) {
	at := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	proj := &fakeProjection{}
	processed := newFakeProcessed()
	c := &inboundkafka.AnalyticsConsumer{Projection: proj, Processed: processed, Logger: slog.Default()}

	raw := cloudEvent(t, "dup", cloudevents.EntityAssignment, "LaborAssigned", at, map[string]any{"associate_id": "a1", "path_id": "pack"})
	for range 2 {
		if err := c.HandleMessage(context.Background(), raw); err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
	}
	if len(proj.calls) != 1 {
		t.Fatalf("expected 1 apply for duplicate delivery, got %d", len(proj.calls))
	}
}

func TestAnalyticsConsumer_IgnoresNonProjectingEventType(t *testing.T) {
	proj := &fakeProjection{}
	processed := newFakeProcessed()
	c := &inboundkafka.AnalyticsConsumer{Projection: proj, Processed: processed, Logger: slog.Default()}

	for _, et := range []string{"ShiftPlanProposed", "ShiftPlanCommitted", "SomethingUnknown"} {
		raw := cloudEvent(t, "e-"+et, cloudevents.EntityShiftPlan, et, time.Now(), map[string]any{"path_id": "pack"})
		if err := c.HandleMessage(context.Background(), raw); err != nil {
			t.Fatalf("HandleMessage(%s): %v", et, err)
		}
	}
	if len(proj.calls) != 0 {
		t.Fatalf("expected non-projecting events to make no call, got %d", len(proj.calls))
	}
	// A non-projecting event must NOT be marked processed, so a later contract
	// change could reprocess it.
	if processed.seen["e-ShiftPlanCommitted"] {
		t.Error("non-projecting event should not be marked processed")
	}
}

// TestAnalyticsConsumer_RejectsLegacyFlatEnvelope proves a retired Envelope
// v1 message (event_id/event_type/occurred_at/schema_version) is NOT parsed:
// HandleMessage returns ErrNotCloudEvent (Run dead-letters exactly that error
// class), nothing is projected and nothing is marked processed (ADR-0026).
func TestAnalyticsConsumer_RejectsLegacyFlatEnvelope(t *testing.T) {
	proj := &fakeProjection{}
	processed := newFakeProcessed()
	c := &inboundkafka.AnalyticsConsumer{Projection: proj, Processed: processed, Logger: slog.Default()}

	legacy := []byte(`{"event_id":"legacy-1","event_type":"LaborAssigned","occurred_at":"2026-05-01T08:00:00Z","source":"workforce-management","schema_version":1,"data":{"associate_id":"a1","path_id":"pack"}}`)
	err := c.HandleMessage(context.Background(), legacy)
	if !errors.Is(err, cloudevents.ErrNotCloudEvent) {
		t.Fatalf("HandleMessage(legacy) err = %v, want ErrNotCloudEvent", err)
	}
	if len(proj.calls) != 0 || len(processed.seen) != 0 {
		t.Fatalf("legacy message must not be projected or marked processed: calls=%d seen=%v", len(proj.calls), processed.seen)
	}
}

// TestAnalyticsConsumer_DispatchesOnFullTypeOnly proves a valid CloudEvent
// whose type is a bare short name is ignored, not projected.
func TestAnalyticsConsumer_DispatchesOnFullTypeOnly(t *testing.T) {
	proj := &fakeProjection{}
	processed := newFakeProcessed()
	c := &inboundkafka.AnalyticsConsumer{Projection: proj, Processed: processed, Logger: slog.Default()}

	e := ce.New(ce.CloudEventsVersionV1)
	e.SetID("short-1")
	e.SetSource(cloudevents.Source)
	e.SetType("LaborAssigned")
	e.SetSubject("a1")
	e.SetTime(time.Now())
	if err := e.SetData("application/json", map[string]any{"associate_id": "a1", "path_id": "pack"}); err != nil {
		t.Fatalf("set data: %v", err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := c.HandleMessage(context.Background(), raw); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if len(proj.calls) != 0 {
		t.Fatalf("short type name must not be dispatched, got %d calls", len(proj.calls))
	}
}

// TestAnalyticsConsumer_UndecodableDataIsAnError proves a valid CloudEvent of
// a projecting type whose data does not decode is rejected before
// MarkProcessed (a deterministic poison message).
func TestAnalyticsConsumer_UndecodableDataIsAnError(t *testing.T) {
	proj := &fakeProjection{}
	processed := newFakeProcessed()
	c := &inboundkafka.AnalyticsConsumer{Projection: proj, Processed: processed, Logger: slog.Default()}

	e := ce.New(ce.CloudEventsVersionV1)
	e.SetID("bad-data")
	e.SetSource(cloudevents.Source)
	e.SetType(cloudevents.TypeLaborAssigned)
	e.SetSubject("a1")
	e.SetTime(time.Now())
	if err := e.SetData("application/json", []string{"not", "an", "object"}); err != nil {
		t.Fatalf("set data: %v", err)
	}
	raw, _ := json.Marshal(e)
	if err := c.HandleMessage(context.Background(), raw); err == nil {
		t.Fatal("expected an error for undecodable data")
	}
	if len(processed.seen) != 0 {
		t.Fatalf("undecodable message must not be marked processed, got %v", processed.seen)
	}
}
